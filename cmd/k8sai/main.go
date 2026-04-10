package main

import (
	"fmt"
	"os"

	"github.com/sharatvikas/k8s-ai-ops/internal/analyzer"
	"github.com/sharatvikas/k8s-ai-ops/internal/collector"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "k8sai",
	Short: "AI-powered Kubernetes operations assistant",
	Long: `k8sai uses Claude AI to diagnose Kubernetes issues, explain cryptic events,
and recommend fixes — all from your terminal.`,
}

var diagnosePodCmd = &cobra.Command{
	Use:   "diagnose pod [pod-name]",
	Short: "Diagnose a specific pod",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ns, _ := cmd.Flags().GetString("namespace")
		return runDiagnosePod(args[0], ns)
	},
}

var explainCmd = &cobra.Command{
	Use:   "explain [event-message]",
	Short: "Explain a cryptic Kubernetes event in plain English",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runExplain(args)
	},
}

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Audit a deployment manifest for issues before applying",
	RunE: func(cmd *cobra.Command, args []string) error {
		file, _ := cmd.Flags().GetString("file")
		return runAudit(file)
	},
}

var recommendCmd = &cobra.Command{
	Use:   "recommend resources [deploy/name]",
	Short: "Get resource request/limit recommendations based on actual usage",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ns, _ := cmd.Flags().GetString("namespace")
		window, _ := cmd.Flags().GetString("window")
		return runRecommend(args[0], ns, window)
	},
}

func init() {
	diagnosePodCmd.Flags().StringP("namespace", "n", "default", "Kubernetes namespace")
	auditCmd.Flags().StringP("file", "f", "", "Path to manifest file")
	recommendCmd.Flags().StringP("namespace", "n", "default", "Kubernetes namespace")
	recommendCmd.Flags().String("window", "7d", "Lookback window for metrics (e.g. 7d, 24h)")

	rootCmd.AddCommand(diagnosePodCmd)
	rootCmd.AddCommand(explainCmd)
	rootCmd.AddCommand(auditCmd)
	rootCmd.AddCommand(recommendCmd)
}

func runDiagnosePod(name, namespace string) error {
	fmt.Printf("Analyzing pod %s in namespace %s...\n\n", name, namespace)

	c, err := collector.NewK8sCollector()
	if err != nil {
		return fmt.Errorf("failed to connect to cluster: %w", err)
	}

	ctx, err := c.CollectPodContext(name, namespace)
	if err != nil {
		return fmt.Errorf("failed to collect pod context: %w", err)
	}

	a, err := analyzer.NewAnalyzer()
	if err != nil {
		return fmt.Errorf("failed to init analyzer: %w", err)
	}

	diagnosis, err := a.DiagnosePod(ctx)
	if err != nil {
		return fmt.Errorf("analysis failed: %w", err)
	}

	fmt.Println(diagnosis)
	return nil
}

func runExplain(messageParts []string) error {
	message := ""
	for _, p := range messageParts {
		message += p + " "
	}

	a, err := analyzer.NewAnalyzer()
	if err != nil {
		return err
	}

	explanation, err := a.ExplainEvent(message)
	if err != nil {
		return err
	}

	fmt.Println(explanation)
	return nil
}

func runAudit(file string) error {
	if file == "" {
		return fmt.Errorf("--file is required")
	}

	a, err := analyzer.NewAnalyzer()
	if err != nil {
		return err
	}

	result, err := a.AuditManifest(file)
	if err != nil {
		return err
	}

	fmt.Println(result)
	return nil
}

func runRecommend(target, namespace, window string) error {
	fmt.Printf("Analyzing resource usage for %s over %s...\n\n", target, window)

	c, err := collector.NewK8sCollector()
	if err != nil {
		return err
	}

	metrics, err := c.CollectResourceMetrics(target, namespace, window)
	if err != nil {
		return err
	}

	a, err := analyzer.NewAnalyzer()
	if err != nil {
		return err
	}

	recommendation, err := a.RecommendResources(metrics)
	if err != nil {
		return err
	}

	fmt.Println(recommendation)
	return nil
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
