// podwatch_controller.go — watches pods cluster-wide, detects crashloops,
// OOM kills, and image-pull failures, requests AI diagnosis via AIInsight
// custom resources, and (when explicitly enabled) applies safe remediations.
package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	aiv1alpha1 "github.com/sharatvikas/k8s-ai-ops/api/v1alpha1"
	opmetrics "github.com/sharatvikas/k8s-ai-ops/internal/metrics"
)

// Incident types detected by the pod watcher.
const (
	IncidentCrashLoop   = "CrashLoopBackOff"
	IncidentOOMKilled   = "OOMKilled"
	IncidentImagePull   = "ImagePullFailure"
	incidentLabelPrefix = "k8s-ai-ops.io/"
)

// oomRecencyWindow bounds how old an OOM termination may be and still count
// as an active incident. LastTerminationState persists indefinitely, so
// without this guard a pod that OOMed once days ago would be flagged forever.
const oomRecencyWindow = 15 * time.Minute

// PodWatchConfig tunes detection and remediation behaviour.
type PodWatchConfig struct {
	// RemediationEnabled gates all mutating actions. When false the watcher
	// is observe-and-diagnose only.
	RemediationEnabled bool
	// RemediationCooldown is the minimum interval between remediations of
	// pods belonging to the same workload (owner).
	RemediationCooldown time.Duration
	// MaxRemediationsPerHour is a global budget across all workloads —
	// a circuit breaker against remediation storms.
	MaxRemediationsPerHour int
	// CrashLoopRestartThreshold is the restart count below which a
	// crashlooping container is not yet treated as an incident.
	CrashLoopRestartThreshold int32
	// IgnoreNamespaces are namespaces the watcher never touches.
	IgnoreNamespaces map[string]struct{}
	// InsightTTLSeconds is applied to auto-created AIInsight resources.
	InsightTTLSeconds int64
}

// DefaultIgnoredNamespaces are control-plane namespaces the watcher must
// never diagnose or remediate.
func DefaultIgnoredNamespaces() map[string]struct{} {
	return map[string]struct{}{
		"kube-system":     {},
		"kube-public":     {},
		"kube-node-lease": {},
	}
}

// incident is a detected workload problem on a single container.
type incident struct {
	Type       string
	Container  string
	Detail     string
	Restarts   int32
	Remediable bool // true when deleting the pod is a safe, useful action
}

// PodWatchReconciler watches pods and turns unhealthy container states into
// AIInsight diagnosis requests and (optionally) safe remediations.
type PodWatchReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Config   PodWatchConfig

	mu              sync.Mutex
	lastRemediation map[string]time.Time // key: namespace/ownerKind/ownerName
	recentActions   []time.Time          // sliding one-hour window for the global budget
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=ai.k8s-ai-ops.io,resources=aiinsights,verbs=get;list;watch;create

func (r *PodWatchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if _, ignored := r.Config.IgnoreNamespaces[req.Namespace]; ignored {
		return ctrl.Result{}, nil
	}

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pod.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	incidents := r.detectIncidents(&pod)
	if len(incidents) == 0 {
		return ctrl.Result{}, nil
	}

	for _, inc := range incidents {
		created, err := r.ensureInsight(ctx, &pod, inc)
		if err != nil {
			logger.Error(err, "failed to ensure AIInsight", "pod", pod.Name, "incident", inc.Type)
			return ctrl.Result{}, err
		}
		if created {
			opmetrics.IncidentsDetected.WithLabelValues(inc.Type, pod.Namespace).Inc()
			logger.Info("incident detected, AIInsight created",
				"pod", pod.Name, "namespace", pod.Namespace,
				"incident", inc.Type, "container", inc.Container,
				"restarts", inc.Restarts, "detail", inc.Detail)
			r.Recorder.Eventf(&pod, corev1.EventTypeWarning, "IncidentDetected",
				"k8s-ai-ops detected %s on container %q (%s); AI diagnosis requested as AIInsight/%s",
				inc.Type, inc.Container, inc.Detail, insightName(&pod, inc.Type))
		}
	}

	// Attempt remediation for the first remediable incident, if any.
	for _, inc := range incidents {
		if inc.Remediable {
			return ctrl.Result{}, r.maybeRemediate(ctx, &pod, inc)
		}
	}
	return ctrl.Result{}, nil
}

