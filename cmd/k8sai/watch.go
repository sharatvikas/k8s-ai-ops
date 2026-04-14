// watch.go — real-time cluster watcher that streams AI-enriched events.
//
// k8sai watch --namespace myapp --severity warning
//
// Watches the Kubernetes event stream and enriches each Warning event
// with a one-line AI explanation using Claude Haiku for low latency.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch cluster events and get real-time AI explanations",
	Long: `Stream Kubernetes events and enrich Warning events with AI-generated
one-line explanations. Uses Claude Haiku for minimal latency.

Examples:
  k8sai watch                         # Watch all namespaces
  k8sai watch -n myapp               # Watch specific namespace
  k8sai watch --severity warning     # Only Warning events
  k8sai watch --filter "OOMKill"     # Filter by event reason`,
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().StringP("namespace", "n", "", "Namespace to watch (default: all)")
	watchCmd.Flags().String("severity", "warning", "Minimum severity: normal | warning")
	watchCmd.Flags().String("filter", "", "Filter events by reason substring (case-insensitive)")
	watchCmd.Flags().Bool("no-ai", false, "Disable AI enrichment (show raw events only)")
	watchCmd.Flags().Duration("timeout", 0, "Stop watching after this duration (0 = run forever)")
	rootCmd.AddCommand(watchCmd)
}

func runWatch(cmd *cobra.Command, args []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	severity, _ := cmd.Flags().GetString("severity")
	filter, _ := cmd.Flags().GetString("filter")
	noAI, _ := cmd.Flags().GetBool("no-ai")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	// Build kubeconfig
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, configOverrides,
	)
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create k8s client: %w", err)
	}

	// Set up AI client
	var aiClient *anthropic.Client
	if !noAI {
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "Warning: ANTHROPIC_API_KEY not set, disabling AI enrichment")
			noAI = true
		} else {
			c := anthropic.NewClient()
			aiClient = &c
		}
	}

	// Set up context with optional timeout
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Handle SIGINT/SIGTERM gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nStopping watch...")
		cancel()
	}()

	// Start watching
	ns := namespace
	if ns == "" {
		ns = metav1.NamespaceAll
		fmt.Printf("Watching events in all namespaces (severity=%s)\n", severity)
	} else {
		fmt.Printf("Watching events in namespace %q (severity=%s)\n", ns, severity)
	}
	if filter != "" {
		fmt.Printf("Filter: reason contains %q\n", filter)
	}
	fmt.Println(strings.Repeat("─", 80))

	watcher, err := clientset.CoreV1().Events(ns).Watch(ctx, metav1.ListOptions{
		Watch:         true,
		FieldSelector: buildFieldSelector(severity),
	})
	if err != nil {
		return fmt.Errorf("start watch: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil
			}
			if event.Type != watch.Added && event.Type != watch.Modified {
				continue
			}

			k8sEvent, ok := event.Object.(*corev1.Event)
			if !ok {
				continue
			}

			// Apply filter
			if filter != "" && !strings.Contains(
				strings.ToLower(k8sEvent.Reason),
				strings.ToLower(filter),
			) {
				continue
			}

			printEvent(k8sEvent)

			// Enrich with AI explanation
			if !noAI && aiClient != nil && k8sEvent.Type == corev1.EventTypeWarning {
				explanation := enrichEvent(ctx, aiClient, k8sEvent)
				if explanation != "" {
					fmt.Printf("  \033[36m💡 %s\033[0m\n", explanation)
				}
			}
		}
	}
}

func printEvent(e *corev1.Event) {
	ts := e.LastTimestamp.Format(time.RFC3339)
	if e.LastTimestamp.IsZero() {
		ts = e.CreationTimestamp.Format(time.RFC3339)
	}

	typeColor := "\033[33m" // yellow for Warning
	if e.Type == corev1.EventTypeNormal {
		typeColor = "\033[32m" // green for Normal
	}
	reset := "\033[0m"

	fmt.Printf("%s  %s%-8s%s  %-40s  %-25s  %s\n",
		ts,
		typeColor, e.Type, reset,
		fmt.Sprintf("%s/%s", e.InvolvedObject.Kind, e.InvolvedObject.Name),
		e.Reason,
		e.Message,
	)
}

func enrichEvent(ctx context.Context, client *anthropic.Client, e *corev1.Event) string {
	prompt := fmt.Sprintf(
		"Kubernetes %s event:\nKind: %s\nName: %s\nNamespace: %s\nReason: %s\nMessage: %s\n\n"+
			"In one sentence (max 15 words), explain what this means and what action to take. Be direct, no preamble.",
		e.Type,
		e.InvolvedObject.Kind,
		e.InvolvedObject.Name,
		e.Namespace,
		e.Reason,
		e.Message,
	)

	enrichCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	msg, err := client.Messages.New(enrichCtx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 60,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return ""
	}

	for _, block := range msg.Content {
		if block.Type == "text" {
			return strings.TrimSpace(block.Text)
		}
	}
	return ""
}

func buildFieldSelector(severity string) string {
	if strings.ToLower(severity) == "warning" {
		return "type=Warning"
	}
	return ""
}
