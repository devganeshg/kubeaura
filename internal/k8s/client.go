package k8s

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Client wraps the Kubernetes clientset and provides high-level, UI-friendly
// read/write operations over cluster resources. Each Client is bound to one
// kubeconfig context; the Manager holds one Client per context.
type Client struct {
	// kubernetes.Interface rather than *Clientset so tests can substitute
	// client-go's fake clientset — the mutating paths (scale, restart, delete,
	// apply) are the ones worth pinning down.
	cs      kubernetes.Interface
	metrics *metricsv.Clientset
	cfg     *rest.Config
	Context string

	pfMu sync.Mutex
	pf   map[string]*PortForward // active port-forwards, keyed by id
}

// New builds a Client for the kubeconfig's current context, falling back to
// in-cluster config and then to the default ~/.kube/config location.
func New(kubeconfig string) (*Client, error) {
	cfg, ctxName, err := restConfig(kubeconfig, "")
	if err != nil {
		return nil, err
	}
	return newClient(cfg, ctxName)
}

func newClient(cfg *rest.Config, ctxName string) (*Client, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	// Metrics client is optional; the metrics-server may not be installed.
	mc, _ := metricsv.NewForConfig(cfg)
	return &Client{cs: cs, metrics: mc, cfg: cfg, Context: ctxName, pf: map[string]*PortForward{}}, nil
}

// restConfig builds a rest.Config. A non-empty contextName overrides the
// kubeconfig's current-context — this is how multi-cluster switching selects a
// specific cluster.
func restConfig(kubeconfig, contextName string) (*rest.Config, string, error) {
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	// In-cluster config only applies when no explicit context is requested.
	if contextName == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, "in-cluster", nil
		}
	}
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	name := contextName
	if name == "" {
		raw, _ := cc.RawConfig()
		name = raw.CurrentContext
	}
	return cfg, name, nil
}

const listTimeout = 15 * time.Second

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), listTimeout)
}

// ---- Summary / dashboard ----

// ClusterSummary is a health snapshot used by the dashboard.
type ClusterSummary struct {
	Context     string `json:"context"`
	Nodes       int    `json:"nodes"`
	NodesReady  int    `json:"nodesReady"`
	Namespaces  int    `json:"namespaces"`
	Pods        int    `json:"pods"`
	PodsRunning int    `json:"podsRunning"`
	PodsFailed  int    `json:"podsFailed"`
	Deployments int    `json:"deployments"`
	Services    int    `json:"services"`
	Warnings    int    `json:"warnings"` // warning events in last hour
}

func (c *Client) Summary(namespace string) (*ClusterSummary, error) {
	cx, cancel := ctx()
	defer cancel()
	s := &ClusterSummary{Context: c.Context}

	nodes, err := c.cs.CoreV1().Nodes().List(cx, metav1.ListOptions{})
	if err != nil {
		// Some deployments run with namespace-scoped RBAC and cannot list
		// cluster-scoped Nodes. Keep the dashboard working with partial data.
		if !apierrors.IsForbidden(err) {
			return nil, err
		}
	} else {
		s.Nodes = len(nodes.Items)
		for _, n := range nodes.Items {
			if nodeReady(n) {
				s.NodesReady++
			}
		}
	}

	nss, err := c.cs.CoreV1().Namespaces().List(cx, metav1.ListOptions{})
	if err == nil {
		s.Namespaces = len(nss.Items)
	}

	pods, err := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{})
	if err == nil {
		s.Pods = len(pods.Items)
		for _, p := range pods.Items {
			switch p.Status.Phase {
			case corev1.PodRunning:
				s.PodsRunning++
			case corev1.PodFailed:
				s.PodsFailed++
			}
		}
	}

	deps, err := c.cs.AppsV1().Deployments(namespace).List(cx, metav1.ListOptions{})
	if err == nil {
		s.Deployments = len(deps.Items)
	}
	svcs, err := c.cs.CoreV1().Services(namespace).List(cx, metav1.ListOptions{})
	if err == nil {
		s.Services = len(svcs.Items)
	}

	evs, err := c.cs.CoreV1().Events(namespace).List(cx, metav1.ListOptions{})
	if err == nil {
		cutoff := time.Now().Add(-time.Hour)
		for _, e := range evs.Items {
			if e.Type == corev1.EventTypeWarning && e.LastTimestamp.After(cutoff) {
				s.Warnings++
			}
		}
	}
	return s, nil
}

