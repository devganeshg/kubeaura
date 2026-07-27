// Package api wires the HTTP routes for KubeAura: a JSON API over the
// Kubernetes client and AI Assistant, plus the embedded single-page web UI.
package api

import (
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
	"github.com/devganeshg/kubeaura/internal/artifactory"
	"github.com/devganeshg/kubeaura/internal/dockerfile"
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
	writeJSON(w, http.StatusOK, rep)
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	logs, err := s.k8s().PodLogs(body.Namespace, body.Name, body.Container, 400)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	answer, err := s.AI.SummarizeLogs(r.Context(), logs)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	snapshot, err := s.snapshot(body.Namespace)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	docCtx := ""
	if s.DocsRAG != nil {
		docCtx = s.DocsRAG.ContextForPrompt(body.Question, s.DocsTopK)
	}
	question := s.foldHistory(body.Session, body.Question)
	answer, err := s.AI.QueryWithDocs(r.Context(), question, snapshot, docCtx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.chatRemember(body.Session, body.Question, answer)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"answer":  answer,
		"objects": s.sourceObjects(body.Namespace, body.Question, answer),
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
	snapshot, err := s.snapshot(body.Namespace)
	if err != nil {
		fail(err)
		return
	}
	docCtx := ""
	if s.DocsRAG != nil {
		emit(map[string]string{"type": "stage", "stage": "docs", "label": "Searching platform docs"})
		docCtx = s.DocsRAG.ContextForPrompt(body.Question, s.DocsTopK)
	}
	emit(map[string]string{"type": "stage", "stage": "model", "label": "Reasoning with " + s.AI.ModelName()})

	question := s.foldHistory(body.Session, body.Question)
	answer, err := s.AI.QueryStreamWithDocs(r.Context(), question, snapshot, docCtx, func(delta string) {
		emit(map[string]string{"type": "token", "text": delta})
	})
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
	dj, _ := json.Marshal(detail)
	answer, err := s.AI.Troubleshoot(r.Context(), string(dj))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
}

func (s *Server) handleAIGenerate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	yaml, err := s.AI.Generate(r.Context(), body.Description)
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
		tj, _ := json.Marshal(t)
		answer, err := s.AI.ExplainTopology(r.Context(), string(tj))
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"topology": t, "explain": answer})
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
	payload, _ := json.Marshal(map[string]interface{}{"summary": sum, "alerts": rep})
	answer, err := s.AI.Triage(r.Context(), string(payload))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"answer": answer, "counts": rep})
}

func (s *Server) handleAIReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		YAML      string `json:"yaml"`
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
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
	answer, err := s.AI.Review(r.Context(), yaml)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
}

// snapshot builds a compact JSON view of the cluster for grounding AI queries.
func (s *Server) snapshot(namespace string) (string, error) {
	sum, err := s.k8s().Summary(namespace)
	if err != nil {
		return "", err
	}
	snap := map[string]interface{}{"summary": sum}
	// Include the most useful resource lists; ignore per-kind errors so a
	// missing API group (e.g. no ingress controller) doesn't fail the query.
	// Cap each list so the AI snapshot stays within a sane token budget.
	for _, kind := range []string{"pods", "deployments", "services", "nodes", "events"} {
		if res, err := s.k8s().List(kind, namespace, k8s.ListParams{Limit: 200}); err == nil {
			snap[kind] = res.Items
		}
	}
	b, err := json.Marshal(snap)
	return string(b), err
}

type errString string

func (e errString) Error() string { return string(e) }
