package agent

import (
	"context"
	"strings"
)

// Provider abstracts the LLM API backend.
type Provider interface {
	// Stream sends a conversation and streams back events.
	// The channel closes when the response is complete.
	Stream(ctx context.Context, system []SystemBlock, messages []Message, tools []ToolDef) (<-chan StreamEvent, error)
}

// NewProvider creates the appropriate provider based on model name.
func NewProvider(model, apiKey, baseURL string, maxTokens int) Provider {
	if isAnthropicModel(model) {
		return &AnthropicProvider{
			Model:     model,
			APIKey:    apiKey,
			BaseURL:   baseURL,
			MaxTokens: maxTokens,
		}
	}
	return &OpenAIProvider{
		Model:     model,
		APIKey:    apiKey,
		BaseURL:   baseURL,
		MaxTokens: maxTokens,
	}
}

func isAnthropicModel(model string) bool {
	return strings.HasPrefix(model, "claude")
}

func isGeminiModel(model string) bool {
	return strings.HasPrefix(model, "gemini")
}
