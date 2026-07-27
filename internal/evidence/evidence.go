// Package evidence prepares cluster state for a model call: it strips the
// material that has no business leaving the machine, caps how much can leave,
// and describes exactly what did in a hash-stamped envelope.
//
// The AI Assistant is the one part of KubeAura that can send cluster state to a
// third party. Before this package, a troubleshoot request marshalled the whole
// corev1.Pod — which carries inline environment values, the
// last-applied-configuration annotation (a full copy of the original manifest),
// internal hostnames and node names — and posted it to whichever backend was
// configured. Disclosing that in a README is not the same as controlling it.
//
// Three properties matter here, in this order:
//
//	Deterministic. Redaction is by rule and by path, never by guessing whether
//	a particular value "looks secret". Every inline env value goes, always.
//	An operator who reads the rules knows what a call contains without
//	inspecting it.
//
//	Accountable. Every call produces an Envelope: the resource it covers, the
//	fields included, what was removed and how much, the log window, the byte
//	count, and a SHA-256 of the exact payload. The hash goes in the audit
//	trail with the diagnosis, so a conclusion can be tied to its inputs
//	without keeping those inputs around.
//
//	Still useful. Redaction preserves the shape a diagnosis needs. Variable
//	names survive when their values do not; secretKeyRef targets survive
//	because a name is a reference, not a body. A model can still say "the pod
//	reads DATABASE_URL from the app-secrets Secret, which does not exist".
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Caps on what one call may carry. These are byte-based on purpose: a
// line-based cap is defeated by a single 50 KB stack trace or JSON log line,
// which is exactly the shape of log an operator reaches for the assistant to
// read.
const (
	MaxLogBytes   = 32 << 10 // 32 KiB of log text
	MaxEventBytes = 16 << 10 // 16 KiB of event messages
	MaxEvents     = 40       // newest events win
	redactedMark  = "«redacted»"
)

// Redaction records one rule firing, so the envelope can say what was removed
// without reproducing any of it.
type Redaction struct {
	Rule  string `json:"rule"`  // stable identifier, e.g. "env-value"
	Path  string `json:"path"`  // where it applied, e.g. "spec.containers[].env[].value"
	Count int    `json:"count"` // how many values the rule removed
	Bytes int    `json:"bytes"` // how many bytes those values held
}

// LogWindow describes the slice of a container's log that was included.
type LogWindow struct {
	Container    string `json:"container,omitempty"`
	LinesAsked   int    `json:"linesAsked"`
	LinesSent    int    `json:"linesSent"`
	Bytes        int    `json:"bytes"`
	Truncated    bool   `json:"truncated"`    // the byte cap dropped older lines
	ScrubbedHits int    `json:"scrubbedHits"` // secret-shaped strings replaced in the text
}

// ResourceRef identifies what the evidence is about. The UID is what makes a
// diagnosis reproducible: names get reused, UIDs do not.
type ResourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

// Envelope is the operator-facing description of one model call: what is going
// out, what was taken out, how much of it there is, and its hash.
type Envelope struct {
	Purpose    string      `json:"purpose"` // "troubleshoot" | "logsummary"
	Resource   ResourceRef `json:"resource"`
	Fields     []string    `json:"fields"`     // top-level content groups included
	Redactions []Redaction `json:"redactions"` // empty means nothing matched
	LogWindow  *LogWindow  `json:"logWindow,omitempty"`
	Events     int         `json:"events"`
	Bytes      int         `json:"bytes"` // payload size leaving the machine
	Hash       string      `json:"hash"`  // sha256 of the exact payload, hex
	PreparedAt time.Time   `json:"preparedAt"`

	// Destination is filled in by the caller once the backend is known, so the
	// preview can say where the bytes are going and whether that is off-machine.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Local    bool   `json:"local"` // true when the model runs on this machine
}

// Payload is the redacted evidence plus its envelope. Payload.JSON is what gets
// sent; nothing else may.
type Payload struct {
	JSON     string   `json:"-"`
	Envelope Envelope `json:"envelope"`
}

