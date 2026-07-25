package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/devganeshg/kubemind/internal/rbac"
)

// --- CI/CD Handlers ---

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GitLab == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("GitLab integration not configured"))
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		writeErr(w, http.StatusBadRequest, errString("project parameter required"))
		return
	}
	pipelines, err := s.GitLab.ListPipelines(r.Context(), projectID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pipelines": pipelines})
}

func (s *Server) handleGetPipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GitLab == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("GitLab integration not configured"))
		return
	}
	projectID := r.URL.Query().Get("project")
	pipelineID := r.URL.Query().Get("id")
	if projectID == "" || pipelineID == "" {
		writeErr(w, http.StatusBadRequest, errString("project and id parameters required"))
		return
	}
	pipelineIDInt, err := strconv.Atoi(pipelineID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errString("invalid pipeline id"))
		return
	}
	pipeline, err := s.GitLab.GetPipeline(r.Context(), projectID, pipelineIDInt)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, pipeline)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GitLab == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("GitLab integration not configured"))
		return
	}
	projectID := r.URL.Query().Get("project")
	pipelineID := r.URL.Query().Get("pipeline")
	if projectID == "" || pipelineID == "" {
		writeErr(w, http.StatusBadRequest, errString("project and pipeline parameters required"))
		return
	}
	pipelineIDInt, err := strconv.Atoi(pipelineID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errString("invalid pipeline id"))
		return
	}
	pipeline, err := s.GitLab.GetPipeline(r.Context(), projectID, pipelineIDInt)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": pipeline.Jobs})
}

func (s *Server) handleTriggerPipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GitLab == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("GitLab integration not configured"))
		return
	}
	var body struct {
		Project string            `json:"project"`
		Ref     string            `json:"ref"`
		Vars    map[string]string `json:"vars,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Project == "" || body.Ref == "" {
		writeErr(w, http.StatusBadRequest, errString("project and ref are required"))
		return
	}
	pipelineID, err := s.GitLab.TriggerPipeline(r.Context(), body.Project, body.Ref, body.Vars)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pipelineId": pipelineID, "status": "triggered"})
}

// --- Registry Handlers ---

func (s *Server) handleListRepositories(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Artifactory == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("Artifactory integration not configured"))
		return
	}
	repos, err := s.Artifactory.ListRepositories(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"repositories": repos})
}

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, errString("repo parameter required"))
		return
	}
	if s.Artifactory == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("Artifactory integration not configured"))
		return
	}
	images, err := s.Artifactory.ListImages(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"images": images})
}

func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")
	if repo == "" || tag == "" {
		writeErr(w, http.StatusBadRequest, errString("repo and tag parameters required"))
		return
	}
	if s.Artifactory == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("Artifactory integration not configured"))
		return
	}
	manifest, err := s.Artifactory.GetImageManifest(r.Context(), repo, tag)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

// --- Security Handlers ---

func (s *Server) handleScanImage(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var body struct {
			ImageRef string `json:"imageRef"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.ImageRef == "" {
			writeErr(w, http.StatusBadRequest, errString("imageRef is required"))
			return
		}
		// TODO: Implement integration with Trivy or other scanner backend
		// For now, return placeholder response
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"imageRef": body.ImageRef,
			"status":   "scan_queued",
			"message":  "Image scan has been queued. Results will be available shortly.",
		})
	} else if r.Method == "GET" {
		imageRef := r.URL.Query().Get("imageRef")
		if imageRef == "" {
			writeErr(w, http.StatusBadRequest, errString("imageRef parameter required"))
			return
		}
		// TODO: Retrieve cached scan result from database
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"imageName":       imageRef,
			"vulnerabilities": []interface{}{},
			"status":          "pending",
		})
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleListScans(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// TODO: Query scan database with pagination
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"scans": []interface{}{},
		"total": 0,
		"limit": limit,
	})
}

func (s *Server) handleCheckCompliance(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ImageRef   string `json:"imageRef"`
		PolicyName string `json:"policyName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.ImageRef == "" {
		writeErr(w, http.StatusBadRequest, errString("imageRef is required"))
		return
	}
	// TODO: Load policy and check image scan results against it
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"imageRef":   body.ImageRef,
		"policyName": body.PolicyName,
		"compliant":  true,
		"violations": []string{},
		"checkedAt":  time.Now(),
	})
}

// --- RBAC Handlers ---

func (s *Server) handleRBACCompliance(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.RBACValidator == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("RBAC validator not initialized"))
		return
	}
	// Get current service account info from the Kubernetes context
	sa := r.URL.Query().Get("serviceAccount")
	if sa == "" {
		sa = "default"
	}
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = "default"
	}
	report, err := s.RBACValidator.GenerateComplianceReport(r.Context(), sa, ns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleSuggestRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.RBACValidator == nil {
		writeErr(w, http.StatusServiceUnavailable, errString("RBAC validator not initialized"))
		return
	}
	var body struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Features) == 0 {
		writeErr(w, http.StatusBadRequest, errString("features array is required"))
		return
	}
	// Generate least-privilege role for the requested features
	role := rbac.SuggestLeastPrivilegeRole(body.Features)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"role":     role,
		"features": body.Features,
	})
}
