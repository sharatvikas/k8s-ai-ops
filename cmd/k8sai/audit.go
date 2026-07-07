// audit.go — AI-powered namespace RBAC and security audit.
//
// k8sai audit -n production
// k8sai audit -n production --severity high
// k8sai audit -n production --no-ai
//
// Checks for common Kubernetes security misconfigurations:
//   - Pods running as root (missing securityContext)
//   - Containers with privileged mode enabled
//   - ServiceAccounts with wildcard RBAC permissions
//   - Pods with hostNetwork/hostPID/hostIPC enabled
//   - Containers missing resource limits (DoS risk)
//   - Images using :latest tag (supply-chain risk)
//   - Secrets mounted as environment variables
//
// Results are color-coded by severity and optionally sent to Claude for
// a prioritized remediation plan.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Finding severity levels
const (
	sevCritical = "CRITICAL"
	sevHigh     = "HIGH"
	sevMedium   = "MEDIUM"
	sevLow      = "LOW"
)

// AuditFinding represents a single security misconfiguration.
type AuditFinding struct {
	Severity    string
	Category    string
	Resource    string // e.g. "Pod/payments-api-abc123"
	Namespace   string
	Detail      string
	Remediation string
}

var securityCmd = &cobra.Command{
	Use:   "security",
	Short: "Audit a namespace for security misconfigurations with AI remediation",
	Long: `Scan a Kubernetes namespace for common security misconfigurations and
generate a prioritized remediation plan using Claude.

Checks:
  • Pods running as root / missing runAsNonRoot
  • Privileged containers
  • hostNetwork / hostPID / hostIPC enabled
  • ServiceAccounts with wildcard (*) RBAC rules
  • Missing resource limits (CPU/memory)
  • :latest image tags
  • Secrets exposed as environment variables

Examples:
  k8sai security -n production
  k8sai security -n production --severity high
  k8sai security -n production --no-ai`,
	RunE: runAudit,
}

func init() {
	securityCmd.Flags().StringP("namespace", "n", "default", "Namespace to audit")
	securityCmd.Flags().String("severity", "all", "Minimum severity to report: critical | high | medium | all")
	securityCmd.Flags().Bool("no-ai", false, "Skip AI remediation plan")
	rootCmd.AddCommand(securityCmd)
}

func runAudit(cmd *cobra.Command, args []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	minSeverity, _ := cmd.Flags().GetString("severity")
	noAI, _ := cmd.Flags().GetBool("no-ai")

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create k8s client: %w", err)
	}

	ctx := context.Background()
	fmt.Printf("Auditing namespace %q...\n\n", namespace)

	var findings []AuditFinding

	// 1. Pod-level security checks
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	for _, pod := range pods.Items {
		findings = append(findings, auditPod(pod)...)
	}

	// 2. RBAC checks — RoleBindings and ClusterRoleBindings scoped to this namespace
	roleBindings, err := clientset.RbacV1().RoleBindings(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, rb := range roleBindings.Items {
			findings = append(findings, auditRoleBinding(rb, namespace, clientset, ctx)...)
		}
	}

	// 3. Filter by minimum severity
	findings = filterBySeverity(findings, minSeverity)

	// 4. Print results
	printAuditFindings(findings, namespace)

	if len(findings) == 0 {
		fmt.Printf("\033[32m✓ No security issues found at severity level %q\033[0m\n", minSeverity)
		return nil
	}

	// 5. AI remediation plan
	if !noAI && os.Getenv("ANTHROPIC_API_KEY") != "" {
		fmt.Println("\n\033[33m─── AI Remediation Plan ─────────────────────────────────────\033[0m")
		if err := getAuditRemediation(ctx, findings, namespace); err != nil {
			fmt.Fprintf(os.Stderr, "AI analysis failed: %v\n", err)
		}
	} else if !noAI {
		fmt.Println("\n[Tip: set ANTHROPIC_API_KEY to get an AI-generated remediation plan]")
	}

	return nil
}

// ── Pod audit checks ────────────────────────────────────────────────────────