// detectIncidents inspects container statuses for the three incident classes.
func (r *PodWatchReconciler) detectIncidents(pod *corev1.Pod) []incident {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)

	var incidents []incident
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff":
				if cs.RestartCount >= r.Config.CrashLoopRestartThreshold {
					detail := fmt.Sprintf("%d restarts", cs.RestartCount)
					// An OOM-killed container usually surfaces as a crashloop;
					// classify by the last termination so the diagnosis prompt
					// and remediation policy are more specific.
					if lt := cs.LastTerminationState.Terminated; lt != nil && lt.Reason == "OOMKilled" {
						incidents = append(incidents, incident{
							Type:       IncidentOOMKilled,
							Container:  cs.Name,
							Detail:     fmt.Sprintf("OOMKilled, exit=%d, %s", lt.ExitCode, detail),
							Restarts:   cs.RestartCount,
							Remediable: true,
						})
						continue
					}
					incidents = append(incidents, incident{
						Type:       IncidentCrashLoop,
						Container:  cs.Name,
						Detail:     detail,
						Restarts:   cs.RestartCount,
						Remediable: true,
					})
				}
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				incidents = append(incidents, incident{
					Type:      IncidentImagePull,
					Container: cs.Name,
					Detail:    fmt.Sprintf("%s: %s", w.Reason, truncate(w.Message, 200)),
					Restarts:  cs.RestartCount,
					// Deleting the pod does not fix a bad image reference or
					// registry auth — never remediate, only diagnose.
					Remediable: false,
				})
			}
			continue
		}
		// Recent OOM kill on a container that is not (yet) in CrashLoopBackOff.
		if lt := cs.LastTerminationState.Terminated; lt != nil &&
			lt.Reason == "OOMKilled" &&
			time.Since(lt.FinishedAt.Time) < oomRecencyWindow {
			incidents = append(incidents, incident{
				Type:       IncidentOOMKilled,
				Container:  cs.Name,
				Detail:     fmt.Sprintf("OOMKilled at %s, exit=%d", lt.FinishedAt.Format(time.RFC3339), lt.ExitCode),
				Restarts:   cs.RestartCount,
				Remediable: false, // container already restarted; nothing safe to do
			})
		}
	}
	return incidents
}

// ensureInsight creates a Diagnose AIInsight for the pod+incident pair if one
// does not already exist. Returns true when a new insight was created — this
// is also what deduplicates incident metrics and events across reconciles.
func (r *PodWatchReconciler) ensureInsight(ctx context.Context, pod *corev1.Pod, inc incident) (bool, error) {
	name := insightName(pod, inc.Type)

	var existing aiv1alpha1.AIInsight
	err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: name}, &existing)
	if err == nil {
		return false, nil // already requested
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get AIInsight %s: %w", name, err)
	}

	hints := map[string]string{
		"detectedCondition": inc.Type,
		"container":         inc.Container,
		"detail":            inc.Detail,
		"restartCount":      fmt.Sprintf("%d", inc.Restarts),
		"trigger":           "podwatch",
	}
	if owner := metav1.GetControllerOf(pod); owner != nil {
		hints["owner"] = fmt.Sprintf("%s/%s", owner.Kind, owner.Name)
	}

	insight := &aiv1alpha1.AIInsight{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pod.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":        "k8s-ai-ops",
				incidentLabelPrefix + "incident-type": strings.ToLower(inc.Type),
				incidentLabelPrefix + "pod":           pod.Name,
			},
		},
		Spec: aiv1alpha1.AIInsightSpec{
			Target: aiv1alpha1.AIInsightTarget{
				Kind:      "Pod",
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			AnalysisType: "Diagnose",
			TTLSeconds:   r.Config.InsightTTLSeconds,
			ContextHints: hints,
		},
	}

	if err := r.Create(ctx, insight); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil // lost a race with a concurrent reconcile
		}
		return false, fmt.Errorf("create AIInsight %s: %w", name, err)
	}
	opmetrics.InsightsCreated.WithLabelValues("Diagnose", "podwatch").Inc()
	return true, nil
}

