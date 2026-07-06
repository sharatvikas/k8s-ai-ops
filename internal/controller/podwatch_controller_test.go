package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newReconciler() *PodWatchReconciler {
	return &PodWatchReconciler{
		Config: PodWatchConfig{
			RemediationEnabled:        true,
			RemediationCooldown:       10 * time.Minute,
			MaxRemediationsPerHour:    2,
			CrashLoopRestartThreshold: 3,
			IgnoreNamespaces:          DefaultIgnoredNamespaces(),
			InsightTTLSeconds:         3600,
		},
	}
}

func podWithStatus(statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "default"},
		Status:     corev1.PodStatus{ContainerStatuses: statuses},
	}
}

func waiting(reason, msg string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         "app",
		RestartCount: restarts,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg},
		},
	}
}

func TestDetectIncidents_CrashLoopAboveThreshold(t *testing.T) {
	r := newReconciler()
	incidents := r.detectIncidents(podWithStatus(waiting("CrashLoopBackOff", "", 5)))
	if len(incidents) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incidents))
	}
	if incidents[0].Type != IncidentCrashLoop || !incidents[0].Remediable {
		t.Errorf("expected remediable %s, got %+v", IncidentCrashLoop, incidents[0])
	}
}

func TestDetectIncidents_CrashLoopBelowThreshold(t *testing.T) {
	r := newReconciler()
	if incidents := r.detectIncidents(podWithStatus(waiting("CrashLoopBackOff", "", 2))); len(incidents) != 0 {
		t.Fatalf("expected no incidents below restart threshold, got %+v", incidents)
	}
}

func TestDetectIncidents_CrashLoopFromOOMClassifiedAsOOM(t *testing.T) {
	r := newReconciler()
	cs := waiting("CrashLoopBackOff", "", 4)
	cs.LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
	}
	incidents := r.detectIncidents(podWithStatus(cs))
	if len(incidents) != 1 || incidents[0].Type != IncidentOOMKilled || !incidents[0].Remediable {
		t.Fatalf("expected remediable OOMKilled classification, got %+v", incidents)
	}
}

func TestDetectIncidents_ImagePullNeverRemediable(t *testing.T) {
	r := newReconciler()
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName"} {
		incidents := r.detectIncidents(podWithStatus(waiting(reason, "manifest unknown", 0)))
		if len(incidents) != 1 {
			t.Fatalf("%s: expected 1 incident, got %d", reason, len(incidents))
		}
		if incidents[0].Type != IncidentImagePull || incidents[0].Remediable {
			t.Errorf("%s: image-pull incidents must not be remediable: %+v", reason, incidents[0])
		}
	}
}

func TestDetectIncidents_StaleOOMIgnored(t *testing.T) {
	r := newReconciler()
	cs := corev1.ContainerStatus{
		Name:  "app",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "OOMKilled",
				ExitCode:   137,
				FinishedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			},
		},
	}
	if incidents := r.detectIncidents(podWithStatus(cs)); len(incidents) != 0 {
		t.Fatalf("OOM kill outside recency window must not be an incident, got %+v", incidents)
	}
}

func TestDetectIncidents_RecentOOMNotRemediable(t *testing.T) {
	r := newReconciler()
	cs := corev1.ContainerStatus{
		Name:  "app",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "OOMKilled",
				ExitCode:   137,
				FinishedAt: metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			},
		},
	}
	incidents := r.detectIncidents(podWithStatus(cs))
	if len(incidents) != 1 || incidents[0].Type != IncidentOOMKilled {
		t.Fatalf("expected recent OOM incident, got %+v", incidents)
	}
	if incidents[0].Remediable {
		t.Error("already-restarted OOM container must not be remediable")
	}
}

func TestAcquireRemediationSlot_CooldownPerWorkload(t *testing.T) {
	r := newReconciler()
	if reason, ok := r.acquireRemediationSlot("default/ReplicaSet/web"); !ok {
		t.Fatalf("first slot should be granted, got skip reason %q", reason)
	}
	if reason, ok := r.acquireRemediationSlot("default/ReplicaSet/web"); ok || reason != "cooldown" {
		t.Fatalf("second slot within cooldown should be denied with 'cooldown', got ok=%v reason=%q", ok, reason)
	}
	// A different workload is not affected by the first workload's cooldown.
	if reason, ok := r.acquireRemediationSlot("default/StatefulSet/db"); !ok {
		t.Fatalf("different workload should be granted, got skip reason %q", reason)
	}
}

func TestAcquireRemediationSlot_GlobalBudget(t *testing.T) {
	r := newReconciler() // MaxRemediationsPerHour: 2
	r.acquireRemediationSlot("ns/ReplicaSet/a")
	r.acquireRemediationSlot("ns/ReplicaSet/b")
	if reason, ok := r.acquireRemediationSlot("ns/ReplicaSet/c"); ok || reason != "budget_exhausted" {
		t.Fatalf("third slot should be denied with 'budget_exhausted', got ok=%v reason=%q", ok, reason)
	}
}

func TestPodLooksUnhealthy(t *testing.T) {
	if podLooksUnhealthy(podWithStatus(corev1.ContainerStatus{
		Name:  "app",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})) {
		t.Error("healthy running pod must not pass the watch predicate")
	}
	if !podLooksUnhealthy(podWithStatus(waiting("CrashLoopBackOff", "", 1))) {
		t.Error("crashlooping pod must pass the watch predicate")
	}
	if !podLooksUnhealthy(podWithStatus(waiting("ImagePullBackOff", "", 0))) {
		t.Error("image-pull-failing pod must pass the watch predicate")
	}
}

func TestIsWorkloadController(t *testing.T) {
	for kind, want := range map[string]bool{
		"ReplicaSet": true, "StatefulSet": true, "DaemonSet": true,
		"ReplicationController": true, "Job": false, "Node": false, "": false,
	} {
		if got := isWorkloadController(kind); got != want {
			t.Errorf("isWorkloadController(%q) = %v, want %v", kind, got, want)
		}
	}
}

func TestInsightName_DeterministicAndBounded(t *testing.T) {
	pod := podWithStatus()
	a := insightName(pod, IncidentCrashLoop)
	b := insightName(pod, IncidentCrashLoop)
	if a != b {
		t.Errorf("insight name must be deterministic: %q != %q", a, b)
	}
	if a != "auto-web-abc123-crashloopbackoff" {
		t.Errorf("unexpected insight name %q", a)
	}
	long := podWithStatus()
	for len(long.Name) < 300 {
		long.Name += "x"
	}
	if n := insightName(long, IncidentOOMKilled); len(n) > 253 {
		t.Errorf("insight name exceeds 253 chars: %d", len(n))
	}
}
