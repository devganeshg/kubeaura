// Package report builds an exportable compliance report from the checks
// KubeAura already runs — image vulnerabilities, policy reports, and RBAC
// posture — and renders it as JSON, CSV, Markdown or a self-contained HTML
// page.
//
// The point of the export is that the evidence leaves the tool: an auditor or a
// change-approval ticket wants a file, not a dashboard. Nothing here reaches
// out to a cluster; the caller gathers the data and this package shapes it, so
// the rendering is testable on its own.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Severity thresholds a report is judged against. Zero means "none allowed".
type Thresholds struct {
	MaxCritical   int `json:"maxCritical"`
	MaxHigh       int `json:"maxHigh"`
	MaxPolicyFail int `json:"maxPolicyFail"`
}

// DefaultThresholds is the production-shaped bar: no critical or high image
// CVEs, and no failing policy rules.
func DefaultThresholds() Thresholds {
	return Thresholds{MaxCritical: 0, MaxHigh: 0, MaxPolicyFail: 0}
}

// Report is the whole document.
type Report struct {
	GeneratedAt time.Time  `json:"generatedAt"`
	Tool        string     `json:"tool"`
	Version     string     `json:"version"`
	Context     string     `json:"context"`        // kubeconfig context the data came from
	ClusterVer  string     `json:"clusterVersion"` // Kubernetes server version
	Namespace   string     `json:"namespace"`      // "" means all namespaces
	Thresholds  Thresholds `json:"thresholds"`

	Scope  ClusterSection `json:"clusterSummary"`
	Vulns  VulnSection    `json:"vulnerabilities"`
	Policy PolicySection  `json:"policy"`
	RBAC   RBACSection    `json:"rbac"`

	// Verdict is the whole point of the export: one pass/fail plus the reasons.
	Compliant  bool     `json:"compliant"`
	Violations []string `json:"violations"`
	// Skipped names checks that could not run — an absent Trivy Operator, a
	// forbidden read. An auditor must be able to tell "clean" from "unchecked".
	Skipped []string `json:"skipped"`
}

// ClusterSection is the scope the report was taken over.
type ClusterSection struct {
	Nodes       int `json:"nodes"`
	NodesReady  int `json:"nodesReady"`
	Namespaces  int `json:"namespaces"`
	Pods        int `json:"pods"`
	PodsRunning int `json:"podsRunning"`
	PodsFailed  int `json:"podsFailed"`
	Deployments int `json:"deployments"`
	Services    int `json:"services"`
}

// VulnSection summarizes container image scanning.
type VulnSection struct {
	Scanner   string    `json:"scanner"`   // e.g. "Trivy Operator"
	Installed bool      `json:"installed"` // false means the check did not run
	Reports   int       `json:"reports"`
	Critical  int64     `json:"critical"`
	High      int64     `json:"high"`
	Medium    int64     `json:"medium"`
	Low       int64     `json:"low"`
	Worst     []VulnRow `json:"worst"` // worst-affected workloads, capped
}

// VulnRow is one workload's image findings.
type VulnRow struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Image     string `json:"image"`
	Critical  int64  `json:"critical"`
	High      int64  `json:"high"`
	Medium    int64  `json:"medium"`
	Low       int64  `json:"low"`
}

// PolicySection summarizes admission/policy engine results.
type PolicySection struct {
	Engine    string      `json:"engine"` // e.g. "Kyverno / OPA PolicyReports"
	Installed bool        `json:"installed"`
	Reports   int64       `json:"reports"`
	Pass      int64       `json:"pass"`
	Fail      int64       `json:"fail"`
	Warn      int64       `json:"warn"`
	Error     int64       `json:"error"`
	Failures  []PolicyRow `json:"failures"` // non-passing findings, capped
}

// PolicyRow is one failing policy finding.
type PolicyRow struct {
	Namespace string `json:"namespace"`
	Policy    string `json:"policy"`
	Rule      string `json:"rule"`
	Result    string `json:"result"`
	Severity  string `json:"severity"`
	Resource  string `json:"resource"`
	Message   string `json:"message"`
}

