package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	tests := []struct {
		name        string
		host        string
		origin      string
		allowRemote bool
		want        int
	}{
		{"loopback ip", "127.0.0.1:8080", "", false, http.StatusOK},
		{"localhost name", "localhost:8080", "", false, http.StatusOK},
		{"ipv6 loopback", "[::1]:8080", "", false, http.StatusOK},
		{"same-origin write", "localhost:8080", "http://localhost:8080", false, http.StatusOK},

		// A rebinding attack arrives as a name that resolves to 127.0.0.1;
		// only the Host header exposes it.
		{"rebinding host", "evil.example.com:8080", "", false, http.StatusForbidden},
		{"lan ip", "192.168.1.20:8080", "", false, http.StatusForbidden},

		// A page on another site posting to the local API.
		{"cross-origin write", "localhost:8080", "https://evil.example.com", false, http.StatusForbidden},
		{"cross-origin port", "localhost:8080", "http://localhost:9999", false, http.StatusForbidden},

		// Shared deployments opt in, but cross-origin stays refused.
		{"remote allowed", "kubemind.internal", "", true, http.StatusOK},
		{"remote same-origin", "kubemind.internal", "https://kubemind.internal", true, http.StatusOK},
		{"remote cross-origin", "kubemind.internal", "https://evil.example.com", true, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/delete", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			guard(ok, tc.allowRemote).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("Host=%q Origin=%q allowRemote=%v: got %d, want %d",
					tc.host, tc.origin, tc.allowRemote, w.Code, tc.want)
			}
		})
	}
}

func TestSingleOperatorOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	tests := []struct {
		name        string
		method      string
		allowRemote bool
		want        int
	}{
		// The single-operator run: the person driving the UI is the person
		// whose credentials it uses, so nothing is gained by blocking them.
		{"local write", http.MethodPost, false, http.StatusOK},
		{"local read", http.MethodGet, false, http.StatusOK},

		// Shared instance: reading the (redacted) connection list is fine,
		// but repointing the backend would redirect cluster data to whoever
		// asked.
		{"shared read", http.MethodGet, true, http.StatusOK},
		{"shared write", http.MethodPost, true, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{AllowRemote: tc.allowRemote}
			r := httptest.NewRequest(tc.method, "/api/ai/connections", nil)
			w := httptest.NewRecorder()
			s.singleOperatorOnly(ok).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("method=%s allowRemote=%v: got %d, want %d",
					tc.method, tc.allowRemote, w.Code, tc.want)
			}
		})
	}
}
