package analyzer

import (
	"context"
	"fmt"
	"os"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sharatvikas/k8s-ai-ops/internal/collector"
)

const defaultModel = "claude-opus-4-6"

const systemPrompt = `You are a Senior Site Reliability Engineer with deep Kubernetes expertise.

When diagnosing issues:
1. Lead with the most probable root cause, not just symptoms
2. Reference specific pod events, exit codes, and log lines as evidence
3. Ask "what changed recently?" — correlate with recent deployments
4. Distinguish symptom from root cause clearly
5. Give 2-3 remediation options ranked by speed vs. correctness
6. Flag when you're uncertain and say what additional info would help

Format your response as:
DIAGNOSIS: [one-line summary]
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Root Cause: [explanation]

Evidence:
  • [specific log lines, exit codes, events]

Recommended Fix:
  Option 1 (immediate): [command or action]
  Option 2 (correct):   [command or action]
  Option 3 (root fix):  [command or action]

Confidence: HIGH/MEDIUM/LOW  •  Auto-remediate? [y/N]`

// Analyzer wraps the Claude API for K8s diagnostics
type Analyzer struct {
	client anthropic.Client
	model  string
}

// New creates an Analyzer for the given model, reading credentials from
// the ANTHROPIC_API_KEY environment variable. Used by the operator so the
// client is constructed once at startup and failures surface immediately.
func New(model string) (*Analyzer, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY environment variable not set")
	}
	if model == "" {
		model = defaultModel
	}
	return &Analyzer{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
		model:  model,
	}, nil
}

// NewAnalyzer creates a new Analyzer using the ANTHROPIC_API_KEY env var and
// the K8SAI_MODEL env var for model selection (CLI entrypoint).
func NewAnalyzer() (*Analyzer, error) {
	return New(os.Getenv("K8SAI_MODEL"))
}

// Model returns the Claude model this analyzer queries.
func (a *Analyzer) Model() string { return a.model }

// DiagnosePod analyzes a pod context and returns a diagnosis
func (a *Analyzer) DiagnosePod(ctx context.Context, pc *collector.PodContext) (string, error) {
	return a.query(ctx, buildPodDiagnosisPrompt(pc))
}

// ExplainEvent translates a cryptic K8s event into plain English
func (a *Analyzer) ExplainEvent(ctx context.Context, event string) (string, error) {
	userMsg := fmt.Sprintf(`Explain this Kubernetes event in plain English for an engineer who may not know what it means.
Include: what it means, why it happens, and what to do about it.

Event: %s`, event)
	return a.query(ctx, userMsg)
}

// AuditManifest reads a manifest file from disk and audits it (CLI path).
func (a *Analyzer) AuditManifest(ctx context.Context, filePath string) (string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest: %w", err)
	}
	return a.AuditManifestContent(ctx, string(content))
}

// AuditManifestContent audits raw manifest YAML (operator path — the manifest
// is collected from the live cluster rather than a local file).
func (a *Analyzer) AuditManifestContent(ctx context.Context, manifest string) (string, error) {
	userMsg := fmt.Sprintf(`Audit this Kubernetes manifest for issues before it is applied to production.

Check for:
- Missing resource requests/limits
- Missing liveness/readiness probes
- Security context issues (running as root, privileged containers)
- Single replica deployments for critical services
- Missing pod disruption budgets
- Deprecated API versions
- Any other production-readiness concerns

Manifest:
---
%s`, manifest)

	return a.query(ctx, userMsg)
}

// RecommendResources suggests resource requests/limits based on actual usage metrics
func (a *Analyzer) RecommendResources(ctx context.Context, metrics *collector.ResourceMetrics) (string, error) {
	userMsg := fmt.Sprintf(`Based on the following resource utilization metrics, recommend appropriate
Kubernetes resource requests and limits.

Current configuration:
  CPU Request: %s | CPU Limit: %s
  Memory Request: %s | Memory Limit: %s

Observed usage over %s:
  CPU P50: %s | P90: %s | P99: %s | Max: %s
  Memory P50: %s | P90: %s | P99: %s | Max: %s

OOMKill events in window: %d
CPU throttling rate: %.1f%%

Provide:
1. Recommended requests (based on P90 usage)
2. Recommended limits (based on P99 + safety margin)
3. Explanation for your choices
4. kubectl patch command to apply the change`,
		metrics.CurrentCPURequest, metrics.CurrentCPULimit,
		metrics.CurrentMemRequest, metrics.CurrentMemLimit,
		metrics.Window,
		metrics.CPUP50, metrics.CPUP90, metrics.CPUP99, metrics.CPUMax,
		metrics.MemP50, metrics.MemP90, metrics.MemP99, metrics.MemMax,
		metrics.OOMKillCount, metrics.CPUThrottleRate,
	)

	return a.query(ctx, userMsg)
}

func (a *Analyzer) query(ctx context.Context, userMessage string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 2048,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("Claude API error: %w", err)
	}

	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

func buildPodDiagnosisPrompt(pc *collector.PodContext) string {
	var sb strings.Builder

	sb.WriteString("Diagnose this Kubernetes pod issue:\n\n")
	sb.WriteString(fmt.Sprintf("Pod: %s/%s\n", pc.Namespace, pc.Name))
	sb.WriteString(fmt.Sprintf("Phase: %s\n", pc.Phase))
	sb.WriteString(fmt.Sprintf("Node: %s\n\n", pc.NodeName))

	if pc.IncidentID != "" {
		sb.WriteString(fmt.Sprintf("Incident: %s\n\n", pc.IncidentID))
	}

	if len(pc.ContainerStatuses) > 0 {
		sb.WriteString("CONTAINER STATUS:\n")
		for _, cs := range pc.ContainerStatuses {
			sb.WriteString(fmt.Sprintf("  %s: ready=%v, restarts=%d\n", cs.Name, cs.Ready, cs.RestartCount))
			if cs.State != "" {
				sb.WriteString(fmt.Sprintf("    State: %s\n", cs.State))
			}
			if cs.LastState != "" {
				sb.WriteString(fmt.Sprintf("    LastState: %s\n", cs.LastState))
			}
		}
	}

	if len(pc.Events) > 0 {
		sb.WriteString("\nRECENT EVENTS:\n")
		for _, e := range pc.Events {
			sb.WriteString(fmt.Sprintf("  [%s] %s (x%d)\n", e.Reason, e.Message, e.Count))
		}
	}

	if len(pc.Logs) > 0 {
		sb.WriteString("\nRECENT LOGS (last 20 lines):\n")
		for _, line := range pc.Logs {
			sb.WriteString("  " + line + "\n")
		}
	}

	if pc.RecentDeployment != "" {
		sb.WriteString(fmt.Sprintf("\nRECENT DEPLOYMENT: %s\n", pc.RecentDeployment))
	}

	return sb.String()
}
