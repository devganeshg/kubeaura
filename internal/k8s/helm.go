package k8s

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigyaml "sigs.k8s.io/yaml"
)

// This file implements the read half of Helm support by decoding Helm v3's own
// storage format directly. Helm keeps each release revision in a Secret of type
// helm.sh/release.v1 whose "release" key is base64(gzip(json)) — reading it
// needs no Helm SDK, no helm binary, and no chart repository access, so listing,
// history, values, manifests, notes and revision diffs work on any cluster.
//
// The write half (install/upgrade/rollback/uninstall) lives in helm_exec.go and
// shells out to the helm binary when one is present, because those operations
// need chart fetching, templating and repository state that only Helm itself
// can supply correctly.

const helmSecretType = "helm.sh/release.v1"

// HelmRelease is one release at one revision, decoded from its storage secret.
type HelmRelease struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Revision    int    `json:"revision"`
	Status      string `json:"status"`
	Chart       string `json:"chart"`      // "nginx-1.2.3"
	ChartName   string `json:"chartName"`  // "nginx"
	ChartVer    string `json:"chartVer"`   // "1.2.3"
	AppVersion  string `json:"appVersion"` // upstream app version
	Description string `json:"description"`
	Updated     string `json:"updated"` // RFC3339, "" when Helm recorded none
	Age         string `json:"age"`
}

// HelmReleaseDetail carries the full decoded payload for one revision.
type HelmReleaseDetail struct {
	HelmRelease
	Notes       string        `json:"notes"`
	Manifest    string        `json:"manifest"`
	UserValues  string        `json:"userValues"`  // YAML of values the user supplied
	AllValues   string        `json:"allValues"`   // YAML of chart defaults merged with user values
	History     []HelmRelease `json:"history"`     // newest revision first
	Resources   []HelmObject  `json:"resources"`   // objects parsed out of the manifest
	ChartHome   string        `json:"chartHome"`   // chart's home URL, when declared
	ChartDesc   string        `json:"chartDesc"`   // chart description
	FirstDeploy string        `json:"firstDeploy"` // RFC3339
}

// HelmObject is one Kubernetes object owned by a release, read from its manifest.
type HelmObject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	APIVer    string `json:"apiVersion"`
}

// HelmResult is the /api/helm payload. Releases is empty rather than null when
// a cluster simply has no Helm releases — an absence, not an error.
type HelmResult struct {
	Releases []HelmRelease `json:"releases"`
	// CLIAvailable reports whether a helm binary was found, which is what
	// decides if install/upgrade/rollback/uninstall are offered.
	CLIAvailable bool   `json:"cliAvailable"`
	CLIVersion   string `json:"cliVersion,omitempty"`
}

