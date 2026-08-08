// Package api wires the HTTP routes for KubeAura: a JSON API over the
// Kubernetes client and AI Assistant, plus the embedded single-page web UI.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/devganeshg/kubeaura/internal/ai"
	"github.com/devganeshg/kubeaura/internal/alertstate"
	"github.com/devganeshg/kubeaura/internal/artifactory"
	"github.com/devganeshg/kubeaura/internal/dockerfile"
	"github.com/devganeshg/kubeaura/internal/evidence"
	"github.com/devganeshg/kubeaura/internal/gitlab"
	"github.com/devganeshg/kubeaura/internal/k8s"
	"github.com/devganeshg/kubeaura/internal/rag"
	"github.com/devganeshg/kubeaura/internal/rbac"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	Mgr      *k8s.Manager // manages per-context Kubernetes clients
	AI       *ai.Assistant
	Static   http.Handler // serves the embedded web UI
	Version  string
	DocsRAG  *rag.Retriever
	DocsTopK int

	// AllowRemote relaxes the loopback-only Host check for shared in-cluster
	// deployments. It must stay false for the normal single-operator run — see
	// guard.go for what it protects against.
	AllowRemote bool

	// Integration clients (optional)
	GitLab             *gitlab.Client
	Artifactory        *artifactory.Client
	RBACValidator      *rbac.Validator
	DockerfileAnalyzer *dockerfile.Analyzer

	audit     *auditLog
	auditOnce sync.Once

	// alertTracker remembers alerts between evaluations. Lazily built so a
	// zero-value Server (tests, the fleet overview) still works.
	alertTracker *alertstate.Tracker
	alertOnce    sync.Once

	// Short per-session chat history for /api/ai/query, so follow-up
	// questions ("and why is it restarting?") keep their referent.
	histMu sync.Mutex
	aiHist map[string][]aiTurn
}

// aiTurn is one question/answer exchange kept for follow-up context.
type aiTurn struct{ q, a string }

// aud records a write action in the audit trail (lazily initializing it).
func (s *Server) aud(action, target, detail string, err error) {
	s.auditOnce.Do(func() { s.audit = newAuditLog() })
	result := "ok"
	if err != nil {
		result = "error"
		if detail == "" {
			detail = err.Error()
		}
	}
	s.audit.record(AuditEntry{Context: s.Mgr.Active(), Action: action, Target: target, Detail: detail, Result: result})
}

// alerts returns the alert-state tracker, building it on first use.
func (s *Server) alerts() *alertstate.Tracker {
	s.alertOnce.Do(func() {
		if s.alertTracker == nil {
			s.alertTracker = alertstate.New()
		}
	})
	return s.alertTracker
}

// k8s returns the Client for the currently active context.
func (s *Server) k8s() *k8s.Client { return s.Mgr.Client() }

// rbacValidator returns the RBAC validator to answer this request with. An
// injected validator wins (tests set one); otherwise it is built from the
// active context, so switching clusters re-checks permissions against the new
// one rather than reporting the previous cluster's answers.
func (s *Server) rbacValidator() *rbac.Validator {
	if s.RBACValidator != nil {
		return s.RBACValidator
	}
	cl := s.k8s()
	if cl == nil {
		return nil
	}
	return rbac.NewValidator(cl.Clientset())
}

