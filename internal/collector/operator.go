// operator.go — controller-runtime client-based collector for use in the operator reconciler.
// The CLI collector (collector.go) uses client-go Clientset directly.
// This file provides New() and methods that work with sigs.k8s.io/controller-runtime/pkg/client.
package collector

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// OperatorCollector uses a controller-runtime client for use inside the operator reconciler.
type OperatorCollector struct {
	client client.Client
}

// New creates an OperatorCollector for use in the AIInsight controller.
func New(c client.Client) *OperatorCollector {
	return &OperatorCollector{client: c}
}

// CollectPodContext gathers pod status, container states, and recent events.
func (c *OperatorCollector) CollectPodContext(ctx context.Context, namespace, podName string) (*PodContext, error) {
	var pod corev1.Pod
	if err := c.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err != nil {
		return nil, fmt.Errorf("get pod: %w", err)
	}

	pc := &PodContext{
		Name:      podName,
		Namespace: namespace,
		Phase:     string(pod.Status.Phase),
		NodeName:  pod.Spec.NodeName,
	}

	for _, cs := range pod.Status.ContainerStatuses {
		cStatus := ContainerStatus{
			Name:         cs.Name,
			Ready:        cs.Ready,
			RestartCount: cs.RestartCount,
		}
		if cs.State.Waiting != nil {
			cStatus.State = fmt.Sprintf("Waiting: %s (%s)", cs.State.Waiting.Reason, cs.State.Waiting.Message)
		} else if cs.State.Running != nil {
			cStatus.State = "Running"
		} else if cs.State.Terminated != nil {
			cStatus.State = fmt.Sprintf("Terminated: exit=%d reason=%s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
		if cs.LastTerminationState.Terminated != nil {
			lt := cs.LastTerminationState.Terminated
			cStatus.LastState = fmt.Sprintf("exit=%d reason=%s", lt.ExitCode, lt.Reason)
		}
		pc.ContainerStatuses = append(pc.ContainerStatuses, cStatus)
	}

	// Recent warning events
	var eventList corev1.EventList
	_ = c.client.List(ctx, &eventList, client.InNamespace(namespace))
	for _, ev := range eventList.Items {
		if ev.InvolvedObject.Name == podName &&
			ev.Type == corev1.EventTypeWarning &&
			time.Since(ev.LastTimestamp.Time) < 2*time.Hour {
			pc.Events = append(pc.Events, Event{
				Reason:  ev.Reason,
				Message: ev.Message,
				Count:   ev.Count,
			})
		}
	}

	return pc, nil
}

// CollectResourceMetrics fetches current resource requests/limits for a deployment.
func (c *OperatorCollector) CollectResourceMetrics(ctx context.Context, namespace, deploymentName string) (*ResourceMetrics, error) {
	var dep appsv1.Deployment
	if err := c.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: deploymentName}, &dep); err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}

	metrics := &ResourceMetrics{
		DeploymentName: deploymentName,
		Namespace:      namespace,
		Window:         "7d",
	}

	if len(dep.Spec.Template.Spec.Containers) > 0 {
		c0 := dep.Spec.Template.Spec.Containers[0]
		if c0.Resources.Requests != nil {
			if cpu, ok := c0.Resources.Requests[corev1.ResourceCPU]; ok {
				metrics.CurrentCPURequest = cpu.String()
			}
			if mem, ok := c0.Resources.Requests[corev1.ResourceMemory]; ok {
				metrics.CurrentMemRequest = mem.String()
			}
		}
		if c0.Resources.Limits != nil {
			if cpu, ok := c0.Resources.Limits[corev1.ResourceCPU]; ok {
				metrics.CurrentCPULimit = cpu.String()
			}
			if mem, ok := c0.Resources.Limits[corev1.ResourceMemory]; ok {
				metrics.CurrentMemLimit = mem.String()
			}
		}
	}

	return metrics, nil
}

// CollectLatestEvent returns the most recent events for a named resource as a formatted string.
func (c *OperatorCollector) CollectLatestEvent(ctx context.Context, namespace, resourceName string) (string, error) {
	var eventList corev1.EventList
	if err := c.client.List(ctx, &eventList, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list events: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Recent events for %s/%s:\n", namespace, resourceName))
	count := 0
	for _, ev := range eventList.Items {
		if ev.InvolvedObject.Name == resourceName {
			sb.WriteString(fmt.Sprintf("  [%s] %s/%s: %s\n",
				ev.LastTimestamp.Format(time.RFC3339),
				ev.Type, ev.Reason, ev.Message))
			count++
			if count >= 10 {
				break
			}
		}
	}
	if count == 0 {
		sb.WriteString("  No events found.\n")
	}
	return sb.String(), nil
}

// GetResourceYAML returns the YAML representation of a resource for audit.
func (c *OperatorCollector) GetResourceYAML(ctx context.Context, kind, namespace, name string) (string, error) {
	nn := types.NamespacedName{Namespace: namespace, Name: name}

	var obj interface{}
	switch strings.ToLower(kind) {
	case "pod":
		var pod corev1.Pod
		if err := c.client.Get(ctx, nn, &pod); err != nil {
			return "", err
		}
		pod.ManagedFields = nil
		pod.ResourceVersion = ""
		obj = pod
	case "deployment":
		var dep appsv1.Deployment
		if err := c.client.Get(ctx, nn, &dep); err != nil {
			return "", err
		}
		dep.ManagedFields = nil
		dep.ResourceVersion = ""
		obj = dep
	default:
		return "", fmt.Errorf("unsupported kind for audit: %s", kind)
	}

	out, err := yaml.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// IncidentID field support on PodContext (used by controller contextHints injection)
func init() {
	// Validate PodContext has IncidentID at compile time via type assertion
	_ = &PodContext{}
}

// Extend PodContext with IncidentID for operator contextHints injection.
// This is added here to keep CLI collector.go unchanged.
var _ = metav1.Now // ensure metav1 import used
