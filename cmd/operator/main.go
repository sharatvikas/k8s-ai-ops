// operator/main.go — controller-manager entrypoint for k8s-ai-ops.
//
// Runs two controllers:
//   - AIInsightReconciler: fulfils AIInsight custom resources by collecting
//     cluster context and querying the Claude API.
//   - PodWatchReconciler: watches pods cluster-wide, detects crashloops,
//     OOM kills, and image-pull failures, files AIInsight diagnosis requests,
//     and (only with --enable-remediation) applies safe remediations.
package main

import (
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	aiv1alpha1 "github.com/sharatvikas/k8s-ai-ops/api/v1alpha1"
	"github.com/sharatvikas/k8s-ai-ops/internal/analyzer"
	"github.com/sharatvikas/k8s-ai-ops/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(aiv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		anthropicModel       string

		enableRemediation      bool
		remediationCooldown    time.Duration
		maxRemediationsPerHour int
		crashLoopThreshold     int
		ignoreNamespaces       string
		insightTTL             time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&anthropicModel, "anthropic-model", "claude-haiku-4-5-20251001", "Claude model to use for AI analysis.")

	flag.BoolVar(&enableRemediation, "enable-remediation", false,
		"Allow the pod watcher to apply safe remediations (delete crashlooping/OOM-killed pods that are managed by a workload controller). Off by default: observe-and-diagnose only.")
	flag.DurationVar(&remediationCooldown, "remediation-cooldown", 10*time.Minute,
		"Minimum interval between remediations of pods belonging to the same workload.")
	flag.IntVar(&maxRemediationsPerHour, "max-remediations-per-hour", 10,
		"Global cap on remediation actions per hour — a circuit breaker against remediation storms.")
	flag.IntVar(&crashLoopThreshold, "crashloop-restart-threshold", 3,
		"Restart count at which a crashlooping container becomes an incident.")
	flag.StringVar(&ignoreNamespaces, "ignore-namespaces", "",
		"Comma-separated namespaces the pod watcher skips, in addition to kube-system, kube-public, and kube-node-lease.")
	flag.DurationVar(&insightTTL, "insight-ttl", time.Hour,
		"How long auto-created AIInsight results are retained before deletion.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Fail fast: the analyzer needs ANTHROPIC_API_KEY at startup, not at the
	// first reconcile in the middle of an incident.
	an, err := analyzer.New(anthropicModel)
	if err != nil {
		setupLog.Error(err, "unable to initialize Claude analyzer")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "k8s-ai-ops-leader",
		// Release the lease immediately on graceful shutdown so a standby
		// replica takes over without waiting for lease expiry.
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptrDuration(30 * time.Second),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.AIInsightReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Analyzer: an,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AIInsight")
		os.Exit(1)
	}

	ignored := controller.DefaultIgnoredNamespaces()
	for _, ns := range strings.Split(ignoreNamespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			ignored[ns] = struct{}{}
		}
	}

	if err = (&controller.PodWatchReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("k8s-ai-ops"),
		Config: controller.PodWatchConfig{
			RemediationEnabled:        enableRemediation,
			RemediationCooldown:       remediationCooldown,
			MaxRemediationsPerHour:    maxRemediationsPerHour,
			CrashLoopRestartThreshold: int32(crashLoopThreshold),
			IgnoreNamespaces:          ignored,
			InsightTTLSeconds:         int64(insightTTL.Seconds()),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PodWatch")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager",
		"model", anthropicModel,
		"leaderElection", enableLeaderElection,
		"remediationEnabled", enableRemediation,
		"remediationCooldown", remediationCooldown.String(),
		"maxRemediationsPerHour", maxRemediationsPerHour,
		"crashLoopRestartThreshold", crashLoopThreshold,
	)
	if !enableRemediation {
		setupLog.Info("auto-remediation is DISABLED — running in observe-and-diagnose mode; start with --enable-remediation to allow safe pod restarts")
	}

	// SetupSignalHandler cancels the context on SIGTERM/SIGINT; the manager
	// then drains work within GracefulShutdownTimeout and releases the lease.
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func ptrDuration(d time.Duration) *time.Duration { return &d }