func nodeReady(n corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ---- Generic resource row for the UI tables ----

// Resource is a normalized, table-friendly view of any Kubernetes object.
type Resource struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Status    string            `json:"status"`
	Info      string            `json:"info"` // kind-specific summary column
	Age       string            `json:"age"`
	Labels    map[string]string `json:"labels,omitempty"`

	// Pod-only: summed container resource requests/limits, so the pods table
	// can show scheduling footprint next to live usage. Zero means unset.
	ReqCPUMilli int64 `json:"reqCpuMilli,omitempty"`
	LimCPUMilli int64 `json:"limCpuMilli,omitempty"`
	ReqMemBytes int64 `json:"reqMemBytes,omitempty"`
	LimMemBytes int64 `json:"limMemBytes,omitempty"`
}

// ListParams controls server-side pagination. Limit caps how many objects the
// API server returns in one page; Continue is the opaque cursor from a prior
// page's ListResult.Continue. This is how KubeMind stays responsive on
// clusters with 100k+ objects — the operator-tool scaling model.
type ListParams struct {
	Limit    int64
	Continue string
}

func (p ListParams) options() metav1.ListOptions {
	return metav1.ListOptions{Limit: p.Limit, Continue: p.Continue}
}

// ListResult is one page of normalized rows plus the cursor for the next page.
// Continue is empty when there are no further pages.
type ListResult struct {
	Items    []Resource `json:"items"`
	Continue string     `json:"continue"`
}

