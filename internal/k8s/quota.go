package k8s

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NamespaceQuota summarizes ResourceQuota usage for a namespace.
type NamespaceQuota struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`

	PodsUsed int64 `json:"podsUsed"`
	PodsHard int64 `json:"podsHard"`

	ReqCPUMilliUsed int64 `json:"reqCpuMilliUsed"`
	ReqCPUMilliHard int64 `json:"reqCpuMilliHard"`
	LimCPUMilliUsed int64 `json:"limCpuMilliUsed"`
	LimCPUMilliHard int64 `json:"limCpuMilliHard"`

	ReqMemBytesUsed int64 `json:"reqMemBytesUsed"`
	ReqMemBytesHard int64 `json:"reqMemBytesHard"`
	LimMemBytesUsed int64 `json:"limMemBytesUsed"`
	LimMemBytesHard int64 `json:"limMemBytesHard"`
}

// NamespaceQuotas returns ResourceQuota usage by namespace (or one namespace when provided).
func (c *Client) NamespaceQuotas(namespace string) ([]NamespaceQuota, error) {
	cx, cancel := ctx()
	defer cancel()

	list, err := c.cs.CoreV1().ResourceQuotas(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	out := make([]NamespaceQuota, 0, len(list.Items))
	for _, rq := range list.Items {
		u, h := rq.Status.Used, rq.Status.Hard
		row := NamespaceQuota{
			Namespace: rq.Namespace,
			Name:      rq.Name,

			PodsUsed: qInt(u[corev1.ResourcePods]),
			PodsHard: qInt(h[corev1.ResourcePods]),

			ReqCPUMilliUsed: qMilli(u[corev1.ResourceRequestsCPU]),
			ReqCPUMilliHard: qMilli(h[corev1.ResourceRequestsCPU]),
			LimCPUMilliUsed: qMilli(u[corev1.ResourceLimitsCPU]),
			LimCPUMilliHard: qMilli(h[corev1.ResourceLimitsCPU]),

			ReqMemBytesUsed: qBytes(u[corev1.ResourceRequestsMemory]),
			ReqMemBytesHard: qBytes(h[corev1.ResourceRequestsMemory]),
			LimMemBytesUsed: qBytes(u[corev1.ResourceLimitsMemory]),
			LimMemBytesHard: qBytes(h[corev1.ResourceLimitsMemory]),
		}
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].Name < out[j].Name
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out, nil
}

func qMilli(q resource.Quantity) int64 { return q.MilliValue() }

func qBytes(q resource.Quantity) int64 { return q.Value() }

func qInt(q resource.Quantity) int64 { return q.Value() }
