// Package config resolves KubeMind's runtime settings from three layers, in
// increasing order of precedence: an optional config file, the environment,
// and command-line flags. Every setting has a working default, so a first run
// needs no configuration at all.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// Config holds runtime configuration for KubeMind.
type Config struct {
	Addr        string // HTTP listen address
	Kubeconfig  string // path to kubeconfig; empty => in-cluster or default
	Context     string // kube context to start on; empty => current-context
	AllowRemote bool   // permit non-loopback Host headers (shared deployments)
	NoBrowser   bool   // don't auto-open a browser on start
	AI          AIConfig
	RAG         RAGConfig

	// Source records where the file layer was read from, for the startup
	// banner. Empty when no config file was found.
	Source string
}

// RAGConfig controls docs retrieval injected into AI answers.
type RAGConfig struct {
	DocsEnabled bool   // KUBEMIND_DOCS_RAG_ENABLED (default true)
	DocsURL     string // KUBEMIND_DOCS_URL (unset = local docs fallback only)
	DocsPath    string // KUBEMIND_DOCS_PATH fallback path (default "docs")
	DocsTopK    int    // KUBEMIND_DOCS_TOPK (default 4)
}

// AIConfig selects and configures the AI Assistant's model backend. KubeMind
// supports a hosted API (Anthropic), a local model server (Ollama), or any
// OpenAI-compatible endpoint — so it can run fully offline on the operator's
// own machine.
type AIConfig struct {
	Provider      string // KUBEMIND_AI_PROVIDER: anthropic|ollama|openai (empty = auto-detect)
	Model         string // KUBEMIND_AI_MODEL: model id override
	AnthropicKey  string // ANTHROPIC_API_KEY
	OllamaHost    string // OLLAMA_HOST (default http://localhost:11434 when provider=ollama)
	OpenAIBaseURL string // OPENAI_BASE_URL (e.g. http://localhost:1234/v1)
	OpenAIKey     string // OPENAI_API_KEY (optional for local servers)
}

// Flags carries command-line overrides. Zero values mean "not set", so they
// fall through to the environment and file layers.
type Flags struct {
	Addr        string
	Kubeconfig  string
	Context     string
	AllowRemote bool
	NoBrowser   bool
	AIProvider  string
	AIModel     string
}

// file mirrors Config in the YAML shape users write. Secrets are deliberately
// absent: API keys come from the environment or from an apiKeyCommand, never
// from a plaintext file KubeMind manages.
type file struct {
	Addr        string `json:"addr,omitempty"`
	Kubeconfig  string `json:"kubeconfig,omitempty"`
	Context     string `json:"context,omitempty"`
	AllowRemote bool   `json:"allowRemote,omitempty"`
	NoBrowser   bool   `json:"noBrowser,omitempty"`

	AI struct {
		Provider      string `json:"provider,omitempty"`
		Model         string `json:"model,omitempty"`
		OllamaHost    string `json:"ollamaHost,omitempty"`
		OpenAIBaseURL string `json:"openaiBaseURL,omitempty"`
		// APIKeyCommand is run to obtain the key, so it can live in a
		// keychain or password manager instead of on disk. Example:
		//   apiKeyCommand: security find-generic-password -w -s kubemind
		APIKeyCommand string `json:"apiKeyCommand,omitempty"`
	} `json:"ai,omitempty"`

	Docs struct {
		Enabled *bool  `json:"enabled,omitempty"`
		URL     string `json:"url,omitempty"`
		Path    string `json:"path,omitempty"`
		TopK    int    `json:"topK,omitempty"`
	} `json:"docs,omitempty"`
}

// DefaultAddr binds loopback only. KubeMind has no authentication of its own
// and exposes mutating endpoints (apply, delete, exec), so reaching the wider
// network has to be a deliberate act — see AllowRemote.
//
// Port 7654 rather than the usual 8080: a developer machine running a
// Kubernetes tool very likely already has something on 8080 (Tomcat, Spring
// Boot, Argo CD port-forwards, half of Docker Hub), and on macOS ports 5000
// and 7000 are taken by AirPlay. 7654 is easy to type, easy to remember, and
// outside the crowded 3000/8000/9000 development bands. If it is busy anyway,
// main() steps to the next free port rather than failing.
const DefaultAddr = "127.0.0.1:7654"

