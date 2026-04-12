// recommend.go — AI-powered resource rightsizing recommendations.
//
// k8sai recommend --namespace myapp
// k8sai recommend --workload payments-api --namespace production
//
// Compares actual resource usage (from Metrics API) against declared
// requests/limits on Deployments and StatefulSets, then asks Claude to
// generate concrete YAML patches to rightsize each container.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/client-go/tools/clientcmd"
)

var recommendCmd = &cobra.Command{
	Use:   "recommend",
	Short: "Generate AI-powered resource rightsizing recommendations",
	Long: `Analyze actual CPU/memory usage versus declared requests and limits
for Deployments and StatefulSets. Produces a Claude-generated YAML patch
for each workload that is over- or under-provisioned.

Examples:
  k8sai recommend                          # All namespaces
  k8sai recommend -n production            # Single namespace
  k8sai recommend --workload payments-api  # Single workload
  k8sai recommend --over-provisioned-only  # Only over-provisioned workloads
  k8sai recommend --output patches/        # Write YAML patches to directory`,
	RunE: runRecommend,
}

func init() {
	recommendCmd.Flags().StringP("namespace", "n", "", "Namespace (default: all)")
	recommendCmd.Flags().StringP("workload", "w", "", "Specific workload name to analyze")
	recommendCmd.Flags().Bool("over-provisioned-only", false, "Only report over-provisioned workloads")
	recommendCmd.Flags().Bool("under-provisioned-only", false, "Only report under-provisioned workloads")
	recommendCmd.Flags().StringP("output", "o", "", "Directory to write YAML patches (optional)")
	recommendCmd.Flags().Bool("no-ai", false, "Skip AI analysis, show raw utilization only")
	recommendCmd.Flags().Float64("cpu-slack", 30.0, "Target CPU headroom % above p95 usage")
	recommendCmd.Flags().Float64("mem-slack", 20.0, "Target memory headroom % above observed max")
	rootCmd.AddCommand(recommendCmd)
}

type workloadAnalysis struct {
	kind       string // Deployment | StatefulSet
	namespace  string
	name       string
	containers []containerAnalysis
}

type containerAnalysis struct {
	name string

	// Declared
	cpuRequest int64 // millicores
	memRequest int64 // bytes
	cpuLimit   int64
	memLimit   int64

	// Observed (aggregated across replicas — max usage)
	cpuUsageMax int64
	memUsageMax int64

	// Utilization ratios
	cpuUtil float64 // usage / request
	memUtil float64

	// Recommendations
	cpuRequestRec int64
	memRequestRec int64
	cpuLimitRec   int64
	memLimitRec   int64

	overProvisioned  bool
	underProvisioned bool
}

func runRecommend(cmd *cobra.Command, _ []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	workloadFilter, _ := cmd.Flags().GetString("workload")
	overOnly, _ := cmd.Flags().GetBool("over-provisioned-only")
	underOnly, _ := cmd.Flags().GetBool("under-provisioned-only")
	outputDir, _ := cmd.Flags().GetString("output")
	noAI, _ := cmd.Flags().GetBool("no-ai")
	cpuSlack, _ := cmd.Flags().GetFloat64("cpu-slack")
	memSlack, _ := cmd.Flags().GetFloat64("mem-slack")

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
	metricsClient, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create metrics client: %w", err)
	}

	ctx := context.Background()
	ns := namespace
	if ns == "" {
		ns = metav1.NamespaceAll
	}

	// Collect current pod metrics
	podMetricsList, err := metricsClient.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch pod metrics: %w", err)
	}

	// Build max-usage map: namespace/workload/container -> {cpuMax, memMax}
	type usageKey struct{ ns, workload, container string }
	type usageVal struct{ cpuMax, memMax int64 }
	usageMap := map[usageKey]usageVal{}

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch pods: %w", err)
	}
	// Map pod -> owner workload name
	podOwner := map[string]string{} // namespace/pod -> workloadName
	for _, pod := range pods.Items {
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "ReplicaSet" || ref.Kind == "StatefulSet" {
				// For ReplicaSet, strip the hash suffix to get Deployment name
				ownerName := ref.Name
				if ref.Kind == "ReplicaSet" {
					parts := strings.Split(ownerName, "-")
					if len(parts) > 2 {
						ownerName = strings.Join(parts[:len(parts)-1], "-")
					}
				}
				podOwner[pod.Namespace+"/"+pod.Name] = ownerName
			}
		}
	}

	for _, pm := range podMetricsList.Items {
		wl, ok := podOwner[pm.Namespace+"/"+pm.Name]
		if !ok {
			continue
		}
		for _, c := range pm.Containers {
			k := usageKey{pm.Namespace, wl, c.Name}
			cpu := c.Usage.Cpu().MilliValue()
			mem := c.Usage.Memory().Value()
			if existing, exists := usageMap[k]; exists {
				if cpu > existing.cpuMax {
					existing.cpuMax = cpu
				}
				if mem > existing.memMax {
					existing.memMax = mem
				}
				usageMap[k] = existing
			} else {
				usageMap[k] = usageVal{cpu, mem}
			}
		}
	}

	// Analyze Deployments
	var analyses []workloadAnalysis

	deployments, err := clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch deployments: %w", err)
	}
	for _, d := range deployments.Items {
		if workloadFilter != "" && d.Name != workloadFilter {
			continue
		}
		if a := analyzeWorkload("Deployment", d.Namespace, d.Name, d.Spec.Template.Spec.Containers, usageMap, cpuSlack, memSlack); a != nil {
			analyses = append(analyses, *a)
		}
	}

	// Analyze StatefulSets
	statefulsets, err := clientset.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch statefulsets: %w", err)
	}
	for _, s := range statefulsets.Items {
		if workloadFilter != "" && s.Name != workloadFilter {
			continue
		}
		if a := analyzeWorkload("StatefulSet", s.Namespace, s.Name, s.Spec.Template.Spec.Containers, usageMap, cpuSlack, memSlack); a != nil {
			analyses = append(analyses, *a)
		}
	}

	// Filter
	filtered := analyses[:0]
	for _, a := range analyses {
		anyOver, anyUnder := false, false
		for _, c := range a.containers {
			if c.overProvisioned {
				anyOver = true
			}
			if c.underProvisioned {
				anyUnder = true
			}
		}
		if overOnly && !anyOver {
			continue
		}
		if underOnly && !anyUnder {
			continue
		}
		filtered = append(filtered, a)
	}
	analyses = filtered

	if len(analyses) == 0 {
		fmt.Println("No workloads found matching the criteria.")
		return nil
	}

	// Sort by namespace + name
	sort.Slice(analyses, func(i, j int) bool {
		if analyses[i].namespace != analyses[j].namespace {
			return analyses[i].namespace < analyses[j].namespace
		}
		return analyses[i].name < analyses[j].name
	})

	// Print utilization table
	printRecommendTable(analyses)

	// AI analysis
	if !noAI && os.Getenv("ANTHROPIC_API_KEY") != "" {
		c := anthropic.NewClient()
		fmt.Println()
		generateRecommendationPatches(ctx, &c, analyses, outputDir)
	}

	return nil
}

