// Package metrics defines Prometheus metrics for the k8s-ai-ops operator.
// All collectors are registered on the controller-runtime metrics registry,
// so they are exposed on the manager's --metrics-bind-address endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// IncidentsDetected counts workload incidents detected by the pod watcher,
	// labelled by incident type (CrashLoopBackOff, OOMKilled, ImagePullFailure).
	IncidentsDetected = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8sai_incidents_detected_total",
			Help: "Total number of workload incidents detected by the pod watcher.",
		},
		[]string{"type", "namespace"},
	)

	// InsightsCreated counts AIInsight custom resources created automatically.
	InsightsCreated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8sai_insights_created_total",
			Help: "Total number of AIInsight resources created by the operator.",
		},
		[]string{"analysis_type", "trigger"},
	)

	// AnalysesTotal counts completed AI analyses by type and outcome.
	AnalysesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8sai_analyses_total",
			Help: "Total number of AI analyses run by the AIInsight controller.",
		},
		[]string{"analysis_type", "outcome"},
	)

	// AnalysisDuration observes wall-clock latency of AI analyses (Claude API
	// round trip plus cluster context collection).
	AnalysisDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "k8sai_analysis_duration_seconds",
			Help:    "Duration of AI analyses in seconds.",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 120},
		},
		[]string{"analysis_type"},
	)

	// RemediationsTotal counts auto-remediation actions attempted.
	RemediationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8sai_remediations_total",
			Help: "Total number of auto-remediation actions attempted.",
		},
		[]string{"action", "result"},
	)

	// RemediationsSkipped counts remediations that were considered but skipped,
	// labelled by reason (disabled, cooldown, budget_exhausted, unmanaged_pod,
	// not_remediable).
	RemediationsSkipped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8sai_remediations_skipped_total",
			Help: "Total number of remediations considered but skipped.",
		},
		[]string{"reason"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		IncidentsDetected,
		InsightsCreated,
		AnalysesTotal,
		AnalysisDuration,
		RemediationsTotal,
		RemediationsSkipped,
	)
}