// scrubbers replace secret-shaped strings found in free text (logs, event
// messages). Structured redaction handles the fields we know about; this
// catches the credential an application printed itself, which is the case an
// operator cannot anticipate.
//
// These are heuristics and are documented as such: they reduce exposure in
// unstructured text, they do not eliminate it. The structured rules above are
// the ones that carry a guarantee.
var scrubbers = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// An auth header carries an optional scheme word before the credential, so
	// stopping at the first token leaves the secret behind. Consuming to end of
	// line instead over-reaches: in a shell entrypoint the rest of the line is
	// usually more commands, and eating them shows the model a mangled
	// entrypoint — the field an operator leans on most. So: optional scheme,
	// then one token.
	{"authorization-header", regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\s*[:=]\s*(?:(?:basic|bearer|digest|token|apikey|negotiate|ntlm)\s+)?[^\s;,'"]+`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-+/=]{12,}`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"url-credentials", regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s:/@]+:[^\s/@]+@`)},
	// The leading prefix is not a word boundary on purpose: an underscore is a
	// word character, so \b never matches inside DB_PASSWORD — the single most
	// common shape of a credential an application prints.
	{"key-value-secret", regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(pass(word|wd)?|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret)\s*[:=]\s*["']?[^\s"',;}&]{4,}`)},
}

// ScrubText replaces secret-shaped substrings and reports how many it replaced.
func ScrubText(s string) (string, int) {
	hits := 0
	for _, sc := range scrubbers {
		s = sc.re.ReplaceAllStringFunc(s, func(match string) string {
			hits++
			// Keep the key so a log line stays readable: "password=«redacted»".
			if i := strings.IndexAny(match, ":="); i > 0 &&
				(sc.name == "key-value-secret" || sc.name == "authorization-header") {
				return match[:i+1] + redactedMark
			}
			if sc.name == "url-credentials" {
				// Preserve the scheme so the destination host stays visible.
				if i := strings.Index(match, "://"); i > 0 {
					return match[:i+3] + redactedMark + "@"
				}
			}
			return redactedMark
		})
	}
	return s, hits
}

// tailBytes trims s to at most max bytes, keeping the end (the newest log
// lines, which is where a failure shows up) and cutting on a line boundary.
func tailBytes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i+1 < len(cut) {
		cut = cut[i+1:]
	}
	return cut, true
}

// redactedPod is the pod shape that leaves the machine. It is an explicit
// allow-list rather than a copy of corev1.Pod with fields deleted: a new field
// in a future client-go must not silently start being transmitted.
type redactedPod struct {
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	UID         string            `json:"uid"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Created     string            `json:"created,omitempty"`
	NodeName    string            `json:"nodeName,omitempty"`

	Phase           string               `json:"phase,omitempty"`
	Reason          string               `json:"reason,omitempty"`
	Message         string               `json:"message,omitempty"`
	QOSClass        string               `json:"qosClass,omitempty"`
	RestartPolicy   string               `json:"restartPolicy,omitempty"`
	ServiceAccount  string               `json:"serviceAccount,omitempty"`
	Conditions      []redactedCondition  `json:"conditions,omitempty"`
	Containers      []redactedContainer  `json:"containers,omitempty"`
	InitContainers  []redactedContainer  `json:"initContainers,omitempty"`
	Volumes         []redactedVolume     `json:"volumes,omitempty"`
	Owners          []string             `json:"owners,omitempty"`
	Tolerations     int                  `json:"tolerations,omitempty"`
	HasNodeSelector bool                 `json:"hasNodeSelector,omitempty"`
	HasAffinity     bool                 `json:"hasAffinity,omitempty"`
	SecurityContext *redactedPodSecurity `json:"securityContext,omitempty"`
}

type redactedCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type redactedPodSecurity struct {
	RunAsNonRoot *bool  `json:"runAsNonRoot,omitempty"`
	RunAsUser    *int64 `json:"runAsUser,omitempty"`
	FSGroup      *int64 `json:"fsGroup,omitempty"`
}

type redactedContainer struct {
	Name            string   `json:"name"`
	Image           string   `json:"image"`
	ImagePullPolicy string   `json:"imagePullPolicy,omitempty"`
	Command         []string `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	Ports           []string `json:"ports,omitempty"`

	// EnvNames carries variable names with their values removed; EnvFrom and
	// EnvFromRefs carry references (names of Secrets/ConfigMaps), which are
	// diagnostically essential and are not secret bodies.
	EnvNames    []string `json:"envNames,omitempty"`
	EnvFromRefs []string `json:"envFromRefs,omitempty"`
	ValueFrom   []string `json:"valueFrom,omitempty"`

	Resources     map[string]string `json:"resources,omitempty"`
	Mounts        []string          `json:"mounts,omitempty"`
	Probes        []string          `json:"probes,omitempty"`
	State         string            `json:"state,omitempty"`
	LastState     string            `json:"lastState,omitempty"`
	Ready         bool              `json:"ready"`
	RestartCount  int32             `json:"restartCount"`
	SecurityBrief []string          `json:"securityBrief,omitempty"`
}

type redactedVolume struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Source string `json:"source,omitempty"` // referenced object name, never contents
}

// sensitiveAnnotations are dropped outright. last-applied-configuration is the
// important one: it is a verbatim copy of the submitted manifest, so it
// reintroduces every env value the structured rules just removed.
var sensitiveAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration",
	"kubectl.kubernetes.io/original-manifest",
}

