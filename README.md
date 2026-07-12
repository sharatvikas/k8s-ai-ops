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
Two controllers run inside a single manager (`cmd/operator`):

- **AIInsight controller** — fulfils `AIInsight` custom resources: collects
  cluster context for the target (pod status, events, logs, manifests),
  queries Claude, and writes the analysis to `.status.summary`. Results are
  TTL-expired and auto-deleted.
- **Pod watcher** — watches pods cluster-wide and detects three incident
  classes from container state: **CrashLoopBackOff** (restart threshold),
  **OOMKilled** (classified separately even when it surfaces as a crashloop),
  and **image-pull failures** (`ImagePullBackOff` / `ErrImagePull` /
  `InvalidImageName`). Each incident files a deduplicated `Diagnose`
  AIInsight and records a Kubernetes event on the pod.

```bash
# Request an analysis declaratively
kubectl apply -f config/samples/diagnose-pod.yaml
kubectl get aiinsights                      # Phase: Running → Completed
kubectl get aiinsight diagnose-pod -o jsonpath='{.status.summary}'

# See what the pod watcher has diagnosed automatically
kubectl get aiinsights -A -l app.kubernetes.io/managed-by=k8s-ai-ops
```

### Guardrails
Auto-remediation is **off by default** (`--enable-remediation` / Helm
`remediation.enabled=true`) and, when on, is restricted to one safe action —
deleting a crashlooping or OOM-killed pod so its controller reschedules it —
behind four gates:

1. The pod must be managed by a workload controller (ReplicaSet, StatefulSet,
   DaemonSet); bare pods are never deleted.
2. Per-workload cooldown (`--remediation-cooldown`, default 10m).
3. Global circuit breaker (`--max-remediations-per-hour`, default 10).
4. Control-plane namespaces (`kube-system`, `kube-public`, `kube-node-lease`)
   plus any `--ignore-namespaces` are never touched.

Image-pull failures are never auto-remediated (a restart cannot fix a bad tag
or registry auth) — they get diagnosis only. Every action is recorded as a
Kubernetes event (`AutoRemediated`) for the audit trail:

```bash
kubectl get events -A --field-selector reason=AutoRemediated
```

