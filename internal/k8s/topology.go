package k8s

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Topology is a node-and-edge graph of how traffic and ownership connect a
// namespace's resources: Ingress -> Service -> Pod, and Workload -> Pod. It's
// what the topology view renders and what the AI "explain" action reasons over.
type Topology struct {
	Namespace string      `json:"namespace"`
	Nodes     []GraphNode `json:"nodes"`
	Edges     []GraphEdge `json:"edges"`
}

// GraphNode is one resource in the graph.
type GraphNode struct {
	ID     string `json:"id"`   // stable id, e.g. "svc/web"
	Kind   string `json:"kind"` // Ingress|Service|Deployment|StatefulSet|DaemonSet|CronJob|Job|Pod
	Name   string `json:"name"`
	Status string `json:"status"` // health hint for coloring
	Info   string `json:"info"`
}

// GraphEdge links two nodes (from -> to) with a relationship label.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"` // routes|selects|owns
}

// Topology builds the graph for a namespace. An empty namespace defaults to
// "default" since a whole-cluster graph would be unreadable.
func (c *Client) Topology(namespace string) (*Topology, error) {
	if namespace == "" {
		namespace = "default"
	}
	cx, cancel := ctx()
	defer cancel()
	t := &Topology{Namespace: namespace, Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	seen := map[string]bool{}
	addNode := func(n GraphNode) {
		if !seen[n.ID] {
			seen[n.ID] = true
			t.Nodes = append(t.Nodes, n)
		}
	}
	addEdge := func(from, to, kind string) { t.Edges = append(t.Edges, GraphEdge{From: from, To: to, Kind: kind}) }

	// Workloads first, with real replica health, so their nodes carry
	// Healthy/Degraded status instead of the generic "Workload" placeholder
	// that pod ownerReferences alone would produce.
	if deps, err := c.cs.AppsV1().Deployments(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, d := range deps.Items {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			status := "Healthy"
			if d.Status.ReadyReplicas < desired {
				status = "Degraded"
			}
			addNode(GraphNode{ID: "Deployment/" + d.Name, Kind: "Deployment", Name: d.Name,
				Status: status, Info: fmt.Sprintf("%d/%d ready", d.Status.ReadyReplicas, desired)})
		}
	}
	if stss, err := c.cs.AppsV1().StatefulSets(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, s := range stss.Items {
			desired := int32(1)
			if s.Spec.Replicas != nil {
				desired = *s.Spec.Replicas
			}
			status := "Healthy"
			if s.Status.ReadyReplicas < desired {
				status = "Degraded"
			}
			addNode(GraphNode{ID: "StatefulSet/" + s.Name, Kind: "StatefulSet", Name: s.Name,
				Status: status, Info: fmt.Sprintf("%d/%d ready", s.Status.ReadyReplicas, desired)})
		}
	}
	if dss, err := c.cs.AppsV1().DaemonSets(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, d := range dss.Items {
			status := "Healthy"
			if d.Status.NumberReady < d.Status.DesiredNumberScheduled {
				status = "Degraded"
			}
			addNode(GraphNode{ID: "DaemonSet/" + d.Name, Kind: "DaemonSet", Name: d.Name,
				Status: status, Info: fmt.Sprintf("%d/%d ready", d.Status.NumberReady, d.Status.DesiredNumberScheduled)})
		}
	}

	pods, err := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	// Map intermediate owners to the workload users think in terms of:
	// ReplicaSet -> Deployment and Job -> CronJob collapsing.
	rsToDeploy := map[string]string{}
	if rss, err := c.cs.AppsV1().ReplicaSets(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, rs := range rss.Items {
			for _, o := range rs.OwnerReferences {
				if o.Kind == "Deployment" {
					rsToDeploy[rs.Name] = o.Name
				}
			}
		}
	}
	jobToCron := map[string]string{}
	if jobs, err := c.cs.BatchV1().Jobs(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, j := range jobs.Items {
			for _, o := range j.OwnerReferences {
				if o.Kind == "CronJob" {
					jobToCron[j.Name] = o.Name
				}
			}
		}
	}

	podNode := func(p corev1.Pod) string {
		id := "pod/" + p.Name
		status := string(p.Status.Phase)
		ready, restarts := 0, int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				status = cs.State.Waiting.Reason
			}
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		if status == "Running" && ready < len(p.Spec.Containers) {
			status = "NotReady"
		}
		info := p.Spec.NodeName
		if restarts > 0 {
			info = fmt.Sprintf("%s · %d restarts", p.Spec.NodeName, restarts)
		}
		addNode(GraphNode{ID: id, Kind: "Pod", Name: p.Name, Status: status, Info: info})
		return id
	}

	for _, p := range pods.Items {
		pid := podNode(p)
		// Pod -> owning workload edge.
		for _, o := range p.OwnerReferences {
			ownerKind, ownerName := o.Kind, o.Name
			if ownerKind == "ReplicaSet" {
				if dep, ok := rsToDeploy[o.Name]; ok {
					ownerKind, ownerName = "Deployment", dep
				}
			}
			if ownerKind == "Job" {
				if cron, ok := jobToCron[o.Name]; ok {
					ownerKind, ownerName = "CronJob", cron
				}
			}
			wid := fmt.Sprintf("%s/%s", ownerKind, ownerName)
			addNode(GraphNode{ID: wid, Kind: ownerKind, Name: ownerName, Status: "Workload", Info: ownerKind})
			addEdge(wid, pid, "owns")
		}
	}

	// Services -> pods they select. A selector matching zero pods is flagged
	// so the UI can surface broken routing instead of a silently empty edge.
	svcs, err := c.cs.CoreV1().Services(namespace).List(cx, metav1.ListOptions{})
	if err == nil {
		for _, s := range svcs.Items {
			sid := "svc/" + s.Name
			status := string(s.Spec.Type)
			matched := 0
			if len(s.Spec.Selector) > 0 {
				sel := labels.SelectorFromSet(s.Spec.Selector)
				for _, p := range pods.Items {
					if sel.Matches(labels.Set(p.Labels)) {
						addEdge(sid, "pod/"+p.Name, "selects")
						matched++
					}
				}
				if matched == 0 {
					status = "NoEndpoints"
				}
			}
			addNode(GraphNode{ID: sid, Kind: "Service", Name: s.Name, Status: status, Info: s.Spec.ClusterIP})
		}
	}

	// Ingresses -> services they route to.
	if ings, err := c.cs.NetworkingV1().Ingresses(namespace).List(cx, metav1.ListOptions{}); err == nil {
		for _, ing := range ings.Items {
			iid := "ing/" + ing.Name
			host := ""
			if len(ing.Spec.Rules) > 0 {
				host = ing.Spec.Rules[0].Host
			}
			addNode(GraphNode{ID: iid, Kind: "Ingress", Name: ing.Name, Status: "Ingress", Info: host})
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				for _, path := range rule.HTTP.Paths {
					if path.Backend.Service != nil {
						addEdge(iid, "svc/"+path.Backend.Service.Name, "routes")
					}
				}
			}
			if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
				addEdge(iid, "svc/"+ing.Spec.DefaultBackend.Service.Name, "routes")
			}
		}
	}
	return t, nil
}
