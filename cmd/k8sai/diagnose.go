// diagnose.go — AI-powered namespace health diagnosis.
//
// k8sai diagnose -n production
// k8sai diagnose --deployment payments-api -n production
//
// Collects pod status, events, resource pressure, HPA state, and
// recent restarts, then asks Claude to produce a plain-English diagnosis
// with prioritized remediation steps.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var diagnosisNamespace string
var diagnosisDeployment string

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "AI-powered namespace or deployment health diagnosis",
	Long: `Collect pod state, events, HPA status, and recent restarts for a namespace
(or a specific deployment) and produce a plain-English diagnosis with
prioritized remediation steps from Claude.

Examples:
  k8sai diagnose -n production
  k8sai diagnose --deployment payments-api -n production`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDiagnose(diagnosisNamespace, diagnosisDeployment)
	},
}

func init() {
	diagnoseCmd.Flags().StringVarP(&diagnosisNamespace, "namespace", "n", "default", "Kubernetes namespace to diagnose")
	diagnoseCmd.Flags().StringVar(&diagnosisDeployment, "deployment", "", "Diagnose a specific Deployment only")
	rootCmd.AddCommand(diagnoseCmd)
}

func runDiagnose(namespace, deployment string) error {
	fmt.Printf("Diagnosing namespace %q", namespace)
	if deployment != "" {
		fmt.Printf(" (deployment: %s)", deployment)
	}
	fmt.Println("...\n")

	cs, err := buildClientset()
	if err != nil {
		return fmt.Errorf("connect to cluster: %w", err)
	}
	ctx := context.Background()

	snapshot, err := collectNamespaceSnapshot(ctx, cs, namespace, deployment)
	if err != nil {
		return fmt.Errorf("collect snapshot: %w", err)
	}

	fmt.Println(snapshot.text())
	fmt.Println("\n--- Sending to Claude for analysis ---\n")

	diagnosis, err := analyzeWithClaude(snapshot)
	if err != nil {
		return fmt.Errorf("AI analysis: %w", err)
	}

	fmt.Println(diagnosis)
	return nil
}

// ── Snapshot collection ───────────────────────────────────────────────────────

type namespaceSnapshot struct {
	namespace   string
	deployment  string
	collectedAt time.Time

	totalPods    int
	runningPods  int
	failingPods  []podSummary
	restartLeaders []podSummary // pods with high restart counts

	deployments  []deploymentSummary
	hpas         []hpaSummary
	recentEvents []eventSummary
}

type podSummary struct {
	name     string
	phase    string
	reason   string
	restarts int
	node     string
	age      time.Duration
}

type deploymentSummary struct {
	name     string
	desired  int32
	ready    int32
	updated  int32
	available int32
}

type hpaSummary struct {
	name        string
	minReplicas int32
	maxReplicas int32
	current     int32
	desired     int32
	cpuTarget   int32
	cpuCurrent  int32
}

type eventSummary struct {
	kind    string
	name    string
	reason  string
	message string
	count   int32
	age     time.Duration
}

