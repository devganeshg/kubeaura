package k8s

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// ServiceObservability is the "every service in observability" view: a service
// joined to its backing pods, each pod's readiness and live CPU/memory usage,
// and rolled-up totals. This is what powers per-service metric levels without
// needing Prometheus — usage comes from the metrics-server.
type ServiceObservability struct {
	Name         string        `json:"name"`
	Namespace    string        `json:"namespace"`
	Type         string        `json:"type"`
	ClusterIP    string        `json:"clusterIP"`
	Selector     string        `json:"selector"`
	MetricsReady bool          `json:"metricsReady"`
	Pods         []ServicePod  `json:"pods"`
	ReadyPods    int           `json:"readyPods"`
	TotalPods    int           `json:"totalPods"`
	CPUMilli     int64         `json:"cpuMilli"` // summed across backing pods
	MemBytes     int64         `json:"memBytes"`
	Ports        []ServicePort `json:"ports"`
}

// ServicePod is one backing pod with its health and usage.
type ServicePod struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Ready    bool   `json:"ready"`
	Node     string `json:"node"`
	Restarts int    `json:"restarts"`
	CPUMilli int64  `json:"cpuMilli"`
	MemBytes int64  `json:"memBytes"`
}

// ServicePort echoes the service's exposed ports.
type ServicePort struct {
	Name       string `json:"name"`
	Port       int32  `json:"port"`
	TargetPort string `json:"targetPort"`
	Protocol   string `json:"protocol"`
}

// ServiceObservability builds the per-service view. Pod usage is best-effort:
// when the metrics-server is absent the pods still appear with health, just
// without CPU/memory numbers (MetricsReady=false).
func (c *Client) ServiceObservability(namespace, name string) (*ServiceObservability, error) {
	cx, cancel := ctx()
	defer cancel()
	svc, err := c.cs.CoreV1().Services(namespace).Get(cx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := &ServiceObservability{
		Name: svc.Name, Namespace: svc.Namespace, Type: string(svc.Spec.Type),
		ClusterIP: svc.Spec.ClusterIP, Selector: labels.Set(svc.Spec.Selector).String(),
	}
	for _, p := range svc.Spec.Ports {
		out.Ports = append(out.Ports, ServicePort{
			Name: p.Name, Port: p.Port, TargetPort: p.TargetPort.String(), Protocol: string(p.Protocol),
		})
	}
	if len(svc.Spec.Selector) == 0 {
		return out, nil // headless/externalName or manually-managed endpoints
	}

	sel := labels.SelectorFromSet(svc.Spec.Selector).String()
	pods, err := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}

	// Join live usage from the metrics-server, keyed by pod name.
	usage := map[string]PodUsage{}
	if pm, err := c.PodMetrics(namespace); err == nil && len(pm) > 0 {
		out.MetricsReady = c.MetricsAvailable()
		for _, u := range pm {
			usage[u.Name] = u
		}
	}

	for _, p := range pods.Items {
		ready, restarts := podReady(p)
		u := usage[p.Name]
		sp := ServicePod{
			Name: p.Name, Status: podDisplayStatus(p), Ready: ready, Node: p.Spec.NodeName,
			Restarts: restarts, CPUMilli: u.CPUMilli, MemBytes: u.MemBytes,
		}
		out.Pods = append(out.Pods, sp)
		out.TotalPods++
		if ready {
			out.ReadyPods++
		}
		out.CPUMilli += u.CPUMilli
		out.MemBytes += u.MemBytes
	}
	return out, nil
}

func podReady(p corev1.Pod) (ready bool, restarts int) {
	readyCount, total := 0, len(p.Spec.Containers)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			readyCount++
		}
		restarts += int(cs.RestartCount)
	}
	return total > 0 && readyCount == total, restarts
}

func podDisplayStatus(p corev1.Pod) string {
	status := string(p.Status.Phase)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			status = cs.State.Waiting.Reason
		}
	}
	return status
}

var _ = fmt.Sprintf