// Load resolves configuration from file, environment, and flags.
func Load(f Flags) (Config, error) {
	fc, src, err := loadFile()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Addr:        pick(f.Addr, env("ADDR"), fc.Addr, DefaultAddr),
		Kubeconfig:  pick(f.Kubeconfig, os.Getenv("KUBECONFIG"), fc.Kubeconfig, ""),
		Context:     pick(f.Context, env("CONTEXT"), fc.Context, ""),
		AllowRemote: f.AllowRemote || truthy(env("ALLOW_REMOTE")) || fc.AllowRemote,
		NoBrowser:   f.NoBrowser || env("NO_BROWSER") != "" || fc.NoBrowser,
		Source:      src,
		RAG: RAGConfig{
			DocsEnabled: boolPick(fc.Docs.Enabled, env("DOCS_RAG_ENABLED"), true),
			DocsURL:     pick("", env("DOCS_URL"), fc.Docs.URL, ""),
			DocsPath:    pick("", env("DOCS_PATH"), fc.Docs.Path, "docs"),
			DocsTopK:    intPick(env("DOCS_TOPK"), fc.Docs.TopK, 4),
		},
	}

	// Back-compat: ANTHROPIC_MODEL still works as a model override.
	envModel := pick(env("AI_MODEL"), os.Getenv("ANTHROPIC_MODEL"))
	cfg.AI = AIConfig{
		Provider:      pick(f.AIProvider, env("AI_PROVIDER"), fc.AI.Provider),
		Model:         pick(f.AIModel, envModel, fc.AI.Model),
		AnthropicKey:  os.Getenv("ANTHROPIC_API_KEY"),
		OllamaHost:    pick(os.Getenv("OLLAMA_HOST"), fc.AI.OllamaHost),
		OpenAIBaseURL: pick(os.Getenv("OPENAI_BASE_URL"), fc.AI.OpenAIBaseURL),
		OpenAIKey:     os.Getenv("OPENAI_API_KEY"),
	}

	// A key from the environment always wins; apiKeyCommand exists for the
	// desktop app, which is launched from Finder/Explorer and so inherits no
	// shell environment at all.
	if fc.AI.APIKeyCommand != "" && cfg.AI.AnthropicKey == "" && cfg.AI.OpenAIKey == "" {
		key, kerr := runKeyCommand(fc.AI.APIKeyCommand)
		if kerr != nil {
			return cfg, fmt.Errorf("config %s: apiKeyCommand failed: %w", src, kerr)
		}
		if strings.EqualFold(cfg.AI.Provider, "openai") {
			cfg.AI.OpenAIKey = key
		} else {
			cfg.AI.AnthropicKey = key
		}
	}

	return cfg, nil
}

// Path returns the config file location KubeMind reads, whether or not it
// exists: $KUBEMIND_CONFIG, else $XDG_CONFIG_HOME/kubemind/config.yaml, else
// ~/.config/kubemind/config.yaml.
func Path() string {
	if p := env("CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "kubemind", "config.yaml")
}

func loadFile() (file, string, error) {
	var fc file
	p := Path()
	if p == "" {
		return fc, "", nil
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return fc, "", nil
	}
	if err != nil {
		return fc, p, fmt.Errorf("read config %s: %w", p, err)
	}
	if err := yaml.Unmarshal(b, &fc); err != nil {
		return fc, p, fmt.Errorf("parse config %s: %w", p, err)
	}
	return fc, p, nil
}

// runKeyCommand executes a shell command and returns its trimmed stdout.
func runKeyCommand(cmdline string) (string, error) {
	shell, flag := "/bin/sh", "-c"
	if os.Getenv("OS") == "Windows_NT" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.Command(shell, flag, cmdline)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(out))
	if key == "" {
		return "", fmt.Errorf("command produced no output")
	}
	return key, nil
}

// env reads a KUBEMIND_-prefixed variable, falling back to the KUBEPILOT_
// name the project used before it was renamed. The fallback is a courtesy for
// existing shell profiles and scripts; drop it at 1.0.
func env(suffix string) string {
	if v := os.Getenv("KUBEMIND_" + suffix); v != "" {
		return v
	}
	if v := os.Getenv("KUBEPILOT_" + suffix); v != "" {
		deprecatedOnce.Do(func() {
			fmt.Fprintf(os.Stderr,
				"note: KUBEPILOT_* environment variables are deprecated; rename them to KUBEMIND_*\n")
		})
		return v
	}
	return ""
}

var deprecatedOnce sync.Once

// pick returns the first non-empty value in precedence order.
func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolPick(fileVal *bool, env string, def bool) bool {
	if env != "" {
		return env != "0" && !strings.EqualFold(env, "false")
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

func intPick(env string, fileVal, def int) int {
	if env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	if fileVal > 0 {
		return fileVal
	}
	return def
}

func truthy(s string) bool {
	return s != "" && s != "0" && !strings.EqualFold(s, "false")
}

// Example is the annotated starter config written by `kubemind config init`.
func Example() string {
	return fmt.Sprintf(`# KubeMind configuration — every key is optional.
# Generated %s. Docs: https://github.com/devganeshg/kubemind#configuration
#
# Precedence: command-line flags > environment variables > this file.

# Listen address. Loopback by default: KubeMind has no authentication and can
# apply, delete, and exec, so only widen this behind an authenticating proxy.
addr: %s

# Kube context to start on. Empty uses your kubeconfig's current-context; you
# can always switch clusters from the header dropdown.
context: ""

# Set true only when serving others (in-cluster behind ingress auth). This
# disables the loopback-only Host check that blocks DNS-rebinding attacks.
allowRemote: false

# Don't open a browser on start.
noBrowser: false

ai:
  # anthropic | ollama | openai — omit to auto-detect.
  provider: ollama
  model: llama3.2
  ollamaHost: http://localhost:11434
  # openaiBaseURL: http://localhost:1234/v1

  # API keys are never stored in this file. Set ANTHROPIC_API_KEY /
  # OPENAI_API_KEY in your environment, or have KubeMind fetch the key from
  # your keychain at startup — useful for the desktop app, which inherits no
  # shell environment when launched from Finder or Explorer:
  #
  #   macOS:     apiKeyCommand: security find-generic-password -w -s kubemind
  #   Linux:     apiKeyCommand: secret-tool lookup service kubemind
  #   1Password: apiKeyCommand: op read "op://Private/Anthropic/credential"

# Ground AI answers in your own MkDocs site (optional).
docs:
  enabled: true
  # url: https://docs.example.com/docs/
  # path: docs
  topK: 4
`, time.Now().Format("2006-01-02"), DefaultAddr)
}