func collectNamespaceSnapshot(ctx context.Context, cs kubernetes.Interface, namespace, filterDeployment string) (*namespaceSnapshot, error) {
	s := &namespaceSnapshot{
		namespace:   namespace,
		deployment:  filterDeployment,
		collectedAt: time.Now(),
	}

	// ── Pods ─────────────────────────────────────────────────────────────────
	podList, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	s.totalPods = len(podList.Items)
	for _, pod := range podList.Items {
		// Filter by deployment if specified
		if filterDeployment != "" {
			owned := false
			for _, ref := range pod.OwnerReferences {
				if strings.Contains(ref.Name, filterDeployment) {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
		}

		if pod.Status.Phase == corev1.PodRunning {
			s.runningPods++
		}

		restarts := int32(0)
		for _, cs := range pod.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}

		age := time.Since(pod.CreationTimestamp.Time)
		ps := podSummary{
			name:     pod.Name,
			phase:    string(pod.Status.Phase),
			restarts: int(restarts),
			node:     pod.Spec.NodeName,
			age:      age,
		}

		// Extract termination reason
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				ps.reason = cs.State.Waiting.Reason
			} else if cs.LastTerminationState.Terminated != nil {
				ps.reason = cs.LastTerminationState.Terminated.Reason
			}
		}

		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
			s.failingPods = append(s.failingPods, ps)
		}
		if restarts >= 3 {
			s.restartLeaders = append(s.restartLeaders, ps)
		}
	}

	// Sort restart leaders descending
	sort.Slice(s.restartLeaders, func(i, j int) bool {
		return s.restartLeaders[i].restarts > s.restartLeaders[j].restarts
	})
	if len(s.restartLeaders) > 10 {
		s.restartLeaders = s.restartLeaders[:10]
	}

	// ── Deployments ───────────────────────────────────────────────────────────
	var deployList *appsv1.DeploymentList
	if filterDeployment != "" {
		d, err := cs.AppsV1().Deployments(namespace).Get(ctx, filterDeployment, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get deployment %s: %w", filterDeployment, err)
		}
		deployList = &appsv1.DeploymentList{Items: []appsv1.Deployment{*d}}
	} else {
		deployList, err = cs.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list deployments: %w", err)
		}
	}

	for _, d := range deployList.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		s.deployments = append(s.deployments, deploymentSummary{
			name:      d.Name,
			desired:   desired,
			ready:     d.Status.ReadyReplicas,
			updated:   d.Status.UpdatedReplicas,
			available: d.Status.AvailableReplicas,
		})
	}

	// ── HPAs ─────────────────────────────────────────────────────────────────
	hpaList, err := cs.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, hpa := range hpaList.Items {
			hs := hpaSummary{
				name:        hpa.Name,
				maxReplicas: hpa.Spec.MaxReplicas,
				current:     hpa.Status.CurrentReplicas,
				desired:     hpa.Status.DesiredReplicas,
			}
			if hpa.Spec.MinReplicas != nil {
				hs.minReplicas = *hpa.Spec.MinReplicas
			}
			for _, m := range hpa.Spec.Metrics {
				if m.Type == autoscalingv2.ResourceMetricSourceType &&
					m.Resource != nil &&
					m.Resource.Name == corev1.ResourceCPU &&
					m.Resource.Target.AverageUtilization != nil {
					hs.cpuTarget = *m.Resource.Target.AverageUtilization
				}
			}
			for _, m := range hpa.Status.CurrentMetrics {
				if m.Type == autoscalingv2.ResourceMetricSourceType &&
					m.Resource != nil &&
					m.Resource.Current.AverageUtilization != nil {
					hs.cpuCurrent = *m.Resource.Current.AverageUtilization
				}
			}
			s.hpas = append(s.hpas, hs)
		}
	}

	// ── Events ───────────────────────────────────────────────────────────────
	fieldSelector := "type=Warning"
	if filterDeployment != "" {
		fieldSelector += fmt.Sprintf(",involvedObject.name=%s", filterDeployment)
	}
	eventList, err := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fieldSelector,
	})
	if err == nil {
		// Sort by count desc, take top 15
		sort.Slice(eventList.Items, func(i, j int) bool {
			return eventList.Items[i].Count > eventList.Items[j].Count
		})
		limit := 15
		if len(eventList.Items) < limit {
			limit = len(eventList.Items)
		}
		for _, ev := range eventList.Items[:limit] {
			s.recentEvents = append(s.recentEvents, eventSummary{
				kind:    ev.InvolvedObject.Kind,
				name:    ev.InvolvedObject.Name,
				reason:  ev.Reason,
				message: ev.Message,
				count:   ev.Count,
				age:     time.Since(ev.LastTimestamp.Time),
			})
		}
	}

	return s, nil
}

