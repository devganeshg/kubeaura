package k8s

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DetailView is a structured, render-ready description of a single object. It's
// what the click-to-detail drawer shows: a header, key/value sections, tables
// (e.g. containers, ports), conditions, and related events. Everything is
// pre-formatted strings so the frontend just lays it out.
type DetailView struct {
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Status      string            `json:"status"`
	Age         string            `json:"age"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Sections    []Section         `json:"sections"`
	Tables      []Table           `json:"tables"`
	Conditions  []Condition       `json:"conditions,omitempty"`
	Events      []EventRow        `json:"events,omitempty"`
}

// Section is a titled list of key/value rows.
type Section struct {
	Title  string  `json:"title"`
	Fields []Field `json:"fields"`
}

// Field is one key/value pair.
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Table is a titled grid (containers, ports, volumes, …).
type Table struct {
	Title   string     `json:"title"`
	Headers []string   `json:"headers"`
	Rows    [][]string `json:"rows"`
}

// Condition is a normalized status condition row.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// EventRow is a related event on the object.
type EventRow struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Age     string `json:"age"`
	Count   int32  `json:"count"`
}

// ResourceDetail assembles a DetailView for a supported kind. Unsupported kinds
// return a header with just metadata (the caller still has the YAML view).
func (c *Client) ResourceDetail(kind, namespace, name string) (*DetailView, error) {
	cx, cancel := ctx()
	defer cancel()

	switch strings.ToLower(kind) {
	case "pod", "pods":
		p, err := c.cs.CoreV1().Pods(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.podDetailView(namespace, name, p), nil
	case "deployment", "deployments":
		d, err := c.cs.AppsV1().Deployments(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.deploymentDetailView(namespace, name, d), nil
	case "statefulset", "statefulsets":
		s, err := c.cs.AppsV1().StatefulSets(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.workloadDetailView(namespace, name, "StatefulSet", s.ObjectMeta, s.Spec.Template.Spec,
			[]Field{{"Replicas", fmt.Sprintf("%d/%d ready", s.Status.ReadyReplicas, deref(s.Spec.Replicas))},
				{"Service", s.Spec.ServiceName}}), nil
	case "daemonset", "daemonsets":
		d, err := c.cs.AppsV1().DaemonSets(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.workloadDetailView(namespace, name, "DaemonSet", d.ObjectMeta, d.Spec.Template.Spec,
			[]Field{{"Ready", fmt.Sprintf("%d/%d", d.Status.NumberReady, d.Status.DesiredNumberScheduled)},
				{"Up-to-date", fmt.Sprintf("%d", d.Status.UpdatedNumberScheduled)}}), nil
	case "service", "services":
		s, err := c.cs.CoreV1().Services(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.serviceDetailView(namespace, name, s), nil
	case "node", "nodes":
		n, err := c.cs.CoreV1().Nodes().Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.nodeDetailView(name, n), nil
	case "job", "jobs":
		j, err := c.cs.BatchV1().Jobs(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return c.jobDetailView(namespace, name, j), nil
	case "configmap", "configmaps":
		cm, err := c.cs.CoreV1().ConfigMaps(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		v := c.base("ConfigMap", cm.ObjectMeta, "—")
		keys := make([]string, 0, len(cm.Data))
		for k := range cm.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rows := make([][]string, 0, len(keys))
		for _, k := range keys {
			rows = append(rows, []string{k, fmt.Sprintf("%d bytes", len(cm.Data[k]))})
		}
		v.Tables = append(v.Tables, Table{Title: "Data", Headers: []string{"Key", "Size"}, Rows: rows})
		return v, nil
	case "pvc", "persistentvolumeclaims":
		p, err := c.cs.CoreV1().PersistentVolumeClaims(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		v := c.base("PVC", p.ObjectMeta, string(p.Status.Phase))
		cap := p.Status.Capacity.Storage()
		modes := make([]string, 0, len(p.Spec.AccessModes))
		for _, m := range p.Spec.AccessModes {
			modes = append(modes, string(m))
		}
		v.Sections = append(v.Sections, Section{Title: "Storage", Fields: []Field{
			{"Capacity", cap.String()},
			{"Access Modes", strings.Join(modes, ", ")},
			{"Storage Class", deref(p.Spec.StorageClassName)},
			{"Volume", p.Spec.VolumeName},
		}})
		v.Events = c.eventsFor(namespace, name)
		return v, nil
	case "certificate", "certificates", "issuer", "issuers", "clusterissuer", "clusterissuers":
		return c.certManagerResourceDetail(cx, kind, namespace, name)
	case "helmrelease", "helmreleases":
		return c.helmReleaseDetail(cx, namespace, name)
	default:
		// Generic fallback: metadata only. The drawer still offers a YAML tab.
		return &DetailView{Kind: kind, Name: name, Namespace: namespace, Status: "—"}, nil
	}
}

// base builds the common header (labels/annotations/age) shared by all kinds.
func (c *Client) base(kind string, m metav1.ObjectMeta, status string) *DetailView {
	return &DetailView{
		Kind:        kind,
		Name:        m.Name,
		Namespace:   m.Namespace,
		Status:      status,
		Age:         age(m.CreationTimestamp),
		Labels:      m.Labels,
		Annotations: m.Annotations,
	}
}

func (c *Client) podDetailView(ns, name string, p *corev1.Pod) *DetailView {
	status := string(p.Status.Phase)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			status = cs.State.Waiting.Reason
		}
	}
	v := c.base("Pod", p.ObjectMeta, status)
	v.Sections = append(v.Sections, Section{Title: "Scheduling", Fields: []Field{
		{"Node", p.Spec.NodeName},
		{"Pod IP", p.Status.PodIP},
		{"Host IP", p.Status.HostIP},
		{"QoS Class", string(p.Status.QOSClass)},
		{"Service Account", p.Spec.ServiceAccountName},
		{"Restart Policy", string(p.Spec.RestartPolicy)},
	}})

	rows := make([][]string, 0, len(p.Spec.Containers))
	stateByName := map[string]string{}
	readyByName := map[string]string{}
	restartsByName := map[string]string{}
	for _, cs := range p.Status.ContainerStatuses {
		stateByName[cs.Name] = containerState(cs)
		if cs.Ready {
			readyByName[cs.Name] = "yes"
		} else {
			readyByName[cs.Name] = "no"
		}
		restartsByName[cs.Name] = fmt.Sprintf("%d", cs.RestartCount)
	}
	for _, ctr := range p.Spec.Containers {
		rows = append(rows, []string{
			ctr.Name, ctr.Image, stateByName[ctr.Name], readyByName[ctr.Name],
			restartsByName[ctr.Name], resourceReq(ctr), portList(ctr.Ports),
		})
	}
	v.Tables = append(v.Tables, Table{
		Title:   "Containers",
		Headers: []string{"Name", "Image", "State", "Ready", "Restarts", "Resources", "Ports"},
		Rows:    rows,
	})

	for _, cond := range p.Status.Conditions {
		v.Conditions = append(v.Conditions, Condition{
			Type: string(cond.Type), Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message,
		})
	}
	v.Events = c.eventsFor(ns, name)
	return v
}

func (c *Client) deploymentDetailView(ns, name string, d *appsv1.Deployment) *DetailView {
	status := "Healthy"
	if d.Status.ReadyReplicas < deref(d.Spec.Replicas) {
		status = "Degraded"
	}
	extra := []Field{
		{"Replicas", fmt.Sprintf("%d desired / %d ready / %d updated / %d available",
			deref(d.Spec.Replicas), d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas)},
		{"Strategy", string(d.Spec.Strategy.Type)},
	}
	v := c.workloadDetailView(ns, name, "Deployment", d.ObjectMeta, d.Spec.Template.Spec, extra)
	v.Status = status
	for _, cond := range d.Status.Conditions {
		v.Conditions = append(v.Conditions, Condition{
			Type: string(cond.Type), Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message,
		})
	}
	return v
}

// workloadDetailView builds the shared view for pod-templated workloads
// (Deployment/StatefulSet/DaemonSet): an overview section plus a container table
// from the pod template.
func (c *Client) workloadDetailView(ns, name, kind string, m metav1.ObjectMeta, spec corev1.PodSpec, extra []Field) *DetailView {
	v := c.base(kind, m, "—")
	v.Sections = append(v.Sections, Section{Title: "Overview", Fields: extra})

	rows := make([][]string, 0, len(spec.Containers))
	for _, ctr := range spec.Containers {
		rows = append(rows, []string{ctr.Name, ctr.Image, resourceReq(ctr), portList(ctr.Ports)})
	}
	v.Tables = append(v.Tables, Table{
		Title: "Containers", Headers: []string{"Name", "Image", "Resources", "Ports"}, Rows: rows,
	})
	v.Events = c.eventsFor(ns, name)
	return v
}

func (c *Client) serviceDetailView(ns, name string, s *corev1.Service) *DetailView {
	v := c.base("Service", s.ObjectMeta, string(s.Spec.Type))
	sel := make([]string, 0, len(s.Spec.Selector))
	for k, val := range s.Spec.Selector {
		sel = append(sel, k+"="+val)
	}
	sort.Strings(sel)
	v.Sections = append(v.Sections, Section{Title: "Networking", Fields: []Field{
		{"Type", string(s.Spec.Type)},
		{"Cluster IP", s.Spec.ClusterIP},
		{"Selector", strings.Join(sel, ", ")},
		{"Session Affinity", string(s.Spec.SessionAffinity)},
	}})
	rows := make([][]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		rows = append(rows, []string{
			p.Name, fmt.Sprintf("%d", p.Port), p.TargetPort.String(),
			fmt.Sprintf("%d", p.NodePort), string(p.Protocol),
		})
	}
	v.Tables = append(v.Tables, Table{
		Title: "Ports", Headers: []string{"Name", "Port", "Target", "NodePort", "Protocol"}, Rows: rows,
	})
	v.Events = c.eventsFor(ns, name)
	return v
}

func (c *Client) nodeDetailView(name string, n *corev1.Node) *DetailView {
	status := "NotReady"
	if nodeReady(*n) {
		status = "Ready"
	}
	v := c.base("Node", n.ObjectMeta, status)
	cap := n.Status.Capacity
	alloc := n.Status.Allocatable
	v.Sections = append(v.Sections, Section{Title: "System", Fields: []Field{
		{"Kubelet", n.Status.NodeInfo.KubeletVersion},
		{"OS Image", n.Status.NodeInfo.OSImage},
		{"Kernel", n.Status.NodeInfo.KernelVersion},
		{"Container Runtime", n.Status.NodeInfo.ContainerRuntimeVersion},
		{"Architecture", n.Status.NodeInfo.Architecture},
	}})
	v.Sections = append(v.Sections, Section{Title: "Capacity", Fields: []Field{
		{"CPU", fmt.Sprintf("%s (allocatable %s)", cap.Cpu(), alloc.Cpu())},
		{"Memory", fmt.Sprintf("%s (allocatable %s)", cap.Memory(), alloc.Memory())},
		{"Pods", cap.Pods().String()},
	}})
	addrs := make([]string, 0, len(n.Status.Addresses))
	for _, a := range n.Status.Addresses {
		addrs = append(addrs, fmt.Sprintf("%s: %s", a.Type, a.Address))
	}
	v.Sections = append(v.Sections, Section{Title: "Addresses", Fields: []Field{
		{"Addresses", strings.Join(addrs, "  ")},
	}})
	for _, cond := range n.Status.Conditions {
		v.Conditions = append(v.Conditions, Condition{
			Type: string(cond.Type), Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message,
		})
	}
	return v
}

func (c *Client) jobDetailView(ns, name string, j *batchv1.Job) *DetailView {
	status := "Running"
	if j.Status.Succeeded > 0 {
		status = "Complete"
	} else if j.Status.Failed > 0 {
		status = "Failed"
	}
	v := c.workloadDetailView(ns, name, "Job", j.ObjectMeta, j.Spec.Template.Spec, []Field{
		{"Completions", fmt.Sprintf("%d succeeded / %d failed", j.Status.Succeeded, j.Status.Failed)},
		{"Parallelism", fmt.Sprintf("%d", deref(j.Spec.Parallelism))},
		{"Backoff Limit", fmt.Sprintf("%d", deref(j.Spec.BackoffLimit))},
	})
	v.Status = status
	return v
}

// eventsFor returns recent events referencing the named object, newest first.
func (c *Client) eventsFor(ns, name string) []EventRow {
	cx, cancel := ctx()
	defer cancel()
	fs := fmt.Sprintf("involvedObject.name=%s", name)
	evs, err := c.cs.CoreV1().Events(ns).List(cx, metav1.ListOptions{FieldSelector: fs})
	if err != nil {
		return nil
	}
	sort.Slice(evs.Items, func(i, j int) bool {
		return evs.Items[i].LastTimestamp.After(evs.Items[j].LastTimestamp.Time)
	})
	out := make([]EventRow, 0, len(evs.Items))
	for _, e := range evs.Items {
		out = append(out, EventRow{
			Type: e.Type, Reason: e.Reason, Message: e.Message,
			Age: age(e.LastTimestamp), Count: e.Count,
		})
		if len(out) >= 15 {
			break
		}
	}
	return out
}

// --- small formatting helpers ---

func containerState(cs corev1.ContainerStatus) string {
	switch {
	case cs.State.Running != nil:
		return "Running"
	case cs.State.Waiting != nil:
		return "Waiting: " + cs.State.Waiting.Reason
	case cs.State.Terminated != nil:
		return "Terminated: " + cs.State.Terminated.Reason
	}
	return "Unknown"
}

func resourceReq(ctr corev1.Container) string {
	req, lim := ctr.Resources.Requests, ctr.Resources.Limits
	parts := make([]string, 0, 2)
	if cpu := req.Cpu(); !cpu.IsZero() {
		s := "cpu " + cpu.String()
		if lcpu := lim.Cpu(); !lcpu.IsZero() {
			s += "/" + lcpu.String()
		}
		parts = append(parts, s)
	}
	if mem := req.Memory(); !mem.IsZero() {
		s := "mem " + mem.String()
		if lmem := lim.Memory(); !lmem.IsZero() {
			s += "/" + lmem.String()
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}

func portList(ports []corev1.ContainerPort) string {
	if len(ports) == 0 {
		return "—"
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol))
	}
	return strings.Join(out, ", ")
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
