package k8s

import (
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Insights bundles the aggregated views the dashboard visualizes: phase
// breakdowns, per-namespace counts, restart leaders, and an event timeline.
// It's computed from plain LIST calls so it works on any cluster (no
// metrics-server required).
type Insights struct {
	PodPhases     []Slice      `json:"podPhases"`    // Running/Pending/Failed/…
	DeployHealth  []Slice      `json:"deployHealth"` // Healthy/Degraded
	PodsByNS      []Slice      `json:"podsByNamespace"`
	TopRestarts   []RestartRow `json:"topRestarts"`
	EventTimeline []TimeBucket `json:"eventTimeline"` // warnings vs normal, hourly
	ContainerImgs int          `json:"containerImages"`
}

// Slice is one labeled value for pie/bar charts.
type Slice struct {
	Label string `json:"label"`
	Value int    `json:"value"`
}

// RestartRow is a pod ranked by cumulative container restarts.
type RestartRow struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Restarts  int    `json:"restarts"`
	Status    string `json:"status"`
}

// TimeBucket counts events in a one-hour window for the timeline chart.
type TimeBucket struct {
	Hour     string `json:"hour"` // "14:00"
	Warnings int    `json:"warnings"`
	Normal   int    `json:"normal"`
}

// Insights computes the dashboard aggregates for the given namespace (empty =
// all namespaces).
func (c *Client) Insights(namespace string) (*Insights, error) {
	cx, cancel := ctx()
	defer cancel()
	in := &Insights{}

	pods, err := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	in.podStats(pods.Items)

	if deps, err := c.cs.AppsV1().Deployments(namespace).List(cx, metav1.ListOptions{}); err == nil {
		in.deployStats(deps.Items)
	}
	if evs, err := c.cs.CoreV1().Events(namespace).List(cx, metav1.ListOptions{}); err == nil {
		in.eventTimeline(evs.Items)
	}
	return in, nil
}

func (in *Insights) podStats(pods []corev1.Pod) {
	phase := map[string]int{}
	byNS := map[string]int{}
	images := map[string]struct{}{}
	restarts := make([]RestartRow, 0)
	for _, p := range pods {
		ph := string(p.Status.Phase)
		// Prefer a waiting reason (CrashLoopBackOff, ImagePullBackOff) when present.
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				ph = cs.State.Waiting.Reason
			}
		}
		phase[ph]++
		byNS[p.Namespace]++
		for _, ctr := range p.Spec.Containers {
			images[ctr.Image] = struct{}{}
		}
		total := 0
		for _, cs := range p.Status.ContainerStatuses {
			total += int(cs.RestartCount)
		}
		if total > 0 {
			restarts = append(restarts, RestartRow{
				Namespace: p.Namespace, Name: p.Name, Restarts: total, Status: ph,
			})
		}
	}
	in.PodPhases = sortedSlices(phase)
	in.PodsByNS = topSlices(byNS, 8)
	in.ContainerImgs = len(images)

	sort.Slice(restarts, func(i, j int) bool { return restarts[i].Restarts > restarts[j].Restarts })
	if len(restarts) > 8 {
		restarts = restarts[:8]
	}
	in.TopRestarts = restarts
}

func (in *Insights) deployStats(deps []appsv1.Deployment) {
	healthy, degraded := 0, 0
	for _, d := range deps {
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		if d.Status.ReadyReplicas < desired {
			degraded++
		} else {
			healthy++
		}
	}
	in.DeployHealth = []Slice{{Label: "Healthy", Value: healthy}, {Label: "Degraded", Value: degraded}}
}

// eventTimeline buckets the last 12 hours of events into hourly warning/normal
// counts, oldest first, so the UI can draw a left-to-right timeline.
func (in *Insights) eventTimeline(events []corev1.Event) {
	now := time.Now()
	const hours = 12
	buckets := make([]TimeBucket, hours)
	for i := 0; i < hours; i++ {
		t := now.Add(time.Duration(-(hours - 1 - i)) * time.Hour)
		buckets[i] = TimeBucket{Hour: t.Format("15:04")}
	}
	for _, e := range events {
		ts := e.LastTimestamp.Time
		if ts.IsZero() {
			ts = e.EventTime.Time
		}
		diff := now.Sub(ts)
		if diff < 0 || diff >= hours*time.Hour {
			continue
		}
		idx := hours - 1 - int(diff/time.Hour)
		if idx < 0 || idx >= hours {
			continue
		}
		if e.Type == corev1.EventTypeWarning {
			buckets[idx].Warnings++
		} else {
			buckets[idx].Normal++
		}
	}
	in.EventTimeline = buckets
}

// sortedSlices returns map entries sorted by value descending.
func sortedSlices(m map[string]int) []Slice {
	out := make([]Slice, 0, len(m))
	for k, v := range m {
		out = append(out, Slice{Label: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out
}

// topSlices returns the top-n entries by value, folding the rest into "other".
func topSlices(m map[string]int, n int) []Slice {
	all := sortedSlices(m)
	if len(all) <= n {
		return all
	}
	top := all[:n]
	other := 0
	for _, s := range all[n:] {
		other += s.Value
	}
	if other > 0 {
		top = append(top, Slice{Label: "other", Value: other})
	}
	return top
}