// maybeRemediate deletes a crashlooping/OOM-killed pod so its controller
// reschedules it — but only when every safety gate passes:
//
//  1. remediation is explicitly enabled,
//  2. the pod is managed by a workload controller (it will be recreated),
//  3. the owning workload is outside its cooldown window,
//  4. the global remediations-per-hour budget is not exhausted.
func (r *PodWatchReconciler) maybeRemediate(ctx context.Context, pod *corev1.Pod, inc incident) error {
	logger := log.FromContext(ctx)

	if !r.Config.RemediationEnabled {
		opmetrics.RemediationsSkipped.WithLabelValues("disabled").Inc()
		return nil
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || !isWorkloadController(owner.Kind) {
		opmetrics.RemediationsSkipped.WithLabelValues("unmanaged_pod").Inc()
		logger.Info("skipping remediation: pod has no workload controller; deletion would not recreate it",
			"pod", pod.Name, "namespace", pod.Namespace)
		return nil
	}

	key := fmt.Sprintf("%s/%s/%s", pod.Namespace, owner.Kind, owner.Name)
	if reason, ok := r.acquireRemediationSlot(key); !ok {
		opmetrics.RemediationsSkipped.WithLabelValues(reason).Inc()
		logger.Info("skipping remediation", "reason", reason, "workload", key, "pod", pod.Name)
		return nil
	}

	if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil // pod already gone or replaced
		}
		opmetrics.RemediationsTotal.WithLabelValues("delete_pod", "error").Inc()
		return fmt.Errorf("delete pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	opmetrics.RemediationsTotal.WithLabelValues("delete_pod", "success").Inc()
	logger.Info("auto-remediation applied: pod deleted for controller reschedule",
		"pod", pod.Name, "namespace", pod.Namespace,
		"incident", inc.Type, "workload", key,
		"cooldown", r.Config.RemediationCooldown.String())
	r.Recorder.Eventf(pod, corev1.EventTypeNormal, "AutoRemediated",
		"k8s-ai-ops deleted pod after %s (%s); %s/%s will reschedule it. Next remediation for this workload allowed after %s.",
		inc.Type, inc.Detail, owner.Kind, owner.Name, r.Config.RemediationCooldown)
	return nil
}

// acquireRemediationSlot checks the per-workload cooldown and the global
// hourly budget, and records the action if both pass. Returns the skip
// reason when the slot is not granted.
func (r *PodWatchReconciler) acquireRemediationSlot(workloadKey string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if r.lastRemediation == nil {
		r.lastRemediation = make(map[string]time.Time)
	}

	if last, ok := r.lastRemediation[workloadKey]; ok && now.Sub(last) < r.Config.RemediationCooldown {
		return "cooldown", false
	}

	// Prune the sliding window, then check the budget.
	cutoff := now.Add(-time.Hour)
	kept := r.recentActions[:0]
	for _, t := range r.recentActions {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.recentActions = kept
	if len(r.recentActions) >= r.Config.MaxRemediationsPerHour {
		return "budget_exhausted", false
	}

	r.lastRemediation[workloadKey] = now
	r.recentActions = append(r.recentActions, now)
	return "", true
}

func isWorkloadController(kind string) bool {
	switch kind {
	case "ReplicaSet", "StatefulSet", "DaemonSet", "ReplicationController":
		return true
	default:
		return false
	}
}

// insightName builds a deterministic, DNS-safe name for the auto-created
// AIInsight so repeated reconciles of the same incident deduplicate.
func insightName(pod *corev1.Pod, incidentType string) string {
	name := fmt.Sprintf("auto-%s-%s", pod.Name, strings.ToLower(incidentType))
	if len(name) > 253 {
		name = name[:253]
	}
	return strings.Trim(name, "-.")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// podLooksUnhealthy is the watch predicate filter: only pods with a waiting
// container in a state we care about, or a recorded OOM kill, enter the
// reconcile queue.
func podLooksUnhealthy(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return true
			}
		}
		if lt := cs.LastTerminationState.Terminated; lt != nil && lt.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

// SetupWithManager registers the pod watcher with the manager.
func (r *PodWatchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	unhealthyPods := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return podLooksUnhealthy(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return podLooksUnhealthy(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return podLooksUnhealthy(e.Object) },
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("podwatch").
		For(&corev1.Pod{}, builder.WithPredicates(unhealthyPods)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}
