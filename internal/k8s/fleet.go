package k8s

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Fleet view: one snapshot of every kubeconfig context at once.
//
// Switching the active context is still how KubeAura drives a cluster — the
// active client is what logs, exec, port-forward and every write path use. What
// this file adds is a read-only overview across all of them, so an operator can
// see which of their clusters is unhealthy without visiting each in turn.
//
// Every context is queried concurrently under its own timeout, and a context
// that fails to connect is reported as unreachable rather than failing the
// whole response: an expired cloud credential in one kubeconfig entry is the
// normal case, not an error condition for the page.

// fleetTimeout bounds one cluster's contribution to the overview. It is
// deliberately shorter than listTimeout — an unreachable cluster should not
// hold the page while a reachable one is already rendered.
const fleetTimeout = 8 * time.Second

// FleetCluster is one context's line in the overview.
type FleetCluster struct {
	Context   string `json:"context"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"` // context's default namespace
	Active    bool   `json:"active"`

	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`     // why it is unreachable
	Version   string `json:"version,omitempty"`   // Kubernetes server version
	LatencyMS int64  `json:"latencyMs,omitempty"` // time to answer the version probe

	Nodes       int `json:"nodes"`
	NodesReady  int `json:"nodesReady"`
	Namespaces  int `json:"namespaces"`
	Pods        int `json:"pods"`
	PodsRunning int `json:"podsRunning"`
	PodsFailed  int `json:"podsFailed"`
	Deployments int `json:"deployments"`
	Services    int `json:"services"`
	Warnings    int `json:"warnings"`

	Critical int `json:"critical"` // critical alerts, when alerts were requested
	Warning  int `json:"warning"`
}

// FleetTotals sums the reachable clusters, which is the number an operator
// actually wants at the top of the page.
type FleetTotals struct {
	Clusters    int `json:"clusters"`
	Reachable   int `json:"reachable"`
	Unreachable int `json:"unreachable"`
	Nodes       int `json:"nodes"`
	NodesReady  int `json:"nodesReady"`
	Pods        int `json:"pods"`
	PodsRunning int `json:"podsRunning"`
	PodsFailed  int `json:"podsFailed"`
	Deployments int `json:"deployments"`
	Services    int `json:"services"`
	Critical    int `json:"critical"`
	Warning     int `json:"warning"`
}

// FleetView is the /api/clusters payload.
type FleetView struct {
	Active   string         `json:"active"`
	Totals   FleetTotals    `json:"totals"`
	Clusters []FleetCluster `json:"clusters"`
}

// Fleet queries every kubeconfig context concurrently and returns a combined
// health snapshot. withAlerts additionally runs each cluster's alert rules,
// which costs a second round of listing and so is opt-in.
func (m *Manager) Fleet(cx context.Context, withAlerts bool) *FleetView {
	details := m.ContextDetails()
	active := m.Active()

	out := make([]FleetCluster, len(details))
	var wg sync.WaitGroup
	for i, d := range details {
		wg.Add(1)
		go func(i int, d ContextDetail) {
			defer wg.Done()
			out[i] = m.fleetEntry(cx, d, active, withAlerts)
		}(i, d)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })

	view := &FleetView{Active: active, Clusters: out}
	view.Totals.Clusters = len(out)
	for _, c := range out {
		if !c.Reachable {
			view.Totals.Unreachable++
			continue
		}
		view.Totals.Reachable++
		view.Totals.Nodes += c.Nodes
		view.Totals.NodesReady += c.NodesReady
		view.Totals.Pods += c.Pods
		view.Totals.PodsRunning += c.PodsRunning
		view.Totals.PodsFailed += c.PodsFailed
		view.Totals.Deployments += c.Deployments
		view.Totals.Services += c.Services
		view.Totals.Critical += c.Critical
		view.Totals.Warning += c.Warning
	}
	return view
}

// fleetEntry collects one cluster's row, converting every failure into an
// unreachable row rather than an error.
func (m *Manager) fleetEntry(parent context.Context, d ContextDetail, active string, withAlerts bool) FleetCluster {
	row := FleetCluster{
		Context:   d.Name,
		Cluster:   d.Cluster,
		Namespace: d.Namespace,
		Active:    d.Name == active,
	}

	cl, err := m.ClientFor(d.Name)
	if err != nil {
		row.Error = err.Error()
		return row
	}

	// Probe with the version endpoint first: it is the cheapest call that
	// proves the API server is actually answering this credential, and its
	// latency is a useful signal on its own.
	cx, cancel := context.WithTimeout(parent, fleetTimeout)
	defer cancel()
	start := time.Now()
	if err := cl.pingWithin(cx); err != nil {
		row.Error = err.Error()
		return row
	}
	row.LatencyMS = time.Since(start).Milliseconds()
	row.Reachable = true
	row.Version = cl.ServerVersion()

	if sum, err := cl.SummaryContext(cx, ""); err == nil && sum != nil {
		row.Nodes = sum.Nodes
		row.NodesReady = sum.NodesReady
		row.Namespaces = sum.Namespaces
		row.Pods = sum.Pods
		row.PodsRunning = sum.PodsRunning
		row.PodsFailed = sum.PodsFailed
		row.Deployments = sum.Deployments
		row.Services = sum.Services
		row.Warnings = sum.Warnings
	} else if err != nil {
		// Reachable but not readable — namespace-scoped RBAC, most often.
		row.Error = err.Error()
	}

	if withAlerts {
		if rep, err := cl.AlertsContext(cx, ""); err == nil && rep != nil {
			row.Critical, row.Warning = rep.Critical, rep.Warning
		}
	}
	return row
}

// pingWithin checks that the API server answers this client's credentials
// within the caller's deadline.
func (c *Client) pingWithin(cx context.Context) error {
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := c.cs.Discovery().ServerVersion()
		done <- result{err}
	}()
	select {
	case r := <-done:
		return r.err
	case <-cx.Done():
		return cx.Err()
	}
}

// ServerVersion returns the cluster's reported version, or "" when the probe
// cannot answer.
func (c *Client) ServerVersion() string {
	v, err := c.cs.Discovery().ServerVersion()
	if err != nil || v == nil {
		return ""
	}
	return v.GitVersion
}
