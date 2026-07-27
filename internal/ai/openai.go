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

// openaiProvider talks to any OpenAI-compatible /chat/completions endpoint. This
// single provider covers a large ecosystem: OpenAI itself, LocalAI, LM Studio,
// vLLM, llama.cpp's server, Ollama's OpenAI-compat endpoint, OpenRouter, Groq,
// Together, and more — anything that speaks the OpenAI chat schema. Point it at
// a base URL and (optionally) an API key.
type openaiProvider struct {
	baseURL string // e.g. https://api.openai.com/v1  or  http://localhost:1234/v1
	apiKey  string // optional for local servers
	model   string
	label   string
	http    *http.Client
}

func newOpenAI(baseURL, apiKey, model, label string) *openaiProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if model == "" {
		model = "gpt-4o-mini"
	}
	if label == "" {
		label = "OpenAI-compatible"
	}
	return &openaiProvider{baseURL: baseURL, apiKey: apiKey, model: model, label: label, http: defaultHTTPClient()}
}

func (p *openaiProvider) Name() string  { return p.label }
func (p *openaiProvider) Model() string { return p.model }

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature"`
	Stream      bool            `json:"stream"`
}

type openaiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (p *openaiProvider) Complete(ctx context.Context, system, user string, maxTokens int, _ string) (string, error) {
	body := openaiRequest{
		Model:       p.model,
		MaxTokens:   maxTokens,
		Temperature: 0.2,
		Stream:      false,
		Messages: []openaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := p.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call model server at %s: %w", p.baseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))

	var out openaiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("model server error: %s", out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model server returned status %d: %s", resp.StatusCode, string(raw))
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("model server returned no choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// openaiStreamChunk is one SSE "data:" payload of a streamed chat completion.
type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CompleteStream is Complete with Stream:true — the server answers with SSE
// lines ("data: {json}"), each carrying a choices[0].delta.content chunk,
// terminated by "data: [DONE]".
func (p *openaiProvider) CompleteStream(ctx context.Context, system, user string, maxTokens int, _ string, onDelta func(string)) (string, error) {
	body := openaiRequest{
		Model:       p.model,
		MaxTokens:   maxTokens,
		Temperature: 0.2,
		Stream:      true,
		Messages: []openaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	if p.apiKey != "" {
		req.Header.Set("authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call model server at %s: %w", p.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		var out openaiResponse
		if json.Unmarshal(raw, &out) == nil && out.Error != nil {
			return "", fmt.Errorf("model server error: %s", out.Error.Message)
		}
		return "", fmt.Errorf("model server returned status %d: %s", resp.StatusCode, string(raw))
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}
		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return "", fmt.Errorf("model server error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			sb.WriteString(chunk.Choices[0].Delta.Content)
			onDelta(chunk.Choices[0].Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read model stream: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}

// Endpoint reports the configured base URL. For this provider it is the only
// way to tell a hosted backend from a local one.
func (p *openaiProvider) Endpoint() string { return p.baseURL }
