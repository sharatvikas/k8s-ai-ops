# Live CrashLoop / ImagePull Detection Demo — Captured Output

Real output captured against a live `kind` cluster (context `kind-sre-platform`,
node k8s v1.35.0) on 2026-07-06. Auto-remediation disabled.

## 1. Build + load the operator image

```
$ docker build --target operator -t k8s-ai-ops:demo .
 => naming to docker.io/library/k8s-ai-ops:demo
$ kind load docker-image k8s-ai-ops:demo --name sre-platform
Image: "k8s-ai-ops:demo" ... loading...
```

## 2. Deploy the operator via Helm (remediation OFF, dummy key)

```
$ kubectl create namespace k8sai-demo
namespace/k8sai-demo created

$ helm install k8s-ai-ops ./helm/k8s-ai-ops -n k8sai-demo \
    --set image.repository=k8s-ai-ops --set image.tag=demo \
    --set remediation.enabled=false \
    --set anthropicApiKey.value=sk-ant-dummy-key-for-demo-not-valid \
    --set logging.encoder=console
STATUS: deployed

$ kubectl rollout status deploy/k8s-ai-ops -n k8sai-demo
deployment "k8s-ai-ops" successfully rolled out
```

Operator startup (observe-and-diagnose mode, both controllers started):

```
INFO  setup  starting manager  {"model": "claude-haiku-4-5-20251001", "leaderElection": false, "remediationEnabled": false, "crashLoopRestartThreshold": 3}
INFO  setup  auto-remediation is DISABLED — running in observe-and-diagnose mode; ...
INFO  Starting Controller  {"controller": "aiinsight", "controllerGroup": "ai.k8s-ai-ops.io", "controllerKind": "AIInsight"}
INFO  Starting Controller  {"controller": "podwatch", "controllerKind": "Pod"}
INFO  Starting workers     {"controller": "podwatch", "worker count": 2}
```

## 3. Create the deliberately-broken workloads

```
$ kubectl apply -f demo/crashloop-deployment.yaml -f demo/imagepull-deployment.yaml
deployment.apps/crashloop-app created
deployment.apps/imagepull-app created

$ kubectl get pods -n k8sai-demo
NAME                            READY   STATUS             RESTARTS      AGE
crashloop-app-66689774f-dhj6n   1/1     Running            3 (29s ago)   51s
imagepull-app-c9c8f5fcd-fkvls   0/1     ImagePullBackOff   0             51s
k8s-ai-ops-6f76bd6ccb-b8jhn     1/1     Running            0             88s
```

## 4. Detection logs (the operator catching both faults)

```
INFO  incident detected, AIInsight created  {"controller":"podwatch", "pod":"imagepull-app-c9c8f5fcd-fkvls", "namespace":"k8sai-demo", "incident":"ImagePullFailure", "container":"badimage", "restarts":0, "detail":"ErrImagePull: ... failed to resolve reference \"registry.k8s.io/pause:this-tag-does-not-exist-9x9x…"}

INFO  incident detected, AIInsight created  {"controller":"podwatch", "pod":"crashloop-app-66689774f-dhj6n", "namespace":"k8sai-demo", "incident":"CrashLoopBackOff", "container":"crasher", "restarts":3, "detail":"3 restarts"}
```

Kubernetes Events filed on the pods:

```
$ kubectl get events -n k8sai-demo --field-selector reason=IncidentDetected
Warning  IncidentDetected  pod/crashloop-app-66689774f-dhj6n  k8s-ai-ops detected CrashLoopBackOff on container "crasher" (3 restarts); AI diagnosis requested as AIInsight/auto-crashloop-app-66689774f-dhj6n-crashloopbackoff
Warning  IncidentDetected  pod/imagepull-app-c9c8f5fcd-fkvls  k8s-ai-ops detected ImagePullFailure on container "badimage" (ErrImagePull: ...); AI diagnosis requested as AIInsight/auto-imagepull-app-c9c8f5fcd-fkvls-imagepullfailure
```

## 5. The AIInsight custom resources it filed

```
$ kubectl get aiinsight -n k8sai-demo
NAME                                                  TARGET   NAME                            TYPE       PHASE    AGE
auto-crashloop-app-66689774f-dhj6n-crashloopbackoff   Pod      crashloop-app-66689774f-dhj6n   Diagnose   Failed   6s
auto-imagepull-app-c9c8f5fcd-fkvls-imagepullfailure   Pod      imagepull-app-c9c8f5fcd-fkvls   Diagnose   Failed   99s
```

Full CrashLoop AIInsight — note the rich `spec.contextHints` populated by
detection, and the `status` showing the Claude call failing on the dummy key:

```yaml
spec:
  analysisType: Diagnose
  contextHints:
    container: crasher
    detail: 3 restarts
    detectedCondition: CrashLoopBackOff
    owner: ReplicaSet/crashloop-app-66689774f
    restartCount: "3"
    trigger: podwatch
  target:
    kind: Pod
    name: crashloop-app-66689774f-dhj6n
    namespace: k8sai-demo
  ttlSeconds: 3600
status:
  conditions:
  - lastTransitionTime: "2026-07-06T03:11:50Z"
    message: 'Claude API error: POST "https://api.anthropic.com/v1/messages": 401
      Unauthorized ... {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}'
    reason: AnalysisFailed
    status: "False"
    type: Ready
  failureReason: 'Claude API error: ... 401 Unauthorized ... "invalid x-api-key"'
  phase: Failed
```

ImagePull AIInsight context hints:

```
{"container":"badimage","detectedCondition":"ImagePullFailure","owner":"ReplicaSet/imagepull-app-c9c8f5fcd","restartCount":"0","trigger":"podwatch",
 "detail":"ErrImagePull: rpc error: code = NotFound ... registry.k8s.io/pause:this-tag-does-not-exist-9x9x…"}
phase=Failed
```

## 6. Prometheus metrics (port-forward to free port 18080)

```
$ kubectl port-forward -n k8sai-demo deploy/k8s-ai-ops 18080:8080 &
$ curl -s http://localhost:18080/metrics | grep k8sai_

k8sai_incidents_detected_total{namespace="k8sai-demo",type="CrashLoopBackOff"} 1
k8sai_incidents_detected_total{namespace="k8sai-demo",type="ImagePullFailure"} 1
k8sai_insights_created_total{analysis_type="Diagnose",trigger="podwatch"} 2
k8sai_analyses_total{analysis_type="Diagnose",outcome="failure"} 3
k8sai_remediations_skipped_total{reason="disabled"} 1
```

- 2 incidents detected (one per type), 2 AIInsights created.
- `analyses_total{outcome="failure"} 3` — the AIInsight controller attempted
  the Claude call and it failed (dummy key), as expected.
- `remediations_skipped_total{reason="disabled"} 1` — the crashloop pod was
  remediable, but the remediation safety gate refused because remediation is
  disabled.

## What needed a key / wasn't reproducible

- The LLM diagnosis is the only step that requires a real `ANTHROPIC_API_KEY`.
  With the dummy key the Claude API returns `401 invalid x-api-key` and the
  AIInsight lands in phase `Failed`. Everything before the LLM call —
  cluster-wide pod watching, incident detection, `AIInsight` CR creation with
  context hints, events, and metrics — runs fully and is captured above.
- One deploy-blocking bug was fixed (see `README.md` "Notes / honesty"):
  missing numeric `runAsUser` under the pod `securityContext`.