// RBACSection records what the credentials this report was taken with can do.
type RBACSection struct {
	Checked          bool     `json:"checked"`
	ServiceAccount   string   `json:"serviceAccount"`
	IsClusterAdmin   bool     `json:"isClusterAdmin"`
	DeniedCount      int      `json:"deniedCount"`
	DegradedFeatures []string `json:"degradedFeatures"`
	Denied           []string `json:"denied"` // "verb group/resource"
}

const maxRows = 25

// Finalize sorts the capped tables, evaluates the thresholds and fills in the
// verdict. Call it once the sections are populated.
func (r *Report) Finalize() {
	if r.GeneratedAt.IsZero() {
		r.GeneratedAt = time.Now().UTC()
	}
	if r.Tool == "" {
		r.Tool = "KubeAura"
	}

	sort.SliceStable(r.Vulns.Worst, func(i, j int) bool {
		a, b := r.Vulns.Worst[i], r.Vulns.Worst[j]
		if a.Critical != b.Critical {
			return a.Critical > b.Critical
		}
		return a.High > b.High
	})
	if len(r.Vulns.Worst) > maxRows {
		r.Vulns.Worst = r.Vulns.Worst[:maxRows]
	}
	if len(r.Policy.Failures) > maxRows {
		r.Policy.Failures = r.Policy.Failures[:maxRows]
	}

	// Both lists are rebuilt from scratch: Finalize is called again whenever a
	// caller adjusts thresholds, and appending would double every entry.
	r.Violations = nil
	r.Skipped = nil
	if r.Vulns.Installed {
		if int(r.Vulns.Critical) > r.Thresholds.MaxCritical {
			r.Violations = append(r.Violations, fmt.Sprintf(
				"critical image CVEs: %d, allowed %d", r.Vulns.Critical, r.Thresholds.MaxCritical))
		}
		if int(r.Vulns.High) > r.Thresholds.MaxHigh {
			r.Violations = append(r.Violations, fmt.Sprintf(
				"high image CVEs: %d, allowed %d", r.Vulns.High, r.Thresholds.MaxHigh))
		}
	} else {
		r.Skipped = append(r.Skipped, "image vulnerability scanning: no Trivy Operator VulnerabilityReports found in scope")
	}

	if r.Policy.Installed {
		if int(r.Policy.Fail) > r.Thresholds.MaxPolicyFail {
			r.Violations = append(r.Violations, fmt.Sprintf(
				"failing policy rules: %d, allowed %d", r.Policy.Fail, r.Thresholds.MaxPolicyFail))
		}
	} else {
		r.Skipped = append(r.Skipped, "policy compliance: no PolicyReports found in scope (no Kyverno/OPA reporting)")
	}

	if !r.RBAC.Checked {
		r.Skipped = append(r.Skipped, "RBAC posture: the validator was not available")
	}

	// A report whose checks could not run is not a passing report. Saying
	// otherwise would be the one genuinely dangerous thing this export could do.
	r.Compliant = len(r.Violations) == 0 && len(r.Skipped) == 0
}

// Filename is the suggested download name for a rendered report.
func (r *Report) Filename(format string) string {
	scope := r.Context
	if scope == "" {
		scope = "cluster"
	}
	scope = strings.Map(func(c rune) rune {
		if c == '/' || c == ' ' || c == ':' {
			return '-'
		}
		return c
	}, scope)
	return fmt.Sprintf("kubeaura-compliance-%s-%s.%s",
		scope, r.GeneratedAt.Format("20060102-150405"), format)
}

// ContentType is the MIME type for a rendered format.
func ContentType(format string) string {
	switch format {
	case "csv":
		return "text/csv; charset=utf-8"
	case "md":
		return "text/markdown; charset=utf-8"
	case "html":
		return "text/html; charset=utf-8"
	default:
		return "application/json"
	}
}

// Render produces the report in one of json, csv, md or html.
func (r *Report) Render(format string) ([]byte, error) {
	switch format {
	case "", "json":
		return json.MarshalIndent(r, "", "  ")
	case "csv":
		return r.renderCSV()
	case "md":
		return []byte(r.renderMarkdown()), nil
	case "html":
		return []byte(r.renderHTML()), nil
	default:
		return nil, fmt.Errorf("unsupported format %q: use json, csv, md or html", format)
	}
}

