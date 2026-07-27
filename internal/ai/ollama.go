package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ollamaProvider talks to a local Ollama server (https://ollama.com) via its
// native chat API (POST /api/chat). This is the "run everything locally, no data
// leaves the machine" path: a small model like llama3.2, qwen2.5, or phi3 can
// answer the Kubernetes queries KubeAura asks.
type ollamaProvider struct {
	host  string // e.g. http://localhost:11434
	model string
	http  *http.Client
}

func newOllama(host, model string) *ollamaProvider {
	if host == "" {
		host = "http://localhost:11434"
	}
	host = strings.TrimRight(host, "/")
	if model == "" {
		model = "llama3.2"
	}
	return &ollamaProvider{host: host, model: model, http: defaultHTTPClient()}
}

func (p *ollamaProvider) Name() string  { return "Ollama (local)" }
func (p *ollamaProvider) Model() string { return p.model }

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	Options  map[string]any      `json:"options,omitempty"`
}

type ollamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
}

func (p *ollamaProvider) Complete(ctx context.Context, system, user string, maxTokens int, _ string) (string, error) {
	body := ollamaRequest{
		Model:  p.model,
		Stream: false,
		Messages: []ollamaChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Options: map[string]any{
			"num_predict": maxTokens,
			"temperature": 0.2, // steadier, more deterministic ops answers
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := p.host + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Ollama at %s (is `ollama serve` running?): %w", p.host, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))

	var out ollamaResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode Ollama response (status %d): %w", resp.StatusCode, err)
	}
	if out.Error != "" {
		// A common case: the model isn't pulled yet.
		if strings.Contains(out.Error, "not found") {
			return "", fmt.Errorf("Ollama model %q is not installed — run `ollama pull %s`", p.model, p.model)
		}
		return "", fmt.Errorf("Ollama error: %s", out.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(raw))
	}
	return strings.TrimSpace(out.Message.Content), nil
}

// CompleteStream is Complete with Stream:true — Ollama then answers with one
// JSON object per line, each carrying a message.content delta.
func (p *ollamaProvider) CompleteStream(ctx context.Context, system, user string, maxTokens int, _ string, onDelta func(string)) (string, error) {
	body := ollamaRequest{
		Model:  p.model,
		Stream: true,
		Messages: []ollamaChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Options: map[string]any{
			"num_predict": maxTokens,
			"temperature": 0.2,
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.host+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Ollama at %s (is `ollama serve` running?): %w", p.host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		var out ollamaResponse
		if json.Unmarshal(raw, &out) == nil && out.Error != "" {
			if strings.Contains(out.Error, "not found") {
				return "", fmt.Errorf("Ollama model %q is not installed — run `ollama pull %s`", p.model, p.model)
			}
			return "", fmt.Errorf("Ollama error: %s", out.Error)
		}
		return "", fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(raw))
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaResponse
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue // tolerate keep-alives / partial noise
		}
		if chunk.Error != "" {
			return "", fmt.Errorf("Ollama error: %s", chunk.Error)
		}
		if chunk.Message.Content != "" {
			sb.WriteString(chunk.Message.Content)
			onDelta(chunk.Message.Content)
		}
		if chunk.Done {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read Ollama stream: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}

// Endpoint reports the Ollama server address, so the evidence envelope can say
// whether the model runs on this machine.
func (p *ollamaProvider) Endpoint() string { return p.host }
