package ai

import "testing"

func TestConfigureProviderSelection(t *testing.T) {
	cases := []struct {
		name     string
		s        Settings
		enabled  bool
		provider string // substring expected in ProviderName (empty = skip)
		model    string // expected model (empty = skip)
	}{
		{"disabled when nothing set", Settings{}, false, "none", ""},
		{"anthropic via key", Settings{AnthropicKey: "sk-x"}, true, "Anthropic", "claude-opus-5"},
		{"explicit ollama defaults", Settings{Provider: "ollama"}, true, "Ollama", "llama3.2"},
		{"explicit ollama model", Settings{Provider: "ollama", Model: "qwen2.5:3b"}, true, "Ollama", "qwen2.5:3b"},
		{"openai via base url", Settings{OpenAIBaseURL: "http://localhost:1234/v1", Model: "local"}, true, "Custom", "local"},
		{"auto anthropic beats ollama", Settings{AnthropicKey: "sk-x", OllamaHost: "http://localhost:11434"}, true, "Anthropic", ""},
		{"unknown provider disabled", Settings{Provider: "bogus"}, false, "none", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Configure(tc.s)
			if c.Enabled() != tc.enabled {
				t.Fatalf("Enabled()=%v, want %v", c.Enabled(), tc.enabled)
			}
			if tc.provider != "" && !contains(c.ProviderName(), tc.provider) {
				t.Errorf("ProviderName()=%q, want substring %q", c.ProviderName(), tc.provider)
			}
			if tc.model != "" && c.ModelName() != tc.model {
				t.Errorf("ModelName()=%q, want %q", c.ModelName(), tc.model)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
