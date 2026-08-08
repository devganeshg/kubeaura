package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/devganeshg/kubeaura/internal/ai"
	"github.com/devganeshg/kubeaura/internal/k8s"
)

// These tests assert on the bytes that actually reach the model provider,
// captured off the wire rather than inspected halfway down the call. The point
// of the evidence layer is a guarantee about what leaves the machine, and the
// only honest place to check that is the socket.

// captureBackend stands in for a model provider and records every request body
// it receives. It speaks the OpenAI chat schema, which is what the openai
// provider posts.
type captureBackend struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string
}

func newCaptureBackend(t *testing.T) *captureBackend {
	t.Helper()
	b := &captureBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.got = append(b.got, string(body))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// sent returns everything the backend received, concatenated. Assertions are
// against the whole transcript so a leak cannot hide in a second call.
func (b *captureBackend) sent() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.got, "\n")
}

func (b *captureBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.got)
}

// testServer wires a Server against the capture backend. Mgr is a zero-value
// Manager: it is only reached here for Active(), which the audit trail needs,
// and these handlers take the request-body path that never touches a cluster.
func testServer(t *testing.T) (*Server, *captureBackend) {
	t.Helper()
	backend := newCaptureBackend(t)
	s := &Server{
		Mgr: &k8s.Manager{},
		// Routes registers the SPA at "/", and http.ServeMux panics on a nil
		// handler. Nothing here requests it.
		Static: http.NotFoundHandler(),
		AI: ai.Configure(ai.Settings{
			Provider: "openai", OpenAIBaseURL: backend.srv.URL, OpenAIKey: "test-key", Model: "test-model",
		}),
	}
	return s, backend
}

func post(t *testing.T, s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(buf)))
	// guard.go requires a loopback Host and a matching Origin for writes.
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

const secretYAML = `apiVersion: v1
kind: Secret
metadata:
  name: leak-canary
  namespace: default
type: Opaque
data:
  password: aHVudGVyMg==
`

// The regression this whole change exists to prevent: /api/ai/review used to
// post a Secret's data block verbatim to the model provider.
func TestAIReviewNeverSendsSecretData(t *testing.T) {
	s, backend := testServer(t)

	rec := post(t, s, "/api/ai/review", map[string]interface{}{
		"yaml": secretYAML, "kind": "Secret", "namespace": "default", "name": "leak-canary",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.calls() != 1 {
		t.Fatalf("expected exactly one model call, got %d", backend.calls())
	}
	if strings.Contains(backend.sent(), "aHVudGVyMg==") {
		t.Errorf("secret data reached the provider:\n%s", backend.sent())
	}
	if !strings.Contains(backend.sent(), "leak-canary") {
		t.Errorf("the object name should still reach the provider:\n%s", backend.sent())
	}

	var out struct {
		Answer   string `json:"answer"`
		Evidence struct {
			Purpose    string `json:"purpose"`
			Hash       string `json:"hash"`
			Bytes      int    `json:"bytes"`
			Redactions []struct {
				Rule string `json:"rule"`
			} `json:"redactions"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Evidence.Purpose != "review" || out.Evidence.Hash == "" || out.Evidence.Bytes == 0 {
		t.Errorf("response should carry a filled-in envelope: %+v", out.Evidence)
	}
	var rules []string
	for _, r := range out.Evidence.Redactions {
		rules = append(rules, r.Rule)
	}
	if !strings.Contains(strings.Join(rules, ","), "secret-data") {
		t.Errorf("envelope should disclose the secret-data rule firing, got %v", rules)
	}
}

// Preview is what makes the guarantee inspectable: it must describe the call
// without making it.
func TestAIReviewPreviewDoesNotCallTheModel(t *testing.T) {
	s, backend := testServer(t)

	rec := post(t, s, "/api/ai/review", map[string]interface{}{
		"yaml": secretYAML, "kind": "Secret", "name": "leak-canary", "preview": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.calls() != 0 {
		t.Errorf("preview made %d model calls, want 0", backend.calls())
	}
	var out struct {
		Answer   string                 `json:"answer"`
		Evidence map[string]interface{} `json:"evidence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Answer != "" {
		t.Errorf("preview should not return an answer, got %q", out.Answer)
	}
	if out.Evidence["hash"] == nil {
		t.Errorf("preview should return the envelope, got %+v", out.Evidence)
	}
	// The destination is half the disclosure: the same evidence is a different
	// decision depending on where it is going.
	if out.Evidence["provider"] == nil {
		t.Errorf("envelope should name the backend, got %+v", out.Evidence)
	}
}

func TestAIReviewAuditsTheCall(t *testing.T) {
	s, _ := testServer(t)

	if rec := post(t, s, "/api/ai/review", map[string]interface{}{
		"yaml": secretYAML, "kind": "Secret", "name": "leak-canary",
	}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d", rec.Code)
	}
	trail := rec.Body.String()
	if !strings.Contains(trail, "ai.review") {
		t.Errorf("model call missing from the audit trail:\n%s", trail)
	}
	// The trail records the hash and the size, never the evidence itself.
	if strings.Contains(trail, "aHVudGVyMg==") {
		t.Errorf("audit trail contains the evidence it is supposed to only reference:\n%s", trail)
	}
}

func TestAIReviewRedactsPastedWorkloadEnv(t *testing.T) {
	// A pasted manifest takes the same path as a fetched one: the operator who
	// pastes a Deployment with an inline password gets the same guarantee.
	s, backend := testServer(t)
	const dep = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
      - name: api
        image: ghcr.io/acme/api:1.4.2
        env:
        - name: DB_PASSWORD
          value: hunter2
`
	if rec := post(t, s, "/api/ai/review", map[string]interface{}{"yaml": dep, "kind": "Deployment"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(backend.sent(), "hunter2") {
		t.Errorf("inline env value reached the provider:\n%s", backend.sent())
	}
	if !strings.Contains(backend.sent(), "DB_PASSWORD") {
		t.Errorf("the variable name should survive:\n%s", backend.sent())
	}
}

func TestAIReviewRequiresSomethingToReview(t *testing.T) {
	s, backend := testServer(t)
	rec := post(t, s, "/api/ai/review", map[string]interface{}{"kind": "Secret"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if backend.calls() != 0 {
		t.Errorf("a rejected request must not reach the provider")
	}
}

func TestAIGenerateIsAudited(t *testing.T) {
	// Generate sends no cluster state, so it has no envelope — but it still
	// leaves the machine, so it still belongs in the trail.
	s, backend := testServer(t)
	if rec := post(t, s, "/api/ai/generate", map[string]string{
		"description": "a redis deployment with one replica",
	}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.calls() != 1 {
		t.Fatalf("expected one model call, got %d", backend.calls())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "ai.generate") {
		t.Errorf("generate missing from the audit trail:\n%s", rec.Body.String())
	}
}
