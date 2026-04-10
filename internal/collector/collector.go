package collector

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// PodContext holds all the context needed to diagnose a pod
type PodContext struct {
	Name              string
	Namespace         string
	Phase             string
	NodeName          string
	ContainerStatuses []ContainerStatus
	Events            []Event
	Logs              []string
	RecentDeployment  string
}

// ContainerStatus summarizes a container's state
type ContainerStatus struct {
	Name         string
	Ready        bool
	RestartCount int32
	State        string
	LastState    string
}

// Event represents a K8s event
type Event struct {
	Reason  string
	Message string
	Count   int32
}

// ResourceMetrics holds utilization data for resource recommendation
type ResourceMetrics struct {
	DeploymentName    string
	Namespace         string
	Window            string
	CurrentCPURequest string
	CurrentCPULimit   string
	CurrentMemRequest string
	CurrentMemLimit   string
	CPUP50            string
	CPUP90            string
	CPUP99            string
	CPUMax            string
	MemP50            string
	MemP90            string
	MemP99            string
	MemMax            string
	OOMKillCount      int
	CPUThrottleRate   float64
}

// K8sCollector collects context from a Kubernetes cluster
type K8sCollector struct {
	clientset *kubernetes.Clientset
}

// NewK8sCollector creates a collector using the current kubeconfig
func NewK8sCollector() (*K8sCollector, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	return &K8sCollector{clientset: clientset}, nil
}

// CollectPodContext gathers all diagnostic information for a pod
func (c *K8sCollector) CollectPodContext(name, namespace string) (*PodContext, error) {
	ctx := context.Background()

	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod not found: %w", err)
	}

	podCtx := &PodContext{
		Name:      name,
		Namespace: namespace,
		Phase:     string(pod.Status.Phase),
		NodeName:  pod.Spec.NodeName,
	}

	// Container statuses
	for _, cs := range pod.Status.ContainerStatuses {
		cStatus := ContainerStatus{
			Name:         cs.Name,
			Ready:        cs.Ready,
			RestartCount: cs.RestartCount,
		}

		if cs.State.Running != nil {
			cStatus.State = "Running"
		} else if cs.State.Waiting != nil {
			cStatus.State = fmt.Sprintf("Waiting: %s (%s)", cs.State.Waiting.Reason, cs.State.Waiting.Message)
		} else if cs.State.Terminated != nil {
			cStatus.State = fmt.Sprintf("Terminated: exit=%d reason=%s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}

		if cs.LastTerminationState.Terminated != nil {
			lt := cs.LastTerminationState.Terminated
			cStatus.LastState = fmt.Sprintf("exit=%d reason=%s finished=%s", lt.ExitCode, lt.Reason, lt.FinishedAt)
		}

		podCtx.ContainerStatuses = append(podCtx.ContainerStatuses, cStatus)
	}

	// Events
	eventList, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", name),
	})
	if err == nil {
		for _, e := range eventList.Items {
			if e.Type == corev1.EventTypeWarning {
				podCtx.Events = append(podCtx.Events, Event{
					Reason:  e.Reason,
					Message: e.Message,
					Count:   e.Count,
				})
			}
		}
	}

	// Logs (last 30 lines, or previous container if crash)
	logOpts := &corev1.PodLogOptions{TailLines: int64Ptr(30)}
	if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].RestartCount > 0 {
		logOpts.Previous = true
	}

	logBytes, err := c.clientset.CoreV1().Pods(namespace).GetLogs(name, logOpts).DoRaw(ctx)
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
		podCtx.Logs = lines
	}

	return podCtx, nil
}

// CollectResourceMetrics gathers utilization metrics for a deployment
// In production this would query Prometheus/metrics-server; returns stub for now
func (c *K8sCollector) CollectResourceMetrics(target, namespace, window string) (*ResourceMetrics, error) {
	parts := strings.SplitN(target, "/", 2)
	name := parts[len(parts)-1]

	ctx := context.Background()
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("deployment not found: %w", err)
	}

	metrics := &ResourceMetrics{
		DeploymentName: name,
		Namespace:      namespace,
		Window:         window,
	}

	// Extract current resource settings
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		c0 := dep.Spec.Template.Spec.Containers[0]
		if c0.Resources.Requests != nil {
			if cpu, ok := c0.Resources.Requests["cpu"]; ok {
				metrics.CurrentCPURequest = cpu.String()
			}
			if mem, ok := c0.Resources.Requests["memory"]; ok {
				metrics.CurrentMemRequest = mem.String()
			}
		}
		if c0.Resources.Limits != nil {
			if cpu, ok := c0.Resources.Limits["cpu"]; ok {
				metrics.CurrentCPULimit = cpu.String()
			}
			if mem, ok := c0.Resources.Limits["memory"]; ok {
				metrics.CurrentMemLimit = mem.String()
			}
		}
	}

	// In production: query Prometheus for actual metrics
	// For now, return placeholder values that show the pattern
	metrics.CPUP50 = "12m"
	metrics.CPUP90 = "45m"
	metrics.CPUP99 = "120m"
	metrics.CPUMax = "250m"
	metrics.MemP50 = "128Mi"
	metrics.MemP90 = "256Mi"
	metrics.MemP99 = "380Mi"
	metrics.MemMax = "512Mi"
	metrics.CPUThrottleRate = 2.3

	return metrics, nil
}

func int64Ptr(i int64) *int64 { return &i }
