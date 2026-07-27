package k8s

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Manager holds the set of available kubeconfig contexts and one lazily-built
// Client per context, with a single "active" context. This is multi-cluster the
// operator-tool way: one instance switches between the operator's own contexts,
// not a central server fanning out to many clusters at once.
type Manager struct {
	mu         sync.RWMutex
	kubeconfig string
	contexts   []string
	ctxDetails []ContextDetail
	active     string
	clients    map[string]*Client
}

// ContextDetail describes one kubeconfig context with its target cluster and default namespace.
type ContextDetail struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
}

// NewManager loads all contexts from the kubeconfig and builds a Client for the
// current context.
func NewManager(kubeconfig string) (*Manager, error) {
	client, err := New(kubeconfig)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		kubeconfig: kubeconfig,
		active:     client.Context,
		clients:    map[string]*Client{client.Context: client},
	}
	// Enumerate all contexts; fall back to just the active one (e.g. in-cluster).
	if details, _, err := listContextDetails(kubeconfig); err == nil && len(details) > 0 {
		m.ctxDetails = details
		m.contexts = make([]string, 0, len(details))
		for _, d := range details {
			m.contexts = append(m.contexts, d.Name)
		}
	} else {
		m.contexts = []string{client.Context}
		m.ctxDetails = []ContextDetail{{Name: client.Context, Cluster: client.Context}}
	}
	return m, nil
}

// Contexts returns the sorted list of available context names.
func (m *Manager) Contexts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.contexts...)
}

// ContextDetails returns kubeconfig context metadata for UI display.
func (m *Manager) ContextDetails() []ContextDetail {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ContextDetail, len(m.ctxDetails))
	copy(out, m.ctxDetails)
	return out
}

// Active returns the currently selected context name.
func (m *Manager) Active() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Client returns the Client for the active context.
func (m *Manager) Client() *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[m.active]
}

// SetActive switches the active context, building and caching its Client on
// first use.
func (m *Manager) SetActive(name string) error {
	if _, err := m.ClientFor(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = name
	return nil
}

// ClientFor returns the Client for one context, building and caching it on
// first use. It is safe for concurrent use, which is what lets the fleet view
// fan out across every context at once.
func (m *Manager) ClientFor(name string) (*Client, error) {
	m.mu.RLock()
	cl, ok := m.clients[name]
	known := false
	for _, c := range m.contexts {
		if c == name {
			known = true
			break
		}
	}
	m.mu.RUnlock()
	if ok {
		return cl, nil
	}
	if !known {
		return nil, fmt.Errorf("unknown context %q", name)
	}

	// Build outside the lock: creating a client can do DNS and exec credential
	// plugins, and holding the write lock through that would stall every other
	// context in a fleet refresh.
	cfg, ctxName, err := restConfig(m.kubeconfig, name)
	if err != nil {
		return nil, fmt.Errorf("connect to context %q: %w", name, err)
	}
	built, err := newClient(cfg, ctxName)
	if err != nil {
		return nil, fmt.Errorf("connect to context %q: %w", name, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Another goroutine may have won the race; keep whichever landed first so
	// port-forward state stays attached to a single Client per context.
	if existing, ok := m.clients[name]; ok {
		return existing, nil
	}
	m.clients[name] = built
	return built, nil
}

// listContexts reads the kubeconfig and returns the sorted context names plus
// the current-context.
func listContexts(kubeconfig string) ([]string, string, error) {
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(raw.Contexts))
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, raw.CurrentContext, nil
}

// listContextDetails reads kubeconfig and returns sorted context metadata plus current-context.
func listContextDetails(kubeconfig string) ([]ContextDetail, string, error) {
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, "", err
	}
	out := make([]ContextDetail, 0, len(raw.Contexts))
	for n, c := range raw.Contexts {
		out = append(out, ContextDetail{Name: n, Cluster: c.Cluster, Namespace: c.Namespace})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, raw.CurrentContext, nil
}
