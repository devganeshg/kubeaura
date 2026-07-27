package evidence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The tests below are written against the payload text rather than the
// intermediate structs on purpose: what matters is what leaves the machine, and
// that is the marshalled JSON. A refactor that keeps the structs but changes
// what gets encoded should fail here.

func podFixture() *corev1.Pod {
	tru := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "api-7d9",
			Namespace:         "payments",
			UID:               "1f0a3c62-11aa-4bd0-9a0e-5f7c8e2d0001",
			CreationTimestamp: metav1.NewTime(time.Unix(1700000000, 0)),
			Labels:            map[string]string{"app": "api"},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"containers":[{"env":[{"name":"DB_PASSWORD","value":"hunter2-in-the-manifest"}]}]}}`,
				"vault.hashicorp.com/agent-inject-token":           "s.CAESIJ-actual-token",
				"prometheus.io/scrape":                             "true",
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api"}},
		},
		Spec: corev1.PodSpec{
			NodeName:           "ip-10-0-3-14",
			ServiceAccountName: "api-sa",
			Containers: []corev1.Container{{
				Name:  "api",
				Image: "ghcr.io/acme/api:1.4.2",
				Env: []corev1.EnvVar{
					{Name: "DB_PASSWORD", Value: "hunter2-plaintext"},
					{Name: "LOG_LEVEL", Value: "debug"},
					{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "app-secrets"},
							Key:                  "api-key",
						}}},
					{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
					{Name: "EMPTY"},
				},
				EnvFrom: []corev1.EnvFromSource{{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"}},
				}},
				SecurityContext: &corev1.SecurityContext{Privileged: &tru},
			}},
			Volumes: []corev1.Volume{{
				Name:         "creds",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "app-secrets"}},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", Ready: false, RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
}

func TestForPodDropsInlineEnvValues(t *testing.T) {
	p, err := ForPod(PodInput{Pod: podFixture()})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2-plaintext", "debug", "hunter2-in-the-manifest", "s.CAESIJ-actual-token"} {
		if strings.Contains(p.JSON, leak) {
			t.Errorf("payload leaked %q:\n%s", leak, p.JSON)
		}
	}
	// The names must survive — a diagnosis needs to know the variable exists.
	for _, keep := range []string{"DB_PASSWORD", "LOG_LEVEL", "API_KEY"} {
		if !strings.Contains(p.JSON, keep) {
			t.Errorf("payload lost env name %q", keep)
		}
	}
	if !strings.Contains(p.JSON, "Secret/app-secrets:api-key") {
		t.Error("secretKeyRef target should survive: it is a reference, not a body")
	}
	if !strings.Contains(p.JSON, "Secret/db-creds") {
		t.Error("envFrom secretRef name should survive")
	}
	if !strings.Contains(p.JSON, "field:status.podIP") {
		t.Error("fieldRef source should survive")
	}
}

// Found by running this against a live cluster: the entrypoint was being sent
// verbatim, and a shell one-liner is one of the most common places a credential
// sits in a pod spec.
func TestForPodScrubsCommandAndArgs(t *testing.T) {
	pod := podFixture()
	pod.Spec.Containers[0].Command = []string{"sh", "-c",
		"echo DB_PASSWORD=hunter2plaintext; curl -H 'Authorization: Bearer abcdefghijklmnop'; exec /app"}
	pod.Spec.Containers[0].Args = []string{"--dsn", "postgres://admin:s3cr3tpw@db.internal:5432/app"}
	p, err := ForPod(PodInput{Pod: pod})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2plaintext", "s3cr3tpw", "abcdefghijklmnop"} {
		if strings.Contains(p.JSON, leak) {
			t.Errorf("command/args leaked %q:\n%s", leak, p.JSON)
		}
	}
	// The shape must survive — the entrypoint is often what explains a failure.
	for _, keep := range []string{"exec /app", "--dsn", "postgres://", "db.internal"} {
		if !strings.Contains(p.JSON, keep) {
			t.Errorf("command/args lost %q", keep)
		}
	}
	var seen bool
	for _, r := range p.Envelope.Redactions {
		if r.Rule == "arg-scrubbed" {
			seen = true
		}
	}
	if !seen {
		t.Error("envelope must report the argv scrub")
	}
}

func TestForPodDropsSensitiveAnnotations(t *testing.T) {
	p, err := ForPod(PodInput{Pod: podFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "last-applied-configuration") {
		t.Error("last-applied-configuration must be dropped: it re-adds every env value")
	}
	if strings.Contains(p.JSON, "agent-inject-token") {
		t.Error("annotation key matching the sensitive pattern must be dropped")
	}
	if !strings.Contains(p.JSON, "prometheus.io/scrape") {
		t.Error("ordinary annotations should survive")
	}
	var found bool
	for _, r := range p.Envelope.Redactions {
		if r.Rule == "annotation-dropped" {
			found = true
		}
	}
	if !found {
		t.Error("envelope must report the dropped annotations")
	}
}

func TestForPodSecretVolumeKeepsNameOnly(t *testing.T) {
	p, err := ForPod(PodInput{Pod: podFixture()})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Pod struct {
			Volumes []redactedVolume `json:"volumes"`
		} `json:"pod"`
	}
	if err := json.Unmarshal([]byte(p.JSON), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pod.Volumes) != 1 {
		t.Fatalf("want 1 volume, got %d", len(out.Pod.Volumes))
	}
	v := out.Pod.Volumes[0]
	if v.Type != "Secret" || v.Source != "app-secrets" {
		t.Errorf("want Secret/app-secrets by name, got %+v", v)
	}
}

func TestForPodEnvelopeShape(t *testing.T) {
	pod := podFixture()
	p, err := ForPod(PodInput{Pod: pod, Logs: "boot ok\n", LogLines: 100, Container: "api"})
	if err != nil {
		t.Fatal(err)
	}
	e := p.Envelope
	if e.Purpose != "troubleshoot" {
		t.Errorf("purpose = %q", e.Purpose)
	}
	if e.Resource.UID != string(pod.UID) || e.Resource.Namespace != "payments" {
		t.Errorf("resource ref = %+v", e.Resource)
	}
	if e.Bytes != len(p.JSON) {
		t.Errorf("Bytes %d should equal payload length %d", e.Bytes, len(p.JSON))
	}
	if len(e.Hash) != 64 {
		t.Errorf("hash should be 64 hex chars, got %q", e.Hash)
	}
	if e.LogWindow == nil || e.LogWindow.Container != "api" || e.LogWindow.LinesAsked != 100 {
		t.Errorf("log window = %+v", e.LogWindow)
	}
	var envRule bool
	for _, r := range e.Redactions {
		if r.Rule == "env-value" && r.Count != 2 {
			t.Errorf("want 2 inline env values removed, got %d", r.Count)
		}
		if r.Rule == "env-value" {
			envRule = true
		}
	}
	if !envRule {
		t.Error("envelope must report env-value redactions")
	}
}

func TestHashIsDeterministicAndContentSensitive(t *testing.T) {
	a, err := ForPod(PodInput{Pod: podFixture(), Logs: "same\n"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ForPod(PodInput{Pod: podFixture(), Logs: "same\n"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Envelope.Hash != b.Envelope.Hash {
		t.Error("identical evidence must hash identically — otherwise the audit trail cannot be reproduced")
	}
	c, err := ForPod(PodInput{Pod: podFixture(), Logs: "different\n"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Envelope.Hash == c.Envelope.Hash {
		t.Error("different evidence must hash differently")
	}
}

func TestForPodRejectsNilPod(t *testing.T) {
	if _, err := ForPod(PodInput{}); err == nil {
		t.Fatal("want an error for a nil pod")
	}
}

func TestLogByteCapKeepsTheNewestLines(t *testing.T) {
	// One line per KiB, 64 of them: twice the cap.
	var b strings.Builder
	for i := 0; i < 64; i++ {
		b.WriteString(strings.Repeat("x", 1020))
		b.WriteString("\n")
	}
	b.WriteString("LAST LINE: the failure\n")
	p, err := ForLogs(LogInput{Namespace: "payments", Pod: "api-7d9", Logs: b.String(), LinesAsked: 400})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Envelope.LogWindow.Truncated {
		t.Fatal("window should be marked truncated")
	}
	if p.Envelope.LogWindow.Bytes > MaxLogBytes {
		t.Errorf("sent %d bytes, cap is %d", p.Envelope.LogWindow.Bytes, MaxLogBytes)
	}
	if !strings.Contains(p.JSON, "LAST LINE: the failure") {
		t.Error("the tail must be kept: that is where a failure shows up")
	}
	var capped bool
	for _, r := range p.Envelope.Redactions {
		if r.Rule == "log-byte-cap" {
			capped = true
		}
	}
	if !capped {
		t.Error("envelope must report the byte cap firing")
	}
}

// A single enormous log line is the case a line-based cap misses entirely.
func TestLogByteCapHandlesOneHugeLine(t *testing.T) {
	p, err := ForLogs(LogInput{Pod: "api", Logs: strings.Repeat("y", 200<<10), LinesAsked: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.Envelope.LogWindow.Bytes > MaxLogBytes {
		t.Errorf("sent %d bytes for a single line, cap is %d", p.Envelope.LogWindow.Bytes, MaxLogBytes)
	}
}

func TestEventCapByCount(t *testing.T) {
	var evs []corev1.Event
	for i := 0; i < MaxEvents*3; i++ {
		evs = append(evs, corev1.Event{
			Type: "Warning", Reason: "BackOff", Message: "restarting",
			LastTimestamp: metav1.NewTime(time.Unix(int64(1700000000+i), 0)),
		})
	}
	p, err := ForPod(PodInput{Pod: podFixture(), Events: evs})
	if err != nil {
		t.Fatal(err)
	}
	if p.Envelope.Events > MaxEvents {
		t.Errorf("sent %d events, cap is %d", p.Envelope.Events, MaxEvents)
	}
	var capped bool
	for _, r := range p.Envelope.Redactions {
		if r.Rule == "event-cap" {
			capped = true
		}
	}
	if !capped {
		t.Error("envelope must report the event cap firing")
	}
}

func TestEventCapByBytes(t *testing.T) {
	var evs []corev1.Event
	for i := 0; i < 10; i++ {
		evs = append(evs, corev1.Event{
			Type: "Warning", Message: strings.Repeat("m", 8<<10),
			LastTimestamp: metav1.NewTime(time.Unix(int64(1700000000+i), 0)),
		})
	}
	p, err := ForPod(PodInput{Pod: podFixture(), Events: evs})
	if err != nil {
		t.Fatal(err)
	}
	// The cap is checked before each event, so the last one admitted may cross
	// it; one message of overshoot is the contract.
	if p.Envelope.Events > 3 {
		t.Errorf("byte cap should have admitted ~2 events, got %d", p.Envelope.Events)
	}
}

func TestEventsAreNewestFirst(t *testing.T) {
	evs := []corev1.Event{
		{Reason: "old", LastTimestamp: metav1.NewTime(time.Unix(1000, 0))},
		{Reason: "new", LastTimestamp: metav1.NewTime(time.Unix(9000, 0))},
	}
	p, err := ForPod(PodInput{Pod: podFixture(), Events: evs})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(p.JSON, `"new"`) > strings.Index(p.JSON, `"old"`) {
		t.Error("events should be ordered newest first so the cap drops stale ones")
	}
}

func TestScrubText(t *testing.T) {
	cases := []struct {
		name, in string
		gone     []string // must not appear in the output
		kept     []string // must appear
	}{
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			[]string{"dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"}, []string{redactedMark}},
		{"aws-key", "using AKIAIOSFODNN7EXAMPLE now", []string{"AKIAIOSFODNN7EXAMPLE"}, []string{"using", "now"}},
		{"github", "auth ghp_abcdefghijklmnopqrstuvwxyz0123456789", []string{"ghp_abcdefghijklmnopqrst"}, []string{redactedMark}},
		{"slack", "hook xoxb-1234567890-abcdefghij", []string{"xoxb-1234567890"}, []string{redactedMark}},
		{"authorization", "Authorization: Basic dXNlcjpwYXNz", []string{"dXNlcjpwYXNz"}, []string{redactedMark}},
		// The rule must take the credential and stop. Swallowing the rest of the
		// line mangles a shell entrypoint, which is the field a diagnosis leans
		// on most.
		{"authorization stops at the credential",
			"sh -c \"curl -H 'Authorization: Bearer abcdefghijklmnop'; exec /app --port 8080\"",
			[]string{"abcdefghijklmnop"},
			[]string{"curl -H", "exec /app --port 8080"}},
		{"authorization without a scheme", "authorization=abc123def456; retry", []string{"abc123def456"}, []string{"retry"}},
		{"bearer", "Bearer abcdefghijklmnopqrstuv", []string{"abcdefghijklmnopqrstuv"}, []string{redactedMark}},
		{"url-creds", "dial postgres://admin:s3cr3tpw@db.internal:5432/app",
			[]string{"s3cr3tpw", "admin:"}, []string{"postgres://", "db.internal"}},
		{"key-value", `password="correct horse"`, []string{"correct"}, []string{"password", redactedMark}},
		{"private-key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
			[]string{"MIIEow"}, []string{redactedMark}},
		{"clean", "connection refused to 10.0.0.5:5432", nil, []string{"connection refused", "10.0.0.5:5432"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, hits := ScrubText(tc.in)
			for _, g := range tc.gone {
				if strings.Contains(out, g) {
					t.Errorf("still contains %q: %s", g, out)
				}
			}
			for _, k := range tc.kept {
				if !strings.Contains(out, k) {
					t.Errorf("lost %q: %s", k, out)
				}
			}
			if len(tc.gone) > 0 && hits == 0 {
				t.Error("want a non-zero hit count")
			}
			if len(tc.gone) == 0 && hits != 0 {
				t.Errorf("clean text should not be scrubbed, got %d hits: %s", hits, out)
			}
		})
	}
}

func TestForLogsScrubsAndCounts(t *testing.T) {
	p, err := ForLogs(LogInput{
		Namespace: "payments", Pod: "api-7d9", Container: "api", LinesAsked: 400,
		Logs: "starting\nDB_PASSWORD=hunter2plaintext\nconnected\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "hunter2plaintext") {
		t.Error("a credential printed by the app should be scrubbed from the log text")
	}
	if p.Envelope.LogWindow.ScrubbedHits == 0 {
		t.Error("window must report the scrub")
	}
	if p.Envelope.Purpose != "logsummary" || p.Envelope.Resource.Name != "api-7d9" {
		t.Errorf("envelope = %+v", p.Envelope)
	}
	if p.Envelope.LogWindow.LinesSent != 3 {
		t.Errorf("want 3 lines sent, got %d", p.Envelope.LogWindow.LinesSent)
	}
}

func TestForLogsEmpty(t *testing.T) {
	p, err := ForLogs(LogInput{Pod: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Envelope.LogWindow.LinesSent != 0 || p.Envelope.LogWindow.Bytes != 0 {
		t.Errorf("empty logs should report an empty window, got %+v", p.Envelope.LogWindow)
	}
}

func TestRedactionsSortedByBytes(t *testing.T) {
	c := newCounter()
	c.add("small", "a", 10)
	c.add("big", "b", 500)
	c.add("small", "a", 5)
	got := c.list()
	if got[0].Rule != "big" {
		t.Errorf("want the largest redaction first, got %+v", got)
	}
	if got[1].Count != 2 || got[1].Bytes != 15 {
		t.Errorf("repeat rules should accumulate, got %+v", got[1])
	}
}

func TestShort(t *testing.T) {
	if got := Short("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("Short = %q", got)
	}
	if got := Short("abc"); got != "abc" {
		t.Errorf("Short should pass short input through, got %q", got)
	}
}
