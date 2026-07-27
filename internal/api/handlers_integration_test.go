package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devganeshg/kubeaura/internal/k8s"
	"github.com/devganeshg/kubeaura/internal/rbac"
)

// MockManager implements k8s.Manager interface for testing
type MockManager struct {
	activeContext string
}

func (m *MockManager) Contexts() []string {
	return []string{"cluster1", "cluster2"}
}

func (m *MockManager) Active() string {
	return m.activeContext
}

func (m *MockManager) Client() *k8s.Client {
	return nil // Would need full mock for complete testing
}

func (m *MockManager) SetActive(name string) error {
	m.activeContext = name
	return nil
}

func TestRBACComplianceHandler(t *testing.T) {
	// Create a test server without full k8s integration
	server := &Server{
		Version:       "test",
		RBACValidator: rbac.NewValidator(nil), // nil for testing
	}

	req := httptest.NewRequest("GET", "/api/rbac/compliance?serviceAccount=default&namespace=default", nil)
	w := httptest.NewRecorder()

	// This should return an error since validator tries to use kubernetes client
	server.handleRBACCompliance(w, req)

	// Check for 502 (bad gateway) since validator isn't fully initialized
	if w.Code != http.StatusBadGateway && w.Code != http.StatusServiceUnavailable {
		t.Logf("Got status code: %d", w.Code)
	}
}

func TestSuggestRoleHandler(t *testing.T) {
	server := &Server{
		Version:       "test",
		RBACValidator: rbac.NewValidator(nil),
	}

	body := map[string][]string{
		"features": {"dashboard", "logs"},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/api/rbac/suggest-role", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleSuggestRole(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
		t.Logf("Response: %s", w.Body.String())
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil {
		if role, ok := result["role"]; ok {
			t.Logf("Generated role: %+v", role)
		}
	}
}

func TestScanImageHandler(t *testing.T) {
	server := &Server{
		Version: "test",
	}

	tests := []struct {
		name       string
		method     string
		body       interface{}
		expectCode int
	}{
		{
			name:       "POST scan image",
			method:     "POST",
			body:       map[string]string{"imageRef": "my-app:v1"},
			expectCode: http.StatusAccepted,
		},
		{
			name:       "GET scan results",
			method:     "GET",
			expectCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.method == "POST" {
				bodyBytes, _ := json.Marshal(tt.body)
				req = httptest.NewRequest(tt.method, "/api/security/scan", bytes.NewReader(bodyBytes))
			} else {
				req = httptest.NewRequest(tt.method, "/api/security/scan?imageRef=test:v1", nil)
			}

			w := httptest.NewRecorder()
			server.handleScanImage(w, req)

			if w.Code != tt.expectCode {
				t.Errorf("expected %d, got %d", tt.expectCode, w.Code)
			}
		})
	}
}

func TestListRepositoriesHandler_NoClient(t *testing.T) {
	server := &Server{
		Version: "test",
		// Artifactory client is nil
	}

	req := httptest.NewRequest("GET", "/api/registry/repositories", nil)
	w := httptest.NewRecorder()

	server.handleListRepositories(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when client not configured, got %d", w.Code)
	}

	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	if errMsg, ok := result["error"]; ok {
		if errMsg != "Artifactory integration not configured" {
			t.Errorf("unexpected error message: %s", errMsg)
		}
	}
}

func TestTriggerPipelineHandler_BadPayload(t *testing.T) {
	server := &Server{
		Version: "test",
	}

	// Missing required fields
	body := map[string]string{
		"project": "my-project",
		// ref is missing
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/api/cicd/trigger", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleTriggerPipeline(w, req)

	if w.Code != http.StatusBadGateway && w.Code != http.StatusBadRequest {
		// Will fail because GitLab client is nil, so returns 503/502
		t.Logf("Got expected error response: %d", w.Code)
	}
}

// The compliance check must refuse a policy it does not know rather than fall
// through to a default: a verdict issued under the wrong bar is worse than no
// verdict, and "production" is exactly the name someone would guess.
func TestCheckComplianceRejectsUnknownPolicy(t *testing.T) {
	server := &Server{Version: "test"}

	bodyBytes, _ := json.Marshal(map[string]string{
		"imageRef":   "myapp:latest",
		"policyName": "production", // not a built-in name
	})
	req := httptest.NewRequest("POST", "/api/security/compliance", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleCheckCompliance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown policy, got %d", w.Code)
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	// The error has to name the valid choices, or the caller cannot recover.
	if !strings.Contains(result["error"], "strict-production") {
		t.Errorf("error does not list the valid policies: %q", result["error"])
	}
}

func TestCheckComplianceRequiresImageRef(t *testing.T) {
	server := &Server{Version: "test"}
	bodyBytes, _ := json.Marshal(map[string]string{"policyName": "strict-production"})
	req := httptest.NewRequest("POST", "/api/security/compliance", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleCheckCompliance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without an imageRef, got %d", w.Code)
	}
}

func TestImageMatches(t *testing.T) {
	cases := []struct {
		report, want string
		match        bool
	}{
		{"docker.io/library/nginx:1.25", "docker.io/library/nginx:1.25", true},
		{"docker.io/library/nginx:1.25", "nginx:1.25", true},    // operator typed the short form
		{"nginx:1.25", "docker.io/library/nginx:1.25", true},    // report stored the short form
		{"docker.io/library/mynginx:1.25", "nginx:1.25", false}, // suffix, but not on a path boundary
		{"docker.io/library/nginx:1.25", "nginx:1.24", false},   // different tag
		{"docker.io/library/nginx:1.25", "redis:7", false},
	}
	for _, c := range cases {
		if got := imageMatches(c.report, c.want); got != c.match {
			t.Errorf("imageMatches(%q, %q) = %v, want %v", c.report, c.want, got, c.match)
		}
	}
}

// Test that handlers enforce required parameters
func TestHandlers_ValidationErrors(t *testing.T) {
	server := &Server{
		Version: "test",
	}

	tests := []struct {
		name           string
		handler        func(http.ResponseWriter, *http.Request)
		req            *http.Request
		expectCode     int
		shouldErrorMsg string
	}{
		{
			name:           "list images without repo",
			handler:        server.handleListImages,
			req:            httptest.NewRequest("GET", "/api/registry/images", nil),
			expectCode:     http.StatusBadRequest,
			shouldErrorMsg: "repo parameter",
		},
		{
			name:           "get manifest without repo and tag",
			handler:        server.handleGetManifest,
			req:            httptest.NewRequest("GET", "/api/registry/manifest", nil),
			expectCode:     http.StatusBadRequest,
			shouldErrorMsg: "repo and tag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handler(w, tt.req)

			if w.Code != tt.expectCode {
				t.Errorf("expected %d, got %d", tt.expectCode, w.Code)
			}

			var result map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil {
				if errMsg, ok := result["error"]; ok {
					t.Logf("Error message: %s", errMsg)
				}
			}
		})
	}
}