// renderCSV emits one row per finding with a leading section column, which is
// the shape spreadsheets and GRC tools import cleanly.
func (r *Report) renderCSV() ([]byte, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	rows := [][]string{
		{"section", "namespace", "subject", "detail", "severity", "value"},
		{"meta", "", "generated", r.GeneratedAt.Format(time.RFC3339), "", ""},
		{"meta", "", "context", r.Context, "", ""},
		{"meta", "", "cluster version", r.ClusterVer, "", ""},
		{"meta", "", "scope", scopeLabel(r.Namespace), "", ""},
		{"verdict", "", "compliant", "", "", strconv.FormatBool(r.Compliant)},
	}
	for _, v := range r.Violations {
		rows = append(rows, []string{"verdict", "", "violation", v, "", ""})
	}
	for _, s := range r.Skipped {
		rows = append(rows, []string{"verdict", "", "not checked", s, "", ""})
	}

	rows = append(rows,
		[]string{"vulnerabilities", "", "scanner installed", r.Vulns.Scanner, "", strconv.FormatBool(r.Vulns.Installed)},
		[]string{"vulnerabilities", "", "totals", "", "CRITICAL", strconv.FormatInt(r.Vulns.Critical, 10)},
		[]string{"vulnerabilities", "", "totals", "", "HIGH", strconv.FormatInt(r.Vulns.High, 10)},
		[]string{"vulnerabilities", "", "totals", "", "MEDIUM", strconv.FormatInt(r.Vulns.Medium, 10)},
		[]string{"vulnerabilities", "", "totals", "", "LOW", strconv.FormatInt(r.Vulns.Low, 10)},
	)
	for _, v := range r.Vulns.Worst {
		rows = append(rows, []string{"vulnerabilities", v.Namespace, v.Workload, v.Image, "CRITICAL", strconv.FormatInt(v.Critical, 10)})
		rows = append(rows, []string{"vulnerabilities", v.Namespace, v.Workload, v.Image, "HIGH", strconv.FormatInt(v.High, 10)})
	}

	rows = append(rows,
		[]string{"policy", "", "engine installed", r.Policy.Engine, "", strconv.FormatBool(r.Policy.Installed)},
		[]string{"policy", "", "pass", "", "", strconv.FormatInt(r.Policy.Pass, 10)},
		[]string{"policy", "", "fail", "", "", strconv.FormatInt(r.Policy.Fail, 10)},
	)
	for _, p := range r.Policy.Failures {
		rows = append(rows, []string{"policy", p.Namespace, p.Policy + "/" + p.Rule, p.Resource + ": " + p.Message, p.Severity, p.Result})
	}

	rows = append(rows,
		[]string{"rbac", "", "checked", "", "", strconv.FormatBool(r.RBAC.Checked)},
		[]string{"rbac", "", "cluster admin", "", "", strconv.FormatBool(r.RBAC.IsClusterAdmin)},
		[]string{"rbac", "", "denied permissions", "", "", strconv.Itoa(r.RBAC.DeniedCount)},
	)
	for _, f := range r.RBAC.DegradedFeatures {
		rows = append(rows, []string{"rbac", "", "degraded feature", f, "", ""})
	}
	for _, d := range r.RBAC.Denied {
		rows = append(rows, []string{"rbac", "", "denied", d, "", ""})
	}

	if err := w.WriteAll(rows); err != nil {
		return nil, err
	}
	w.Flush()
	return []byte(b.String()), w.Error()
}

