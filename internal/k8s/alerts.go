package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// Alert is one derived cluster problem. KubeAura has no external alerting
// backend (Prometheus/Alertmanager); instead it evaluates rules over live
// cluster state on demand — the operator-tool model. Severity is one of
// "critical", "warning", "info".
type Alert struct {
	Severity  string `json:"severity"`
	Category  string `json:"category"` // Workload, Node, Storage, Config, Events
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Age       string `json:"age"`
}

// AlertReport is the alert list plus per-severity counts for badges.
type AlertReport struct {
	Alerts   []Alert `json:"alerts"`
	Critical int     `json:"critical"`
	Warning  int     `json:"warning"`
	Info     int     `json:"info"`
}

// thresholds for the resource-pressure rules.
const (
	restartWarn   = 5
	restartCrit   = 20
	usagePctWarn  = 85.0
	usagePctCrit  = 95.0
	pendingMaxAge = 5 * time.Minute
)

// Alerts evaluates the rule set for the given namespace (empty = all) and
// returns alerts sorted by severity (critical first) then age.
func (c *Client) Alerts(namespace string) (*AlertReport, error) {
	cx, cancel := ctx()
	defer cancel()
	return c.AlertsContext(cx, namespace)
}

// AlertsContext is Alerts under a caller-supplied deadline, used by the fleet
// view so one slow cluster cannot hold the whole overview open.
func (c *Client) AlertsContext(cx context.Context, namespace string) (*AlertReport, error) {
	rep := &AlertReport{Alerts: []Alert{}}
	add := func(a Alert) { rep.Alerts = append(rep.Alerts, a) }

	if pods, err := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			podAlerts(p, add)
		}
	}
	if deps, err := c.cs.AppsV1().Deployments(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, d := range deps.Items {
			workloadAlert(add, "Deployment", d.Namespace, d.Name, d.Status.ReadyReplicas, deref(d.Spec.Replicas), age(d.CreationTimestamp))
		}
	}
	if ss, err := c.cs.AppsV1().StatefulSets(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, s := range ss.Items {
			workloadAlert(add, "StatefulSet", s.Namespace, s.Name, s.Status.ReadyReplicas, deref(s.Spec.Replicas), age(s.CreationTimestamp))
		}
	}
	if ds, err := c.cs.AppsV1().DaemonSets(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, d := range ds.Items {
			workloadAlert(add, "DaemonSet", d.Namespace, d.Name, d.Status.NumberReady, d.Status.DesiredNumberScheduled, age(d.CreationTimestamp))
		}
	}
	if jobs, err := c.cs.BatchV1().Jobs(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, j := range jobs.Items {
			if j.Status.Failed > 0 {
				add(Alert{Severity: "warning", Category: "Workload", Kind: "Job", Namespace: j.Namespace, Name: j.Name,
					Title: "Job has failed pods", Detail: fmt.Sprintf("%d failed", j.Status.Failed), Age: age(j.CreationTimestamp)})
			}
		}
	}
	if pvcs, err := c.cs.CoreV1().PersistentVolumeClaims(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, p := range pvcs.Items {
			if p.Status.Phase != corev1.ClaimBound {
				add(Alert{Severity: "warning", Category: "Storage", Kind: "PVC", Namespace: p.Namespace, Name: p.Name,
					Title: "PVC not bound", Detail: string(p.Status.Phase), Age: age(p.CreationTimestamp)})
			}
		}
	}

	// cert-manager Certificates about to expire (only when the CRD exists).
	c.certExpiryAlerts(cx, namespace, add)

	// Node conditions (cluster-scoped; always evaluated).
	nodeMetricByName := map[string]NodeUsage{}
	if nm, err := c.NodeMetrics(); err == nil {
		for _, n := range nm {
			nodeMetricByName[n.Name] = n
		}
	}
	if nodes, err := c.cs.CoreV1().Nodes().List(cx, metav1.ListOptions{}); err == nil {
		for _, n := range nodes.Items {
			nodeAlerts(n, nodeMetricByName[n.Name], add)
		}
	}

	// Recent warning events (deduped by object+reason) surface transient issues
	// that the object's own status may not still reflect.
	if evs, err := c.cs.CoreV1().Events(namespace).List(cx, metav1.ListOptions{}); err == nil {
		eventAlerts(evs.Items, add)
	}

	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(rep.Alerts, func(i, j int) bool {
		return rank[rep.Alerts[i].Severity] < rank[rep.Alerts[j].Severity]
	})
	for _, a := range rep.Alerts {
		switch a.Severity {
		case "critical":
			rep.Critical++
		case "warning":
			rep.Warning++
		default:
			rep.Info++
		}
	}
	return rep, nil
}

func podAlerts(p corev1.Pod, add func(Alert)) {
	base := func(sev, title, detail string) Alert {
		return Alert{Severity: sev, Category: "Workload", Kind: "Pod", Namespace: p.Namespace, Name: p.Name,
			Title: title, Detail: detail, Age: age(p.CreationTimestamp)}
	}
	if p.Status.Phase == corev1.PodFailed {
		add(base("critical", "Pod failed", p.Status.Reason))
	}
	if p.Status.Phase == corev1.PodPending && time.Since(p.CreationTimestamp.Time) > pendingMaxAge {
		add(base("warning", "Pod stuck in Pending", "possibly unschedulable or image pull issue"))
	}
	restarts := 0
	for _, cs := range p.Status.ContainerStatuses {
		restarts += int(cs.RestartCount)
		if cs.State.Waiting != nil {
			r := cs.State.Waiting.Reason
			if r == "CrashLoopBackOff" || r == "ImagePullBackOff" || r == "ErrImagePull" || r == "CreateContainerConfigError" || r == "CreateContainerError" {
				add(base("critical", r, fmt.Sprintf("container %s: %s", cs.Name, cs.State.Waiting.Message)))
			}
		}
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			add(base("critical", "OOMKilled", fmt.Sprintf("container %s was killed for exceeding its memory limit", cs.Name)))
		}
	}
	if restarts >= restartCrit {
		add(base("critical", "Excessive restarts", fmt.Sprintf("%d container restarts", restarts)))
	} else if restarts >= restartWarn {
		add(base("warning", "High restart count", fmt.Sprintf("%d container restarts", restarts)))
	}
	// Best-practice: containers with no resource requests can't be scheduled or
	// rightsized well. Info-level so it doesn't drown out real failures.
	for _, ctr := range p.Spec.Containers {
		if ctr.Resources.Requests.Cpu().IsZero() && ctr.Resources.Requests.Memory().IsZero() {
			add(base("info", "No resource requests", fmt.Sprintf("container %s has no CPU/memory requests set", ctr.Name)))
			break
		}
	}
}

