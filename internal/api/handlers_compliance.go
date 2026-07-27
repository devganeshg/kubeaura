package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/devganeshg/kubeaura/internal/report"
)

// Compliance report export. Every input is a check KubeAura already runs — the
// value added here is that the evidence leaves the tool as a file an auditor or
// a change ticket can keep.

func (s *Server) handleComplianceReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.buildComplianceReport(r)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleComplianceExport renders the report as a downloadable file.
func (s *Server) handleComplianceExport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}
	switch format {
	case "json", "csv", "md", "html":
	default:
		writeErr(w, http.StatusBadRequest, errString("unsupported format "+format+": use json, csv, md or html"))
		return
	}

	rep, err := s.buildComplianceReport(r)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	body, err := rep.Render(format)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", report.ContentType(format))
	// The rendered HTML is built from cluster-controlled strings, so it must
	// never be treated as a page from this origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", rep.Filename(format)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// buildComplianceReport gathers every section from the active cluster. A
// section that cannot be read is recorded as unchecked rather than failing the
// request: a partial report that says what it could not see is useful, and a
// report that silently omits a check is not.
func (s *Server) buildComplianceReport(r *http.Request) (*report.Report, error) {
	q := r.URL.Query()
	ns := q.Get("namespace")
	cl := s.k8s()

	rep := &report.Report{
		Tool:       "KubeAura",
		Version:    s.Version,
		Context:    cl.Context,
		Namespace:  ns,
		Thresholds: thresholdsFromQuery(q),
	}

	if sum, err := cl.Summary(ns); err == nil && sum != nil {
		rep.Scope = report.ClusterSection{
			Nodes:       sum.Nodes,
			NodesReady:  sum.NodesReady,
			Namespaces:  sum.Namespaces,
			Pods:        sum.Pods,
			PodsRunning: sum.PodsRunning,
			PodsFailed:  sum.PodsFailed,
			Deployments: sum.Deployments,
			Services:    sum.Services,
		}
	}

	rep.Vulns.Scanner = "Trivy Operator"
	if v, err := cl.VulnerabilityReports(r.Context(), ns); err == nil && v != nil {
		rep.Vulns.Installed = v.Installed
		rep.Vulns.Reports = v.Summary.Reports
		rep.Vulns.Critical = v.Summary.Critical
		rep.Vulns.High = v.Summary.High
		rep.Vulns.Medium = v.Summary.Medium
		rep.Vulns.Low = v.Summary.Low
		for _, rr := range v.Reports {
			if rr.Critical == 0 && rr.High == 0 && rr.Medium == 0 && rr.Low == 0 {
				continue
			}
			rep.Vulns.Worst = append(rep.Vulns.Worst, report.VulnRow{
				Namespace: rr.Namespace,
				Workload:  rr.Workload,
				Image:     rr.Image,
				Critical:  rr.Critical,
				High:      rr.High,
				Medium:    rr.Medium,
				Low:       rr.Low,
			})
		}
	}

	rep.Policy.Engine = "Kyverno / OPA PolicyReports"
	if p, err := cl.PolicyReports(r.Context(), ns); err == nil && p != nil {
		rep.Policy.Installed = p.Installed
		rep.Policy.Reports = p.Summary.Reports
		rep.Policy.Pass = p.Summary.Pass
		rep.Policy.Fail = p.Summary.Fail
		rep.Policy.Warn = p.Summary.Warn
		rep.Policy.Error = p.Summary.Error
		for _, pr := range p.Reports {
			for _, f := range pr.Findings {
				rep.Policy.Failures = append(rep.Policy.Failures, report.PolicyRow{
					Namespace: pr.Namespace,
					Policy:    f.Policy,
					Rule:      f.Rule,
					Result:    f.Result,
					Severity:  f.Severity,
					Resource:  f.Resource,
					Message:   f.Message,
				})
			}
		}
	}

	if v := s.rbacValidator(); v != nil {
		sa := q.Get("serviceAccount")
		if sa == "" {
			sa = "default"
		}
		rbacNS := ns
		if rbacNS == "" {
			rbacNS = "default"
		}
		if cr, err := v.GenerateComplianceReport(r.Context(), sa, rbacNS); err == nil && cr != nil {
			rep.RBAC.Checked = true
			rep.RBAC.ServiceAccount = cr.ServiceAccount
			rep.RBAC.IsClusterAdmin = cr.IsClusterAdmin
			rep.RBAC.DegradedFeatures = cr.DegradedFeatures
			for _, p := range cr.Permissions {
				if p.IsAllowed {
					continue
				}
				rep.RBAC.DeniedCount++
				group := p.APIGroup
				if group == "" {
					group = "core"
				}
				rep.RBAC.Denied = append(rep.RBAC.Denied, p.Verb+" "+group+"/"+p.Resource)
			}
		}
	}

	rep.ClusterVer = cl.ServerVersion()
	rep.Finalize()
	return rep, nil
}

// thresholdsFromQuery lets a caller relax the default production bar, e.g.
// ?maxHigh=5 for a development cluster.
func thresholdsFromQuery(q map[string][]string) report.Thresholds {
	t := report.DefaultThresholds()
	get := func(key string) (int, bool) {
		vals, ok := q[key]
		if !ok || len(vals) == 0 {
			return 0, false
		}
		n, err := strconv.Atoi(vals[0])
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	if n, ok := get("maxCritical"); ok {
		t.MaxCritical = n
	}
	if n, ok := get("maxHigh"); ok {
		t.MaxHigh = n
	}
	if n, ok := get("maxPolicyFail"); ok {
		t.MaxPolicyFail = n
	}
	return t
}
