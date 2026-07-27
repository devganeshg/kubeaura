package api

import (
	"encoding/json"
	"net/http"

	"github.com/devganeshg/kubeaura/internal/k8s"
)

// Helm handlers. The read endpoints decode Helm's storage secrets and work on
// any cluster; the single write endpoint shells out to the helm binary and is
// therefore restricted to a loopback run — see handleHelmAction.

func (s *Server) handleHelmReleases(w http.ResponseWriter, r *http.Request) {
	res, err := s.k8s().HelmReleases(r.Context(), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleHelmRelease(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, errString("name is required"))
		return
	}
	rev, err := k8s.ParseRevision(q.Get("revision"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	detail, err := s.k8s().HelmReleaseDetail(r.Context(), q.Get("namespace"), name, rev)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleHelmDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, errString("name is required"))
		return
	}
	from, err := k8s.ParseRevision(q.Get("from"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	to, err := k8s.ParseRevision(q.Get("to"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if from == 0 || to == 0 {
		writeErr(w, http.StatusBadRequest, errString("from and to revisions are required"))
		return
	}
	diff, err := s.k8s().HelmDiff(r.Context(), q.Get("namespace"), name, from, to)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// handleHelmAction runs one lifecycle action. It is registered behind
// sharedInstanceReadOnly because it executes a local binary that can read
// charts and values from this machine's filesystem: harmless when the operator
// is the only caller, an arbitrary-file-read primitive on a shared instance.
func (s *Server) handleHelmAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errString("method not allowed"))
		return
	}
	var body struct {
		Action    string `json:"action"`
		Release   string `json:"release"`
		Namespace string `json:"namespace"`
		Chart     string `json:"chart"`
		Version   string `json:"version"`
		Values    string `json:"values"`
		Revision  int    `json:"revision"`
		Wait      bool   `json:"wait"`
		DryRun    bool   `json:"dryRun"`
		Atomic    bool   `json:"atomic"`
		CreateNS  bool   `json:"createNamespace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	op := k8s.HelmOp{
		Action:    body.Action,
		Release:   body.Release,
		Namespace: body.Namespace,
		Chart:     body.Chart,
		Version:   body.Version,
		Values:    body.Values,
		Revision:  body.Revision,
		Wait:      body.Wait,
		DryRun:    body.DryRun,
		Atomic:    body.Atomic,
		CreateNS:  body.CreateNS,
	}
	res, err := s.k8s().RunHelm(r.Context(), op)

	// A dry run changes nothing, so recording it would only dilute the trail.
	if !body.DryRun {
		target := body.Namespace + "/" + body.Release
		detail := ""
		if res != nil {
			detail = res.Command
		}
		s.aud("helm."+body.Action, target, detail, err)
	}
	if err != nil {
		// A missing helm binary is a precondition, not a cluster failure.
		code := http.StatusBadGateway
		if res == nil {
			code = http.StatusPreconditionFailed
		}
		if res != nil {
			writeJSON(w, code, map[string]interface{}{"error": err.Error(), "output": res.Output, "command": res.Command})
			return
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