func (r *Report) renderMarkdown() string {
	var b strings.Builder
	verdict := "**FAIL**"
	if r.Compliant {
		verdict = "**PASS**"
	}
	fmt.Fprintf(&b, "# KubeAura compliance report\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Generated | %s |\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "| Context | %s |\n", mdCell(r.Context))
	fmt.Fprintf(&b, "| Cluster version | %s |\n", mdCell(r.ClusterVer))
	fmt.Fprintf(&b, "| Scope | %s |\n", scopeLabel(r.Namespace))
	fmt.Fprintf(&b, "| Tool | %s %s |\n", r.Tool, r.Version)
	fmt.Fprintf(&b, "| Verdict | %s |\n\n", verdict)

	if len(r.Violations) > 0 {
		b.WriteString("## Violations\n\n")
		for _, v := range r.Violations {
			fmt.Fprintf(&b, "- %s\n", v)
		}
		b.WriteString("\n")
	}
	if len(r.Skipped) > 0 {
		b.WriteString("## Not checked\n\n")
		for _, s := range r.Skipped {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Scope\n\n")
	fmt.Fprintf(&b, "%d nodes (%d ready) · %d namespaces · %d pods (%d running, %d failed) · %d deployments · %d services\n\n",
		r.Scope.Nodes, r.Scope.NodesReady, r.Scope.Namespaces,
		r.Scope.Pods, r.Scope.PodsRunning, r.Scope.PodsFailed,
		r.Scope.Deployments, r.Scope.Services)

	fmt.Fprintf(&b, "## Image vulnerabilities (%s)\n\n", r.Vulns.Scanner)
	if !r.Vulns.Installed {
		b.WriteString("_Not checked: no vulnerability reports found in scope._\n\n")
	} else {
		fmt.Fprintf(&b, "%d reports · critical %d · high %d · medium %d · low %d\n\n",
			r.Vulns.Reports, r.Vulns.Critical, r.Vulns.High, r.Vulns.Medium, r.Vulns.Low)
		if len(r.Vulns.Worst) > 0 {
			b.WriteString("| Namespace | Workload | Image | Critical | High | Medium | Low |\n")
			b.WriteString("|---|---|---|---:|---:|---:|---:|\n")
			for _, v := range r.Vulns.Worst {
				fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d |\n",
					mdCell(v.Namespace), mdCell(v.Workload), mdCell(v.Image), v.Critical, v.High, v.Medium, v.Low)
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "## Policy compliance (%s)\n\n", r.Policy.Engine)
	if !r.Policy.Installed {
		b.WriteString("_Not checked: no PolicyReports found in scope._\n\n")
	} else {
		fmt.Fprintf(&b, "%d reports · pass %d · fail %d · warn %d · error %d\n\n",
			r.Policy.Reports, r.Policy.Pass, r.Policy.Fail, r.Policy.Warn, r.Policy.Error)
		if len(r.Policy.Failures) > 0 {
			b.WriteString("| Namespace | Policy / rule | Result | Severity | Resource | Message |\n")
			b.WriteString("|---|---|---|---|---|---|\n")
			for _, p := range r.Policy.Failures {
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
					mdCell(p.Namespace), mdCell(p.Policy+"/"+p.Rule), mdCell(p.Result),
					mdCell(p.Severity), mdCell(p.Resource), mdCell(p.Message))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("## RBAC posture\n\n")
	if !r.RBAC.Checked {
		b.WriteString("_Not checked._\n\n")
	} else {
		fmt.Fprintf(&b, "Service account `%s` · cluster-admin: %v · denied permissions: %d\n\n",
			r.RBAC.ServiceAccount, r.RBAC.IsClusterAdmin, r.RBAC.DeniedCount)
		if len(r.RBAC.DegradedFeatures) > 0 {
			fmt.Fprintf(&b, "Degraded features: %s\n\n", strings.Join(r.RBAC.DegradedFeatures, ", "))
		}
		for _, d := range r.RBAC.Denied {
			fmt.Fprintf(&b, "- denied: `%s`\n", d)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderHTML produces a standalone page with no external references, so the
// file can be attached to a ticket or opened from a share without pulling
// anything over the network.
func (r *Report) renderHTML() string {
	var b strings.Builder
	verdictClass, verdictText := "fail", "NOT COMPLIANT"
	if r.Compliant {
		verdictClass, verdictText = "pass", "COMPLIANT"
	}
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	fmt.Fprintf(&b, `<title>KubeAura compliance report — %s</title>`, html.EscapeString(r.Context))
	b.WriteString(`<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#16181d;--muted:#616a76;--line:#e3e6ea;--card:#f7f8fa;--pass:#1a7f37;--fail:#c4291c;--brand:#ff4000}
@media (prefers-color-scheme:dark){:root{--bg:#0f1116;--fg:#e6e8ec;--muted:#98a1af;--line:#252a33;--card:#171a21;--pass:#3fb950;--fail:#f85149}}
*{box-sizing:border-box}body{margin:0;padding:2rem 1.25rem;background:var(--bg);color:var(--fg);font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
main{max-width:60rem;margin:0 auto}h1{font-size:1.6rem;margin:0 0 .25rem}h2{font-size:1.1rem;margin:2rem 0 .6rem;padding-bottom:.3rem;border-bottom:1px solid var(--line)}
.sub{color:var(--muted);margin:0 0 1.5rem}
.verdict{display:inline-block;padding:.35rem .8rem;border-radius:999px;font-weight:700;letter-spacing:.04em;font-size:.8rem;color:#fff}
.verdict.pass{background:var(--pass)}.verdict.fail{background:var(--fail)}
dl.meta{display:grid;grid-template-columns:auto 1fr;gap:.35rem 1rem;margin:0 0 1.5rem}dl.meta dt{color:var(--muted)}dl.meta dd{margin:0}
.tablewrap{overflow-x:auto;border:1px solid var(--line);border-radius:8px}
table{border-collapse:collapse;width:100%;font-size:.9rem;min-width:36rem}
th,td{text-align:left;padding:.5rem .7rem;border-bottom:1px solid var(--line);vertical-align:top}
th{background:var(--card);font-weight:600}tr:last-child td{border-bottom:none}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
ul{margin:.4rem 0 0;padding-left:1.2rem}li{margin:.2rem 0}
.note{color:var(--muted);font-style:italic}
.stat{display:flex;flex-wrap:wrap;gap:.5rem;margin:.5rem 0}
.stat span{background:var(--card);border:1px solid var(--line);border-radius:6px;padding:.3rem .6rem;font-size:.85rem}
footer{margin-top:2.5rem;padding-top:1rem;border-top:1px solid var(--line);color:var(--muted);font-size:.85rem}
</style></head><body><main>`)

	b.WriteString(`<h1>Kubernetes compliance report</h1>`)
	fmt.Fprintf(&b, `<p class="sub"><span class="verdict %s">%s</span></p>`, verdictClass, verdictText)
	b.WriteString(`<dl class="meta">`)
	htmlMeta(&b, "Generated", r.GeneratedAt.Format(time.RFC3339))
	htmlMeta(&b, "Context", r.Context)
	htmlMeta(&b, "Cluster version", r.ClusterVer)
	htmlMeta(&b, "Scope", scopeLabel(r.Namespace))
	htmlMeta(&b, "Tool", strings.TrimSpace(r.Tool+" "+r.Version))
	b.WriteString(`</dl>`)

	if len(r.Violations) > 0 {
		b.WriteString(`<h2>Violations</h2><ul>`)
		for _, v := range r.Violations {
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(v))
		}
		b.WriteString(`</ul>`)
	}
	if len(r.Skipped) > 0 {
		b.WriteString(`<h2>Not checked</h2><ul>`)
		for _, s := range r.Skipped {
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(s))
		}
		b.WriteString(`</ul>`)
	}

	b.WriteString(`<h2>Scope</h2><div class="stat">`)
	for _, s := range []string{
		fmt.Sprintf("%d nodes (%d ready)", r.Scope.Nodes, r.Scope.NodesReady),
		fmt.Sprintf("%d namespaces", r.Scope.Namespaces),
		fmt.Sprintf("%d pods (%d running, %d failed)", r.Scope.Pods, r.Scope.PodsRunning, r.Scope.PodsFailed),
		fmt.Sprintf("%d deployments", r.Scope.Deployments),
		fmt.Sprintf("%d services", r.Scope.Services),
	} {
		fmt.Fprintf(&b, `<span>%s</span>`, html.EscapeString(s))
	}
	b.WriteString(`</div>`)

	fmt.Fprintf(&b, `<h2>Image vulnerabilities — %s</h2>`, html.EscapeString(r.Vulns.Scanner))
	if !r.Vulns.Installed {
		b.WriteString(`<p class="note">Not checked: no vulnerability reports found in scope.</p>`)
	} else {
		fmt.Fprintf(&b, `<div class="stat"><span>%d reports</span><span>critical %d</span><span>high %d</span><span>medium %d</span><span>low %d</span></div>`,
			r.Vulns.Reports, r.Vulns.Critical, r.Vulns.High, r.Vulns.Medium, r.Vulns.Low)
		if len(r.Vulns.Worst) > 0 {
			b.WriteString(`<div class="tablewrap"><table><thead><tr><th>Namespace</th><th>Workload</th><th>Image</th><th class="num">Critical</th><th class="num">High</th><th class="num">Medium</th><th class="num">Low</th></tr></thead><tbody>`)
			for _, v := range r.Vulns.Worst {
				fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td></tr>`,
					html.EscapeString(v.Namespace), html.EscapeString(v.Workload), html.EscapeString(v.Image),
					v.Critical, v.High, v.Medium, v.Low)
			}
			b.WriteString(`</tbody></table></div>`)
		}
	}

	fmt.Fprintf(&b, `<h2>Policy compliance — %s</h2>`, html.EscapeString(r.Policy.Engine))
	if !r.Policy.Installed {
		b.WriteString(`<p class="note">Not checked: no PolicyReports found in scope.</p>`)
	} else {
		fmt.Fprintf(&b, `<div class="stat"><span>%d reports</span><span>pass %d</span><span>fail %d</span><span>warn %d</span><span>error %d</span></div>`,
			r.Policy.Reports, r.Policy.Pass, r.Policy.Fail, r.Policy.Warn, r.Policy.Error)
		if len(r.Policy.Failures) > 0 {
			b.WriteString(`<div class="tablewrap"><table><thead><tr><th>Namespace</th><th>Policy / rule</th><th>Result</th><th>Severity</th><th>Resource</th><th>Message</th></tr></thead><tbody>`)
			for _, p := range r.Policy.Failures {
				fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
					html.EscapeString(p.Namespace), html.EscapeString(p.Policy+"/"+p.Rule),
					html.EscapeString(p.Result), html.EscapeString(p.Severity),
					html.EscapeString(p.Resource), html.EscapeString(p.Message))
			}
			b.WriteString(`</tbody></table></div>`)
		}
	}

	b.WriteString(`<h2>RBAC posture</h2>`)
	if !r.RBAC.Checked {
		b.WriteString(`<p class="note">Not checked.</p>`)
	} else {
		fmt.Fprintf(&b, `<div class="stat"><span>service account %s</span><span>cluster-admin: %v</span><span>%d denied permissions</span></div>`,
			html.EscapeString(r.RBAC.ServiceAccount), r.RBAC.IsClusterAdmin, r.RBAC.DeniedCount)
		if len(r.RBAC.DegradedFeatures) > 0 {
			fmt.Fprintf(&b, `<p>Degraded features: %s</p>`, html.EscapeString(strings.Join(r.RBAC.DegradedFeatures, ", ")))
		}
		if len(r.RBAC.Denied) > 0 {
			b.WriteString(`<ul>`)
			for _, d := range r.RBAC.Denied {
				fmt.Fprintf(&b, `<li>denied: %s</li>`, html.EscapeString(d))
			}
			b.WriteString(`</ul>`)
		}
	}

	fmt.Fprintf(&b, `<footer>Generated by %s from live cluster state. Findings reflect what the credentials used could read; sections marked "not checked" were not evaluated.</footer>`,
		html.EscapeString(strings.TrimSpace(r.Tool+" "+r.Version)))
	b.WriteString(`</main></body></html>`)
	return b.String()
}

func htmlMeta(b *strings.Builder, k, v string) {
	if v == "" {
		v = "—"
	}
	fmt.Fprintf(b, `<dt>%s</dt><dd>%s</dd>`, html.EscapeString(k), html.EscapeString(v))
}

func scopeLabel(ns string) string {
	if ns == "" {
		return "all namespaces"
	}
	return "namespace " + ns
}

// mdCell keeps a value from breaking out of a Markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "—"
	}
	return s
}