// annotationKeyLooksSensitive matches annotation keys that commonly hold
// credentials or tokens injected by controllers.
var annotationKeyLooksSensitive = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|apikey|api-key|private-key)`)

// counter accumulates redactions by rule so the envelope reports one line per
// rule rather than one per value.
type counter struct {
	order []string
	byKey map[string]*Redaction
}

func newCounter() *counter { return &counter{byKey: map[string]*Redaction{}} }

func (c *counter) add(rule, path string, bytes int) {
	key := rule + "|" + path
	r, ok := c.byKey[key]
	if !ok {
		r = &Redaction{Rule: rule, Path: path}
		c.byKey[key] = r
		c.order = append(c.order, key)
	}
	r.Count++
	r.Bytes += bytes
}

func (c *counter) list() []Redaction {
	out := make([]Redaction, 0, len(c.order))
	for _, k := range c.order {
		out = append(out, *c.byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// PodInput is the raw material ForPod works from — the same data the assistant
// used to receive verbatim.
type PodInput struct {
	Pod       *corev1.Pod
	Events    []corev1.Event
	Logs      string
	LogLines  int    // how many lines were requested from the API server
	Container string // container the logs came from, when pinned
}

// ForPod builds the redacted troubleshooting payload and its envelope.
func ForPod(in PodInput) (*Payload, error) {
	if in.Pod == nil {
		return nil, fmt.Errorf("no pod supplied")
	}
	c := newCounter()
	pod := in.Pod

	rp := redactedPod{
		Kind:            "Pod",
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		UID:             string(pod.UID),
		Labels:          pod.Labels,
		NodeName:        pod.Spec.NodeName,
		Phase:           string(pod.Status.Phase),
		Reason:          pod.Status.Reason,
		Message:         pod.Status.Message,
		QOSClass:        string(pod.Status.QOSClass),
		RestartPolicy:   string(pod.Spec.RestartPolicy),
		ServiceAccount:  pod.Spec.ServiceAccountName,
		Tolerations:     len(pod.Spec.Tolerations),
		HasNodeSelector: len(pod.Spec.NodeSelector) > 0,
		HasAffinity:     pod.Spec.Affinity != nil,
	}
	if !pod.CreationTimestamp.IsZero() {
		rp.Created = pod.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	rp.Annotations = redactAnnotations(pod.Annotations, c)
	for _, o := range pod.OwnerReferences {
		rp.Owners = append(rp.Owners, o.Kind+"/"+o.Name)
	}
	if sc := pod.Spec.SecurityContext; sc != nil {
		rp.SecurityContext = &redactedPodSecurity{
			RunAsNonRoot: sc.RunAsNonRoot, RunAsUser: sc.RunAsUser, FSGroup: sc.FSGroup,
		}
	}
	for _, cond := range pod.Status.Conditions {
		rp.Conditions = append(rp.Conditions, redactedCondition{
			Type: string(cond.Type), Status: string(cond.Status),
			Reason: cond.Reason, Message: cond.Message,
		})
	}

	statuses := map[string]corev1.ContainerStatus{}
	for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...),
		pod.Status.InitContainerStatuses...) {
		statuses[cs.Name] = cs
	}
	for _, ct := range pod.Spec.Containers {
		rp.Containers = append(rp.Containers, redactContainer(ct, statuses[ct.Name], c))
	}
	for _, ct := range pod.Spec.InitContainers {
		rp.InitContainers = append(rp.InitContainers, redactContainer(ct, statuses[ct.Name], c))
	}
	for _, v := range pod.Spec.Volumes {
		rp.Volumes = append(rp.Volumes, redactVolume(v))
	}

	// Events: newest first, capped by count and by bytes, messages scrubbed.
	evs, evScrub := redactEvents(in.Events, c)

	logs, window := redactLogs(in.Logs, in.LogLines, in.Container, c)
	window.ScrubbedHits += evScrub

	payload := map[string]interface{}{
		"pod":    rp,
		"events": evs,
	}
	fields := []string{"pod.metadata", "pod.spec (redacted)", "pod.status", "events"}
	if logs != "" {
		payload["logs"] = logs
		fields = append(fields, "logs")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode evidence: %w", err)
	}
	env := Envelope{
		Purpose: "troubleshoot",
		Resource: ResourceRef{
			Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID),
		},
		Fields:     fields,
		Redactions: c.list(),
		Events:     len(evs),
		Bytes:      len(body),
		Hash:       hashOf(body),
		PreparedAt: time.Now().UTC(),
	}
	if logs != "" {
		env.LogWindow = &window
	}
	return &Payload{JSON: string(body), Envelope: env}, nil
}

// LogInput is the raw material ForLogs works from.
type LogInput struct {
	Namespace  string
	Pod        string
	Container  string
	Logs       string
	LinesAsked int
}

// ForLogs builds the redacted log-summary payload and its envelope.
func ForLogs(in LogInput) (*Payload, error) {
	c := newCounter()
	logs, window := redactLogs(in.Logs, in.LinesAsked, in.Container, c)

	body, err := json.Marshal(map[string]interface{}{
		"pod":       in.Namespace + "/" + in.Pod,
		"container": in.Container,
		"logs":      logs,
	})
	if err != nil {
		return nil, fmt.Errorf("encode evidence: %w", err)
	}
	env := Envelope{
		Purpose:    "logsummary",
		Resource:   ResourceRef{Kind: "Pod", Namespace: in.Namespace, Name: in.Pod},
		Fields:     []string{"logs"},
		Redactions: c.list(),
		LogWindow:  &window,
		Bytes:      len(body),
		Hash:       hashOf(body),
		PreparedAt: time.Now().UTC(),
	}
	return &Payload{JSON: string(body), Envelope: env}, nil
}

func redactAnnotations(in map[string]string, c *counter) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if isSensitiveAnnotation(k) {
			c.add("annotation-dropped", "metadata.annotations["+k+"]", len(v))
			continue
		}
		scrubbed, hits := ScrubText(v)
		if hits > 0 {
			c.add("annotation-scrubbed", "metadata.annotations", len(v)-len(scrubbed))
		}
		out[k] = scrubbed
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isSensitiveAnnotation(k string) bool {
	for _, s := range sensitiveAnnotations {
		if k == s {
			return true
		}
	}
	return annotationKeyLooksSensitive.MatchString(k)
}

func redactContainer(ct corev1.Container, st corev1.ContainerStatus, c *counter) redactedContainer {
	rc := redactedContainer{
		Name:            ct.Name,
		Image:           ct.Image,
		ImagePullPolicy: string(ct.ImagePullPolicy),
		Ready:           st.Ready,
		RestartCount:    st.RestartCount,
	}
	// command and args are free text under the operator's control and are a
	// routine hiding place for credentials: `--password=…`, a connection string,
	// or a shell one-liner that echoes a token. They are worth sending — an
	// entrypoint often explains the failure — but only scrubbed.
	rc.Command = scrubArgs(ct.Command, "spec.containers[].command", c)
	rc.Args = scrubArgs(ct.Args, "spec.containers[].args", c)

	for _, p := range ct.Ports {
		rc.Ports = append(rc.Ports, fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol))
	}

	// The rule that matters: an inline env value never leaves, regardless of
	// what it looks like. The name stays so the model can still reason about
	// configuration.
	for _, e := range ct.Env {
		switch {
		case e.Value != "":
			c.add("env-value", "spec.containers[].env[].value", len(e.Value))
			rc.EnvNames = append(rc.EnvNames, e.Name+"="+redactedMark)
		case e.ValueFrom != nil:
			rc.EnvNames = append(rc.EnvNames, e.Name)
			rc.ValueFrom = append(rc.ValueFrom, e.Name+" ← "+valueFromRef(e.ValueFrom))
		default:
			rc.EnvNames = append(rc.EnvNames, e.Name+"=\"\"")
		}
	}
	for _, ef := range ct.EnvFrom {
		if ef.SecretRef != nil {
			rc.EnvFromRefs = append(rc.EnvFromRefs, "Secret/"+ef.SecretRef.Name)
		}
		if ef.ConfigMapRef != nil {
			rc.EnvFromRefs = append(rc.EnvFromRefs, "ConfigMap/"+ef.ConfigMapRef.Name)
		}
	}

	if len(ct.Resources.Requests) > 0 || len(ct.Resources.Limits) > 0 {
		rc.Resources = map[string]string{}
		for k, v := range ct.Resources.Requests {
			rc.Resources["requests."+string(k)] = v.String()
		}
		for k, v := range ct.Resources.Limits {
			rc.Resources["limits."+string(k)] = v.String()
		}
	}
	for _, m := range ct.VolumeMounts {
		ro := ""
		if m.ReadOnly {
			ro = " (ro)"
		}
		rc.Mounts = append(rc.Mounts, m.Name+" → "+m.MountPath+ro)
	}
	if ct.LivenessProbe != nil {
		rc.Probes = append(rc.Probes, "liveness")
	}
	if ct.ReadinessProbe != nil {
		rc.Probes = append(rc.Probes, "readiness")
	}
	if ct.StartupProbe != nil {
		rc.Probes = append(rc.Probes, "startup")
	}
	if sc := ct.SecurityContext; sc != nil {
		if sc.RunAsNonRoot != nil {
			rc.SecurityBrief = append(rc.SecurityBrief, fmt.Sprintf("runAsNonRoot=%v", *sc.RunAsNonRoot))
		}
		if sc.Privileged != nil && *sc.Privileged {
			rc.SecurityBrief = append(rc.SecurityBrief, "privileged=true")
		}
		if sc.ReadOnlyRootFilesystem != nil {
			rc.SecurityBrief = append(rc.SecurityBrief, fmt.Sprintf("readOnlyRootFilesystem=%v", *sc.ReadOnlyRootFilesystem))
		}
	}
	rc.State, rc.LastState = containerStates(st)
	return rc
}

// scrubArgs runs the free-text scrubbers over an argv slice.
func scrubArgs(in []string, path string, c *counter) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, a := range in {
		s, hits := ScrubText(a)
		if hits > 0 {
			c.add("arg-scrubbed", path, len(a)-len(s))
		}
		out = append(out, s)
	}
	return out
}

// valueFromRef names the source of an env var without reading it. The name of a
// Secret is a reference, not a body, and a diagnosis frequently turns on it
// ("the Secret it reads from does not exist").
func valueFromRef(vf *corev1.EnvVarSource) string {
	switch {
	case vf.SecretKeyRef != nil:
		return "Secret/" + vf.SecretKeyRef.Name + ":" + vf.SecretKeyRef.Key
	case vf.ConfigMapKeyRef != nil:
		return "ConfigMap/" + vf.ConfigMapKeyRef.Name + ":" + vf.ConfigMapKeyRef.Key
	case vf.FieldRef != nil:
		return "field:" + vf.FieldRef.FieldPath
	case vf.ResourceFieldRef != nil:
		return "resource:" + vf.ResourceFieldRef.Resource
	}
	return "unknown"
}

func containerStates(st corev1.ContainerStatus) (string, string) {
	describe := func(s corev1.ContainerState) string {
		switch {
		case s.Waiting != nil:
			if s.Waiting.Reason != "" {
				return "Waiting:" + s.Waiting.Reason
			}
			return "Waiting"
		case s.Running != nil:
			return "Running"
		case s.Terminated != nil:
			return fmt.Sprintf("Terminated:%s(exit %d)", s.Terminated.Reason, s.Terminated.ExitCode)
		}
		return ""
	}
	return describe(st.State), describe(st.LastTerminationState)
}

func redactVolume(v corev1.Volume) redactedVolume {
	rv := redactedVolume{Name: v.Name}
	switch {
	case v.Secret != nil:
		// The name of the mounted Secret, never its contents. KubeAura does not
		// read Secret data anywhere, and this path must not become the first
		// place that it does.
		rv.Type, rv.Source = "Secret", v.Secret.SecretName
	case v.ConfigMap != nil:
		rv.Type, rv.Source = "ConfigMap", v.ConfigMap.Name
	case v.PersistentVolumeClaim != nil:
		rv.Type, rv.Source = "PVC", v.PersistentVolumeClaim.ClaimName
	case v.EmptyDir != nil:
		rv.Type = "EmptyDir"
	case v.HostPath != nil:
		rv.Type, rv.Source = "HostPath", v.HostPath.Path
	case v.Projected != nil:
		rv.Type = "Projected"
	case v.DownwardAPI != nil:
		rv.Type = "DownwardAPI"
	default:
		rv.Type = "Other"
	}
	return rv
}

type redactedEvent struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int32  `json:"count,omitempty"`
	Last    string `json:"last,omitempty"`
}

// redactEvents caps events by count and by total message bytes, newest first,
// and scrubs the messages. Events were previously sent in full: a pod with a
// noisy controller could contribute more text than its logs.
func redactEvents(in []corev1.Event, c *counter) ([]redactedEvent, int) {
	if len(in) == 0 {
		return []redactedEvent{}, 0
	}
	evs := append([]corev1.Event(nil), in...)
	sort.SliceStable(evs, func(i, j int) bool {
		return eventTime(evs[i]).After(eventTime(evs[j]))
	})

	out := make([]redactedEvent, 0, len(evs))
	hits, used, dropped, droppedBytes := 0, 0, 0, 0
	for _, e := range evs {
		if len(out) >= MaxEvents || used >= MaxEventBytes {
			dropped++
			droppedBytes += len(e.Message)
			continue
		}
		msg, h := ScrubText(e.Message)
		hits += h
		used += len(msg)
		re := redactedEvent{Type: e.Type, Reason: e.Reason, Message: msg, Count: e.Count}
		if t := eventTime(e); !t.IsZero() {
			re.Last = t.UTC().Format(time.RFC3339)
		}
		out = append(out, re)
	}
	if dropped > 0 {
		c.add("event-cap", "events", droppedBytes)
	}
	if hits > 0 {
		c.add("event-message-scrubbed", "events[].message", 0)
	}
	return out, hits
}

func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

// redactLogs scrubs and byte-caps log text, returning the window description.
func redactLogs(logs string, linesAsked int, container string, c *counter) (string, LogWindow) {
	w := LogWindow{Container: container, LinesAsked: linesAsked}
	if logs == "" {
		return "", w
	}
	original := len(logs)
	trimmed, truncated := tailBytes(logs, MaxLogBytes)
	if truncated {
		c.add("log-byte-cap", "logs", original-len(trimmed))
	}
	scrubbed, hits := ScrubText(trimmed)
	if hits > 0 {
		c.add("log-scrubbed", "logs", len(trimmed)-len(scrubbed))
	}
	w.Truncated = truncated
	w.ScrubbedHits = hits
	w.Bytes = len(scrubbed)
	w.LinesSent = strings.Count(strings.TrimRight(scrubbed, "\n"), "\n") + 1
	if scrubbed == "" {
		w.LinesSent = 0
	}
	return scrubbed, w
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Short returns the first 12 hex characters of a hash, which is what the audit
// trail and the UI display.
func Short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
