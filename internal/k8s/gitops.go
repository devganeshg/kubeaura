package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// GitOps status is read from whichever GitOps engine the cluster runs:
// Argo CD Applications and/or Flux Kustomizations + HelmReleases. Same
// detect-don't-install pattern as the Trivy integration — missing CRDs mean
// the tool is absent, not an error (CNCF_INTEGRATION_ROADMAP.md item 2).
var (
	argoAppsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

	// Flux API groups have moved through beta versions; try newest first.
	fluxKustomizationGVRs = []schema.GroupVersionResource{
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"},
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1beta2", Resource: "kustomizations"},
	}
	fluxHelmReleaseGVRs = []schema.GroupVersionResource{
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2beta2", Resource: "helmreleases"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2beta1", Resource: "helmreleases"},
	}
)

// GitOpsApp is one deployable unit managed by a GitOps engine.
type GitOpsApp struct {
	Tool      string `json:"tool"` // ArgoCD|Flux
	Kind      string `json:"kind"` // Application|Kustomization|HelmRelease
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Sync      string `json:"sync"`   // Synced|OutOfSync|Ready|NotReady|Unknown
	Health    string `json:"health"` // Healthy|Degraded|Progressing|Missing|Unknown ("" for Flux)
	Source    string `json:"source"` // repo URL or sourceRef
	Revision  string `json:"revision"`
	Message   string `json:"message"` // last condition message, for troubleshooting
	Age       string `json:"age"`
}

// GitOpsResult is the /api/gitops payload.
type GitOpsResult struct {
	ArgoInstalled bool        `json:"argoInstalled"`
	FluxInstalled bool        `json:"fluxInstalled"`
	Apps          []GitOpsApp `json:"apps"`
}

// GitOpsStatus lists Argo and Flux apps for a namespace (empty = all).
func (c *Client) GitOpsStatus(cx context.Context, namespace string) (*GitOpsResult, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil, err
	}
	res := &GitOpsResult{Apps: []GitOpsApp{}}

	if items, ok := listFirstAvailable(cx, dyn, namespace, []schema.GroupVersionResource{argoAppsGVR}); ok {
		res.ArgoInstalled = true
		for _, it := range items {
			res.Apps = append(res.Apps, argoAppRow(it))
		}
	}
	if items, ok := listFirstAvailable(cx, dyn, namespace, fluxKustomizationGVRs); ok {
		res.FluxInstalled = true
		for _, it := range items {
			res.Apps = append(res.Apps, fluxRow("Kustomization", it))
		}
	}
	if items, ok := listFirstAvailable(cx, dyn, namespace, fluxHelmReleaseGVRs); ok {
		res.FluxInstalled = true
		for _, it := range items {
			res.Apps = append(res.Apps, fluxRow("HelmRelease", it))
		}
	}

	// Problems first, then alphabetical, so the table reads like a worklist.
	bad := func(a GitOpsApp) int {
		if a.Sync == "OutOfSync" || a.Sync == "NotReady" || a.Health == "Degraded" || a.Health == "Missing" {
			return 0
		}
		if a.Sync == "Unknown" || a.Health == "Progressing" {
			return 1
		}
		return 2
	}
	sort.Slice(res.Apps, func(i, j int) bool {
		a, b := res.Apps[i], res.Apps[j]
		if bad(a) != bad(b) {
			return bad(a) < bad(b)
		}
		return a.Namespace+a.Name < b.Namespace+b.Name
	})
	return res, nil
}

// listFirstAvailable tries each GVR in order and returns items from the first
// one the API server actually serves. (false, nil) means none exist.
func listFirstAvailable(cx context.Context, dyn dynamic.Interface, namespace string, gvrs []schema.GroupVersionResource) ([]unstructured.Unstructured, bool) {
	for _, gvr := range gvrs {
		list, err := dyn.Resource(gvr).Namespace(namespace).List(cx, metav1.ListOptions{})
		if err == nil {
			return list.Items, true
		}
		if !apierrors.IsNotFound(err) {
			// CRD exists but the read failed (RBAC, timeout): treat as installed
			// with no rows rather than hiding the tool entirely.
			return nil, true
		}
	}
	return nil, false
}