func analyzeWorkload(
	kind, ns, name string,
	containers []corev1.Container,
	usageMap map[struct{ ns, workload, container string }]struct{ cpuMax, memMax int64 },
	cpuSlack, memSlack float64,
) *workloadAnalysis {
	wa := &workloadAnalysis{kind: kind, namespace: ns, name: name}
	for _, c := range containers {
		ca := containerAnalysis{name: c.Name}

		if req := c.Resources.Requests; req != nil {
			if cpu, ok := req[corev1.ResourceCPU]; ok {
				ca.cpuRequest = cpu.MilliValue()
			}
			if mem, ok := req[corev1.ResourceMemory]; ok {
				ca.memRequest = mem.Value()
			}
		}
		if lim := c.Resources.Limits; lim != nil {
			if cpu, ok := lim[corev1.ResourceCPU]; ok {
				ca.cpuLimit = cpu.MilliValue()
			}
			if mem, ok := lim[corev1.ResourceMemory]; ok {
				ca.memLimit = mem.Value()
			}
		}

		if u, ok := usageMap[struct{ ns, workload, container string }{ns, name, c.Name}]; ok {
			ca.cpuUsageMax = u.cpuMax
			ca.memUsageMax = u.memMax
		}

		if ca.cpuRequest > 0 {
			ca.cpuUtil = float64(ca.cpuUsageMax) / float64(ca.cpuRequest) * 100
		}
		if ca.memRequest > 0 {
			ca.memUtil = float64(ca.memUsageMax) / float64(ca.memRequest) * 100
		}

		// Recommendations: target usage + slack headroom
		if ca.cpuUsageMax > 0 {
			ca.cpuRequestRec = int64(float64(ca.cpuUsageMax) * (1 + cpuSlack/100))
			ca.cpuLimitRec = ca.cpuRequestRec * 2 // limit = 2x request
		}
		if ca.memUsageMax > 0 {
			ca.memRequestRec = int64(float64(ca.memUsageMax) * (1 + memSlack/100))
			ca.memLimitRec = int64(float64(ca.memRequestRec) * 1.5)
		}

		// Classify
		if ca.cpuRequest > 0 && ca.cpuUtil < 20 {
			ca.overProvisioned = true
		}
		if ca.memRequest > 0 && ca.memUtil < 30 {
			ca.overProvisioned = true
		}
		if ca.cpuRequest > 0 && ca.cpuUtil > 80 {
			ca.underProvisioned = true
		}
		if ca.memRequest > 0 && ca.memUtil > 80 {
			ca.underProvisioned = true
		}

		wa.containers = append(wa.containers, ca)
	}
	if len(wa.containers) == 0 {
		return nil
	}
	return wa
}

