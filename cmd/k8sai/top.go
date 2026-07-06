// top.go — AI-enriched resource utilization summary.
//
// k8sai top pods --namespace myapp
// k8sai top nodes
//
// Shows CPU/memory utilization from the Metrics API (kubectl top equivalent)
// and annotates outliers with AI-generated recommendations.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

var topCmd = &cobra.Command{
	Use:   "top [pods|nodes]",
	Short: "Show resource utilization with AI recommendations for outliers",
	Long: `Display CPU and memory utilization for pods or nodes.
Automatically flags containers that are over-utilized (>80%) or
under-utilized (<10%) and provides AI-powered rightsizing recommendations.

Examples:
  k8sai top pods                     # All pods, all namespaces
  k8sai top pods -n production       # Specific namespace
  k8sai top nodes                    # Node-level utilization
  k8sai top pods --sort cpu          # Sort by CPU usage
  k8sai top pods --threshold 70      # Flag at 70% utilization`,
	Args: cobra.ExactArgs(1),
	RunE: runTop,
}

func init() {
	topCmd.Flags().StringP("namespace", "n", "", "Namespace (default: all)")
	topCmd.Flags().String("sort", "cpu", "Sort by: cpu | memory | name")
	topCmd.Flags().Int("threshold", 80, "Utilization % threshold for flagging high usage")
	topCmd.Flags().Bool("no-ai", false, "Skip AI recommendations")
	rootCmd.AddCommand(topCmd)
}

type podMetricRow struct {
	namespace  string
	pod        string
	container  string
	cpuUsage   int64   // millicores
	memUsage   int64   // bytes
	cpuRequest int64   // millicores
	memRequest int64   // bytes
	cpuPct     float64 // usage / request
	memPct     float64
	cpuLimit   int64
	memLimit   int64
}

type nodeMetricRow struct {
	node     string
	cpuUsage int64
	memUsage int64
	cpuCap   int64
	memCap   int64
	cpuPct   float64
	memPct   float64
}

func runTop(cmd *cobra.Command, args []string) error {
	target := strings.ToLower(args[0])
	if target != "pods" && target != "nodes" {
		return fmt.Errorf("argument must be 'pods' or 'nodes', got %q", target)
	}

	namespace, _ := cmd.Flags().GetString("namespace")
	sortBy, _ := cmd.Flags().GetString("sort")
	threshold, _ := cmd.Flags().GetInt("threshold")
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

	metricsClient, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create metrics client: %w", err)
	}

	ctx := context.Background()

	var aiClient *anthropic.Client
	if !noAI && os.Getenv("ANTHROPIC_API_KEY") != "" {
		c := anthropic.NewClient()
		aiClient = &c
	}

	if target == "nodes" {
		return runTopNodes(ctx, clientset, metricsClient, aiClient, threshold, noAI)
	}
	return runTopPods(ctx, clientset, metricsClient, aiClient, namespace, sortBy, threshold, noAI)
}

func runTopPods(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsclient.Clientset,
	aiClient *anthropic.Client,
	namespace, sortBy string,
	threshold int,
	noAI bool,
) error {
	ns := namespace
	if ns == "" {
		ns = metav1.NamespaceAll
	}

	podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch pod metrics: %w", err)
	}

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch pods: %w", err)
	}

	// Build request/limit map
	type resourceSpec struct{ cpuReq, memReq, cpuLim, memLim int64 }
	podResourceMap := map[string]map[string]resourceSpec{} // pod -> container -> spec
	for _, pod := range pods.Items {
		key := pod.Namespace + "/" + pod.Name
		podResourceMap[key] = map[string]resourceSpec{}
		for _, c := range pod.Spec.Containers {
			rs := resourceSpec{}
			if req := c.Resources.Requests; req != nil {
				if cpu := req[corev1.ResourceCPU]; !cpu.IsZero() {
					rs.cpuReq = cpu.MilliValue()
				}
				if mem := req[corev1.ResourceMemory]; !mem.IsZero() {
					rs.memReq = mem.Value()
				}
			}
			if lim := c.Resources.Limits; lim != nil {
				if cpu := lim[corev1.ResourceCPU]; !cpu.IsZero() {
					rs.cpuLim = cpu.MilliValue()
				}
				if mem := lim[corev1.ResourceMemory]; !mem.IsZero() {
					rs.memLim = mem.Value()
				}
			}
			podResourceMap[key][c.Name] = rs
		}
	}

	var rows []podMetricRow
	for _, pm := range podMetrics.Items {
		key := pm.Namespace + "/" + pm.Name
		for _, c := range pm.Containers {
			row := podMetricRow{
				namespace: pm.Namespace,
				pod:       pm.Name,
				container: c.Name,
				cpuUsage:  c.Usage.Cpu().MilliValue(),
				memUsage:  c.Usage.Memory().Value(),
			}
			if rs, ok := podResourceMap[key][c.Name]; ok {
				row.cpuRequest = rs.cpuReq
				row.memRequest = rs.memReq
				row.cpuLimit = rs.cpuLim
				row.memLimit = rs.memLim
				if rs.cpuReq > 0 {
					row.cpuPct = float64(row.cpuUsage) / float64(rs.cpuReq) * 100
				}
				if rs.memReq > 0 {
					row.memPct = float64(row.memUsage) / float64(rs.memReq) * 100
				}
			}
			rows = append(rows, row)
		}
	}

	// Sort
	switch sortBy {
	case "memory":
		sort.Slice(rows, func(i, j int) bool { return rows[i].memUsage > rows[j].memUsage })
	case "name":
		sort.Slice(rows, func(i, j int) bool { return rows[i].pod < rows[j].pod })
	default:
		sort.Slice(rows, func(i, j int) bool { return rows[i].cpuUsage > rows[j].cpuUsage })
	}

	// Print header
	fmt.Printf("%-20s %-45s %-20s %12s %12s %8s %8s\n",
		"NAMESPACE", "POD", "CONTAINER", "CPU(m)", "MEM(Mi)", "CPU%", "MEM%")
	fmt.Println(strings.Repeat("─", 130))

	var highCPU, highMem []podMetricRow
	for _, r := range rows {
		cpuColor, memColor := colorCode(r.cpuPct, float64(threshold)), colorCode(r.memPct, float64(threshold))
		fmt.Printf("%-20s %-45s %-20s %s%11dm\033[0m %s%10dMi\033[0m %s%7.0f%%\033[0m %s%7.0f%%\033[0m\n",
			r.namespace, truncate(r.pod, 44), truncate(r.container, 19),
			"", r.cpuUsage, "", r.memUsage/1024/1024,
			cpuColor, r.cpuPct, memColor, r.memPct,
		)
		if r.cpuPct > float64(threshold) {
			highCPU = append(highCPU, r)
		}
		if r.memPct > float64(threshold) {
			highMem = append(highMem, r)
		}
	}

	if !noAI && aiClient != nil && (len(highCPU) > 0 || len(highMem) > 0) {
		fmt.Println()
		fmt.Printf("\033[33m⚠  %d containers above %d%% CPU, %d above %d%% memory\033[0m\n",
			len(highCPU), threshold, len(highMem), threshold)
		fmt.Println()
		getTopRecommendations(ctx, aiClient, highCPU, highMem)
	}

	return nil
}

