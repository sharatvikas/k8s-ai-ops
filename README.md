# k8s-ai-ops

> **AI-powered Kubernetes operations assistant.** Uses Claude API + live cluster context to diagnose degraded pods, explain events in plain English, suggest fixes, and optionally auto-remediate — all from your terminal or Slack.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.22+](https://img.shields.io/badge/go-1.22+-00ADD8.svg)](https://go.dev)
[![Kubernetes](https://img.shields.io/badge/kubernetes-1.28+-326CE5.svg)](https://kubernetes.io)
[![Claude API](https://img.shields.io/badge/Claude-API-orange.svg)](https://anthropic.com)

---

## What It Does

`k8s-ai-ops` is a CLI + in-cluster operator that continuously monitors your Kubernetes workloads and applies AI reasoning to surface actionable insights — not just raw events.

```
$ k8sai diagnose pod payments-api-7d4b9c-xvz2k

Analyzing pod payments-api-7d4b9c-xvz2k in namespace production...

DIAGNOSIS: OOMKilled (Exit Code 137)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Root Cause: Container exceeded memory limit (512Mi). JVM heap not
            bounded — -Xmx not set, defaulting to 25% of host RAM.

Evidence:
  • 3 OOMKill events in last 2 hours
  • Memory usage was at 98% of limit before each kill
  • Recent deployment (6h ago) bumped connection pool from 20→100

Recommended Fix:
  Option 1 (immediate): kubectl set resources deploy/payments-api \
                          --limits=memory=1Gi
  Option 2 (correct):   Set JVM_OPTS="-Xmx400m" in deployment env
  Option 3 (root fix):  Profile memory under load — connection pool
                        increase likely the driver

Confidence: HIGH  •  Auto-remediate? [y/N]
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    k8s-ai-ops                           │
│                                                         │
│  ┌──────────┐    ┌──────────────┐    ┌───────────────┐  │
│  │  CLI     │    │  Operator    │    │  Slack Bot    │  │
│  │ (k8sai)  │    │  (in-cluster)│    │  (optional)   │  │
│  └────┬─────┘    └──────┬───────┘    └───────┬───────┘  │
│       │                 │                    │           │
│  ┌────▼─────────────────▼────────────────────▼────────┐  │
│  │              Context Collector                     │  │
│  │  Pod events │ Logs │ Metrics │ Recent Deployments  │  │
│  └────────────────────────┬───────────────────────────┘  │
│                           │                              │
│  ┌────────────────────────▼───────────────────────────┐  │
│  │              Claude API (claude-opus-4)            │  │
│  │  System prompt: Senior SRE with K8s expertise      │  │
│  │  Tool use: kubectl, Prometheus queries             │  │
│  └────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

---

## Features

### CLI Mode
```bash
# Diagnose a specific pod
k8sai diagnose pod <name> [-n namespace]

# Explain a cryptic K8s event in plain English
k8sai explain event "Readiness probe failed: dial tcp: connect: connection refused"

# Audit a deployment manifest for issues before applying
k8sai audit -f deployment.yaml

# Why is my HPA not scaling?
k8sai diagnose hpa <name>

# Get a capacity recommendation for a deployment
k8sai recommend resources deploy/<name> --window 7d
```

### In-Cluster Operator Mode
- Watches for `Warning` events across all namespaces
- On repeated events (configurable threshold), triggers AI analysis
- Annotates the offending resource with diagnosis + recommended action
- Optionally sends findings to Slack / PagerDuty
- Can auto-remediate pre-approved action types (restart, resource patch)

### Guardrails
- Dry-run mode by default for all mutations
- Allowlist of approved remediation actions per namespace
- Audit trail of every AI decision + action taken
- Cost controls: max tokens per diagnosis, caching repeated contexts

---

## Quick Start

```bash
# CLI install
go install github.com/sharatvikas/k8s-ai-ops/cmd/k8sai@latest

# Set your API key
export ANTHROPIC_API_KEY=sk-...

# Point at your cluster (uses KUBECONFIG by default)
k8sai diagnose pod my-broken-pod -n production
```

### Operator Install (Helm)

```bash
helm repo add k8s-ai-ops https://sharatvikas.github.io/k8s-ai-ops/charts
helm install k8s-ai-ops k8s-ai-ops/k8s-ai-ops \
  --set anthropic.apiKey=$ANTHROPIC_API_KEY \
  --set slack.webhookUrl=$SLACK_WEBHOOK \
  --namespace k8s-ai-ops --create-namespace
```

---

## Project Structure

```
k8s-ai-ops/
├── cmd/
│   └── k8sai/           # CLI entrypoint
├── internal/
│   ├── analyzer/        # Claude API integration + prompt engineering
│   ├── collector/       # K8s event + log + metric collection
│   ├── operator/        # Kubernetes operator (controller-runtime)
│   ├── remediation/     # Auto-remediation engine with allowlist
│   └── notifier/        # Slack / PagerDuty notifiers
├── charts/              # Helm chart for operator deployment
├── config/              # CRDs and RBAC manifests
├── examples/            # Example broken deployments for testing
└── docs/
    ├── PROMPT_DESIGN.md # How prompts are structured
    └── COST_ANALYSIS.md # Expected API cost at scale
```

---

## Prompt Engineering

The system prompt is carefully crafted to behave like a Senior SRE:
- Always asks "what changed recently?" before diagnosing
- Ranks hypotheses by probability, not just possibility
- Distinguishes symptom from root cause
- Gives multiple remediation options ranked by risk/speed
- Flags when it's uncertain and recommends human review

See [`docs/PROMPT_DESIGN.md`](docs/PROMPT_DESIGN.md) for the full prompt strategy.

---

## Roadmap

- [x] Pod OOMKill diagnosis
- [x] CrashLoopBackOff analysis
- [x] Pending pod scheduling diagnosis
- [ ] HPA / VPA recommendation engine
- [ ] Network policy debugging
- [ ] Istio service mesh diagnostics
- [ ] Multi-cluster support
- [ ] OpenAI / Gemini backend support (pluggable)
- [ ] VS Code extension

---

## License

MIT — see [LICENSE](LICENSE).
