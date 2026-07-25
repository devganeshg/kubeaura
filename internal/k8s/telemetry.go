package k8s

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Telemetry reports which observability backends KubeMind auto-discovered in
// the cluster, so the UI can show live metrics and deep-link out to dashboards
// without any configuration (the "zero-config telemetry" requirement).
type Telemetry struct {
	MetricsServer bool           `json:"metricsServer"`
	Sources       []TelemetryRef `json:"sources"` // discovered Prometheus/Grafana/Loki services
}

// TelemetryRef is a discovered observability service and a best-effort URL.
type TelemetryRef struct {
	Kind      string `json:"kind"` // Prometheus | Grafana | Loki | Alertmanager
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Port      int32  `json:"port"`
	URL       string `json:"url"` // in-cluster service URL (deep-link target)
}

// knownTelemetry maps a substring of a service name to the backend it indicates.
var knownTelemetry = []struct {
	match string
	kind  string
}{
	{"prometheus", "Prometheus"},
	{"grafana", "Grafana"},
	{"loki", "Loki"},
	{"alertmanager", "Alertmanager"},
	{"thanos-query", "Prometheus"},
}

// DiscoverTelemetry scans all Services for well-known observability backends and
// checks whether the metrics-server is answering.
func (c *Client) DiscoverTelemetry() (*Telemetry, error) {
	cx, cancel := ctx()
	defer cancel()
	t := &Telemetry{MetricsServer: c.MetricsAvailable(), Sources: []TelemetryRef{}}

	svcs, err := c.cs.CoreV1().Services("").List(cx, metav1.ListOptions{})
	if err != nil {
		return t, nil // discovery is best-effort
	}
	seen := map[string]bool{}
	for _, s := range svcs.Items {
		name := strings.ToLower(s.Name)
		for _, k := range knownTelemetry {
			if !strings.Contains(name, k.match) {
				continue
			}
			// De-dupe headless/operator sub-services by kind+namespace.
			key := k.kind + "/" + s.Namespace
			if seen[key] {
				continue
			}
			var port int32
			if len(s.Spec.Ports) > 0 {
				port = s.Spec.Ports[0].Port
			}
			seen[key] = true
			t.Sources = append(t.Sources, TelemetryRef{
				Kind: k.kind, Namespace: s.Namespace, Service: s.Name, Port: port,
				URL: fmt.Sprintf("http://%s.%s.svc:%d", s.Name, s.Namespace, port),
			})
			break
		}
	}
	return t, nil
}
