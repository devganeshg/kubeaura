package ai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Connection is a user-defined model backend that can be added, activated, and
// removed at runtime from the UI — so operators can wire up "any model" without
// restarting or editing env vars. Connections live in memory only: API keys are
// never written to disk, honoring the "never persist secrets" requirement.
type Connection struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"` // anthropic | ollama | openai
	BaseURL  string `json:"baseURL"`  // ollama host or openai-compatible base url
	APIKey   string `json:"apiKey"`   // redacted when listed
	Model    string `json:"model"`
	Source   string `json:"source"` // "env" (from startup config) or "user" (added via UI)
}

// redacted returns a copy safe to send to the browser: the API key is replaced
// with a masked placeholder so it's never exposed after being entered.
func (c Connection) redacted() Connection {
	if c.APIKey != "" {
		c.APIKey = "••••••" + lastN(c.APIKey, 4)
	}
	return c
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}

// buildProvider constructs a live Provider from a connection's settings.
func buildProvider(c Connection) (Provider, error) {
	switch strings.ToLower(c.Provider) {
	case "anthropic", "claude":
		if c.APIKey == "" {
			return nil, fmt.Errorf("Anthropic connections require an API key")
		}
		return newAnthropic(c.APIKey, c.Model), nil
	case "ollama", "local":
		return newOllama(c.BaseURL, c.Model), nil
	case "openai", "openai-compatible", "localai", "lmstudio", "vllm", "custom":
		return newOpenAI(c.BaseURL, c.APIKey, c.Model, providerLabel(c.BaseURL)), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (use anthropic, ollama, or openai)", c.Provider)
	}
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- model discovery: list what a backend can serve, so the UI offers a picker ---

// DiscoverModels queries a backend for the models it can serve. Works for Ollama
// (GET /api/tags) and OpenAI-compatible servers (GET /v1/models). Anthropic has
// no list endpoint, so a curated set of current model ids is returned.
func DiscoverModels(ctx context.Context, provider, baseURL, apiKey string) ([]string, error) {
	switch strings.ToLower(provider) {
	case "ollama", "local":
		return discoverOllama(ctx, baseURL)
	case "openai", "openai-compatible", "localai", "lmstudio", "vllm", "custom":
		return discoverOpenAI(ctx, baseURL, apiKey)
	case "anthropic", "claude":
		return []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}

func discoverOllama(ctx context.Context, host string) ([]string, error) {
	if host == "" {
		host = "http://localhost:11434"
	}
	host = strings.TrimRight(host, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Ollama at %s (is `ollama serve` running?): %w", host, err)
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode Ollama models: %w", err)
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func discoverOpenAI(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if apiKey != "" {
		req.Header.Set("authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach model server at %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode models (status %d): %w", resp.StatusCode, err)
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		names = append(names, m.ID)
	}
	return names, nil
}

// pingProvider does a tiny generation to confirm a connection actually works.
func pingProvider(ctx context.Context, p Provider) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := p.Complete(cctx, "You are a health check.", "Reply with the single word: ok", 16, "low")
	return err
}

// (keep bytes imported for potential future request bodies in discovery)
var _ = bytes.NewReader
