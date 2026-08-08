package evidence

import (
	"fmt"
	"strings"
	"time"

	sigyaml "sigs.k8s.io/yaml"
)

// MaxManifestBytes caps a manifest review. A manifest is authored, not
// generated, so it is far smaller than a log tail — but a ConfigMap can carry
// an embedded file and a CRD can carry a schema, and neither has a natural
// bound.
const MaxManifestBytes = 64 << 10

// ManifestInput is the raw material ForManifest works from. YAML is either the
// live object's manifest or one the operator pasted; both are treated the same,
// because a pasted Secret leaks exactly as well as a fetched one.
type ManifestInput struct {
	YAML      string
	Kind      string
	Namespace string
	Name      string
}

// ForManifest builds the redacted review payload and its envelope.
//
// Unlike ForPod, this cannot be an allow-list: the whole point of review is to
// show the model the manifest the operator wrote, including fields KubeAura has
// no model for (CRDs, admission annotations, sidecar injection config). So it
// is a deny-list applied by walking the document — but the rules are the same
// rules, matched by key rather than by Go field, and every firing is counted.
func ForManifest(in ManifestInput) (*Payload, error) {
	if strings.TrimSpace(in.YAML) == "" {
		return nil, fmt.Errorf("no manifest supplied")
	}
	c := newCounter()

	// Documents that do not parse are still worth reviewing — a syntax error is
	// a legitimate thing to ask about — but an unparsed document cannot be
	// walked, so it gets the free-text scrubbers and nothing more.
	var doc interface{}
	if err := sigyaml.Unmarshal([]byte(in.YAML), &doc); err != nil {
		scrubbed, hits := ScrubText(in.YAML)
		if hits > 0 {
			c.add("free-text-secret", "manifest", len(in.YAML)-len(scrubbed))
		}
		return manifestPayload(scrubbed, in, c, []string{"manifest (unparsed, scrubbed)"})
	}

	kind := documentKind(doc)
	if kind == "" {
		kind = in.Kind
	}
	redacted := redactNode(doc, "", kind, c)

	out, err := sigyaml.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("encode manifest evidence: %w", err)
	}
	return manifestPayload(string(out), in, c, []string{"manifest (redacted)"})
}

func manifestPayload(body string, in ManifestInput, c *counter, fields []string) (*Payload, error) {
	// headBytes, not tailBytes: a manifest's identity lives at the top. Losing
	// the tail of a long spec costs less than losing kind and metadata.
	trimmed, truncated := headBytes(body, MaxManifestBytes)
	if truncated {
		fields = append(fields, "truncated at cap")
	}
	kind := in.Kind
	if kind == "" {
		kind = "Manifest"
	}
	env := Envelope{
		Purpose:    "review",
		Resource:   ResourceRef{Kind: kind, Namespace: in.Namespace, Name: in.Name},
		Fields:     fields,
		Redactions: c.list(),
		Bytes:      len(trimmed),
		Hash:       hashOf([]byte(trimmed)),
		Truncated:  truncated,
		PreparedAt: time.Now().UTC(),
	}
	return &Payload{JSON: trimmed, Envelope: env}, nil
}

// documentKind reads .kind off a parsed document, so the Secret rule fires on
// what the document says it is rather than on what the caller claimed.
func documentKind(doc interface{}) string {
	m, ok := doc.(map[string]interface{})
	if !ok {
		return ""
	}
	k, _ := m["kind"].(string)
	return k
}

// redactNode walks a parsed manifest applying the same rules ForPod applies to
// a typed pod. docKind is the document's own kind, needed because the Secret
// rule keys off the document rather than off the local path.
func redactNode(node interface{}, path, docKind string, c *counter) interface{} {
	switch v := node.(type) {
	case map[string]interface{}:
		return redactMap(v, path, docKind, c)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, redactNode(item, path+"[]", docKind, c))
		}
		return out
	case string:
		s, hits := ScrubText(v)
		if hits > 0 {
			c.add("free-text-secret", trimPath(path), len(v)-len(s))
		}
		return s
	default:
		return node
	}
}

func redactMap(m map[string]interface{}, path, docKind string, c *counter) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, val := range m {
		child := k
		if path != "" {
			child = path + "." + k
		}
		switch {
		// A Secret's body never leaves, whatever it looks like. Keys stay: a
		// review turns on which keys exist ("the Deployment reads DB_URL, the
		// Secret only defines DB_HOST"), never on their values.
		case docKind == "Secret" && (k == "data" || k == "stringData"):
			out[k] = redactValues(val, "secret-data", child, c)

		// managedFields is server noise, and last-applied-configuration is a
		// verbatim copy of the original manifest — the annotation rule catches
		// the latter, this catches the former.
		case k == "managedFields":
			continue

		case k == "annotations":
			if am, ok := stringMap(val); ok {
				// An annotations block whose every entry was dropped is
				// omitted rather than emitted as null: "annotations: null" is
				// noise the model has to read past.
				if red := redactAnnotations(am, c); len(red) > 0 {
					out[k] = red
				}
				continue
			}
			out[k] = redactNode(val, child, docKind, c)

		// An inline env value goes regardless of shape — the ForPod rule,
		// matched structurally so it applies to every workload kind and to
		// pod templates nested at any depth.
		case k == "env":
			out[k] = redactEnvList(val, child, c)

		default:
			out[k] = redactNode(val, child, docKind, c)
		}
	}
	return out
}

// redactEnvList applies the env-value rule to a list of {name, value} maps,
// leaving valueFrom alone: a secretKeyRef is a reference, not a body.
func redactEnvList(val interface{}, path string, c *counter) interface{} {
	list, ok := val.([]interface{})
	if !ok {
		return redactNode(val, path, "", c)
	}
	out := make([]interface{}, 0, len(list))
	for _, item := range list {
		e, ok := item.(map[string]interface{})
		if !ok {
			out = append(out, item)
			continue
		}
		cp := make(map[string]interface{}, len(e))
		for k, v := range e {
			cp[k] = v
		}
		if s, ok := cp["value"].(string); ok && s != "" {
			c.add("env-value", trimPath(path)+"[].value", len(s))
			cp["value"] = redactedMark
		}
		out = append(out, cp)
	}
	return out
}

// redactValues replaces every value in a map while keeping its keys.
func redactValues(val interface{}, rule, path string, c *counter) interface{} {
	m, ok := val.(map[string]interface{})
	if !ok {
		return val
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			c.add(rule, trimPath(path)+"[]", len(s))
			out[k] = redactedMark
			continue
		}
		out[k] = redactedMark
	}
	return out
}

func stringMap(val interface{}) (map[string]string, bool) {
	m, ok := val.(map[string]interface{})
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

// trimPath collapses list indices so the envelope reports one line per rule per
// field rather than one per element.
func trimPath(p string) string {
	if p == "" {
		return "manifest"
	}
	return p
}

// headBytes trims s to at most max bytes, keeping the beginning and cutting on
// a line boundary. The mirror of tailBytes, for content whose useful end is the
// start.
func headBytes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i+1]
	}
	return cut, true
}