func auditPod(pod corev1.Pod) []AuditFinding {
	var findings []AuditFinding
	res := fmt.Sprintf("Pod/%s", pod.Name)
	ns := pod.Namespace
	sc := pod.Spec.SecurityContext

	// hostNetwork / hostPID / hostIPC
	if pod.Spec.HostNetwork {
		findings = append(findings, AuditFinding{
			Severity:    sevCritical,
			Category:    "HostNamespace",
			Resource:    res,
			Namespace:   ns,
			Detail:      "Pod uses hostNetwork=true — shares the node's network namespace",
			Remediation: "Remove spec.hostNetwork or justify with an explicit annotation",
		})
	}
	if pod.Spec.HostPID {
		findings = append(findings, AuditFinding{
			Severity:    sevCritical,
			Category:    "HostNamespace",
			Resource:    res,
			Namespace:   ns,
			Detail:      "Pod uses hostPID=true — can see all processes on the node",
			Remediation: "Remove spec.hostPID unless required for debugging tools",
		})
	}
	if pod.Spec.HostIPC {
		findings = append(findings, AuditFinding{
			Severity:    sevHigh,
			Category:    "HostNamespace",
			Resource:    res,
			Namespace:   ns,
			Detail:      "Pod uses hostIPC=true — shares the node's IPC namespace",
			Remediation: "Remove spec.hostIPC",
		})
	}

	// Pod-level runAsRoot
	if sc != nil && sc.RunAsUser != nil && *sc.RunAsUser == 0 {
		findings = append(findings, AuditFinding{
			Severity:    sevHigh,
			Category:    "RunAsRoot",
			Resource:    res,
			Namespace:   ns,
			Detail:      "Pod-level securityContext sets runAsUser=0 (root)",
			Remediation: "Set spec.securityContext.runAsNonRoot=true and runAsUser>=1000",
		})
	}

	// Container-level checks
	for _, c := range pod.Spec.Containers {
		cRes := fmt.Sprintf("Pod/%s[%s]", pod.Name, c.Name)
		csc := c.SecurityContext

		// Privileged
		if csc != nil && csc.Privileged != nil && *csc.Privileged {
			findings = append(findings, AuditFinding{
				Severity:    sevCritical,
				Category:    "Privileged",
				Resource:    cRes,
				Namespace:   ns,
				Detail:      "Container runs in privileged mode — equivalent to root on the node",
				Remediation: "Remove securityContext.privileged or use more specific capabilities",
			})
		}

		// Missing runAsNonRoot
		podRunsAsRoot := sc != nil && sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot
		containerMissingNonRoot := csc == nil || csc.RunAsNonRoot == nil
		if containerMissingNonRoot && podRunsAsRoot {
			findings = append(findings, AuditFinding{
				Severity:    sevHigh,
				Category:    "RunAsRoot",
				Resource:    cRes,
				Namespace:   ns,
				Detail:      "Container inherits pod-level root permission (no runAsNonRoot override)",
				Remediation: "Set securityContext.runAsNonRoot=true on the container",
			})
		} else if containerMissingNonRoot && (sc == nil || sc.RunAsNonRoot == nil) {
			findings = append(findings, AuditFinding{
				Severity:    sevMedium,
				Category:    "RunAsRoot",
				Resource:    cRes,
				Namespace:   ns,
				Detail:      "Container has no runAsNonRoot — may run as root if image defaults to UID 0",
				Remediation: "Set securityContext.runAsNonRoot=true and runAsUser>=1000",
			})
		}

		// Missing resource limits
		hasLimits := c.Resources.Limits != nil &&
			(c.Resources.Limits.Cpu() != nil && !c.Resources.Limits.Cpu().IsZero()) &&
			(c.Resources.Limits.Memory() != nil && !c.Resources.Limits.Memory().IsZero())
		if !hasLimits {
			findings = append(findings, AuditFinding{
				Severity:    sevMedium,
				Category:    "ResourceLimits",
				Resource:    cRes,
				Namespace:   ns,
				Detail:      "Container has no CPU/memory limits — can consume unbounded node resources",
				Remediation: "Set resources.limits.cpu and resources.limits.memory",
			})
		}

		// :latest image tag
		if strings.HasSuffix(c.Image, ":latest") || !strings.Contains(c.Image, ":") {
			findings = append(findings, AuditFinding{
				Severity:    sevMedium,
				Category:    "ImageTag",
				Resource:    cRes,
				Namespace:   ns,
				Detail:      fmt.Sprintf("Container uses mutable image tag: %s", c.Image),
				Remediation: "Pin image to an immutable digest or specific version tag",
			})
		}

		// Secrets in env vars
		for _, env := range c.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				findings = append(findings, AuditFinding{
					Severity:    sevLow,
					Category:    "SecretExposure",
					Resource:    cRes,
					Namespace:   ns,
					Detail:      fmt.Sprintf("Secret %q exposed as env var %q — visible in pod spec and process listing", env.ValueFrom.SecretKeyRef.Name, env.Name),
					Remediation: "Mount secrets as files via spec.volumes/volumeMounts instead of env vars",
				})
				break // one finding per container is enough
			}
		}
	}

	return findings
}