func argoAppRow(it unstructured.Unstructured) GitOpsApp {
	syncStatus, _, _ := unstructured.NestedString(it.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(it.Object, "status", "health", "status")
	repo, _, _ := unstructured.NestedString(it.Object, "spec", "source", "repoURL")
	rev, _, _ := unstructured.NestedString(it.Object, "status", "sync", "revision")
	msg := ""
	if conds, found, _ := unstructured.NestedSlice(it.Object, "status", "conditions"); found && len(conds) > 0 {
		if m, ok := conds[len(conds)-1].(map[string]interface{}); ok {
			msg, _, _ = unstructured.NestedString(m, "message")
		}
	}
	if syncStatus == "" {
		syncStatus = "Unknown"
	}
	if health == "" {
		health = "Unknown"
	}
	return GitOpsApp{
		Tool: "ArgoCD", Kind: "Application",
		Namespace: it.GetNamespace(), Name: it.GetName(),
		Sync: syncStatus, Health: health,
		Source: repo, Revision: shortRev(rev), Message: msg,
		Age: age(it.GetCreationTimestamp()),
	}
}

func fluxRow(kind string, it unstructured.Unstructured) GitOpsApp {
	sync, msg := fluxReadyCondition(it)
	rev, _, _ := unstructured.NestedString(it.Object, "status", "lastAppliedRevision")
	if rev == "" {
		rev, _, _ = unstructured.NestedString(it.Object, "status", "lastAttemptedRevision")
	}
	srcKind, _, _ := unstructured.NestedString(it.Object, "spec", "sourceRef", "kind")
	srcName, _, _ := unstructured.NestedString(it.Object, "spec", "sourceRef", "name")
	if srcKind == "" { // HelmRelease nests the ref under spec.chart.spec
		srcKind, _, _ = unstructured.NestedString(it.Object, "spec", "chart", "spec", "sourceRef", "kind")
		srcName, _, _ = unstructured.NestedString(it.Object, "spec", "chart", "spec", "sourceRef", "name")
	}
	source := ""
	if srcKind != "" {
		source = fmt.Sprintf("%s/%s", srcKind, srcName)
	}
	return GitOpsApp{
		Tool: "Flux", Kind: kind,
		Namespace: it.GetNamespace(), Name: it.GetName(),
		Sync: sync, Health: "",
		Source: source, Revision: shortRev(rev), Message: msg,
		Age: age(it.GetCreationTimestamp()),
	}
}

// fluxReadyCondition maps Flux's Ready condition to Ready/NotReady/Unknown.
func fluxReadyCondition(it unstructured.Unstructured) (string, string) {
	conds, found, _ := unstructured.NestedSlice(it.Object, "status", "conditions")
	if !found {
		return "Unknown", ""
	}
	for _, cd := range conds {
		m, ok := cd.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		if !strings.EqualFold(t, "Ready") {
			continue
		}
		s, _, _ := unstructured.NestedString(m, "status")
		msg, _, _ := unstructured.NestedString(m, "message")
		switch s {
		case "True":
			return "Ready", msg
		case "False":
			return "NotReady", msg
		}
		return "Unknown", msg
	}
	return "Unknown", ""
}

// shortRev shortens long git SHAs but leaves branch/tag revisions readable.
func shortRev(rev string) string {
	if i := strings.LastIndexByte(rev, ':'); i >= 0 && len(rev)-i > 8 {
		// Flux style "main@sha1:abcdef123456..." — keep prefix + short sha.
		return rev[:i+9]
	}
	if len(rev) == 40 && !strings.ContainsAny(rev, "@/:") {
		return rev[:8]
	}
	return rev
}
