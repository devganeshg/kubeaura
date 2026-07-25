package k8s

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeUsage is per-node CPU/memory usage vs. allocatable capacity. Percentages
// are 0-100 and are what the dashboard heatmap and bar charts render. Usage
// comes from the metrics-server; capacity from the Node object itself.
type NodeUsage struct {
	Name        string  `json:"name"`
	CPUMilli    int64   `json:"cpuMilli"`    // used millicores
	CPUCapMilli int64   `json:"cpuCapMilli"` // allocatable millicores
	CPUPct      float64 `json:"cpuPct"`      // 0-100
	CPUReqPct   float64 `json:"cpuReqPct"`   // requested millicores vs allocatable
	CPULimitPct float64 `json:"cpuLimitPct"` // limit millicores vs allocatable
	MemBytes    int64   `json:"memBytes"`    // used bytes
	MemCapBytes int64   `json:"memCapBytes"` // allocatable bytes
	MemPct      float64 `json:"memPct"`      // 0-100
	MemReqPct   float64 `json:"memReqPct"`   // requested memory vs allocatable
	MemLimitPct float64 `json:"memLimitPct"` // limit memory vs allocatable
	InternalIP  string  `json:"internalIP"`
	Ready       bool    `json:"ready"`
	Pods        int     `json:"pods"` // scheduled pods on this node
}

// PodUsage is per-pod CPU/memory usage summed across containers. Used for the
// "top consumers" tables and the pod heatmap.
type PodUsage struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	CPUMilli  int64  `json:"cpuMilli"`
	MemBytes  int64  `json:"memBytes"`
}

// MetricsAvailable reports whether the metrics-server is reachable. The UI hides
// usage charts when it is not, rather than showing empty panels.
func (c *Client) MetricsAvailable() bool {
	if c.metrics == nil {
		return false
	}
	cx, cancel := ctx()
	defer cancel()
	_, err := c.metrics.MetricsV1beta1().NodeMetricses().List(cx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// NodeMetrics returns per-node usage joined with each node's allocatable
// capacity and scheduled pod count. Returns an empty slice (no error) when the
// metrics-server is not installed so the dashboard degrades gracefully.
func (c *Client) NodeMetrics() ([]NodeUsage, error) {
	if c.metrics == nil {
		return []NodeUsage{}, nil
	}
	cx, cancel := ctx()
	defer cancel()

	mlist, err := c.metrics.MetricsV1beta1().NodeMetricses().List(cx, metav1.ListOptions{})
	if err != nil {
		return []NodeUsage{}, nil // metrics-server absent → no charts, not an error
	}
	used := make(map[string]NodeUsage, len(mlist.Items))
	for _, m := range mlist.Items {
		cpu := m.Usage.Cpu().MilliValue()
		mem := m.Usage.Memory().Value()
		used[m.Name] = NodeUsage{Name: m.Name, CPUMilli: cpu, MemBytes: mem}
	}

	nodes, err := c.cs.CoreV1().Nodes().List(cx, metav1.ListOptions{})
	if err != nil {
		// Namespace-scoped RBAC may deny cluster-scoped node listing.
		// Return an empty set so the UI can continue rendering.
		if apierrors.IsForbidden(err) {
			return []NodeUsage{}, nil
		}
		return nil, err
	}

	// Count scheduled pods per node in one list.
	podsPerNode := map[string]int{}
	type alloc struct {
		cpuReqMilli   int64
		cpuLimitMilli int64
		memReqBytes   int64
		memLimitBytes int64
	}
	allocByNode := map[string]alloc{}
	if pods, err := c.cs.CoreV1().Pods("").List(cx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			if p.Spec.NodeName != "" {
				podsPerNode[p.Spec.NodeName]++
				a := allocByNode[p.Spec.NodeName]
				for _, ctr := range p.Spec.Containers {
					a.cpuReqMilli += ctr.Resources.Requests.Cpu().MilliValue()
					a.cpuLimitMilli += ctr.Resources.Limits.Cpu().MilliValue()
					a.memReqBytes += ctr.Resources.Requests.Memory().Value()
					a.memLimitBytes += ctr.Resources.Limits.Memory().Value()
				}
				allocByNode[p.Spec.NodeName] = a
			}
		}
	}

	out := make([]NodeUsage, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		u := used[n.Name]
		u.Name = n.Name
		u.Ready = nodeReady(n)
		u.Pods = podsPerNode[n.Name]
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				u.InternalIP = a.Address
				break
			}
		}
		alloc := n.Status.Allocatable
		u.CPUCapMilli = alloc.Cpu().MilliValue()
		u.MemCapBytes = alloc.Memory().Value()
		nodeAlloc := allocByNode[n.Name]
		if u.CPUCapMilli > 0 {
			u.CPUPct = round1(float64(u.CPUMilli) / float64(u.CPUCapMilli) * 100)
			u.CPUReqPct = round1(float64(nodeAlloc.cpuReqMilli) / float64(u.CPUCapMilli) * 100)
			u.CPULimitPct = round1(float64(nodeAlloc.cpuLimitMilli) / float64(u.CPUCapMilli) * 100)
		}
		if u.MemCapBytes > 0 {
			u.MemPct = round1(float64(u.MemBytes) / float64(u.MemCapBytes) * 100)
			u.MemReqPct = round1(float64(nodeAlloc.memReqBytes) / float64(u.MemCapBytes) * 100)
			u.MemLimitPct = round1(float64(nodeAlloc.memLimitBytes) / float64(u.MemCapBytes) * 100)
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PodMetrics returns per-pod usage (summed over containers), sorted by CPU
// descending. Empty (no error) when the metrics-server is absent.
func (c *Client) PodMetrics(namespace string) ([]PodUsage, error) {
	if c.metrics == nil {
		return []PodUsage{}, nil
	}
	cx, cancel := ctx()
	defer cancel()

	mlist, err := c.metrics.MetricsV1beta1().PodMetricses(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return []PodUsage{}, nil
	}
	out := make([]PodUsage, 0, len(mlist.Items))
	for _, m := range mlist.Items {
		var cpu, mem int64
		for _, ctr := range m.Containers {
			cpu += ctr.Usage.Cpu().MilliValue()
			mem += ctr.Usage.Memory().Value()
		}
		out = append(out, PodUsage{Name: m.Name, Namespace: m.Namespace, CPUMilli: cpu, MemBytes: mem})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CPUMilli > out[j].CPUMilli })
	return out, nil
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

// (compile-time: ensure corev1 stays referenced if future edits trim usage)
var _ = corev1.Node{}
