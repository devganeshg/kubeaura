package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	withCleanEnv(t)
	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want the loopback default %q", cfg.Addr, DefaultAddr)
	}
	if cfg.AllowRemote {
		t.Error("AllowRemote defaulted to true; remote exposure must be opt-in")
	}
	if !cfg.RAG.DocsEnabled || cfg.RAG.DocsTopK != 4 {
		t.Errorf("docs defaults = %+v", cfg.RAG)
	}
}

func TestPrecedence(t *testing.T) {
	dir := withCleanEnv(t)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("addr: 1.1.1.1:1\nai:\n  provider: ollama\n  model: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBEMIND_CONFIG", path)

	// File only.
	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "1.1.1.1:1" || cfg.AI.Model != "from-file" {
		t.Errorf("file layer not applied: %+v", cfg)
	}
	if cfg.Source != path {
		t.Errorf("Source = %q, want %q", cfg.Source, path)
	}

	// Env beats file.
	t.Setenv("KUBEMIND_ADDR", "2.2.2.2:2")
	t.Setenv("KUBEMIND_AI_MODEL", "from-env")
	if cfg, err = Load(Flags{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "2.2.2.2:2" || cfg.AI.Model != "from-env" {
		t.Errorf("env did not beat file: %+v", cfg)
	}

	// Flags beat env.
	if cfg, err = Load(Flags{Addr: "3.3.3.3:3", AIModel: "from-flag"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "3.3.3.3:3" || cfg.AI.Model != "from-flag" {
		t.Errorf("flags did not beat env: %+v", cfg)
	}
}

func TestAPIKeyCommand(t *testing.T) {
	dir := withCleanEnv(t)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("ai:\n  provider: anthropic\n  apiKeyCommand: printf sk-from-keychain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBEMIND_CONFIG", path)

	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AI.AnthropicKey != "sk-from-keychain" {
		t.Errorf("AnthropicKey = %q, want the command's output", cfg.AI.AnthropicKey)
	}

	// An environment key wins, so the command is never run.
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	if cfg, err = Load(Flags{}); err != nil {
		t.Fatal(err)
	}
	if cfg.AI.AnthropicKey != "sk-from-env" {
		t.Errorf("AnthropicKey = %q, want the environment to win", cfg.AI.AnthropicKey)
	}
}

func TestDeprecatedEnvPrefix(t *testing.T) {
	withCleanEnv(t)

	// The pre-rename prefix still works, so existing shell profiles don't break.
	t.Setenv("KUBEPILOT_ADDR", "127.0.0.1:1234")
	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:1234" {
		t.Errorf("Addr = %q, want the KUBEPILOT_ fallback to apply", cfg.Addr)
	}

	// The current prefix wins when both are set.
	t.Setenv("KUBEMIND_ADDR", "127.0.0.1:5678")
	if cfg, err = Load(Flags{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:5678" {
		t.Errorf("Addr = %q, want KUBEMIND_ to take precedence", cfg.Addr)
	}
}

func TestExampleParses(t *testing.T) {
	dir := withCleanEnv(t)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(Example()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBEMIND_CONFIG", path)
	if _, err := Load(Flags{}); err != nil {
		t.Fatalf("the generated starter config does not load: %v", err)
	}
}

// withCleanEnv isolates a test from the developer's own environment and
// returns a scratch directory.
func withCleanEnv(t *testing.T) string {
	t.Helper()
	suffixes := []string{
		"CONFIG", "ADDR", "CONTEXT", "ALLOW_REMOTE", "NO_BROWSER",
		"AI_PROVIDER", "AI_MODEL",
		"DOCS_RAG_ENABLED", "DOCS_URL", "DOCS_PATH", "DOCS_TOPK",
	}
	var keys []string
	for _, s := range suffixes {
		// Both prefixes: the deprecated one still feeds Load.
		keys = append(keys, "KUBEMIND_"+s, "KUBEPILOT_"+s)
	}
	keys = append(keys,
		"ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", "OPENAI_API_KEY", "OPENAI_BASE_URL",
		"OLLAMA_HOST", "KUBECONFIG",
	)
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	t.Setenv("KUBEMIND_CONFIG", filepath.Join(dir, "does-not-exist.yaml"))
	return dir
}
