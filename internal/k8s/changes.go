package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// Change is one thing that happened to the cluster at a known time.
//
// "What changed?" is the first question of most incidents, and until now
// KubeAura could not answer it: every view showed the present. Nothing new is
// read to build this — Helm's storage secrets already carry a full deploy
// history with timestamps, and every Deployment rollout leaves a dated
// ReplicaSet behind. The information was there; it was being thrown away.
type Change struct {
	At        time.Time `json:"at"`
	Source    string    `json:"source"` // helm | rollout | node | gitops
	Kind      string    `json:"kind"`   // human label, e.g. "Helm upgrade"
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	Detail    string    `json:"detail,omitempty"`
	Ago       string    `json:"ago"`
	Revision  int       `json:"revision,omitempty"` // helm revision / rollout number
}

// defaultChangeWindow is how far back the timeline looks when the caller does
// not say. Long enough to cover the deploy that broke this morning, short
// enough that the list stays readable.
const defaultChangeWindow = 24 * time.Hour

// Changes returns everything that changed in the window, newest first. An empty
// namespace covers the whole cluster.
//
// Per-source errors are swallowed on purpose: no Helm, no permission on
// ReplicaSets, or no Argo CRD should each cost you that source and nothing
// else. A partial timeline is far more useful than an error.
func (c *Client) Changes(namespace string, window time.Duration) ([]Change, error) {
	cx, cancel := ctx()
	defer cancel()
	if window <= 0 {
		window = defaultChangeWindow
	}
	cutoff := time.Now().Add(-window)

	var out []Change
	out = append(out, c.helmChanges(cx, namespace, cutoff)...)
	out = append(out, c.rolloutChanges(cx, namespace, cutoff)...)
	out = append(out, c.gitopsChanges(cx, namespace, cutoff)...)
	if namespace == "" {
		out = append(out, c.nodeChanges(cx, cutoff)...)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	now := time.Now()
	for i := range out {
		out[i].Ago = shortSince(now.Sub(out[i].At))
	}
	return out, nil
}

// helmChanges reads every revision of every release in scope. Revision 1 is an
// install; anything above it is an upgrade or a rollback, which Helm records in
// the revision's description.
func (c *Client) helmChanges(cx context.Context, namespace string, cutoff time.Time) []Change {
	revs, err := c.helmRevisions(cx, namespace, "")
	if err != nil {
		return nil
	}
	var out []Change
	for _, r := range revs {
		at, err := time.Parse(time.RFC3339, r.Updated)
		if err != nil || at.Before(cutoff) {
			continue
		}
		kind := "Helm upgrade"
		if r.Revision == 1 {
			kind = "Helm install"
		}
		out = append(out, Change{
			At: at, Source: "helm", Kind: kind,
			Namespace: r.Namespace, Name: r.Name, Revision: r.Revision,
			Summary: fmt.Sprintf("%s → revision %d (%s)", r.Name, r.Revision, r.Chart),
			Detail:  r.Description,
		})
	}
	return out
}

// rolloutChanges derives Deployment rollouts from their ReplicaSets. A rollout
// creates a new ReplicaSet, so the RS's creation time is the moment the new
// version started going out — including rollouts nobody recorded anywhere else,
// like a `kubectl set image` or an HPA-driven template change.
func (c *Client) rolloutChanges(cx context.Context, namespace string, cutoff time.Time) []Change {
	list, err := c.cs.AppsV1().ReplicaSets(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []Change
	for _, rs := range list.Items {
		at := rs.CreationTimestamp.Time
		if at.Before(cutoff) {
			continue
		}
		owner := ""
		for _, o := range rs.OwnerReferences {
			if o.Kind == "Deployment" {
				owner = o.Name
				break
			}
		}
		if owner == "" {
			continue // a bare ReplicaSet is not a rollout
		}
		rev, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
		// The first ReplicaSet of a Deployment is its creation, not a rollout.
		kind := "Rollout"
		if rev <= 1 {
			kind = "Deployment created"
		}
		image := ""
		if ctrs := rs.Spec.Template.Spec.Containers; len(ctrs) > 0 {
			image = ctrs[0].Image
		}
		out = append(out, Change{
			At: at, Source: "rollout", Kind: kind,
			Namespace: rs.Namespace, Name: owner, Revision: rev,
			Summary: fmt.Sprintf("%s → revision %d", owner, rev),
			Detail:  image,
		})
	}
	return out
}

// nodeChanges reports nodes that joined recently. A node leaving is not
// observable after the fact — the object is gone — so this is deliberately
// one-sided rather than pretending otherwise.
func (c *Client) nodeChanges(cx context.Context, cutoff time.Time) []Change {
	list, err := c.cs.CoreV1().Nodes().List(cx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []Change
	for _, n := range list.Items {
		at := n.CreationTimestamp.Time
		if at.Before(cutoff) {
			continue
		}
		out = append(out, Change{
			At: at, Source: "node", Kind: "Node joined", Name: n.Name,
			Summary: n.Name + " joined the cluster",
			Detail:  n.Status.NodeInfo.KubeletVersion,
		})
	}
	return out
}

// gitopsChanges reports Argo CD syncs. Argo stamps the finish time of its last
// operation, which is the moment the cluster actually changed. Flux records a
// revision but no comparable completion timestamp, so it is not inferred here
// rather than dated wrongly.
func (c *Client) gitopsChanges(cx context.Context, namespace string, cutoff time.Time) []Change {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil
	}
	list, err := dyn.Resource(argoAppsGVR).Namespace(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil // Argo not installed, or not readable
	}
	var out []Change
	for _, it := range list.Items {
		finished, found, _ := unstructured.NestedString(it.Object, "status", "operationState", "finishedAt")
		if !found {
			continue
		}
		at, err := time.Parse(time.RFC3339, finished)
		if err != nil || at.Before(cutoff) {
			continue
		}
		phase, _, _ := unstructured.NestedString(it.Object, "status", "operationState", "phase")
		rev, _, _ := unstructured.NestedString(it.Object, "status", "sync", "revision")
		out = append(out, Change{
			At: at, Source: "gitops", Kind: "Argo CD sync",
			Namespace: it.GetNamespace(), Name: it.GetName(),
			Summary: fmt.Sprintf("%s synced to %s", it.GetName(), shortRev(rev)),
			Detail:  phase,
		})
	}
	return out
}

// CorrelateChanges attaches, to each alert, the changes that landed shortly
// before it started firing.
//
// This is correlation, not causation, and the field name says so: a rollout two
// minutes before a CrashLoopBackOff is a strong lead, not a verdict. The window
// is deliberately short — widen it and every alert acquires a suspect, which is
// the same as having none.
func CorrelateChanges(alerts []Alert, changes []Change, window time.Duration) {
	if window <= 0 {
		window = 15 * time.Minute
	}
	for i := range alerts {
		a := &alerts[i]
		if a.FirstSeen.IsZero() {
			continue // untracked: nothing to correlate against
		}
		// Info alerts are standing advice ("this container sets no resource
		// requests"), not incidents. Pointing at the deploy that happened to
		// precede one implies a causal link that does not exist.
		if a.Severity == "info" {
			continue
		}
		var direct, nearby []Change
		for _, ch := range changes {
			if ch.At.After(a.FirstSeen) || ch.At.Before(a.FirstSeen.Add(-window)) {
				continue
			}
			// Cluster-scoped changes (a node joining) are relevant to
			// everything; namespaced ones only to their own namespace.
			if ch.Namespace != "" && a.Namespace != "" && ch.Namespace != a.Namespace {
				continue
			}
			if sameWorkload(a.Name, ch.Name) {
				direct = append(direct, ch)
			} else {
				nearby = append(nearby, ch)
			}
		}
		// A change to the very object that is alerting outranks anything that
		// merely shares its namespace. Offering both would bury the answer: a
		// busy namespace deploys all day, and "something else also changed" is
		// not a lead. Only when nothing touched this object do the neighbours
		// become worth mentioning.
		a.Suspects = direct
		if len(a.Suspects) == 0 {
			a.Suspects = nearby
		}
		sort.Slice(a.Suspects, func(x, y int) bool { return a.Suspects[x].At.After(a.Suspects[y].At) })
		if len(a.Suspects) > 3 {
			a.Suspects = a.Suspects[:3]
		}
	}
}

// sameWorkload reports whether an alert's object and a change's object are the
// same thing. An alert usually names a Pod ("api-7d9f8b-x2k1") while the change
// names its Deployment ("api"), and without another API call the name is the
// only link there is.
//
// A bare prefix test is not enough: "api-gateway-7d9" also starts with "api-",
// and calling a gateway rollout the cause of an API outage is worse than
// offering nothing. So what follows the prefix must actually look like the
// suffix Kubernetes generates, not like another word.
func sameWorkload(alertName, changeName string) bool {
	if alertName == "" || changeName == "" {
		return false
	}
	if alertName == changeName {
		return true
	}
	if !strings.HasPrefix(alertName, changeName+"-") {
		return false
	}
	return isGeneratedSuffix(alertName[len(changeName)+1:])
}

// isGeneratedSuffix recognises what Kubernetes appends to build a pod name:
// either a ReplicaSet hash plus a five-character pod suffix ("7d9f8b64fc-x2k1p"),
// or — for DaemonSets and StatefulSet-less controllers — just the pod suffix.
func isGeneratedSuffix(s string) bool {
	parts := strings.Split(s, "-")
	switch len(parts) {
	case 1:
		return isPodSuffix(parts[0]) || isHash(parts[0])
	case 2:
		return isHash(parts[0]) && isPodSuffix(parts[1])
	default:
		return false
	}
}

// isHash matches a ReplicaSet's pod-template hash. These are base-36 digests
// and in practice always carry at least one digit, which is what separates
// "7d98b64fc9" from a word like "gateway".
func isHash(s string) bool {
	if len(s) < 5 || len(s) > 10 {
		return false
	}
	digit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'z':
		default:
			return false
		}
	}
	return digit
}

// isPodSuffix matches the five random characters Kubernetes appends last.
func isPodSuffix(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// shortSince renders elapsed time the way an operator reads it.
func shortSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}
