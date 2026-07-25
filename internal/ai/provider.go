// Package ai implements the KubeAura Assistant: natural-language cluster
// querying, troubleshooting, and manifest generation.
//
// The Assistant is model-agnostic. It talks to a pluggable Provider so KubeAura
// can be driven by a hosted API (Anthropic Claude), a local model server
// (Ollama), or any OpenAI-compatible endpoint (LocalAI, LM Studio, vLLM,
// llama.cpp, OpenRouter, Groq, …). This keeps the operator-tool promise: you can
// run the whole thing — cluster UI and AI — entirely on your own machine with no
// data leaving it.
package ai

import (
	"context"
	"net/http"
	"time"
)

// Provider is a single-turn chat backend. Implementations must be safe for
// concurrent use. effort is an optional hint ("low"/"medium"/"high"); providers
// that don't support it ignore it.
type Provider interface {
	// Complete sends one system+user turn and returns the assistant's text.
	Complete(ctx context.Context, system, user string, maxTokens int, effort string) (string, error)
	// Name is a short human label shown in the UI (e.g. "Ollama").
	Name() string
	// Model is the model id in use.
	Model() string
}

// StreamingProvider is an optional Provider extension for backends that can
// deliver output incrementally. onDelta is called from the request goroutine
// with each text chunk as it arrives; the full assembled text is returned at
// the end, exactly like Complete. Callers must treat onDelta as best-effort
// UI feedback and use the returned string as the source of truth.
type StreamingProvider interface {
	CompleteStream(ctx context.Context, system, user string, maxTokens int, effort string, onDelta func(string)) (string, error)
}

// defaultHTTPClient is shared by the HTTP-based providers. Local model servers
// can be slow on first token, hence the generous timeout.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 180 * time.Second}
}