// Routes builds the HTTP handler with all routes registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/contexts", s.handleContexts)
	mux.HandleFunc("/api/context", s.handleSwitchContext)
	mux.HandleFunc("/api/clusters", s.handleClusters) // GET ?alerts=1 — fleet overview
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/insights", s.handleInsights)
	mux.HandleFunc("/api/alerts", s.handleAlerts)
	mux.HandleFunc("/api/alerts/ack", s.handleAlertAck)
	mux.HandleFunc("/api/changes", s.handleChanges)
	mux.HandleFunc("/api/prom/status", s.handlePromStatus)
	mux.HandleFunc("/api/prom/query", s.handlePromQuery)
	mux.HandleFunc("/api/prom/history", s.handlePromHistory)
	mux.HandleFunc("/api/metrics/nodes", s.handleNodeMetrics)
	mux.HandleFunc("/api/metrics/pods", s.handlePodMetrics)
	mux.HandleFunc("/api/service/observability", s.handleServiceObs)
	mux.HandleFunc("/api/topology", s.handleTopology)
	mux.HandleFunc("/api/namespaces", s.handleNamespaces)
	mux.HandleFunc("/api/quotas", s.handleQuotas)
	mux.HandleFunc("/api/resources", s.handleResources)
	mux.HandleFunc("/api/detail", s.handleDetail)
	mux.HandleFunc("/api/cani", s.handleCanI)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/logs/stream", s.handleLogStream)
	mux.HandleFunc("/api/containers", s.handleContainers)
	mux.HandleFunc("/api/ai/logsummary", s.handleAILogSummary)
	mux.HandleFunc("/api/yaml", s.handleGetYAML)
	mux.HandleFunc("/api/apply", s.handleApply)
	mux.HandleFunc("/api/scale", s.handleScale)
	mux.HandleFunc("/api/restart", s.handleRestart)
	mux.HandleFunc("/api/delete", s.handleDelete)
	mux.HandleFunc("/api/portforward", s.handlePortForward)
	mux.HandleFunc("/api/portforward/stop", s.handlePortForwardStop)
	mux.HandleFunc("/api/exec", s.handleExec)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/diff", s.handleDiff)
	mux.HandleFunc("/api/telemetry", s.handleTelemetry)

	// AI Assistant
	mux.HandleFunc("/api/ai/status", s.handleAIStatus)
	mux.HandleFunc("/api/ai/query", s.handleAIQuery)
	mux.HandleFunc("/api/ai/query/stream", s.handleAIQueryStream)
	mux.HandleFunc("/api/ai/troubleshoot", s.handleAITroubleshoot)
	mux.HandleFunc("/api/ai/generate", s.handleAIGenerate)
	mux.HandleFunc("/api/ai/triage", s.handleAITriage)
	mux.HandleFunc("/api/ai/review", s.handleAIReview)

	// AI model connections (runtime-configurable backends). Mutating these
	// decides where cluster data is sent, so they are loopback-only — see
	// singleOperatorOnly.
	mux.HandleFunc("/api/ai/connections", s.singleOperatorOnly(s.handleConnections))           // GET list, POST add
	mux.HandleFunc("/api/ai/connections/activate", s.singleOperatorOnly(s.handleActivateConn)) // POST {id}
	mux.HandleFunc("/api/ai/connections/remove", s.singleOperatorOnly(s.handleRemoveConn))     // POST {id}
	mux.HandleFunc("/api/ai/connections/test", s.singleOperatorOnly(s.handleTestConn))         // POST {id}
	mux.HandleFunc("/api/ai/models", s.singleOperatorOnly(s.handleDiscoverModels))             // POST {provider,baseURL,apiKey}
	// CI/CD Integration
	mux.HandleFunc("/api/cicd/pipelines", s.handleListPipelines) // GET ?project=...&limit=20
	mux.HandleFunc("/api/cicd/pipeline", s.handleGetPipeline)    // GET ?project=...&id=...
	mux.HandleFunc("/api/cicd/jobs", s.handleListJobs)           // GET logs for a job
	mux.HandleFunc("/api/cicd/trigger", s.handleTriggerPipeline) // POST {project, ref, vars}

	// Container Registry
	mux.HandleFunc("/api/registry/repositories", s.handleListRepositories) // GET
	mux.HandleFunc("/api/registry/images", s.handleListImages)             // GET ?repo=...
	mux.HandleFunc("/api/registry/manifest", s.handleGetManifest)          // GET ?repo=...&tag=...

	// Helm. Reads decode Helm's own storage secrets and always work; the write
	// action runs the local helm binary, so it is loopback-only.
	mux.HandleFunc("/api/helm/releases", s.handleHelmReleases) // GET ?namespace=
	mux.HandleFunc("/api/helm/release", s.handleHelmRelease)   // GET ?namespace=&name=&revision=
	mux.HandleFunc("/api/helm/diff", s.handleHelmDiff)         // GET ?namespace=&name=&from=&to=
	mux.HandleFunc("/api/helm/action", s.sharedInstanceReadOnly(
		"Helm install, upgrade, rollback and uninstall are disabled on a shared "+
			"instance: they run the helm binary on the server, which can read charts "+
			"and values from its filesystem. Run KubeAura locally to use them.",
		s.handleHelmAction)) // POST {action, release, namespace, ...}

	// GitOps (Argo CD / Flux, detected via CRDs)
	mux.HandleFunc("/api/gitops", s.handleGitOps) // GET ?namespace=

	// Policy compliance (Kyverno/OPA PolicyReports, detected via CRDs)
	mux.HandleFunc("/api/policy", s.handlePolicy) // GET ?namespace=

	// Autoscaling (core HPAs + KEDA ScaledObjects when detected)
	mux.HandleFunc("/api/autoscaling", s.handleAutoscaling) // GET ?namespace=

	// Security & Scanning
	mux.HandleFunc("/api/security/vulnerabilities", s.handleVulnerabilities) // GET Trivy Operator reports
	mux.HandleFunc("/api/security/scan", s.handleScanImage)                  // POST {imageRef} / GET {imageRef}
	mux.HandleFunc("/api/security/scans", s.handleListScans)                 // GET paginated
	mux.HandleFunc("/api/security/compliance", s.handleCheckCompliance)      // POST {imageRef, policyName}

	// Consolidated compliance report (vulnerabilities + policy + RBAC), and its
	// exportable renderings for auditors and change tickets.
	mux.HandleFunc("/api/compliance/report", s.handleComplianceReport) // GET ?namespace=&maxCritical=&maxHigh=
	mux.HandleFunc("/api/compliance/export", s.handleComplianceExport) // GET ?format=json|csv|md|html

	// RBAC Compliance
	mux.HandleFunc("/api/rbac/compliance", s.handleRBACCompliance) // GET
	mux.HandleFunc("/api/rbac/suggest-role", s.handleSuggestRole)  // POST {features: []}
	// Web UI (catch-all)
	mux.Handle("/", s.Static)

	return guard(logging(mux), s.AllowRemote)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" && len(r.URL.Path) > 4 && r.URL.Path[:4] == "/api" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// --- resource handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"context": s.k8s().Context,
		"version": s.Version,
	})
}