func (s *namespaceSnapshot) text() string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("=== Namespace Snapshot: %s (at %s) ===\n\n",
		s.namespace, s.collectedAt.Format(time.RFC3339)))

	b.WriteString(fmt.Sprintf("Pods: %d total, %d running, %d failing\n",
		s.totalPods, s.runningPods, len(s.failingPods)))

	if len(s.failingPods) > 0 {
		b.WriteString("\nFailing Pods:\n")
		for _, p := range s.failingPods {
			b.WriteString(fmt.Sprintf("  - %s [%s] reason=%s restarts=%d age=%s\n",
				p.name, p.phase, p.reason, p.restarts, p.age.Round(time.Second)))
		}
	}

	if len(s.restartLeaders) > 0 {
		b.WriteString("\nHigh Restart Pods:\n")
		for _, p := range s.restartLeaders {
			b.WriteString(fmt.Sprintf("  - %s restarts=%d phase=%s\n",
				p.name, p.restarts, p.phase))
		}
	}

	b.WriteString("\nDeployments:\n")
	for _, d := range s.deployments {
		status := "OK"
		if d.ready < d.desired {
			status = fmt.Sprintf("DEGRADED (%d/%d ready)", d.ready, d.desired)
		}
		b.WriteString(fmt.Sprintf("  - %s: desired=%d ready=%d updated=%d [%s]\n",
			d.name, d.desired, d.ready, d.updated, status))
	}

	if len(s.hpas) > 0 {
		b.WriteString("\nHPAs:\n")
		for _, h := range s.hpas {
			b.WriteString(fmt.Sprintf("  - %s: min=%d max=%d current=%d desired=%d cpu_target=%d%% cpu_current=%d%%\n",
				h.name, h.minReplicas, h.maxReplicas, h.current, h.desired, h.cpuTarget, h.cpuCurrent))
		}
	}

	if len(s.recentEvents) > 0 {
		b.WriteString("\nRecent Warning Events (top 15 by count):\n")
		for _, ev := range s.recentEvents {
			b.WriteString(fmt.Sprintf("  [x%d, %s ago] %s/%s %s: %s\n",
				ev.count, ev.age.Round(time.Second), ev.kind, ev.name, ev.reason, truncateMsg(ev.message, 120)))
		}
	}

	return b.String()
}

// ── AI analysis ───────────────────────────────────────────────────────────────

func analyzeWithClaude(s *namespaceSnapshot) (string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Sprintf(
			"[ANTHROPIC_API_KEY not set — skipping AI analysis]\n\n"+
				"Raw snapshot above contains all diagnostic data.\n"+
				"Key observations:\n"+
				"  - %d failing pods\n"+
				"  - %d pods with high restart counts\n"+
				"  - %d degraded deployments",
			len(s.failingPods), len(s.restartLeaders),
			countDegraded(s.deployments),
		), nil
	}

	client := anthropic.NewClient()

	prompt := fmt.Sprintf(`You are an expert Kubernetes SRE. Analyze the following namespace snapshot and provide:

1. **Executive Summary** (2-3 sentences): What is the overall health and what are the top 2-3 issues?
2. **Root Cause Analysis**: For each failing pod or degraded deployment, explain the most likely root cause based on the events and state shown.
3. **Remediation Steps**: Numbered, prioritized list of concrete kubectl commands or config changes to fix the issues.
4. **Preventive Measures**: 2-3 recommendations to prevent recurrence.

Be specific and actionable. Include actual kubectl commands where relevant.

Namespace snapshot:
%s`, s.text())

	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.ModelClaude3_7Sonnet20250219,
		MaxTokens: 2000,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		return "", fmt.Errorf("claude API: %w", err)
	}

	if len(msg.Content) == 0 {
		return "", fmt.Errorf("empty response from claude")
	}

	return msg.Content[0].Text, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func buildClientset() (kubernetes.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func countDegraded(deployments []deploymentSummary) int {
	n := 0
	for _, d := range deployments {
		if d.ready < d.desired {
			n++
		}
	}
	return n
}

func truncateMsg(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