### Operator metrics
Prometheus metrics on `--metrics-bind-address` (default `:8080`, scrapeable
via the chart's ServiceMonitor):

| Metric | Labels | Meaning |
|--------|--------|---------|
| `k8sai_incidents_detected_total` | `type`, `namespace` | Crashloops / OOMs / image-pull failures found |
| `k8sai_insights_created_total` | `analysis_type`, `trigger` | AIInsights filed automatically |
| `k8sai_analyses_total` | `analysis_type`, `outcome` | Claude analyses run (success/failure) |
| `k8sai_analysis_duration_seconds` | `analysis_type` | Analysis latency histogram |
| `k8sai_remediations_total` | `action`, `result` | Remediation actions attempted |
| `k8sai_remediations_skipped_total` | `reason` | Gated remediations (cooldown, budget, unmanaged pod, disabled) |

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
git clone https://github.com/sharatvikas/k8s-ai-ops && cd k8s-ai-ops

# Recommended: reference an existing secret containing ANTHROPIC_API_KEY
kubectl create namespace k8s-ai-ops
kubectl create secret generic anthropic-api-key -n k8s-ai-ops \
  --from-literal=ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY

helm install k8s-ai-ops ./helm/k8s-ai-ops \
  --namespace k8s-ai-ops \
  --set anthropicApiKey.existingSecret=anthropic-api-key
```

The chart installs the `AIInsight` CRD (from `crds/`), a minimally-scoped
ClusterRole (read-only unless remediation is enabled — the pods `delete` verb
is only granted with `remediation.enabled=true`), leader-election-ready
Deployment, metrics Service, and optional ServiceMonitor / PDB.

Key values (`helm/k8s-ai-ops/values.yaml`):

```yaml
anthropicModel: "claude-haiku-4-5-20251001"   # model for in-cluster analyses
anthropicApiKey:
  existingSecret: ""        # name of a Secret holding ANTHROPIC_API_KEY
  value: ""                 # or inline (chart creates the Secret)
remediation:
  enabled: false            # gate for ALL mutating actions
  cooldown: 10m             # per-workload remediation interval
  maxPerHour: 10            # global circuit breaker
  crashloopRestartThreshold: 3
  ignoreNamespaces: []      # in addition to kube-* namespaces
leaderElection:
  enabled: false            # enable when replicaCount > 1
insightTTL: 1h              # retention for auto-created AIInsight results
logging:
  encoder: json             # structured logs (zap)
  level: info
```

Enable auto-remediation and HA once you trust the diagnoses:

```bash
helm upgrade k8s-ai-ops ./helm/k8s-ai-ops -n k8s-ai-ops --reuse-values \
  --set remediation.enabled=true \
  --set replicaCount=2 \
  --set leaderElection.enabled=true \
  --set podDisruptionBudget.enabled=true
```

### Running the operator locally

```bash
export ANTHROPIC_API_KEY=sk-...
kubectl apply -f config/crd/aiinsight.yaml
go run ./cmd/operator \
  --enable-remediation=false \
  --crashloop-restart-threshold=3 \
  --zap-encoder=console --zap-log-level=debug
```

---

## Try it locally

The `demo/` directory runs the operator end-to-end on a real Kubernetes cluster (e.g. [`kind`](https://kind.sigs.k8s.io/)): it deploys the operator in observe-and-diagnose mode, applies two faulty workloads, and shows the pod watcher detect **CrashLoopBackOff** and **ImagePullBackOff** incidents, file an `AIInsight` CR for each, emit Kubernetes events, and increment the Prometheus counters.

```bash
# Requires a Kubernetes cluster (e.g. kind). Full step-by-step in demo/README.md.
docker build --target operator -t k8s-ai-ops:demo . && kind load docker-image k8s-ai-ops:demo --name sre-platform
kubectl create namespace k8sai-demo
helm install k8s-ai-ops ./helm/k8s-ai-ops -n k8sai-demo \
  --set image.repository=k8s-ai-ops --set image.tag=demo \
  --set remediation.enabled=false --set logging.encoder=console \
  --set anthropicApiKey.value=sk-ant-dummy-key-for-demo-not-valid
kubectl apply -f demo/crashloop-deployment.yaml -f demo/imagepull-deployment.yaml
```

This proves the full detection → `AIInsight` → events → metrics path without a real cluster incident (the Claude call itself needs a valid `ANTHROPIC_API_KEY`). See [`demo/OUTPUT.md`](demo/OUTPUT.md) for the captured real run and [`demo/README.md`](demo/README.md) for the complete walkthrough.

---

## Project Structure

```
k8s-ai-ops/
├── cmd/
│   ├── k8sai/                  # CLI entrypoint (cobra)
│   └── operator/               # Controller-manager entrypoint
├── api/
│   └── v1alpha1/               # AIInsight CRD types (+ deepcopy)
├── internal/
│   ├── analyzer/               # Claude API integration + prompt engineering
│   ├── collector/              # K8s context collection (CLI clientset + operator client)
│   ├── controller/             # AIInsight reconciler + pod watcher/remediator
│   └── metrics/                # Prometheus metrics (controller-runtime registry)
├── helm/
│   └── k8s-ai-ops/             # Helm chart (Deployment, RBAC, CRD, ServiceMonitor, PDB)
├── config/
│   ├── crd/                    # AIInsight CRD for kubectl-apply installs
│   └── samples/                # Example AIInsight CRs
└── Dockerfile                  # Multi-stage: cli + operator distroless images
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
- [x] In-cluster operator: AIInsight CRD + pod watcher with gated auto-remediation
- [x] Helm chart (CRD, minimal RBAC, leader election, metrics, ServiceMonitor, PDB)
- [ ] HPA / VPA recommendation engine
- [ ] Network policy debugging
- [ ] Istio service mesh diagnostics
- [ ] Multi-cluster support
- [ ] OpenAI / Gemini backend support (pluggable)
- [ ] VS Code extension

---

## License

MIT — see [LICENSE](LICENSE).
