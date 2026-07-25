package k8s

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Trivy Operator writes one VulnerabilityReport CR per scanned container.
// KubeAura never scans anything itself — it just reads these reports when the
// operator is installed, following the same detect-don't-install pattern as
// cert-manager and Flux (see CNCF_INTEGRATION_ROADMAP.md).
var vulnReportsGVR = schema.GroupVersionResource{
	Group: "aquasecurity.github.io", Version: "v1alpha1", Resource: "vulnerabilityreports",
}

// VulnCVE is one notable finding within a report (worst-first sample, not the
// full list — the full report stays in the cluster).
type VulnCVE struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
	Title     string `json:"title"`
}

// VulnReport is the normalized, table-friendly view of one VulnerabilityReport.
type VulnReport struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Workload  string    `json:"workload"` // e.g. "Deployment/api"
	Container string    `json:"container"`
	Image     string    `json:"image"`
	Critical  int64     `json:"critical"`
	High      int64     `json:"high"`
	Medium    int64     `json:"medium"`
	Low       int64     `json:"low"`
	Age       string    `json:"age"`
	Top       []VulnCVE `json:"top,omitempty"` // worst findings, capped
}

// VulnSummary aggregates all reports in scope.
type VulnSummary struct {
	Reports  int   `json:"reports"`
	Critical int64 `json:"critical"`
	High     int64 `json:"high"`
	Medium   int64 `json:"medium"`
	Low      int64 `json:"low"`
}

// VulnResult is the /api/security/vulnerabilities payload. Installed=false
// means the Trivy Operator CRD is absent — a state, not an error.
type VulnResult struct {
	Installed bool         `json:"installed"`
	Summary   VulnSummary  `json:"summary"`
	Reports   []VulnReport `json:"reports"`
}

const maxTopCVEs = 5

// VulnerabilityReports lists Trivy Operator reports for a namespace (empty =
// all namespaces), sorted worst-first.
func (c *Client) VulnerabilityReports(cx context.Context, namespace string) (*VulnResult, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil, err
	}
	list, err := dyn.Resource(vulnReportsGVR).Namespace(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		// Missing CRD means the operator isn't installed — report that state.
		if apierrors.IsNotFound(err) {
			return &VulnResult{Installed: false, Reports: []VulnReport{}}, nil
		}
		return nil, err
	}

	res := &VulnResult{Installed: true, Reports: make([]VulnReport, 0, len(list.Items))}
	for _, it := range list.Items {
		r := vulnReportRow(it)
		res.Summary.Reports++
		res.Summary.Critical += r.Critical
		res.Summary.High += r.High
		res.Summary.Medium += r.Medium
		res.Summary.Low += r.Low
		res.Reports = append(res.Reports, r)
	}
	sort.Slice(res.Reports, func(i, j int) bool {
		a, b := res.Reports[i], res.Reports[j]
		if a.Critical != b.Critical {
			return a.Critical > b.Critical
		}
		if a.High != b.High {
			return a.High > b.High
		}
		if a.Medium != b.Medium {
			return a.Medium > b.Medium
		}
		return a.Namespace+a.Name < b.Namespace+b.Name
	})
	return res, nil
}

func vulnReportRow(it unstructured.Unstructured) VulnReport {
	labels := it.GetLabels()
	workload := ""
	if k, n := labels["trivy-operator.resource.kind"], labels["trivy-operator.resource.name"]; k != "" {
		workload = fmt.Sprintf("%s/%s", k, n)
	}
	registry, _, _ := unstructured.NestedString(it.Object, "report", "registry", "server")
	repo, _, _ := unstructured.NestedString(it.Object, "report", "artifact", "repository")
	tag, _, _ := unstructured.NestedString(it.Object, "report", "artifact", "tag")
	image := repo
	if registry != "" {
		image = registry + "/" + repo
	}
	if tag != "" {
		image += ":" + tag
	}

	crit, _, _ := unstructured.NestedInt64(it.Object, "report", "summary", "criticalCount")
	high, _, _ := unstructured.NestedInt64(it.Object, "report", "summary", "highCount")
	med, _, _ := unstructured.NestedInt64(it.Object, "report", "summary", "mediumCount")
	low, _, _ := unstructured.NestedInt64(it.Object, "report", "summary", "lowCount")

	return VulnReport{
		Namespace: it.GetNamespace(),
		Name:      it.GetName(),
		Workload:  workload,
		Container: labels["trivy-operator.container.name"],
		Image:     image,
		Critical:  crit,
		High:      high,
		Medium:    med,
		Low:       low,
		Age:       age(it.GetCreationTimestamp()),
		Top:       topCVEs(it),
	}
}

// topCVEs pulls the worst findings (Critical first, then High) capped at
// maxTopCVEs so the list payload stays small.
func topCVEs(it unstructured.Unstructured) []VulnCVE {
	vulns, found, _ := unstructured.NestedSlice(it.Object, "report", "vulnerabilities")
	if !found {
		return nil
	}
	rank := map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}
	out := make([]VulnCVE, 0, len(vulns))
	for _, v := range vulns {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		id, _, _ := unstructured.NestedString(m, "vulnerabilityID")
		sev, _, _ := unstructured.NestedString(m, "severity")
		pkg, _, _ := unstructured.NestedString(m, "resource")
		inst, _, _ := unstructured.NestedString(m, "installedVersion")
		fixed, _, _ := unstructured.NestedString(m, "fixedVersion")
		title, _, _ := unstructured.NestedString(m, "title")
		out = append(out, VulnCVE{ID: id, Severity: sev, Package: pkg, Installed: inst, Fixed: fixed, Title: title})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, ok := rank[out[i].Severity]
		if !ok {
			ri = 4
		}
		rj, ok := rank[out[j].Severity]
		if !ok {
			rj = 4
		}
		return ri < rj
	})
	if len(out) > maxTopCVEs {
		out = out[:maxTopCVEs]
	}
	return out
}