func (s *Server) handleContexts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"contexts":       s.Mgr.Contexts(),
		"contextDetails": s.Mgr.ContextDetails(),
		"active":         s.Mgr.Active(),
	})
}

func (s *Server) handleSwitchContext(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Context string `json:"context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Mgr.SetActive(body.Context); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"active": s.Mgr.Active()})
}

// handleClusters returns a health snapshot of every kubeconfig context at
// once. Unreachable contexts come back as rows carrying their connection
// error, so a single expired credential does not blank the page.
func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	withAlerts := r.URL.Query().Get("alerts") == "1"
	writeJSON(w, http.StatusOK, s.Mgr.Fleet(r.Context(), withAlerts))
}

// handleLogStream streams a pod's logs live over Server-Sent Events.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.URL.Query().Get("name")
	container := r.URL.Query().Get("container")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errString("streaming unsupported"))
		return
	}
	stream, err := s.k8s().PodLogStream(r.Context(), ns, name, container, 200)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			// One SSE "data:" line per log line keeps the client parsing simple.
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if line != "" {
					_, _ = w.Write([]byte("data: " + line + "\n\n"))
				}
			}
			flusher.Flush()
		}
		if err != nil {
			return // EOF, context cancel, or pod gone
		}
	}
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	sum, err := s.k8s().Summary(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	in, err := s.k8s().Insights(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	rep, err := s.k8s().Alerts(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Rule evaluation is stateless; the tracker is what turns a list of what is
	// wrong into a queue of what to look at, and it only learns by being asked.
	tracked := s.alerts().Observe(s.Mgr.Active(), ns, rep)

	// Correlation needs the alert clocks the tracker just stamped, so it runs
	// after Observe. Skipped when the caller opts out, since it costs a Helm
	// and ReplicaSet listing on every dashboard refresh.
	if r.URL.Query().Get("correlate") != "0" {
		if changes, err := s.k8s().Changes(ns, correlationLookback); err == nil {
			k8s.CorrelateChanges(tracked.Alerts, changes, 0)
		}
	}
	writeJSON(w, http.StatusOK, tracked)
}

// correlationLookback bounds how far back the alert view reads changes. An
// alert that started 30 minutes ago cannot have been caused by something that
// has not happened yet, and reading a full day of history on every refresh is
// waste.
const correlationLookback = 6 * time.Hour

// --- Prometheus ------------------------------------------------------------

// promWindow parses the ?hours= window shared by the Prometheus handlers.
func promWindow(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("hours")
	if v == "" {
		return time.Hour, nil
	}
	h, err := strconv.Atoi(v)
	if err != nil || h <= 0 || h > 720 {
		return 0, errString("hours must be between 1 and 720")
	}
	return time.Duration(h) * time.Hour, nil
}

// handlePromStatus reports whether this cluster has a Prometheus KubeAura can
// query, so the UI can hide trend charts rather than showing empty ones — the
// same thing it already does for metrics-server.
func (s *Server) handlePromStatus(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.k8s().PrometheusRef()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"available": false})
		return
	}
	// Discovery only proves a Service exists. Whether it answers is a different
	// question, and the one the UI actually needs answered.
	cx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.k8s().PromQuery(cx, "up"); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"available": false,
			"service":   ref.Namespace + "/" + ref.Service,
			"error":     err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"available": true,
		"service":   ref.Namespace + "/" + ref.Service,
		"url":       ref.URL,
	})
}

// handlePromQuery runs an operator-supplied PromQL query. Instant by default;
// ?range=1 turns it into a range query over ?hours=.
func (s *Server) handlePromQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeErr(w, http.StatusBadRequest, errString("q is required"))
		return
	}
	window, err := promWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var res *k8s.PromResult
	if r.URL.Query().Get("range") == "1" {
		res, err = s.k8s().PromRange(cx, q, window, 0)
	} else {
		res, err = s.k8s().PromQuery(cx, q)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handlePromHistory serves the curated series, so the common questions do not
// require the operator to write PromQL.
func (s *Server) handlePromHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := k8s.PromHistoryKind(q.Get("kind"))
	if kind == "" {
		writeErr(w, http.StatusBadRequest, errString("kind is required (pod-cpu, pod-memory, node-cpu, node-memory, restarts)"))
		return
	}
	window, err := promWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.k8s().PromHistory(cx, kind, q.Get("namespace"), q.Get("name"), window)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleChanges is the "what changed?" timeline: deploys, rollouts, syncs and
// node joins, newest first.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	window := defaultChangeWindow
	if v := r.URL.Query().Get("hours"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h <= 0 || h > 720 {
			writeErr(w, http.StatusBadRequest, errString("hours must be between 1 and 720"))
			return
		}
		window = time.Duration(h) * time.Hour
	}
	changes, err := s.k8s().Changes(ns, window)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"changes": changes,
		"window":  window.String(),
	})
}

// defaultChangeWindow matches the client default so the UI and a bare curl see
// the same timeline.
const defaultChangeWindow = 24 * time.Hour

// handleAlertAck acknowledges an alert so it sinks to the bottom of the queue.
// The acknowledgement is dropped automatically if the alert resolves, so a
// recurrence is surfaced again rather than staying silently triaged.
func (s *Server) handleAlertAck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Fingerprint string `json:"fingerprint"`
		Note        string `json:"note"`
		Minutes     int    `json:"minutes"` // 0 = until the alert resolves
		Undo        bool   `json:"undo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Fingerprint == "" {
		writeErr(w, http.StatusBadRequest, errString("fingerprint is required"))
		return
	}
	cluster := s.Mgr.Active()
	if body.Undo {
		s.alerts().Unack(cluster, body.Fingerprint)
		s.aud("alert.unack", body.Fingerprint, "", nil)
		writeJSON(w, http.StatusOK, map[string]bool{"acked": false})
		return
	}
	var until time.Time
	detail := body.Note
	if body.Minutes > 0 {
		until = time.Now().Add(time.Duration(body.Minutes) * time.Minute)
		detail = fmt.Sprintf("%s (for %dm)", body.Note, body.Minutes)
	}
	s.alerts().Ack(cluster, body.Fingerprint, body.Note, until)
	s.aud("alert.ack", body.Fingerprint, detail, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"acked": true})
}

