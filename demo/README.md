# Live CrashLoop / ImagePull Detection Demo (kind)

End-to-end demo of the k8s-ai-ops operator on a real `kind` cluster: it
watches pods cluster-wide, detects CrashLoopBackOff and ImagePullBackOff
incidents, files an `AIInsight` custom resource for each, and requests a Claude
diagnosis. Auto-remediation is **disabled** (observe-and-diagnose mode).

`OUTPUT.md` has the actual captured commands + output.

## What it proves

| Capability | Evidence in `OUTPUT.md` |
|---|---|
| Operator deploys (observe-and-diagnose) | startup log: `remediationEnabled: false` |
| CrashLoopBackOff detection (>= 3 restarts) | log `incident detected ... CrashLoopBackOff ... restarts: 3` |
| ImagePullBackOff detection | log `incident detected ... ImagePullFailure` |
| `AIInsight` CR creation with context hints | `kubectl get aiinsight -o yaml` (spec.contextHints) |
| Kubernetes Events filed | `IncidentDetected` warning events on both pods |
| Prometheus incident counters | `k8sai_incidents_detected_total`, `k8sai_insights_created_total` |
| Remediation safety gate | `k8sai_remediations_skipped_total{reason="disabled"}` = 1 |
| LLM step (needs real key) | `AIInsight` phase `Failed`: Claude `401 invalid x-api-key` |

## Files

- `crashloop-deployment.yaml` — busybox that exits 1 on a loop (owned by a Deployment/ReplicaSet, so it is "remediable")
- `imagepull-deployment.yaml` — references a non-existent image tag (never remediable)

## Reproduce

```bash
# 1. Build + load the operator image
docker build --target operator -t k8s-ai-ops:demo .
kind load docker-image k8s-ai-ops:demo --name sre-platform

# 2. Deploy the operator (remediation OFF, dummy API key so the pod starts)
kubectl create namespace k8sai-demo
helm install k8s-ai-ops ./helm/k8s-ai-ops -n k8sai-demo \
  --set image.repository=k8s-ai-ops --set image.tag=demo \
  --set remediation.enabled=false \
  --set anthropicApiKey.value=sk-ant-dummy-key-for-demo-not-valid \
  --set logging.encoder=console

# 3. Create the faulty workloads
kubectl apply -f demo/crashloop-deployment.yaml -f demo/imagepull-deployment.yaml

# 4. Watch detection + AIInsight creation
kubectl logs -n k8sai-demo deploy/k8s-ai-ops -f
kubectl get aiinsight -n k8sai-demo -w

# 5. Metrics (port-forward to a free local port)
kubectl port-forward -n k8sai-demo deploy/k8s-ai-ops 18080:8080 &
curl -s http://localhost:18080/metrics | grep k8sai_
```

## Notes / honesty

- There is **no valid `ANTHROPIC_API_KEY`** in this environment. A dummy value
  is supplied so the operator pod starts (the analyzer constructs its client at
  startup and fails fast if the env var is missing). The **detection + CR
  creation** is the proof; the Claude call itself returns `401 invalid x-api-key`
  and the `AIInsight` lands in phase `Failed` — this is the expected, honest
  outcome without a real key. Everything up to the LLM call works.
- **One bug was fixed to make the chart deploy** (committed): the pod
  `securityContext` set `runAsNonRoot: true` but no numeric `runAsUser`. The
  distroless `nonroot` user is declared non-numerically, so the kubelet cannot
  verify it and the container fails with `CreateContainerConfigError`. Added
  `runAsUser: 65532` (the distroless nonroot UID).
- Remediation was intentionally left **disabled**. The crashloop pod is
  remediable, so with remediation on the operator would delete it for a
  controller reschedule; here it is only counted as
  `k8sai_remediations_skipped_total{reason="disabled"}`.
