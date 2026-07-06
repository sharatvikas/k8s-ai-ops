package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	aiv1alpha1 "github.com/sharatvikas/k8s-ai-ops/api/v1alpha1"
	"github.com/sharatvikas/k8s-ai-ops/internal/analyzer"
	"github.com/sharatvikas/k8s-ai-ops/internal/collector"
	opmetrics "github.com/sharatvikas/k8s-ai-ops/internal/metrics"
)

// analysisTimeout bounds a single AI analysis (cluster context collection plus
// the Claude API round trip). Reconcile contexts have no deadline of their own.
const analysisTimeout = 2 * time.Minute

// AIInsightReconciler reconciles AIInsight objects
type AIInsightReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Analyzer is the shared Claude API client, constructed once at startup.
	Analyzer *analyzer.Analyzer
}

// +kubebuilder:rbac:groups=ai.k8s-ai-ops.io,resources=aiinsights,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.k8s-ai-ops.io,resources=aiinsights/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods;pods/log;events;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets,verbs=get;list;watch

func (r *AIInsightReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var insight aiv1alpha1.AIInsight
	if err := r.Get(ctx, req.NamespacedName, &insight); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// TTL-based cleanup
	if insight.Status.CompletedAt != nil {
		ttl := time.Duration(insight.Spec.TTLSeconds) * time.Second
		expiry := insight.Status.CompletedAt.Add(ttl)
		if time.Now().After(expiry) {
			logger.Info("AIInsight TTL expired, deleting", "name", insight.Name)
			return ctrl.Result{}, r.Delete(ctx, &insight)
		}
		// Requeue to delete at expiry
		return ctrl.Result{RequeueAfter: time.Until(expiry)}, nil
	}

	// Skip if already terminal
	if insight.Status.Phase == "Completed" || insight.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}

	// Mark as Running
	if insight.Status.Phase != "Running" {
		patch := client.MergeFrom(insight.DeepCopy())
		insight.Status.Phase = "Running"
		meta.SetStatusCondition(&insight.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "Analyzing",
			Message:            "AI analysis in progress",
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Patch(ctx, &insight, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Collect context from the cluster and run the AI analysis with a bounded deadline
	col := collector.New(r.Client)
	start := time.Now()
	summary, err := r.runAnalysis(ctx, col, &insight)
	opmetrics.AnalysisDuration.WithLabelValues(insight.Spec.AnalysisType).Observe(time.Since(start).Seconds())
	if err != nil {
		opmetrics.AnalysesTotal.WithLabelValues(insight.Spec.AnalysisType, "failure").Inc()
		logger.Error(err, "analysis failed")
		return ctrl.Result{}, r.markFailed(ctx, &insight, err.Error())
	}
	opmetrics.AnalysesTotal.WithLabelValues(insight.Spec.AnalysisType, "success").Inc()

	return ctrl.Result{RequeueAfter: time.Duration(insight.Spec.TTLSeconds) * time.Second}, r.markCompleted(ctx, &insight, summary)
}

func (r *AIInsightReconciler) runAnalysis(
	ctx context.Context,
	col *collector.OperatorCollector,
	insight *aiv1alpha1.AIInsight,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, analysisTimeout)
	defer cancel()

	an := r.Analyzer
	target := insight.Spec.Target
	hints := insight.Spec.ContextHints

	switch insight.Spec.AnalysisType {
	case "Diagnose":
		if target.Kind != "Pod" {
			return "", fmt.Errorf("Diagnose requires Kind=Pod, got %s", target.Kind)
		}
		podCtx, err := col.CollectPodContext(ctx, target.Namespace, target.Name)
		if err != nil {
			return "", fmt.Errorf("collect pod context: %w", err)
		}
		if h, ok := hints["incidentID"]; ok {
			podCtx.IncidentID = h
		}
		return an.DiagnosePod(ctx, podCtx)

	case "Audit":
		// For audit, we pass the raw resource YAML via kubectl-style collection
		manifest, err := col.GetResourceYAML(ctx, target.Kind, target.Namespace, target.Name)
		if err != nil {
			return "", fmt.Errorf("collect manifest: %w", err)
		}
		return an.AuditManifestContent(ctx, manifest)

	case "Recommend":
		if target.Kind != "Deployment" {
			return "", fmt.Errorf("Recommend requires Kind=Deployment, got %s", target.Kind)
		}
		resCtx, err := col.CollectResourceMetrics(ctx, target.Namespace, target.Name)
		if err != nil {
			return "", fmt.Errorf("collect resource metrics: %w", err)
		}
		return an.RecommendResources(ctx, resCtx)

	case "Explain":
		event, err := col.CollectLatestEvent(ctx, target.Namespace, target.Name)
		if err != nil {
			return "", fmt.Errorf("collect event: %w", err)
		}
		return an.ExplainEvent(ctx, event)

	default:
		return "", fmt.Errorf("unknown analysisType: %s", insight.Spec.AnalysisType)
	}
}

func (r *AIInsightReconciler) markCompleted(ctx context.Context, insight *aiv1alpha1.AIInsight, summary string) error {
	patch := client.MergeFrom(insight.DeepCopy())
	now := metav1.Now()
	insight.Status.Phase = "Completed"
	insight.Status.Summary = summary
	insight.Status.CompletedAt = &now
	insight.Status.Model = r.Analyzer.Model()
	meta.SetStatusCondition(&insight.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "AnalysisComplete",
		Message:            "AI analysis completed successfully",
		LastTransitionTime: now,
	})
	return r.Status().Patch(ctx, insight, patch)
}

func (r *AIInsightReconciler) markFailed(ctx context.Context, insight *aiv1alpha1.AIInsight, reason string) error {
	patch := client.MergeFrom(insight.DeepCopy())
	insight.Status.Phase = "Failed"
	insight.Status.FailureReason = reason
	meta.SetStatusCondition(&insight.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "AnalysisFailed",
		Message:            reason,
		LastTransitionTime: metav1.Now(),
	})
	return r.Status().Patch(ctx, insight, patch)
}

func (r *AIInsightReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiv1alpha1.AIInsight{}).
		Complete(r)
}
