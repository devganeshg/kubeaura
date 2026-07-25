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

const anthropicURL = "https://api.anthropic.com/v1/messages"

// anthropicProvider talks to the Anthropic Messages API (POST /v1/messages)
// directly over net/http to avoid pulling an SDK into the dependency tree.
type anthropicProvider struct {
	apiKey string
	model  string
	http   *http.Client
}

func newAnthropic(apiKey, model string) *anthropicProvider {
	if model == "" {
		model = "claude-opus-5"
	}
	return &anthropicProvider{apiKey: apiKey, model: model, http: defaultHTTPClient()}
}

func (p *anthropicProvider) Name() string  { return "Anthropic Claude" }
func (p *anthropicProvider) Model() string { return p.model }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *anthropicProvider) Complete(ctx context.Context, system, user string, maxTokens int, effort string) (string, error) {
	body := anthropicRequest{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
	}
	if effort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: effort}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Claude API: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))

	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode Claude response (status %d): %w", resp.StatusCode, err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("Claude API error (%s): %s", out.Error.Type, out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Claude API returned status %d: %s", resp.StatusCode, string(raw))
	}
	if out.StopReason == "refusal" {
		return "", fmt.Errorf("the AI declined to answer this request")
	}
	var sb strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// anthropicStreamEvent is one SSE "data:" payload of a streamed message.
// Only the fields needed for text deltas and error reporting are decoded.
type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// CompleteStream is Complete with stream:true — the API answers with SSE
// events; text arrives as content_block_delta events with delta.text.
func (p *anthropicProvider) CompleteStream(ctx context.Context, system, user string, maxTokens int, effort string, onDelta func(string)) (string, error) {
	body := anthropicRequest{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
		Stream:    true,
	}
	if effort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: effort}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Claude API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		var out anthropicResponse
		if json.Unmarshal(raw, &out) == nil && out.Error != nil {
			return "", fmt.Errorf("Claude API error (%s): %s", out.Error.Type, out.Error.Message)
		}
		return "", fmt.Errorf("Claude API returned status %d: %s", resp.StatusCode, string(raw))
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
		if data == "" {
			continue
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "error":
			if ev.Error != nil {
				return "", fmt.Errorf("Claude API error (%s): %s", ev.Error.Type, ev.Error.Message)
			}
			return "", fmt.Errorf("Claude API stream error")
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				sb.WriteString(ev.Delta.Text)
				onDelta(ev.Delta.Text)
			}
		case "message_stop":
			// done
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read Claude stream: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}
