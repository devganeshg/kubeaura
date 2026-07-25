package k8s

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Kyverno, OPA Gatekeeper (via export), and other policy engines write the
// Kubernetes Policy WG's standard PolicyReport CRD. KubeMind reads whatever
// engine produced them — detect-don't-install, roadmap item 3.
var (
	policyReportGVRs = []schema.GroupVersionResource{
		{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"},
		{Group: "wgpolicyk8s.io", Version: "v1beta1", Resource: "policyreports"},
	}
	clusterPolicyReportGVRs = []schema.GroupVersionResource{
		{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "clusterpolicyreports"},
		{Group: "wgpolicyk8s.io", Version: "v1beta1", Resource: "clusterpolicyreports"},
	}
)

// PolicyFinding is one non-passing result within a report.
type PolicyFinding struct {
	Policy   string `json:"policy"`
	Rule     string `json:"rule"`
	Result   string `json:"result"` // fail|warn|error
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Resource string `json:"resource"` // "Kind/name" of the offending object
}

// PolicyReportRow is the normalized view of one (Cluster)PolicyReport.
type PolicyReportRow struct {
	Namespace string          `json:"namespace"` // "" for cluster-scoped
	Name      string          `json:"name"`
	Scope     string          `json:"scope"` // subject of the report when recorded
	Pass      int64           `json:"pass"`
	Fail      int64           `json:"fail"`
	Warn      int64           `json:"warn"`
	Error     int64           `json:"error"`
	Skip      int64           `json:"skip"`
	Age       string          `json:"age"`
	Findings  []PolicyFinding `json:"findings,omitempty"` // non-passing only, capped
}

// PolicySummary aggregates all reports in scope.
type PolicySummary struct {
	Reports int64 `json:"reports"`
	Pass    int64 `json:"pass"`
	Fail    int64 `json:"fail"`
	Warn    int64 `json:"warn"`
	Error   int64 `json:"error"`
}

// PolicyResult is the /api/policy payload. Installed=false means no policy
// engine writes PolicyReports here — a state, not an error.
type PolicyResult struct {
	Installed bool              `json:"installed"`
	Summary   PolicySummary     `json:"summary"`
	Reports   []PolicyReportRow `json:"reports"`
}

const maxFindings = 8

// PolicyReports lists namespaced PolicyReports (plus ClusterPolicyReports when
// no namespace filter is given), sorted worst-first.
func (c *Client) PolicyReports(cx context.Context, namespace string) (*PolicyResult, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil, err
	}
	res := &PolicyResult{Reports: []PolicyReportRow{}}

	if items, ok := listFirstAvailable(cx, dyn, namespace, policyReportGVRs); ok {
		res.Installed = true
		for _, it := range items {
			res.Reports = append(res.Reports, policyReportRow(it))
		}
	}
	if namespace == "" {
		if items, ok := listFirstAvailable(cx, dyn, "", clusterPolicyReportGVRs); ok {
			res.Installed = true
			for _, it := range items {
				res.Reports = append(res.Reports, policyReportRow(it))
			}
		}
	}

	for _, r := range res.Reports {
		res.Summary.Reports++
		res.Summary.Pass += r.Pass
		res.Summary.Fail += r.Fail
		res.Summary.Warn += r.Warn
		res.Summary.Error += r.Error
	}
	sort.Slice(res.Reports, func(i, j int) bool {
		a, b := res.Reports[i], res.Reports[j]
		if a.Fail+a.Error != b.Fail+b.Error {
			return a.Fail+a.Error > b.Fail+b.Error
		}
		if a.Warn != b.Warn {
			return a.Warn > b.Warn
		}
		return a.Namespace+a.Name < b.Namespace+b.Name
	})
	return res, nil
}

func policyReportRow(it unstructured.Unstructured) PolicyReportRow {
	pass, _, _ := unstructured.NestedInt64(it.Object, "summary", "pass")
	fail, _, _ := unstructured.NestedInt64(it.Object, "summary", "fail")
	warn, _, _ := unstructured.NestedInt64(it.Object, "summary", "warn")
	errC, _, _ := unstructured.NestedInt64(it.Object, "summary", "error")
	skip, _, _ := unstructured.NestedInt64(it.Object, "summary", "skip")
	scopeKind, _, _ := unstructured.NestedString(it.Object, "scope", "kind")
	scopeName, _, _ := unstructured.NestedString(it.Object, "scope", "name")
	scope := ""
	if scopeKind != "" {
		scope = scopeKind + "/" + scopeName
	}
	return PolicyReportRow{
		Namespace: it.GetNamespace(),
		Name:      it.GetName(),
		Scope:     scope,
		Pass:      pass, Fail: fail, Warn: warn, Error: errC, Skip: skip,
		Age:      age(it.GetCreationTimestamp()),
		Findings: policyFindings(it),
	}
}

// policyFindings extracts non-passing results, worst-first, capped at
// maxFindings so list payloads stay small.
func policyFindings(it unstructured.Unstructured) []PolicyFinding {
	results, found, _ := unstructured.NestedSlice(it.Object, "results")
	if !found {
		return nil
	}
	rank := map[string]int{"error": 0, "fail": 1, "warn": 2}
	out := make([]PolicyFinding, 0)
	for _, r := range results {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		result, _, _ := unstructured.NestedString(m, "result")
		if _, bad := rank[result]; !bad {
			continue // pass/skip results stay in the cluster, not the payload
		}
		policy, _, _ := unstructured.NestedString(m, "policy")
		rule, _, _ := unstructured.NestedString(m, "rule")
		sev, _, _ := unstructured.NestedString(m, "severity")
		msg, _, _ := unstructured.NestedString(m, "message")
		resource := ""
		if subjects, found, _ := unstructured.NestedSlice(m, "resources"); found && len(subjects) > 0 {
			if sm, ok := subjects[0].(map[string]interface{}); ok {
				k, _, _ := unstructured.NestedString(sm, "kind")
				n, _, _ := unstructured.NestedString(sm, "name")
				if k != "" {
					resource = k + "/" + n
				}
			}
		}
		out = append(out, PolicyFinding{
			Policy: policy, Rule: rule, Result: result,
			Severity: sev, Message: msg, Resource: resource,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Result] < rank[out[j].Result] })
	if len(out) > maxFindings {
		out = out[:maxFindings]
	}
	return out
}
