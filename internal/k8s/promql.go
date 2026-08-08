package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prometheus querying gives KubeAura the one thing it structurally lacked: a
// past. Every other view answers "what is true now" — metrics come from
// metrics-server, which keeps roughly a minute of data and no history at all.
// "Is this getting worse?" and "was it like this yesterday?" had no answer.
//
// KubeAura already discovers Prometheus (see DiscoverTelemetry); it just never
// asked it anything. Reaching it goes through the API server's service proxy
// rather than a port-forward or a direct dial: the in-cluster service URL is
// not routable from an operator's laptop, but the proxy is, and it reuses the
// kubeconfig credentials that are already loaded. That means this works
// wherever kubectl works, with no configuration and no extra listening socket.

// PromSample is one point in a series.
type PromSample struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// PromSeries is one labelled time series.
type PromSeries struct {
	Labels  map[string]string `json:"labels"`
	Name    string            `json:"name"` // best-effort display name from the labels
	Samples []PromSample      `json:"samples"`
}

// PromResult is a decoded query response.
type PromResult struct {
	Query  string       `json:"query"`
	Series []PromSeries `json:"series"`
	Source string       `json:"source"` // namespace/service that answered
}

// promEnvelope is Prometheus' HTTP response shape. Values arrive as
// [unixSeconds, "stringifiedFloat"], hence the json.RawMessage juggling.
type promEnvelope struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// PrometheusRef returns the discovered Prometheus to query, or false when the
// cluster has none. The first discovered instance wins; a cluster with several
// is unusual enough that guessing is better than refusing.
func (c *Client) PrometheusRef() (TelemetryRef, bool) {
	t, err := c.DiscoverTelemetry()
	if err != nil || t == nil {
		return TelemetryRef{}, false
	}
	for _, s := range t.Sources {
		if s.Kind == "Prometheus" && s.Port > 0 {
			return s, true
		}
	}
	return TelemetryRef{}, false
}

// PromQuery runs an instant query.
func (c *Client) PromQuery(cx context.Context, query string) (*PromResult, error) {
	return c.promDo(cx, "/api/v1/query", map[string]string{"query": query}, query)
}

// PromRange runs a range query, the one that can actually draw a line.
func (c *Client) PromRange(cx context.Context, query string, window time.Duration, step time.Duration) (*PromResult, error) {
	if window <= 0 {
		window = time.Hour
	}
	if step <= 0 {
		// Aim for a few hundred points: enough to see shape, few enough that
		// the payload and the browser both stay comfortable.
		step = window / 240
		if step < 15*time.Second {
			step = 15 * time.Second
		}
	}
	end := time.Now()
	return c.promDo(cx, "/api/v1/query_range", map[string]string{
		"query": query,
		"start": strconv.FormatInt(end.Add(-window).Unix(), 10),
		"end":   strconv.FormatInt(end.Unix(), 10),
		"step":  strconv.Itoa(int(step.Seconds())) + "s",
	}, query)
}

// promDo proxies one request through the API server to the discovered
// Prometheus and decodes the response.
func (c *Client) promDo(cx context.Context, path string, params map[string]string, query string) (*PromResult, error) {
	ref, ok := c.PrometheusRef()
	if !ok {
		return nil, fmt.Errorf("no Prometheus service found in this cluster")
	}
	raw, reqErr := c.cs.CoreV1().
		Services(ref.Namespace).
		ProxyGet("http", ref.Service, strconv.Itoa(int(ref.Port)), path, params).
		DoRaw(cx)

	// Order matters here. Prometheus answers a malformed query with HTTP 400
	// and a precise message ("parse error: unexpected end of input"), but the
	// API server proxy surfaces that to client-go as "the server rejected our
	// request for an unknown reason" — useless to whoever wrote the query. The
	// body usually survives, so decode it first and prefer its message; fall
	// back to the transport error only when there is nothing better.
	var env promEnvelope
	decoded := json.Unmarshal(raw, &env) == nil
	switch {
	case decoded && env.Error != "":
		return nil, fmt.Errorf("Prometheus: %s", env.Error)
	case reqErr != nil:
		return nil, fmt.Errorf("querying %s/%s: %w", ref.Namespace, ref.Service, reqErr)
	case !decoded:
		return nil, fmt.Errorf("decoding response from %s/%s: not a Prometheus API reply", ref.Namespace, ref.Service)
	case env.Status != "success":
		return nil, fmt.Errorf("Prometheus returned status %q", env.Status)
	}

	out := &PromResult{Query: query, Source: ref.Namespace + "/" + ref.Service}
	for _, r := range env.Data.Result {
		s := PromSeries{Labels: r.Metric, Name: seriesName(r.Metric)}
		if len(r.Value) == 2 {
			if p, ok := decodeSample(r.Value); ok {
				s.Samples = append(s.Samples, p)
			}
		}
		for _, v := range r.Values {
			if p, ok := decodeSample(v); ok {
				s.Samples = append(s.Samples, p)
			}
		}
		out.Series = append(out.Series, s)
	}
	sort.Slice(out.Series, func(i, j int) bool { return out.Series[i].Name < out.Series[j].Name })
	return out, nil
}

