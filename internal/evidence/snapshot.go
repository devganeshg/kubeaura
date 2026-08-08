package evidence

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// MaxSnapshotBytes caps a cluster snapshot. Unlike a log tail this has no
// natural end: a 2,000-pod namespace produces a payload that is both expensive
// and useless, because the answer is buried. The cap forces the snapshot to
// stay a summary.
const MaxSnapshotBytes = 48 << 10

// SnapshotItem is one row of cluster state, already flattened by the caller.
//
// This deliberately mirrors k8s.Resource without importing it: evidence is a
// leaf package and its tests run without a cluster or a clientset. The mapping
// lives in the API layer, which owns both sides.
type SnapshotItem struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Status    string            `json:"status,omitempty"`
	Info      string            `json:"info,omitempty"`
	Age       string            `json:"age,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// SnapshotInput is the raw material ForSnapshot works from.
type SnapshotInput struct {
	// Purpose distinguishes the endpoints that share this shape: "query",
	// "triage", "topology".
	Purpose   string
	Namespace string
	// Summary is the cluster counts block; it is small, fixed-shape and
	// operator-visible, so it is included whole.
	Summary interface{}
	// Groups are the resource lists keyed by kind, in the order they should be
	// dropped last — earlier groups survive trimming longer.
	Groups []SnapshotGroup
	// The operator's question is deliberately not part of this envelope. The
	// envelope discloses what KubeAura added to the prompt on the operator's
	// behalf; what they typed themselves they already know, and scrubbing it
	// would mangle the very question they are asking ("why does the pod with
	// DB_PASSWORD set keep crashing?").
}

// SnapshotGroup is one kind's worth of rows. Items must be newest-first: the
// byte cap drops from the tail.
type SnapshotGroup struct {
	Kind  string
	Items []SnapshotItem
}

// ForSnapshot builds the redacted cluster-snapshot payload and its envelope.
//
// The rows arriving here are already flat — name, status, a summary column —
// so there is no spec to strip. What is left is free text the cluster's own
// workloads wrote: an event message quoting a failed connection string, a label
// an operator set to something they should not have. Those get the scrubbers,
// and the whole thing gets a ceiling.
func ForSnapshot(in SnapshotInput) (*Payload, error) {
	c := newCounter()
	purpose := in.Purpose
	if purpose == "" {
		purpose = "query"
	}

	groups := make([]SnapshotGroup, 0, len(in.Groups))
	total := 0
	for _, g := range in.Groups {
		items := make([]SnapshotItem, 0, len(g.Items))
		for _, it := range g.Items {
			items = append(items, scrubItem(it, g.Kind, c))
		}
		total += len(items)
		groups = append(groups, SnapshotGroup{Kind: g.Kind, Items: items})
	}

	body, kept, truncated, err := encodeSnapshot(in, groups)
	if err != nil {
		return nil, err
	}

	fields := []string{"summary"}
	for _, g := range groups {
		fields = append(fields, g.Kind+" (name/status/summary only)")
	}

	env := Envelope{
		Purpose:    purpose,
		Resource:   ResourceRef{Kind: "Cluster", Namespace: in.Namespace},
		Fields:     fields,
		Redactions: c.list(),
		Items:      kept,
		Bytes:      len(body),
		Hash:       hashOf(body),
		Truncated:  truncated,
		PreparedAt: time.Now().UTC(),
	}
	return &Payload{JSON: string(body), Envelope: env}, nil
}

// encodeSnapshot marshals the snapshot, shrinking it until it fits the cap.
//
// Trimming is proportional and repeated rather than computed in one pass:
// row sizes vary by an order of magnitude between a Node and an Event, so a
// single estimate overshoots badly in both directions. Halving the largest
// group converges in a handful of iterations and always terminates.
func encodeSnapshot(in SnapshotInput, groups []SnapshotGroup) ([]byte, int, bool, error) {
	truncated := false
	for {
		payload := map[string]interface{}{"summary": in.Summary}
		if in.Namespace != "" {
			payload["namespace"] = in.Namespace
		}
		kept := 0
		for _, g := range groups {
			payload[g.Kind] = g.Items
			kept += len(g.Items)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, false, fmt.Errorf("encode snapshot evidence: %w", err)
		}
		if len(body) <= MaxSnapshotBytes || kept == 0 {
			return body, kept, truncated, nil
		}
		truncated = true
		trimLargest(groups)
	}
}

// trimLargest halves the group holding the most rows, keeping the newest half.
func trimLargest(groups []SnapshotGroup) {
	idx, best := -1, 1
	for i, g := range groups {
		if len(g.Items) > best {
			idx, best = i, len(g.Items)
		}
	}
	if idx < 0 {
		// Every group is down to one row; drop the last non-empty one rather
		// than spinning.
		for i := len(groups) - 1; i >= 0; i-- {
			if len(groups[i].Items) > 0 {
				groups[i].Items = nil
				return
			}
		}
		return
	}
	groups[idx].Items = groups[idx].Items[:best/2]
}

// scrubItem runs the free-text scrubbers over the fields a workload controls.
// Info is where event messages land, and an event message is the single most
// common place a cluster prints someone's credential back at them.
func scrubItem(it SnapshotItem, kind string, c *counter) SnapshotItem {
	if s, hits := ScrubText(it.Info); hits > 0 {
		c.add("free-text-secret", kind+"[].info", len(it.Info)-len(s))
		it.Info = s
	}
	if s, hits := ScrubText(it.Status); hits > 0 {
		c.add("free-text-secret", kind+"[].status", len(it.Status)-len(s))
		it.Status = s
	}
	if len(it.Labels) > 0 {
		out := make(map[string]string, len(it.Labels))
		keys := make([]string, 0, len(it.Labels))
		for k := range it.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := it.Labels[k]
			if isSensitiveAnnotation(k) {
				c.add("sensitive-label", kind+"[].labels", len(v))
				out[k] = redactedMark
				continue
			}
			s, hits := ScrubText(v)
			if hits > 0 {
				c.add("free-text-secret", kind+"[].labels", len(v)-len(s))
			}
			out[k] = s
		}
		it.Labels = out
	}
	return it
}
