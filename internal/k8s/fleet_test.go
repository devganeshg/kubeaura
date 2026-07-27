package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// fleetManager builds a Manager whose clients are already seeded, which is what
// lets the aggregation be tested without a kubeconfig or a live API server.
func fleetManager(active string, clients map[string]*Client) *Manager {
	m := &Manager{active: active, clients: clients}
	for name := range clients {
		m.contexts = append(m.contexts, name)
		m.ctxDetails = append(m.ctxDetails, ContextDetail{Name: name, Cluster: name + "-cluster"})
	}
	return m
}

func fakeClusterClient(name string, objs ...runtime.Object) *Client {
	return &Client{cs: fake.NewSimpleClientset(objs...), Context: name}
}

func pod(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestFleetAggregatesEveryContext(t *testing.T) {
	prod := fakeClusterClient("prod",
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		pod("default", "a", corev1.PodRunning),
		pod("default", "b", corev1.PodFailed),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d1", Namespace: "default"}},
	)
	staging := fakeClusterClient("staging",
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		pod("default", "c", corev1.PodRunning),
	)
	m := fleetManager("prod", map[string]*Client{"prod": prod, "staging": staging})

	view := m.Fleet(context.Background(), false)

	if view.Active != "prod" {
		t.Errorf("active = %q, want prod", view.Active)
	}
	if len(view.Clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(view.Clusters))
	}
	// Sorted by context name, so staging is second.
	if view.Clusters[0].Context != "prod" || view.Clusters[1].Context != "staging" {
		t.Errorf("clusters not sorted by context: %+v", view.Clusters)
	}
	if !view.Clusters[0].Active || view.Clusters[1].Active {
		t.Errorf("wrong cluster flagged active: %+v", view.Clusters)
	}
	if view.Totals.Reachable != 2 || view.Totals.Unreachable != 0 {
		t.Errorf("totals = %+v, want 2 reachable", view.Totals)
	}
	if view.Totals.Nodes != 2 || view.Totals.NodesReady != 1 {
		t.Errorf("nodes = %d ready %d, want 2 and 1", view.Totals.Nodes, view.Totals.NodesReady)
	}
	if view.Totals.Pods != 3 || view.Totals.PodsRunning != 2 || view.Totals.PodsFailed != 1 {
		t.Errorf("pod totals = %+v, want 3/2/1", view.Totals)
	}
	if view.Totals.Deployments != 1 {
		t.Errorf("deployments = %d, want 1", view.Totals.Deployments)
	}
}

// One bad kubeconfig entry is the normal case — an expired cloud credential,
// a cluster that was torn down. It must degrade to a row, not an error page.
func TestFleetReportsUnreachableContextWithoutFailingOthers(t *testing.T) {
	good := fakeClusterClient("good", pod("default", "a", corev1.PodRunning))
	m := fleetManager("good", map[string]*Client{"good": good})
	// A context listed in the kubeconfig but with no client and no way to build
	// one: ClientFor will fail on it exactly as it would for a dead cluster.
	m.contexts = append(m.contexts, "broken")
	m.ctxDetails = append(m.ctxDetails, ContextDetail{Name: "broken", Cluster: "broken-cluster"})
	m.kubeconfig = "/nonexistent/kubeconfig/for/test"

	view := m.Fleet(context.Background(), false)

	if len(view.Clusters) != 2 {
		t.Fatalf("want 2 rows, got %d", len(view.Clusters))
	}
	var broken, ok *FleetCluster
	for i := range view.Clusters {
		switch view.Clusters[i].Context {
		case "broken":
			broken = &view.Clusters[i]
		case "good":
			ok = &view.Clusters[i]
		}
	}
	if broken == nil || ok == nil {
		t.Fatalf("missing a row: %+v", view.Clusters)
	}
	if broken.Reachable {
		t.Error("broken context reported reachable")
	}
	if broken.Error == "" {
		t.Error("broken context carries no error to show the operator")
	}
	if !ok.Reachable || ok.Pods != 1 {
		t.Errorf("healthy context was affected by the broken one: %+v", ok)
	}
	if view.Totals.Reachable != 1 || view.Totals.Unreachable != 1 {
		t.Errorf("totals = %+v, want 1 reachable and 1 unreachable", view.Totals)
	}
}

func TestFleetEmptyWhenNoContexts(t *testing.T) {
	m := fleetManager("", map[string]*Client{})
	view := m.Fleet(context.Background(), false)
	if len(view.Clusters) != 0 || view.Totals.Clusters != 0 {
		t.Errorf("want an empty fleet, got %+v", view)
	}
}

// ClientFor is called concurrently by every fleet goroutine; all of them must
// end up sharing one Client so port-forward state stays in a single place.
func TestClientForReturnsOneClientPerContext(t *testing.T) {
	c := fakeClusterClient("prod")
	m := fleetManager("prod", map[string]*Client{"prod": c})

	got := make(chan *Client, 8)
	for i := 0; i < 8; i++ {
		go func() {
			cl, err := m.ClientFor("prod")
			if err != nil {
				t.Error(err)
			}
			got <- cl
		}()
	}
	for i := 0; i < 8; i++ {
		if cl := <-got; cl != c {
			t.Fatalf("ClientFor returned a different Client on call %d", i)
		}
	}
}

func TestClientForRejectsUnknownContext(t *testing.T) {
	m := fleetManager("prod", map[string]*Client{"prod": fakeClusterClient("prod")})
	if _, err := m.ClientFor("nope"); err == nil {
		t.Fatal("want an error for a context that is not in the kubeconfig")
	}
}