func (s *Server) handleServiceObs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ns, name := q.Get("namespace"), q.Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, errString("name is required"))
		return
	}
	obs, err := s.k8s().ServiceObservability(ns, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, obs)
}

func (s *Server) handleNodeMetrics(w http.ResponseWriter, r *http.Request) {
	nm, err := s.k8s().NodeMetrics()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"available": s.k8s().MetricsAvailable(),
		"nodes":     nm,
	})
}

func (s *Server) handlePodMetrics(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	pm, err := s.k8s().PodMetrics(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pods": pm})
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, ns, name := q.Get("kind"), q.Get("namespace"), q.Get("name")
	if kind == "" || name == "" {
		writeErr(w, http.StatusBadRequest, errString("kind and name are required"))
		return
	}
	view, err := s.k8s().ResourceDetail(kind, ns, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleCanI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Checks []k8s.AccessCheck `json:"checks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	results, err := s.k8s().CanI(body.Checks)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	// POST creates a namespace; GET lists them.
	if r.Method == http.MethodPost {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if msgs := validation.IsDNS1123Label(body.Name); len(msgs) > 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid namespace name %q: %s", body.Name, strings.Join(msgs, "; ")))
			return
		}
		err := s.k8s().CreateNamespace(body.Name)
		s.aud("create", "Namespace/"+body.Name, "", err)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"created": body.Name})
		return
	}
	nss, err := s.k8s().Namespaces()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, nss)
}

// handleAutoscaling serves HPA status (always available) plus KEDA
// ScaledObjects when the CRD exists.
func (s *Server) handleAutoscaling(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	res, err := s.k8s().Autoscaling(r.Context(), ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handlePolicy serves Kyverno/OPA PolicyReports; no policy engine installed
// is a valid state ({installed:false}), not an error.
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	res, err := s.k8s().PolicyReports(r.Context(), ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleGitOps serves Argo CD / Flux app sync status; both absent is a valid
// state ({argoInstalled:false, fluxInstalled:false}), not an error.
func (s *Server) handleGitOps(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	res, err := s.k8s().GitOpsStatus(r.Context(), ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleVulnerabilities serves Trivy Operator VulnerabilityReports when the
// operator is installed; {installed:false} otherwise (absence is a state, not
// an error).
func (s *Server) handleVulnerabilities(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	res, err := s.k8s().VulnerabilityReports(r.Context(), ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleQuotas(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	rows, err := s.k8s().NamespaceQuotas(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": rows})
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	ns := q.Get("namespace")
	if kind == "" {
		writeErr(w, http.StatusBadRequest, errString("kind is required"))
		return
	}
	// Server-side pagination: default page of 500, overridable via ?limit=,
	// with ?continue= carrying the cursor from a prior page.
	limit := int64(500)
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.ParseInt(l, 10, 64); err == nil && n > 0 {
			limit = n
		}
	}
	res, err := s.k8s().List(kind, ns, k8s.ListParams{Limit: limit, Continue: q.Get("continue")})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.URL.Query().Get("name")
	container := r.URL.Query().Get("container")
	tail := int64(200)
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			tail = n
		}
	}
	logs, err := s.k8s().PodLogs(ns, name, container, tail)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"logs": logs})
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.URL.Query().Get("name")
	cs, err := s.k8s().PodContainers(ns, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"containers": cs})
}

func (s *Server) handleAILogSummary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Container string `json:"container"`
		// Preview builds the evidence and returns its envelope without calling
		// the model, so an operator can see what would leave the machine first.
		Preview bool `json:"preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	const linesAsked = 400
	logs, err := s.k8s().PodLogs(body.Namespace, body.Name, body.Container, linesAsked)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	payload, err := evidence.ForLogs(evidence.LogInput{
		Namespace:  body.Namespace,
		Pod:        body.Name,
		Container:  body.Container,
		Logs:       logs,
		LinesAsked: linesAsked,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.describeDestination(&payload.Envelope)

	target := body.Namespace + "/" + body.Name
	if body.Preview {
		writeJSON(w, http.StatusOK, map[string]interface{}{"evidence": payload.Envelope})
		return
	}

	answer, err := s.AI.SummarizeLogs(r.Context(), payload.JSON)
	s.aud("ai.logsummary", "Pod/"+target, evidenceDetail(payload.Envelope), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"answer":   answer,
		"evidence": payload.Envelope,
	})
}

// describeDestination stamps the envelope with the backend that will receive it.
// This is the half of the disclosure an operator actually acts on: the same
// evidence is a different decision depending on whether it stays on the machine.
func (s *Server) describeDestination(env *evidence.Envelope) {
	if s.AI == nil {
		return
	}
	env.Provider = s.AI.ProviderName()
	env.Model = s.AI.ModelName()
	env.Local = s.AI.OnMachine()
}

// evidenceDetail is the audit-trail line for one model call. It records the
// hash and the size, never the evidence: the point is that a diagnosis can be
// tied back to its inputs without keeping those inputs anywhere.
func evidenceDetail(env evidence.Envelope) string {
	return fmt.Sprintf("evidence %s, %d bytes → %s",
		evidence.Short(env.Hash), env.Bytes, destinationName(env))
}

// destinationName renders the backend half of an audit line.
func destinationName(env evidence.Envelope) string {
	where := env.Provider
	if where == "" {
		where = "unknown backend"
	}
	if env.Local {
		where += " (on this machine)"
	}
	return where
}

func (s *Server) handleGetYAML(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	ns := r.URL.Query().Get("namespace")
	name := r.URL.Query().Get("name")
	y, err := s.k8s().GetYAML(kind, ns, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": y})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	results, err := s.k8s().ApplyYAML(body.YAML)
	s.aud("apply", "yaml", strings.Join(results, "; "), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Replicas  int32  `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	err := s.k8s().ScaleDeployment(body.Namespace, body.Name, body.Replicas)
	s.aud("scale", "Deployment/"+body.Namespace+"/"+body.Name, fmt.Sprintf("replicas=%d", body.Replicas), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "scaled"})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	err := s.k8s().RestartDeployment(body.Namespace, body.Name)
	s.aud("restart", "Deployment/"+body.Namespace+"/"+body.Name, "rolling restart", err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	err := s.k8s().DeleteResource(body.Kind, body.Namespace, body.Name)
	s.aud("delete", body.Kind+"/"+body.Namespace+"/"+body.Name, "", err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePortForward(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Namespace  string `json:"namespace"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			LocalPort  uint16 `json:"localPort"`
			RemotePort uint16 `json:"remotePort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.RemotePort == 0 {
			writeErr(w, http.StatusBadRequest, errString("remotePort is required"))
			return
		}
		pf, err := s.k8s().StartPortForward(body.Namespace, body.Kind, body.Name, body.LocalPort, body.RemotePort)
		detail := ""
		if pf != nil {
			detail = fmt.Sprintf("localhost:%d -> %d", pf.LocalPort, pf.RemotePort)
		}
		s.aud("port-forward", body.Kind+"/"+body.Namespace+"/"+body.Name, detail, err)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, pf)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"forwards": s.k8s().PortForwards()})
}

func (s *Server) handlePortForwardStop(w http.ResponseWriter, r *http.Request) {
	id := decodeID(w, r)
	if id == "" {
		return
	}
	if err := s.k8s().StopPortForward(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Container string `json:"container"`
		Command   string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	argv := k8s.ParseCommand(body.Command)
	if len(argv) == 0 {
		writeErr(w, http.StatusBadRequest, errString("command is required"))
		return
	}
	out, err := s.k8s().ExecCommand(r.Context(), body.Namespace, body.Name, body.Container, argv)
	s.aud("exec", "Pod/"+body.Namespace+"/"+body.Name, body.Command, err)
	// Return output even on non-zero exit so the user sees stderr.
	resp := map[string]string{"output": out}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	docs, err := s.k8s().DiffYAML(body.YAML)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"docs": docs})
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	t, err := s.k8s().DiscoverTelemetry()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	s.auditOnce.Do(func() { s.audit = newAuditLog() })
	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": s.audit.list()})
}

// --- AI handlers ---

func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":  s.AI.Enabled(),
		"provider": s.AI.ProviderName(),
		"model":    s.AI.ModelName(),
	})
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var conn ai.Connection
		if err := json.NewDecoder(r.Body).Decode(&conn); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		saved, err := s.AI.AddConnection(conn)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
		return
	}
	conns, active := s.AI.Connections()
	writeJSON(w, http.StatusOK, map[string]interface{}{"connections": conns, "active": active})
}

func (s *Server) handleActivateConn(w http.ResponseWriter, r *http.Request) {
	id := decodeID(w, r)
	if id == "" {
		return
	}
	if err := s.AI.Activate(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"active": id})
}

func (s *Server) handleRemoveConn(w http.ResponseWriter, r *http.Request) {
	id := decodeID(w, r)
	if id == "" {
		return
	}
	if err := s.AI.RemoveConnection(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handleTestConn(w http.ResponseWriter, r *http.Request) {
	id := decodeID(w, r)
	if id == "" {
		return
	}
	if err := s.AI.TestConnection(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDiscoverModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"baseURL"`
		APIKey   string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	models, err := ai.DiscoverModels(r.Context(), body.Provider, body.BaseURL, body.APIKey)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": models})
}

// decodeID reads {"id": "..."} from the body, writing a 400 and returning "" on
// failure so callers can early-return.
func decodeID(w http.ResponseWriter, r *http.Request) string {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeErr(w, http.StatusBadRequest, errString("id is required"))
		return ""
	}
	return body.ID
}

func (s *Server) handleAIQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Question  string `json:"question"`
		Namespace string `json:"namespace"`
		Session   string `json:"session"`
		Preview   bool   `json:"preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	payload, err := s.snapshot("query", body.Namespace)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.describeDestination(&payload.Envelope)
	if body.Preview {
		writeJSON(w, http.StatusOK, map[string]interface{}{"evidence": payload.Envelope})
		return
	}
	docCtx := ""
	if s.DocsRAG != nil {
		docCtx = s.DocsRAG.ContextForPrompt(body.Question, s.DocsTopK)
	}
	question := s.foldHistory(body.Session, body.Question)
	answer, err := s.AI.QueryWithDocs(r.Context(), question, payload.JSON, docCtx)
	s.aud("ai.query", nsRef(body.Namespace), evidenceDetail(payload.Envelope), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.chatRemember(body.Session, body.Question, answer)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"answer":   answer,
		"objects":  s.sourceObjects(body.Namespace, body.Question, answer),
		"evidence": payload.Envelope,
	})
}

// foldHistory folds recent session turns into the question so follow-ups
// resolve against what was just discussed, without changing the
// provider-facing API.
func (s *Server) foldHistory(session, question string) string {
	hist := s.chatHistory(session)
	if len(hist) == 0 {
		return question
	}
	var b strings.Builder
	b.WriteString("CONVERSATION SO FAR (for context on follow-up questions):\n")
	for _, t := range hist {
		fmt.Fprintf(&b, "User: %s\nAssistant: %s\n", t.q, t.a)
	}
	b.WriteString("\nCURRENT QUESTION: ")
	b.WriteString(question)
	return b.String()
}

// handleAIQueryStream is handleAIQuery with incremental delivery. It answers
// with NDJSON (one JSON object per line, flushed as produced):
//
//	{"type":"stage","stage":"snapshot","label":"..."}   pipeline progress
//	{"type":"token","text":"..."}                       model output chunk
//	{"type":"done","answer":"...","objects":[...]}      final result
//	{"type":"error","error":"..."}                      terminal failure
//
// The UI renders the stages as a reasoning trace, streams tokens into the
// chat bubble, and treats "done" as the authoritative answer.
func (s *Server) handleAIQueryStream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Question  string `json:"question"`
		Namespace string `json:"namespace"`
		Session   string `json:"session"`
		Preview   bool   `json:"preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	emit := func(v interface{}) {
		if buf, err := json.Marshal(v); err == nil {
			w.Write(buf)
			w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
	fail := func(err error) {
		emit(map[string]string{"type": "error", "error": err.Error()})
	}

	emit(map[string]string{"type": "stage", "stage": "snapshot", "label": "Scanning live cluster state"})
	payload, err := s.snapshot("query", body.Namespace)
	if err != nil {
		fail(err)
		return
	}
	s.describeDestination(&payload.Envelope)
	// The envelope reaches the client before the first token, so the operator
	// can see what is leaving while the answer is still being written.
	emit(map[string]interface{}{"type": "evidence", "evidence": payload.Envelope})
	docCtx := ""
	if s.DocsRAG != nil {
		emit(map[string]string{"type": "stage", "stage": "docs", "label": "Searching platform docs"})
		docCtx = s.DocsRAG.ContextForPrompt(body.Question, s.DocsTopK)
	}
	emit(map[string]string{"type": "stage", "stage": "model", "label": "Reasoning with " + s.AI.ModelName()})

	question := s.foldHistory(body.Session, body.Question)
	answer, err := s.AI.QueryStreamWithDocs(r.Context(), question, payload.JSON, docCtx, func(delta string) {
		emit(map[string]string{"type": "token", "text": delta})
	})
	s.aud("ai.query", nsRef(body.Namespace), evidenceDetail(payload.Envelope), err)
	if err != nil {
		fail(err)
		return
	}
	s.chatRemember(body.Session, body.Question, answer)
	emit(map[string]interface{}{
		"type":    "done",
		"answer":  answer,
		"objects": s.sourceObjects(body.Namespace, body.Question, answer),
	})
}

// chatHistory returns the remembered turns for a session ("" is a valid
// shared session for clients that don't send one).
func (s *Server) chatHistory(session string) []aiTurn {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	return s.aiHist[session]
}

// chatRemember appends a turn, keeping histories and the session table small.
func (s *Server) chatRemember(session, q, a string) {
	const maxTurns, maxSessions = 6, 64
	s.histMu.Lock()
	defer s.histMu.Unlock()
	if s.aiHist == nil {
		s.aiHist = map[string][]aiTurn{}
	}
	if len(s.aiHist) >= maxSessions {
		if _, ok := s.aiHist[session]; !ok {
			s.aiHist = map[string][]aiTurn{}
		}
	}
	h := append(s.aiHist[session], aiTurn{q: q, a: a})
	if len(h) > maxTurns {
		h = h[len(h)-maxTurns:]
	}
	s.aiHist[session] = h
}

// sourceObjects ranks the cluster objects an answer likely drew on by
// matching object names against the question and answer text. Exact name
// matches weigh extra; an empty result means the exchange wasn't about
// specific objects (small talk, general questions).
func (s *Server) sourceObjects(namespace, question, answer string) []map[string]string {
	type cand struct {
		kind, ns, name string
		score          int
	}
	ql, al := strings.ToLower(question), strings.ToLower(answer)
	// Words from the question worth fuzzy-matching against object names
	// ("payments" should find payments-7d9fb...).
	qWords := []string{}
	for _, w := range strings.FieldsFunc(ql, func(r rune) bool {
		return !(r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		if len(w) >= 4 {
			qWords = append(qWords, w)
		}
	}
	seen := map[string]bool{}
	var cands []cand
	add := func(kind, ns, name string) {
		key := kind + "/" + ns + "/" + name
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		nl := strings.ToLower(name)
		score := 0
		if strings.Contains(al, nl) {
			score += 3
		}
		if strings.Contains(ql, nl) {
			score += 2
		}
		if score == 0 {
			for _, w := range qWords {
				if strings.Contains(nl, w) {
					score++
					break
				}
			}
		}
		if score > 0 {
			cands = append(cands, cand{kind: kind, ns: ns, name: name, score: score})
		}
	}
	if t, err := s.k8s().Topology(namespace); err == nil {
		for _, n := range t.Nodes {
			add(n.Kind, t.Namespace, n.Name)
		}
	}
	if res, err := s.k8s().List("nodes", "", k8s.ListParams{Limit: 100}); err == nil {
		for _, it := range res.Items {
			add("Node", "", it.Name)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > 6 {
		cands = cands[:6]
	}
	objs := []map[string]string{}
	for _, c := range cands {
		objs = append(objs, map[string]string{"kind": c.kind, "namespace": c.ns, "name": c.name})
	}
	return objs
}

func (s *Server) handleAITroubleshoot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Preview   bool   `json:"preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	detail, err := s.k8s().PodDetail(body.Namespace, body.Name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// The whole PodDetail used to be marshalled and sent. It carries inline env
	// values, the last-applied-configuration annotation and uncapped events, so
	// it now goes through the redaction layer first and nothing else may be sent.
	payload, err := evidence.ForPod(evidence.PodInput{
		Pod:      detail.Pod,
		Events:   detail.Events,
		Logs:     detail.Logs,
		LogLines: 100, // what PodDetail asks the API server for
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.describeDestination(&payload.Envelope)

	if body.Preview {
		writeJSON(w, http.StatusOK, map[string]interface{}{"evidence": payload.Envelope})
		return
	}

	answer, err := s.AI.Troubleshoot(r.Context(), payload.JSON)
	s.aud("ai.troubleshoot", "Pod/"+body.Namespace+"/"+body.Name, evidenceDetail(payload.Envelope), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"answer":   answer,
		"evidence": payload.Envelope,
	})
}

func (s *Server) handleAIGenerate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// No redaction here on purpose: the payload is the description the operator
	// typed, and nothing is read from the cluster. It still gets an audit line,
	// so the trail accounts for every call that leaves the machine.
	yaml, err := s.AI.Generate(r.Context(), body.Description)
	s.aud("ai.generate", "manifest", s.destinationDetail(len(body.Description)), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": yaml})
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	t, err := s.k8s().Topology(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Optional AI explanation when ?explain=1.
	if r.URL.Query().Get("explain") == "1" {
		nodes, links := topologyItems(t)
		payload, err := evidence.ForSnapshot(evidence.SnapshotInput{
			Purpose:   "topology",
			Namespace: ns,
			Summary:   map[string]interface{}{"nodes": len(t.Nodes), "edges": len(t.Edges)},
			Groups: []evidence.SnapshotGroup{
				{Kind: "graph", Items: nodes},
				{Kind: "links", Items: links},
			},
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		s.describeDestination(&payload.Envelope)
		answer, err := s.AI.ExplainTopology(r.Context(), payload.JSON)
		s.aud("ai.topology", nsRef(ns), evidenceDetail(payload.Envelope), err)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"topology": t, "explain": answer, "evidence": payload.Envelope,
		})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleAITriage(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	rep, err := s.k8s().Alerts(ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	sum, _ := s.k8s().Summary(ns)
	// Alert rows are built from event messages, which are the least trustworthy
	// free text in the cluster — scrub and cap them like any other evidence.
	payload, err := evidence.ForSnapshot(evidence.SnapshotInput{
		Purpose:   "triage",
		Namespace: ns,
		Summary:   sum,
		Groups:    []evidence.SnapshotGroup{{Kind: "alerts", Items: alertItems(rep)}},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.describeDestination(&payload.Envelope)
	if r.URL.Query().Get("preview") == "1" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"evidence": payload.Envelope})
		return
	}
	answer, err := s.AI.Triage(r.Context(), payload.JSON)
	s.aud("ai.triage", nsRef(ns), evidenceDetail(payload.Envelope), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"answer": answer, "counts": rep, "evidence": payload.Envelope,
	})
}

func (s *Server) handleAIReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		YAML      string `json:"yaml"`
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Preview   bool   `json:"preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	yaml := body.YAML
	// If no YAML supplied, fetch the live object's manifest to review.
	if strings.TrimSpace(yaml) == "" {
		if body.Name == "" {
			writeErr(w, http.StatusBadRequest, errString("provide yaml, or kind+name to review a live object"))
			return
		}
		y, err := s.k8s().GetYAML(body.Kind, body.Namespace, body.Name)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		yaml = y
	}
	// GetYAML strips only server noise, so reviewing a Secret used to post its
	// entire data block to the model provider. A pasted manifest takes the same
	// path, for the same reason.
	payload, err := evidence.ForManifest(evidence.ManifestInput{
		YAML: yaml, Kind: body.Kind, Namespace: body.Namespace, Name: body.Name,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.describeDestination(&payload.Envelope)
	if body.Preview {
		writeJSON(w, http.StatusOK, map[string]interface{}{"evidence": payload.Envelope})
		return
	}
	answer, err := s.AI.Review(r.Context(), payload.JSON)
	s.aud("ai.review", objRef(body.Kind, body.Namespace, body.Name), evidenceDetail(payload.Envelope), err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"answer": answer, "evidence": payload.Envelope,
	})
}

// snapshot builds a compact JSON view of the cluster for grounding AI queries.
// nsRef and objRef name the subject of an audit line. A model call is not
// scoped to one object the way a scale or a delete is, so the reference says
// which slice of the cluster the evidence came from.
func nsRef(ns string) string {
	if ns == "" {
		return "cluster (all namespaces)"
	}
	return "namespace/" + ns
}

func objRef(kind, ns, name string) string {
	if name == "" {
		return "pasted manifest"
	}
	if kind == "" {
		kind = "object"
	}
	if ns == "" {
		return kind + "/" + name
	}
	return kind + "/" + ns + "/" + name
}

// destinationDetail is the audit line for a call that carries no cluster
// evidence: there is no hash to record, but where the bytes went and how many
// there were still belongs in the trail.
func (s *Server) destinationDetail(n int) string {
	var env evidence.Envelope
	s.describeDestination(&env)
	return fmt.Sprintf("no cluster evidence, %d bytes → %s", n, destinationName(env))
}

// alertItems maps an alert report into evidence rows. Title and Detail are
// derived from event messages, so they land in Info where the scrubbers run.
func alertItems(rep *k8s.AlertReport) []evidence.SnapshotItem {
	if rep == nil {
		return nil
	}
	out := make([]evidence.SnapshotItem, 0, len(rep.Alerts))
	for _, a := range rep.Alerts {
		out = append(out, evidence.SnapshotItem{
			Kind:      a.Kind,
			Name:      a.Name,
			Namespace: a.Namespace,
			Status:    a.Severity,
			Info:      a.Category + ": " + a.Title + " — " + a.Detail,
			Age:       a.Age,
		})
	}
	return out
}

// topologyItems flattens the graph into evidence rows: one group for the nodes,
// one for the edges. The edges are what make the graph explainable, so they are
// carried as rows of their own rather than folded into the node summaries,
// where the byte cap would drop them first.
func topologyItems(t *k8s.Topology) (nodes, links []evidence.SnapshotItem) {
	for _, n := range t.Nodes {
		nodes = append(nodes, evidence.SnapshotItem{
			Kind: n.Kind, Name: n.Name, Namespace: t.Namespace,
			Status: n.Status, Info: n.Info,
		})
	}
	for _, e := range t.Edges {
		links = append(links, evidence.SnapshotItem{
			Kind: "Edge", Name: e.From + " → " + e.To, Namespace: t.Namespace, Info: e.Kind,
		})
	}
	return nodes, links
}

// snapshot builds the redacted cluster payload that grounds a model call.
//
// The rows are already flat — k8s.List returns a summary row, not the object —
// so nothing here carries a pod spec or a Secret body. What it does carry is
// free text the cluster wrote: an event message is verbatim controller output
// and routinely quotes a connection string. That, and the absence of any
// ceiling on 5 kinds x 200 rows, is why this goes through evidence like every
// other model call.
func (s *Server) snapshot(purpose, namespace string) (*evidence.Payload, error) {
	sum, err := s.k8s().Summary(namespace)
	if err != nil {
		return nil, err
	}
	in := evidence.SnapshotInput{Purpose: purpose, Namespace: namespace, Summary: sum}
	// Ordered least-droppable first: trimming halves the largest group, and the
	// node/deployment lists are both small and load-bearing for a diagnosis.
	// Ignore per-kind errors so a missing API group (e.g. no ingress
	// controller) doesn't fail the whole query.
	for _, kind := range []string{"nodes", "deployments", "services", "pods", "events"} {
		res, err := s.k8s().List(kind, namespace, k8s.ListParams{Limit: 200})
		if err != nil {
			continue
		}
		in.Groups = append(in.Groups, evidence.SnapshotGroup{Kind: kind, Items: snapshotItems(res.Items)})
	}
	return evidence.ForSnapshot(in)
}

// snapshotItems maps cluster rows into the evidence package's own shape. The
// mapping lives here rather than in evidence so that package stays free of a
// client-go dependency and its tests stay fast.
func snapshotItems(items []k8s.Resource) []evidence.SnapshotItem {
	out := make([]evidence.SnapshotItem, 0, len(items))
	for _, it := range items {
		out = append(out, evidence.SnapshotItem{
			Kind: it.Kind, Name: it.Name, Namespace: it.Namespace,
			Status: it.Status, Info: it.Info, Age: it.Age, Labels: it.Labels,
		})
	}
	return out
}

type errString string

func (e errString) Error() string { return string(e) }