func runTopNodes(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsclient.Clientset,
	aiClient *anthropic.Client,
	threshold int,
	noAI bool,
) error {
	nodeMetrics, err := metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch node metrics: %w", err)
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("fetch nodes: %w", err)
	}

	capMap := map[string]corev1.ResourceList{}
	for _, n := range nodes.Items {
		capMap[n.Name] = n.Status.Allocatable
	}

	var rows []nodeMetricRow
	for _, nm := range nodeMetrics.Items {
		cap := capMap[nm.Name]
		row := nodeMetricRow{
			node:     nm.Name,
			cpuUsage: nm.Usage.Cpu().MilliValue(),
			memUsage: nm.Usage.Memory().Value(),
		}
		if cpuCap, ok := cap[corev1.ResourceCPU]; ok {
			row.cpuCap = cpuCap.MilliValue()
			row.cpuPct = float64(row.cpuUsage) / float64(row.cpuCap) * 100
		}
		if memCap, ok := cap[corev1.ResourceMemory]; ok {
			row.memCap = memCap.Value()
			row.memPct = float64(row.memUsage) / float64(row.memCap) * 100
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].cpuPct > rows[j].cpuPct })

	fmt.Printf("%-50s %12s %12s %8s %8s\n", "NODE", "CPU(m)", "MEM(Mi)", "CPU%", "MEM%")
	fmt.Println(strings.Repeat("─", 96))

	for _, r := range rows {
		cpuColor, memColor := colorCode(r.cpuPct, float64(threshold)), colorCode(r.memPct, float64(threshold))
		fmt.Printf("%-50s %s%11dm\033[0m %s%10dMi\033[0m %s%7.0f%%\033[0m %s%7.0f%%\033[0m\n",
			truncate(r.node, 49),
			"", r.cpuUsage, "", r.memUsage/1024/1024,
			cpuColor, r.cpuPct, memColor, r.memPct,
		)
	}
	_ = resource.NewMilliQuantity(0, resource.DecimalSI) // keep import
	return nil
}

func getTopRecommendations(ctx context.Context, client *anthropic.Client, highCPU, highMem []podMetricRow) {
	var lines []string
	for i, r := range highCPU {
		if i >= 3 {
			break
		}
		lines = append(lines, fmt.Sprintf("- %s/%s container=%s: CPU %dm (%.0f%% of request %dm)",
			r.namespace, r.pod, r.container, r.cpuUsage, r.cpuPct, r.cpuRequest))
	}
	for i, r := range highMem {
		if i >= 3 {
			break
		}
		lines = append(lines, fmt.Sprintf("- %s/%s container=%s: Memory %dMi (%.0f%% of request %dMi)",
			r.namespace, r.pod, r.container, r.memUsage/1024/1024, r.memPct, r.memRequest/1024/1024))
	}

	prompt := fmt.Sprintf(`Kubernetes containers with high resource utilization:
%s

For each, provide ONE specific action (resource limit increase, HPA config, or code optimization).
Format: <namespace/pod/container>: <action> (one line each, max 20 words per line)`, strings.Join(lines, "\n"))

	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 200,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		return
	}
	for _, b := range msg.Content {
		if b.Type == "text" {
			fmt.Printf("\033[36m💡 AI Recommendations:\033[0m\n%s\n", b.Text)
		}
	}
}

func colorCode(pct, threshold float64) string {
	if pct > threshold {
		return "\033[31m" // red
	}
	if pct > threshold*0.7 {
		return "\033[33m" // yellow
	}
	return "\033[32m" // green
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