// decodeSample turns Prometheus' [timestamp, "value"] pair into a point. A
// sample that will not parse is dropped rather than failing the whole series:
// Prometheus emits "NaN" for a gap, and one gap should not lose an hour of
// data.
func decodeSample(pair []json.RawMessage) (PromSample, bool) {
	if len(pair) != 2 {
		return PromSample{}, false
	}
	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return PromSample{}, false
	}
	var str string
	if err := json.Unmarshal(pair[1], &str); err != nil {
		return PromSample{}, false
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		// NaN and Inf are not just useless here, they are dangerous:
		// encoding/json refuses to marshal them, so letting one through would
		// fail the whole response rather than lose one point.
		return PromSample{}, false
	}
	return PromSample{At: time.Unix(int64(ts), 0).UTC(), Value: v}, true
}

// seriesName picks the label that best identifies a series for a chart legend.
func seriesName(labels map[string]string) string {
	for _, k := range []string{"pod", "node", "instance", "container", "deployment", "namespace", "__name__"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// --- Curated queries -------------------------------------------------------
//
// An operator should not have to write PromQL to see whether a workload's
// memory has been climbing all afternoon. These cover the questions the
// existing views raise but cannot answer, using the kube-state-metrics and
// cAdvisor series that ship with every standard Prometheus install.

// PromHistoryKind selects a curated series.
type PromHistoryKind string

const (
	HistoryPodCPU     PromHistoryKind = "pod-cpu"
	HistoryPodMemory  PromHistoryKind = "pod-memory"
	HistoryNodeCPU    PromHistoryKind = "node-cpu"
	HistoryNodeMemory PromHistoryKind = "node-memory"
	HistoryRestarts   PromHistoryKind = "restarts"
)

// PromHistory runs a curated query for a namespace or a single object.
//
// The label matchers are built from quoted, escaped values rather than
// concatenated raw: a namespace or pod name arriving from a query string must
// not be able to close the matcher and append its own selector.
func (c *Client) PromHistory(cx context.Context, kind PromHistoryKind, namespace, name string, window time.Duration) (*PromResult, error) {
	var q string
	switch kind {
	case HistoryPodCPU:
		q = fmt.Sprintf(`sum by (pod) (rate(container_cpu_usage_seconds_total{%s,container!=""}[5m]))`,
			matchers(namespace, "pod", name))
	case HistoryPodMemory:
		q = fmt.Sprintf(`sum by (pod) (container_memory_working_set_bytes{%s,container!=""})`,
			matchers(namespace, "pod", name))
	case HistoryNodeCPU:
		q = `1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))`
	case HistoryNodeMemory:
		q = `1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`
	case HistoryRestarts:
		q = fmt.Sprintf(`sum by (pod) (kube_pod_container_status_restarts_total{%s})`,
			matchers(namespace, "pod", name))
	default:
		return nil, fmt.Errorf("unknown history kind %q", kind)
	}
	return c.PromRange(cx, q, window, 0)
}

// matchers builds a PromQL label selector from untrusted values.
func matchers(namespace, nameLabel, name string) string {
	var parts []string
	if namespace != "" {
		parts = append(parts, `namespace=`+quoteLabel(namespace))
	}
	if name != "" {
		parts = append(parts, nameLabel+`=`+quoteLabel(name))
	}
	if len(parts) == 0 {
		// An unfiltered matcher still needs to be a valid selector.
		return `namespace!=""`
	}
	return strings.Join(parts, ",")
}

// quoteLabel renders a PromQL string literal, escaping what would otherwise
// end it.
func quoteLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}