func printRecommendTable(analyses []workloadAnalysis) {
	fmt.Printf("\n%-12s %-30s %-30s %-20s %10s %10s %10s %10s %8s %8s\n",
		"KIND", "NAMESPACE/NAME", "CONTAINER",
		"CPU REQ/LIM", "CPU USED", "CPU%",
		"MEM REQ/LIM", "MEM USED", "MEM%", "STATUS")
	fmt.Println(strings.Repeat("─", 140))

	for _, a := range analyses {
		for i, c := range a.containers {
			label := ""
			if i == 0 {
				label = fmt.Sprintf("%s/%s", a.namespace, a.name)
			}

			status := "\033[32mOK\033[0m"
			if c.overProvisioned && c.underProvisioned {
				status = "\033[35mMIXED\033[0m"
			} else if c.overProvisioned {
				status = "\033[33mOVER\033[0m"
			} else if c.underProvisioned {
				status = "\033[31mUNDER\033[0m"
			}

			cpuRL := fmt.Sprintf("%dm/%dm", c.cpuRequest, c.cpuLimit)
			memRL := fmt.Sprintf("%dMi/%dMi", c.memRequest/1024/1024, c.memLimit/1024/1024)

			fmt.Printf("%-12s %-30s %-30s %-20s %10s %9.0f%% %-20s %10s %9.0f%% %s\n",
				a.kind,
				truncate(label, 29),
				truncate(c.name, 29),
				cpuRL,
				fmt.Sprintf("%dm", c.cpuUsageMax),
				c.cpuUtil,
				memRL,
				fmt.Sprintf("%dMi", c.memUsageMax/1024/1024),
				c.memUtil,
				status,
			)
		}
	}
}

func generateRecommendationPatches(ctx context.Context, client *anthropic.Client, analyses []workloadAnalysis, outputDir string) {
	var sb strings.Builder
	sb.WriteString("Kubernetes workload resource analysis — generate strategic patch recommendations:\n\n")

	for _, a := range analyses {
		for _, c := range a.containers {
			if !c.overProvisioned && !c.underProvisioned {
				continue
			}
			direction := "over-provisioned"
			if c.underProvisioned {
				direction = "under-provisioned"
			}
			sb.WriteString(fmt.Sprintf(
				"- %s %s/%s container=%s: %s\n  Current: cpu=%dm/%dm mem=%dMi/%dMi\n  Max observed: cpu=%dm mem=%dMi\n  Suggested: cpu_req=%dm cpu_lim=%dm mem_req=%dMi mem_lim=%dMi\n\n",
				a.kind, a.namespace, a.name, c.name, direction,
				c.cpuRequest, c.cpuLimit, c.memRequest/1024/1024, c.memLimit/1024/1024,
				c.cpuUsageMax, c.memUsageMax/1024/1024,
				c.cpuRequestRec, c.cpuLimitRec, c.memRequestRec/1024/1024, c.memLimitRec/1024/1024,
			))
		}
	}

	sb.WriteString(`
For each workload above, produce a Kubernetes strategic merge patch in YAML.
Format each patch as:
---
# <kind>/<namespace>/<name> — <container>
apiVersion: apps/v1
kind: <kind>
metadata:
  name: <name>
  namespace: <namespace>
spec:
  template:
    spec:
      containers:
      - name: <container>
        resources:
          requests:
            cpu: <recommended>
            memory: <recommended>
          limits:
            cpu: <recommended>
            memory: <recommended>
---

Include a one-line comment explaining the rationale.
Apply kubectl with: kubectl patch <kind> <name> -n <namespace> --patch-file <filename>
`)

	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaude_Haiku_4_5,
		MaxTokens: 2000,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(sb.String()))},
	})
	if err != nil {
		fmt.Printf("\033[33mAI analysis unavailable: %v\033[0m\n", err)
		return
	}

	var output string
	for _, b := range msg.Content {
		if b.Type == anthropic.ContentBlockTypeText {
			output = b.Text
		}
	}

	fmt.Printf("\033[36m💡 AI Rightsizing Patches:\033[0m\n%s\n", output)

	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0755); err == nil {
			patchFile := outputDir + "/rightsizing-patches.yaml"
			if err := os.WriteFile(patchFile, []byte(output), 0644); err == nil {
				fmt.Printf("\033[32m✓ Patches written to %s\033[0m\n", patchFile)
			}
		}
	}

	// Estimate savings
	var totalCPUSaved, totalMemSaved int64
	for _, a := range analyses {
		for _, c := range a.containers {
			if c.overProvisioned && c.cpuRequestRec > 0 && c.cpuRequest > c.cpuRequestRec {
				totalCPUSaved += c.cpuRequest - c.cpuRequestRec
			}
			if c.overProvisioned && c.memRequestRec > 0 && c.memRequest > c.memRequestRec {
				totalMemSaved += c.memRequest - c.memRequestRec
			}
		}
	}
	if totalCPUSaved > 0 || totalMemSaved > 0 {
		fmt.Printf("\n\033[32m📊 Estimated capacity savings if all patches applied:\033[0m\n")
		fmt.Printf("   CPU: %dm cores freed\n", totalCPUSaved)
		fmt.Printf("   Memory: %dMi freed\n", totalMemSaved/1024/1024)
	}

	_ = resource.NewMilliQuantity(0, resource.DecimalSI) // keep import
	_ = appsv1.SchemeGroupVersion                        // keep import
}