// helmStorage is the subset of Helm's release JSON that KubeAura reads. Helm
// writes many more fields; decoding only these keeps us tolerant of Helm
// releases written by other Helm versions.
type helmStorage struct {
	Name string `json:"name"`
	Info struct {
		FirstDeployed string `json:"first_deployed"`
		LastDeployed  string `json:"last_deployed"`
		Description   string `json:"description"`
		Status        string `json:"status"`
		Notes         string `json:"notes"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			AppVersion  string `json:"appVersion"`
			Description string `json:"description"`
			Home        string `json:"home"`
		} `json:"metadata"`
		Values map[string]interface{} `json:"values"` // chart defaults
	} `json:"chart"`
	Config    map[string]interface{} `json:"config"` // user-supplied values
	Manifest  string                 `json:"manifest"`
	Version   int                    `json:"version"`
	Namespace string                 `json:"namespace"`
}

// HelmReleases lists the latest revision of every Helm release in a namespace
// (empty namespace means all namespaces), newest first.
func (c *Client) HelmReleases(cx context.Context, namespace string) (*HelmResult, error) {
	revs, err := c.helmRevisions(cx, namespace, "")
	if err != nil {
		return nil, err
	}
	// Keep only the highest revision per namespace/name.
	latest := map[string]HelmRelease{}
	for _, r := range revs {
		key := r.Namespace + "/" + r.Name
		if cur, ok := latest[key]; !ok || r.Revision > cur.Revision {
			latest[key] = r
		}
	}
	out := make([]HelmRelease, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].Name < out[j].Name
		}
		return out[i].Namespace < out[j].Namespace
	})
	version, available := helmCLIVersion()
	return &HelmResult{Releases: out, CLIAvailable: available, CLIVersion: version}, nil
}

// HelmReleaseDetail decodes one release. revision 0 means "the latest".
func (c *Client) HelmReleaseDetail(cx context.Context, namespace, name string, revision int) (*HelmReleaseDetail, error) {
	if name == "" {
		return nil, fmt.Errorf("release name is required")
	}
	secrets, err := c.helmSecrets(cx, namespace, name)
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("helm release %q not found in namespace %q", name, namespace)
	}

	// Decode every revision for the history table, and remember the one asked for.
	var (
		history []HelmRelease
		want    *helmStorage
		wantNS  string
	)
	for _, s := range secrets {
		rel, err := decodeHelmSecret(s.Data["release"])
		if err != nil {
			// A single unreadable revision must not hide the rest of the history.
			continue
		}
		ns := rel.Namespace
		if ns == "" {
			ns = s.Namespace
		}
		history = append(history, helmReleaseFrom(rel, ns, s.CreationTimestamp))
		if revision == 0 || rel.Version == revision {
			if want == nil || rel.Version > want.Version {
				want, wantNS = rel, ns
			}
		}
	}
	if want == nil {
		return nil, fmt.Errorf("release %q has no revision %d in namespace %q", name, revision, namespace)
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Revision > history[j].Revision })

	d := &HelmReleaseDetail{
		HelmRelease: helmReleaseFrom(want, wantNS, metav1.Time{}),
		Notes:       want.Info.Notes,
		Manifest:    want.Manifest,
		History:     history,
		Resources:   helmManifestObjects(want.Manifest, wantNS),
		ChartHome:   want.Chart.Metadata.Home,
		ChartDesc:   want.Chart.Metadata.Description,
		FirstDeploy: want.Info.FirstDeployed,
	}
	// Reuse the age already computed for this revision in the history pass.
	for _, h := range history {
		if h.Revision == want.Version {
			d.Age = h.Age
			break
		}
	}
	d.UserValues = helmValuesYAML(want.Config)
	d.AllValues = helmValuesYAML(mergeHelmValues(want.Chart.Values, want.Config))
	return d, nil
}

// HelmRevisionManifest returns just the rendered manifest of one revision,
// which is what the revision-diff view compares. revision 0 means "the latest".
func (c *Client) HelmRevisionManifest(cx context.Context, namespace, name string, revision int) (string, error) {
	secrets, err := c.helmSecrets(cx, namespace, name)
	if err != nil {
		return "", err
	}
	best := -1
	manifest := ""
	for _, s := range secrets {
		rel, err := decodeHelmSecret(s.Data["release"])
		if err != nil {
			continue
		}
		if revision != 0 && rel.Version != revision {
			continue
		}
		if rel.Version > best {
			best, manifest = rel.Version, rel.Manifest
		}
	}
	if best < 0 {
		return "", fmt.Errorf("release %q has no revision %d in namespace %q", name, revision, namespace)
	}
	return manifest, nil
}

// HelmRevisionDiff is two revisions' manifests side by side. It deliberately
// mirrors DiffDoc's Live/Proposed shape so the UI renders it with the same line
// differ it already uses for YAML applies.
type HelmRevisionDiff struct {
	Name     string `json:"name"`
	From     int    `json:"from"`
	To       int    `json:"to"`
	Live     string `json:"live"`     // the "from" revision's manifest
	Proposed string `json:"proposed"` // the "to" revision's manifest
}

// HelmDiff returns the manifests of two revisions for comparison.
func (c *Client) HelmDiff(cx context.Context, namespace, name string, from, to int) (*HelmRevisionDiff, error) {
	a, err := c.HelmRevisionManifest(cx, namespace, name, from)
	if err != nil {
		return nil, err
	}
	b, err := c.HelmRevisionManifest(cx, namespace, name, to)
	if err != nil {
		return nil, err
	}
	return &HelmRevisionDiff{Name: name, From: from, To: to, Live: a, Proposed: b}, nil
}

// helmRevisions decodes every release secret in scope. An empty release name
// matches all releases.
func (c *Client) helmRevisions(cx context.Context, namespace, name string) ([]HelmRelease, error) {
	secrets, err := c.helmSecrets(cx, namespace, name)
	if err != nil {
		return nil, err
	}
	out := make([]HelmRelease, 0, len(secrets))
	for _, s := range secrets {
		rel, err := decodeHelmSecret(s.Data["release"])
		if err != nil {
			continue
		}
		ns := rel.Namespace
		if ns == "" {
			ns = s.Namespace
		}
		out = append(out, helmReleaseFrom(rel, ns, s.CreationTimestamp))
	}
	return out, nil
}

// helmSecrets lists the Helm storage secrets in a namespace, optionally
// narrowed to one release. It selects on type first and falls back to the
// owner=helm label selector, because listing by field selector is what keeps
// this cheap on clusters with many secrets.
func (c *Client) helmSecrets(cx context.Context, namespace, name string) ([]secretLike, error) {
	opts := metav1.ListOptions{FieldSelector: "type=" + helmSecretType}
	if name != "" {
		opts.LabelSelector = "owner=helm,name=" + name
	} else {
		opts.LabelSelector = "owner=helm"
	}
	list, err := c.cs.CoreV1().Secrets(namespace).List(cx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]secretLike, 0, len(list.Items))
	for _, s := range list.Items {
		if len(s.Data["release"]) == 0 {
			continue
		}
		// The label selector already narrowed by name, but releases stored by
		// very old Helm versions can lack the label — re-check by secret name.
		if name != "" && s.Labels["name"] == "" && helmNameFromSecret(s.Name) != name {
			continue
		}
		out = append(out, secretLike{
			Name:              s.Name,
			Namespace:         s.Namespace,
			Labels:            s.Labels,
			Data:              s.Data,
			CreationTimestamp: s.CreationTimestamp,
		})
	}
	return out, nil
}

// secretLike is the slice of a corev1.Secret this file needs, so the decoding
// helpers stay independent of the clientset type.
type secretLike struct {
	Name              string
	Namespace         string
	Labels            map[string]string
	Data              map[string][]byte
	CreationTimestamp metav1.Time
}

// decodeHelmSecret unwraps Helm's base64(gzip(json)) release payload. Helm has
// always gzipped since v3.0, but the gzip layer is detected by magic number so
// an ungzipped payload still decodes.
func decodeHelmSecret(raw []byte) (*helmStorage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty release payload")
	}
	// Kubernetes already base64-decoded the secret value for us; Helm's own
	// base64 layer is still in place.
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(raw)))
	n, err := base64.StdEncoding.Decode(decoded, raw)
	if err != nil {
		// Not base64 — assume the payload is already the gzip/JSON bytes.
		decoded, n = raw, len(raw)
	}
	payload := decoded[:n]

	if len(payload) > 2 && payload[0] == 0x1f && payload[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("gunzip release: %w", err)
		}
		defer zr.Close()
		// Releases are small; the cap guards against a malformed gzip bomb.
		unzipped, err := io.ReadAll(io.LimitReader(zr, 32<<20))
		if err != nil {
			return nil, fmt.Errorf("gunzip release: %w", err)
		}
		payload = unzipped
	}

	var rel helmStorage
	if err := json.Unmarshal(payload, &rel); err != nil {
		return nil, fmt.Errorf("decode release json: %w", err)
	}
	return &rel, nil
}

// helmReleaseFrom projects the decoded storage into the API shape. created is
// used only as an age fallback for releases Helm never stamped.
func helmReleaseFrom(rel *helmStorage, namespace string, created metav1.Time) HelmRelease {
	m := rel.Chart.Metadata
	chart := m.Name
	if m.Version != "" {
		chart = m.Name + "-" + m.Version
	}
	status := rel.Info.Status
	if status == "" {
		status = "unknown"
	}
	r := HelmRelease{
		Name:        rel.Name,
		Namespace:   namespace,
		Revision:    rel.Version,
		Status:      helmTitle(status),
		Chart:       chart,
		ChartName:   m.Name,
		ChartVer:    m.Version,
		AppVersion:  m.AppVersion,
		Description: rel.Info.Description,
		Updated:     rel.Info.LastDeployed,
	}
	if t, err := time.Parse(time.RFC3339, rel.Info.LastDeployed); err == nil {
		r.Age = age(metav1.NewTime(t))
	} else if !created.IsZero() {
		r.Age = age(created)
	}
	return r
}

// helmTitle upper-cases the first letter of Helm's lower-case status strings
// ("deployed" -> "Deployed") so they match the status colours used elsewhere.
func helmTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// helmValuesYAML renders a values map as YAML, with a readable placeholder when
// there are none (a chart with no user overrides is the common case).
func helmValuesYAML(v map[string]interface{}) string {
	if len(v) == 0 {
		return "{}\n"
	}
	out, err := sigyaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("# could not render values: %v\n", err)
	}
	return string(out)
}

// mergeHelmValues overlays user-supplied values on chart defaults the way Helm
// does: maps merge recursively, every other value is replaced wholesale.
func mergeHelmValues(defaults, overrides map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(defaults)+len(overrides))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range overrides {
		if sub, ok := v.(map[string]interface{}); ok {
			if base, ok := out[k].(map[string]interface{}); ok {
				out[k] = mergeHelmValues(base, sub)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// helmManifestObjects lists the objects a release's manifest declares, so the
// UI can link a release to the resources it owns.
func helmManifestObjects(manifest, defaultNS string) []HelmObject {
	docs := splitYAML(manifest)
	out := make([]HelmObject, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := sigyaml.Unmarshal([]byte(doc), &m); err != nil {
			continue
		}
		if m.Kind == "" || m.Metadata.Name == "" {
			continue
		}
		ns := m.Metadata.Namespace
		if ns == "" {
			ns = defaultNS
		}
		out = append(out, HelmObject{
			Kind:      m.Kind,
			Name:      m.Metadata.Name,
			Namespace: ns,
			APIVer:    m.APIVersion,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].Name < out[j].Name
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// ParseRevision reads a revision query parameter, where empty means "latest".
func ParseRevision(s string) (int, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid revision %q", s)
	}
	return n, nil
}
