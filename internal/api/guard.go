package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// KubeMind has no authentication of its own: it acts with the permissions of
// the kubeconfig it was handed, and it is meant to be reached by exactly one
// person — the operator running it on their own machine. That model only holds
// while two browser-level attacks are closed off:
//
//	DNS rebinding — evil.example.com resolves to 127.0.0.1, so a page the
//	operator visits can address the local API as a same-origin host and drive
//	/api/delete or /api/exec against their cluster.
//
//	Cross-site requests — a form or fetch on any other site posting to
//	http://localhost:8080/api/scale. The browser sends the request even when
//	it will not let the attacker read the response, and a write is all the
//	attacker needs.
//
// guard closes both by pinning the Host header to loopback (unless the
// operator opted into remote listening) and by requiring that any Origin the
// browser attaches matches the host being served.

// guard wraps h with Host and Origin validation. allowRemote relaxes the
// loopback requirement for shared in-cluster deployments, which are expected
// to sit behind an authenticating ingress.
func guard(h http.Handler, allowRemote bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowRemote && !isLoopbackHost(r.Host) {
			http.Error(w, "KubeMind only serves loopback hosts. This request arrived with "+
				"Host: "+r.Host+", which is how DNS-rebinding attacks reach a local API. "+
				"Set KUBEMIND_ALLOW_REMOTE=1 (and put an authenticating proxy in front) "+
				"if you meant to expose it.", http.StatusForbidden)
			return
		}
		// Origin is attached by browsers to every cross-origin request and to
		// all same-origin writes; its absence means a non-browser client such
		// as curl, which is not the threat here.
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			http.Error(w, "cross-origin request rejected: Origin "+o+" does not match "+r.Host,
				http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// singleOperatorOnly gates endpoints that are safe for the one person running
// KubeMind on their own machine, but not for a shared in-cluster instance.
//
// Model-connection management is the case that matters. Adding a connection
// tells the assistant where to send its prompts — and those prompts carry
// cluster snapshots and, when troubleshooting, pod specs, events, and logs. On
// a shared instance anyone who reaches the Service could point that at a server
// they control and quietly collect all of it. Discovery is also an SSRF
// primitive there: the pod will fetch any URL it is handed, from inside the
// cluster network.
//
// On a normal loopback run none of this is a boundary — the operator can
// already curl anything the process can. So the gate follows AllowRemote.
func (s *Server) singleOperatorOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AllowRemote && r.Method != http.MethodGet {
			http.Error(w, "changing the AI model connection is disabled on a shared "+
				"instance: it controls where cluster data is sent. Configure the backend "+
				"with ANTHROPIC_API_KEY / KUBEMIND_AI_* on the Deployment instead.",
				http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// isLoopbackHost reports whether a Host header addresses this machine. Named
// hosts other than "localhost" are rejected outright: a name is exactly what a
// rebinding attack controls.
func isLoopbackHost(host string) bool {
	h := host
	if v, _, err := net.SplitHostPort(host); err == nil {
		h = v
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// sameOrigin reports whether an Origin header refers to the host being served.
// Only the host:port is compared — the scheme is not, because the desktop
// window and a plain browser tab reach the same server over http while a
// reverse proxy may terminate TLS and forward as http.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Host == host {
		return true
	}
	// A reverse proxy may forward Host without the port while the browser's
	// Origin carries the public one (or the reverse). Fall back to comparing
	// hostnames only when exactly one side omits a port — when both carry one,
	// differing ports are genuinely different origins.
	oh, _, oerr := net.SplitHostPort(u.Host)
	hh, _, herr := net.SplitHostPort(host)
	if (oerr == nil) == (herr == nil) {
		return false
	}
	if oerr != nil {
		oh = u.Host
	}
	if herr != nil {
		hh = host
	}
	return oh == hh
}