// ── RBAC audit checks ───────────────────────────────────────────────────────

func auditRoleBinding(rb rbacv1.RoleBinding, namespace string, clientset kubernetes.Interface, ctx context.Context) []AuditFinding {
	var findings []AuditFinding

	// Resolve the referenced Role or ClusterRole
	var rules []rbacv1.PolicyRule
	switch rb.RoleRef.Kind {
	case "Role":
		role, err := clientset.RbacV1().Roles(namespace).Get(ctx, rb.RoleRef.Name, metav1.GetOptions{})
		if err == nil {
			rules = role.Rules
		}
	case "ClusterRole":
		cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, rb.RoleRef.Name, metav1.GetOptions{})
		if err == nil {
			rules = cr.Rules
		}
	}

	for _, rule := range rules {
		for _, verb := range rule.Verbs {
			if verb == "*" {
				subjects := make([]string, 0, len(rb.Subjects))
				for _, s := range rb.Subjects {
					subjects = append(subjects, fmt.Sprintf("%s/%s", s.Kind, s.Name))
				}
				findings = append(findings, AuditFinding{
					Severity:  sevHigh,
					Category:  "WildcardRBAC",
					Resource:  fmt.Sprintf("RoleBinding/%s", rb.Name),
					Namespace: namespace,
					Detail: fmt.Sprintf(
						"Subjects [%s] have wildcard verb (*) on resources %v via %s %s",
						strings.Join(subjects, ", "),
						rule.Resources,
						rb.RoleRef.Kind,
						rb.RoleRef.Name,
					),
					Remediation: "Replace wildcard verb with specific verbs (get, list, watch, create, update)",
				})
				break
			}
		}
	}

	return findings
}

// ── Filtering and printing ──────────────────────────────────────────────────

var severityRank = map[string]int{
	sevCritical: 4,
	sevHigh:     3,
	sevMedium:   2,
	sevLow:      1,
}

func filterBySeverity(findings []AuditFinding, minSev string) []AuditFinding {
	if minSev == "all" || minSev == "" {
		return findings
	}
	minRank := severityRank[strings.ToUpper(minSev)]
	var filtered []AuditFinding
	for _, f := range findings {
		if severityRank[f.Severity] >= minRank {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

func printAuditFindings(findings []AuditFinding, namespace string) {
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}

	fmt.Printf("%-10s %-20s %-50s %s\n", "SEVERITY", "CATEGORY", "RESOURCE", "DETAIL")
	fmt.Println(strings.Repeat("─", 130))

	for _, f := range findings {
		color := severityColor(f.Severity)
		fmt.Printf("%s%-10s\033[0m %-20s %-50s %s\n",
			color, f.Severity,
			f.Category,
			truncate(f.Resource, 49),
			f.Detail,
		)
	}

	if len(findings) > 0 {
		fmt.Printf("\nSummary: %d findings — CRITICAL:%d HIGH:%d MEDIUM:%d LOW:%d\n",
			len(findings), counts[sevCritical], counts[sevHigh], counts[sevMedium], counts[sevLow])
	}
}

func severityColor(sev string) string {
	switch sev {
	case sevCritical:
		return "\033[35m" // magenta
	case sevHigh:
		return "\033[31m" // red
	case sevMedium:
		return "\033[33m" // yellow
	default:
		return "\033[36m" // cyan
	}
}

// ── AI remediation ──────────────────────────────────────────────────────────

func getAuditRemediation(ctx context.Context, findings []AuditFinding, namespace string) error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	client := anthropic.NewClient()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Kubernetes namespace %q has the following security findings:\n\n", namespace))
	for i, f := range findings {
		if i >= 20 { // cap prompt size
			sb.WriteString(fmt.Sprintf("... and %d more findings\n", len(findings)-20))
			break
		}
		sb.WriteString(fmt.Sprintf("[%s] %s — %s\n  Resource: %s\n  Fix: %s\n\n",
			f.Severity, f.Category, f.Detail, f.Resource, f.Remediation))
	}
	sb.WriteString("\nProvide a prioritized remediation plan with specific kubectl commands or YAML patches.")

	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet4_5,
		MaxTokens: 1500,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(sb.String()))},
	})
	if err != nil {
		return err
	}

	for _, block := range resp.Content {
		if block.Type == "text" {
			fmt.Println(block.Text)
		}
	}
	return nil
}