func workloadAlert(add func(Alert), kind, ns, name string, ready, desired int32, ageStr string) {
	if desired == 0 || ready >= desired {
		return
	}
	sev := "warning"
	if ready == 0 {
		sev = "critical"
	}
	add(Alert{Severity: sev, Category: "Workload", Kind: kind, Namespace: ns, Name: name,
		Title: kind + " degraded", Detail: fmt.Sprintf("%d/%d replicas ready", ready, desired), Age: ageStr})
}

func nodeAlerts(n corev1.Node, m NodeUsage, add func(Alert)) {
	base := func(sev, title, detail string) Alert {
		return Alert{Severity: sev, Category: "Node", Kind: "Node", Name: n.Name, Title: title, Detail: detail, Age: age(n.CreationTimestamp)}
	}
	if !nodeReady(n) {
		add(base("critical", "Node NotReady", "kubelet is not reporting Ready"))
	}
	for _, cond := range n.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case corev1.NodeMemoryPressure:
			add(base("warning", "Node under memory pressure", cond.Message))
		case corev1.NodeDiskPressure:
			add(base("warning", "Node under disk pressure", cond.Message))
		case corev1.NodePIDPressure:
			add(base("warning", "Node under PID pressure", cond.Message))
		}
	}
	if m.CPUCapMilli > 0 {
		pressureAlert(add, base, "CPU", m.CPUPct)
	}
	if m.MemCapBytes > 0 {
		pressureAlert(add, base, "memory", m.MemPct)
	}
}

func pressureAlert(add func(Alert), base func(sev, title, detail string) Alert, res string, pct float64) {
	if pct >= usagePctCrit {
		add(base("critical", "Node "+res+" critical", fmt.Sprintf("%.0f%% of allocatable %s in use", pct, res)))
	} else if pct >= usagePctWarn {
		add(base("warning", "Node "+res+" high", fmt.Sprintf("%.0f%% of allocatable %s in use", pct, res)))
	}
}

// eventAlerts turns recent (last 30m) warning events into info/warning alerts,
// deduplicated by object+reason so a hot-looping event doesn't flood the list.
func eventAlerts(events []corev1.Event, add func(Alert)) {
	cutoff := time.Now().Add(-30 * time.Minute)
	seen := map[string]bool{}
	for _, e := range events {
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		ts := e.LastTimestamp.Time
		if ts.Before(cutoff) {
			continue
		}
		key := e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name + "/" + e.Reason
		if seen[key] {
			continue
		}
		seen[key] = true
		add(Alert{Severity: "warning", Category: "Events", Kind: e.InvolvedObject.Kind, Namespace: e.Namespace,
			Name: e.InvolvedObject.Name, Title: e.Reason, Detail: strings.TrimSpace(e.Message), Age: age(e.LastTimestamp)})
	}
}

// keep appsv1 referenced (used indirectly via list item types above)
var _ = appsv1.Deployment{}

// certExpiryAlerts flags cert-manager Certificates that are expired or expire
// within 14 days (warning) / 7 days (critical). Absent CRD = no-op, matching
// the detect-don't-install pattern (CNCF roadmap, Phase 3 quick win).
func (c *Client) certExpiryAlerts(cx context.Context, namespace string, add func(Alert)) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return
	}
	list, err := dyn.Resource(certificatesGVR).Namespace(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return // CRD missing or forbidden — certs simply aren't monitored
	}
	now := time.Now()
	for _, it := range list.Items {
		notAfter, found, _ := unstructured.NestedString(it.Object, "status", "notAfter")
		if !found {
			continue
		}
		exp, err := time.Parse(time.RFC3339, notAfter)
		if err != nil {
			continue
		}
		left := exp.Sub(now)
		base := Alert{Category: "Certificates", Kind: "Certificate",
			Namespace: it.GetNamespace(), Name: it.GetName(), Age: age(it.GetCreationTimestamp())}
		switch {
		case left <= 0:
			base.Severity, base.Title = "critical", "Certificate expired"
			base.Detail = fmt.Sprintf("expired %s ago (notAfter %s)", (-left).Round(time.Hour), exp.Format("2006-01-02"))
		case left < 7*24*time.Hour:
			base.Severity, base.Title = "critical", "Certificate expires in under 7 days"
			base.Detail = fmt.Sprintf("%.0f days left (notAfter %s)", left.Hours()/24, exp.Format("2006-01-02"))
		case left < 14*24*time.Hour:
			base.Severity, base.Title = "warning", "Certificate expires in under 14 days"
			base.Detail = fmt.Sprintf("%.0f days left (notAfter %s)", left.Hours()/24, exp.Format("2006-01-02"))
		default:
			continue
		}
		add(base)
	}
}