func age(t metav1.Time) string {
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Namespaces returns the list of namespace names, sorted.
func (c *Client) Namespaces() ([]string, error) {
	cx, cancel := ctx()
	defer cancel()
	nss, err := c.cs.CoreV1().Namespaces().List(cx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nss.Items))
	for _, n := range nss.Items {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out, nil
}

// CreateNamespace creates a namespace with the given name.
func (c *Client) CreateNamespace(name string) error {
	cx, cancel := ctx()
	defer cancel()
	_, err := c.cs.CoreV1().Namespaces().Create(cx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	return err
}

// List returns one page of normalized rows for the given resource kind, honoring
// server-side pagination via ListParams.
func (c *Client) List(kind, namespace string, p ListParams) (ListResult, error) {
	cx, cancel := ctx()
	defer cancel()
	opts := p.options()
	// RBAC kinds are handled in rbac.go; fall through if not one of them.
	if res, handled, err := c.listRBAC(cx, kind, namespace, opts); handled {
		return res, err
	}
	switch strings.ToLower(kind) {
	case "pods", "pod":
		return c.listPods(cx, namespace, opts)
	case "deployments", "deployment":
		return c.listDeployments(cx, namespace, opts)
	case "statefulsets", "statefulset":
		return c.listStatefulSets(cx, namespace, opts)
	case "daemonsets", "daemonset":
		return c.listDaemonSets(cx, namespace, opts)
	case "services", "service":
		return c.listServices(cx, namespace, opts)
	case "ingresses", "ingress":
		return c.listIngresses(cx, namespace, opts)
	case "configmaps", "configmap":
		return c.listConfigMaps(cx, namespace, opts)
	case "secrets", "secret":
		return c.listSecrets(cx, namespace, opts)
	case "jobs", "job":
		return c.listJobs(cx, namespace, opts)
	case "cronjobs", "cronjob":
		return c.listCronJobs(cx, namespace, opts)
	case "nodes", "node":
		return c.listNodes(cx, opts)
	case "namespaces", "namespace":
		return c.listNamespaces(cx, opts)
	case "pvcs", "persistentvolumeclaims":
		return c.listPVCs(cx, namespace, opts)
	case "resourcequotas", "resourcequota", "quotas", "quota":
		return c.listResourceQuotas(cx, namespace, opts)
	case "events", "event":
		return c.listEvents(cx, namespace, opts)
	case "certificates", "certificate":
		return c.listCertificates(cx, namespace, opts)
	case "issuers", "issuer":
		return c.listIssuers(cx, namespace, opts)
	case "clusterissuers", "clusterissuer":
		return c.listClusterIssuers(cx, opts)
	case "helmreleases", "helmrelease":
		return c.listHelmReleases(cx, namespace, opts)
	default:
		return ListResult{}, fmt.Errorf("unsupported kind %q", kind)
	}
}

func (c *Client) listPods(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Pods(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, p := range list.Items {
		ready, total := 0, len(p.Spec.Containers)
		restarts := 0
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += int(cs.RestartCount)
		}
		status := string(p.Status.Phase)
		// Surface container-level problems (CrashLoopBackOff etc).
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				status = cs.State.Waiting.Reason
			}
		}
		var reqCPU, limCPU, reqMem, limMem int64
		for _, ctr := range p.Spec.Containers {
			reqCPU += ctr.Resources.Requests.Cpu().MilliValue()
			limCPU += ctr.Resources.Limits.Cpu().MilliValue()
			reqMem += ctr.Resources.Requests.Memory().Value()
			limMem += ctr.Resources.Limits.Memory().Value()
		}
		out = append(out, Resource{
			Kind: "Pod", Name: p.Name, Namespace: p.Namespace, Status: status,
			Info: fmt.Sprintf("%d/%d ready, %d restarts", ready, total, restarts),
			Age:  age(p.CreationTimestamp), Labels: p.Labels,
			ReqCPUMilli: reqCPU, LimCPUMilli: limCPU,
			ReqMemBytes: reqMem, LimMemBytes: limMem,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listDeployments(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.AppsV1().Deployments(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, d := range list.Items {
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		status := "Healthy"
		if d.Status.ReadyReplicas < desired {
			status = "Degraded"
		}
		out = append(out, Resource{
			Kind: "Deployment", Name: d.Name, Namespace: d.Namespace, Status: status,
			Info: fmt.Sprintf("%d/%d replicas", d.Status.ReadyReplicas, desired),
			Age:  age(d.CreationTimestamp), Labels: d.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listStatefulSets(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.AppsV1().StatefulSets(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, s := range list.Items {
		desired := int32(0)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		status := "Healthy"
		if s.Status.ReadyReplicas < desired {
			status = "Degraded"
		}
		out = append(out, Resource{
			Kind: "StatefulSet", Name: s.Name, Namespace: s.Namespace, Status: status,
			Info: fmt.Sprintf("%d/%d ready", s.Status.ReadyReplicas, desired),
			Age:  age(s.CreationTimestamp), Labels: s.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listDaemonSets(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.AppsV1().DaemonSets(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, d := range list.Items {
		status := "Healthy"
		if d.Status.NumberReady < d.Status.DesiredNumberScheduled {
			status = "Degraded"
		}
		out = append(out, Resource{
			Kind: "DaemonSet", Name: d.Name, Namespace: d.Namespace, Status: status,
			Info: fmt.Sprintf("%d/%d ready", d.Status.NumberReady, d.Status.DesiredNumberScheduled),
			Age:  age(d.CreationTimestamp), Labels: d.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listServices(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Services(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, s := range list.Items {
		ports := make([]string, 0, len(s.Spec.Ports))
		for _, p := range s.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
		out = append(out, Resource{
			Kind: "Service", Name: s.Name, Namespace: s.Namespace, Status: string(s.Spec.Type),
			Info: fmt.Sprintf("%s  %s", s.Spec.ClusterIP, strings.Join(ports, ",")),
			Age:  age(s.CreationTimestamp), Labels: s.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listIngresses(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.NetworkingV1().Ingresses(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, i := range list.Items {
		hosts := make([]string, 0)
		for _, r := range i.Spec.Rules {
			hosts = append(hosts, r.Host)
		}
		out = append(out, Resource{
			Kind: "Ingress", Name: i.Name, Namespace: i.Namespace, Status: "—",
			Info: strings.Join(hosts, ", "), Age: age(i.CreationTimestamp), Labels: i.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listConfigMaps(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().ConfigMaps(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, cm := range list.Items {
		out = append(out, Resource{
			Kind: "ConfigMap", Name: cm.Name, Namespace: cm.Namespace, Status: "—",
			Info: fmt.Sprintf("%d keys", len(cm.Data)), Age: age(cm.CreationTimestamp), Labels: cm.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listSecrets(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Secrets(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, s := range list.Items {
		out = append(out, Resource{
			Kind: "Secret", Name: s.Name, Namespace: s.Namespace, Status: string(s.Type),
			Info: fmt.Sprintf("%d keys", len(s.Data)), Age: age(s.CreationTimestamp), Labels: s.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listJobs(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.BatchV1().Jobs(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, j := range list.Items {
		status := "Running"
		if j.Status.Succeeded > 0 {
			status = "Complete"
		} else if j.Status.Failed > 0 {
			status = "Failed"
		}
		out = append(out, Resource{
			Kind: "Job", Name: j.Name, Namespace: j.Namespace, Status: status,
			Info: fmt.Sprintf("%d succeeded, %d failed", j.Status.Succeeded, j.Status.Failed),
			Age:  age(j.CreationTimestamp), Labels: j.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listCronJobs(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.BatchV1().CronJobs(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, j := range list.Items {
		out = append(out, Resource{
			Kind: "CronJob", Name: j.Name, Namespace: j.Namespace, Status: "—",
			Info: j.Spec.Schedule, Age: age(j.CreationTimestamp), Labels: j.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listNodes(cx context.Context, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Nodes().List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, n := range list.Items {
		status := "NotReady"
		if nodeReady(n) {
			status = "Ready"
		}
		out = append(out, Resource{
			Kind: "Node", Name: n.Name, Status: status,
			Info: fmt.Sprintf("k8s %s, %s", n.Status.NodeInfo.KubeletVersion, n.Status.NodeInfo.OSImage),
			Age:  age(n.CreationTimestamp), Labels: n.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listNamespaces(cx context.Context, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Namespaces().List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, n := range list.Items {
		out = append(out, Resource{
			Kind: "Namespace", Name: n.Name, Status: string(n.Status.Phase),
			Age: age(n.CreationTimestamp), Labels: n.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listPVCs(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().PersistentVolumeClaims(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, p := range list.Items {
		capacity := p.Status.Capacity.Storage()
		out = append(out, Resource{
			Kind: "PVC", Name: p.Name, Namespace: p.Namespace, Status: string(p.Status.Phase),
			Info: capacity.String(), Age: age(p.CreationTimestamp), Labels: p.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listResourceQuotas(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().ResourceQuotas(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]Resource, 0, len(list.Items))
	for _, rq := range list.Items {
		podsUsed := rq.Status.Used[corev1.ResourcePods]
		podsHard := rq.Status.Hard[corev1.ResourcePods]
		cpuUsed := rq.Status.Used[corev1.ResourceRequestsCPU]
		cpuHard := rq.Status.Hard[corev1.ResourceRequestsCPU]
		out = append(out, Resource{
			Kind: "ResourceQuota", Name: rq.Name, Namespace: rq.Namespace, Status: "Active",
			Info: fmt.Sprintf("pods %d/%d, cpu req %dm/%dm", podsUsed.Value(), podsHard.Value(),
				cpuUsed.MilliValue(),
				cpuHard.MilliValue(),
			),
			Age: age(rq.CreationTimestamp), Labels: rq.Labels,
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

func (c *Client) listEvents(cx context.Context, ns string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Events(ns).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].LastTimestamp.After(list.Items[j].LastTimestamp.Time)
	})
	out := make([]Resource, 0, len(list.Items))
	for _, e := range list.Items {
		out = append(out, Resource{
			Kind: "Event", Name: e.InvolvedObject.Name, Namespace: e.Namespace, Status: e.Type,
			Info: fmt.Sprintf("%s: %s", e.Reason, e.Message), Age: age(e.LastTimestamp),
		})
	}
	return ListResult{Items: out, Continue: list.Continue}, nil
}

// ---- Operations ----

// PodLogs returns the last tail lines of logs for a pod. container may be empty
// to use the pod's first/default container.
func (c *Client) PodLogs(namespace, name, container string, tail int64) (string, error) {
	cx, cancel := ctx()
	defer cancel()
	req := c.cs.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{TailLines: &tail, Container: container})
	stream, err := req.Stream(cx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	buf := new(strings.Builder)
	_, err = copyLimited(buf, stream, 256*1024)
	return buf.String(), err
}

// PodContainers returns the container names of a pod (init + regular), so the UI
// can offer a container picker for logs.
func (c *Client) PodContainers(namespace, name string) ([]string, error) {
	cx, cancel := ctx()
	defer cancel()
	p, err := c.cs.CoreV1().Pods(namespace).Get(cx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers))
	for _, ct := range p.Spec.InitContainers {
		out = append(out, ct.Name)
	}
	for _, ct := range p.Spec.Containers {
		out = append(out, ct.Name)
	}
	return out, nil
}

// PodLogStream opens a following log stream for a pod's container (empty =
// default). The caller must Close the returned stream. Used by the SSE endpoint.
func (c *Client) PodLogStream(ctx context.Context, namespace, name, container string, tail int64) (io.ReadCloser, error) {
	req := c.cs.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{
		Follow:    true,
		TailLines: &tail,
		Container: container,
	})
	return req.Stream(ctx)
}

// ScaleDeployment sets the replica count of a deployment.
func (c *Client) ScaleDeployment(namespace, name string, replicas int32) error {
	cx, cancel := ctx()
	defer cancel()
	sc, err := c.cs.AppsV1().Deployments(namespace).GetScale(cx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	sc.Spec.Replicas = replicas
	_, err = c.cs.AppsV1().Deployments(namespace).UpdateScale(cx, name, sc, metav1.UpdateOptions{})
	return err
}

// RestartDeployment triggers a rolling restart via a template annotation.
func (c *Client) RestartDeployment(namespace, name string) error {
	cx, cancel := ctx()
	defer cancel()
	// The same annotation key `kubectl rollout restart` uses, so a restart
	// triggered here is indistinguishable from one triggered by kubectl and is
	// recognised by anything watching for it.
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339))
	_, err := c.cs.AppsV1().Deployments(namespace).Patch(cx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// DeleteResource deletes a supported namespaced/cluster resource by kind+name.
func (c *Client) DeleteResource(kind, namespace, name string) error {
	cx, cancel := ctx()
	defer cancel()
	opts := metav1.DeleteOptions{}
	switch strings.ToLower(kind) {
	case "pod", "pods":
		return c.cs.CoreV1().Pods(namespace).Delete(cx, name, opts)
	case "deployment", "deployments":
		return c.cs.AppsV1().Deployments(namespace).Delete(cx, name, opts)
	case "service", "services":
		return c.cs.CoreV1().Services(namespace).Delete(cx, name, opts)
	case "configmap", "configmaps":
		return c.cs.CoreV1().ConfigMaps(namespace).Delete(cx, name, opts)
	case "job", "jobs":
		return c.cs.BatchV1().Jobs(namespace).Delete(cx, name, opts)
	case "namespace", "namespaces":
		return c.cs.CoreV1().Namespaces().Delete(cx, name, opts)
	case "certificate", "certificates":
		return c.deleteCertManagerResource(cx, namespace, "certificates", name)
	case "issuer", "issuers":
		return c.deleteCertManagerResource(cx, namespace, "issuers", name)
	case "clusterissuer", "clusterissuers":
		return c.deleteCertManagerClusterResource(cx, "clusterissuers", name)
	default:
		return fmt.Errorf("delete not supported for kind %q", kind)
	}
}

// ---- Rich detail used by the AI troubleshooter ----

// PodDetail bundles everything the AI needs to reason about a pod.
type PodDetail struct {
	Pod    *corev1.Pod             `json:"pod"`
	Events []corev1.Event          `json:"events"`
	Logs   string                  `json:"logs"`
	Owners []metav1.OwnerReference `json:"owners"`
}

func (c *Client) PodDetail(namespace, name string) (*PodDetail, error) {
	cx, cancel := ctx()
	defer cancel()
	pod, err := c.cs.CoreV1().Pods(namespace).Get(cx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	d := &PodDetail{Pod: pod, Owners: pod.OwnerReferences}
	fs := fmt.Sprintf("involvedObject.name=%s", name)
	evs, err := c.cs.CoreV1().Events(namespace).List(cx, metav1.ListOptions{FieldSelector: fs})
	if err == nil {
		d.Events = evs.Items
	}
	// Best-effort logs (may fail if not started).
	if logs, err := c.PodLogs(namespace, name, "", 100); err == nil {
		d.Logs = logs
	}
	return d, nil
}

// compile-time interface checks to keep imports used
var (
	_ = appsv1.Deployment{}
	_ = batchv1.Job{}
	_ = networkingv1.Ingress{}
)
