package agent

import "testing"

func TestNewProviderWithProtocolUsesExplicitOpenAIForClaudeModel(t *testing.T) {
	p := NewProviderWithProtocol("openai", "claude-sonnet-4-5", "test-key", "http://127.0.0.1/copilot", 8192)
	if _, ok := p.(*OpenAIProvider); !ok {
		t.Fatalf("expected OpenAIProvider for explicit openai protocol, got %T", p)
	}
}

func TestDetectProviderConfigPrefersExplicitProviderOverModelPrefix(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("COPILOT_BASE_URL", "http://127.0.0.1/copilot")

	p, ok := detectProviderConfig("claude-sonnet-4-5", "copilot")
	if !ok {
		t.Fatal("expected copilot provider config")
	}
	if p.Key != "copilot" {
		t.Fatalf("expected provider key copilot, got %q", p.Key)
	}
	if p.Protocol != "openai" {
		t.Fatalf("expected protocol openai, got %q", p.Protocol)
	}
}

func TestDetectProviderConfigFallsBackToOpenAIWhenAnthropicUnavailable(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	p, ok := detectProviderConfig("claude-sonnet-4-5", "")
	if !ok {
		t.Fatal("expected fallback provider config")
	}
	if p.Key != "openai" {
		t.Fatalf("expected provider key openai, got %q", p.Key)
	}
	if p.Protocol != "openai" {
		t.Fatalf("expected protocol openai, got %q", p.Protocol)
	}
}
