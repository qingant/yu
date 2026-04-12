package agent

import (
	"strings"
	"testing"
)

func TestSanitizeMessagesCompactsOnlyOlderToolResults(t *testing.T) {
	oldOutput := strings.Repeat("line\n", 200)
	newOutput := "recent tool output"
	messages := []Message{
		{
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "old-call", Name: "bash"},
			},
		},
		{
			Role: "user",
			Content: []ContentBlock{
				{Type: "tool_result", ToolUseID: "old-call", Content: oldOutput},
			},
		},
		{
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "text", Text: "I used the old result already."},
			},
		},
		{
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "new-call", Name: "bash"},
			},
		},
		{
			Role: "user",
			Content: []ContentBlock{
				{Type: "tool_result", ToolUseID: "new-call", Content: newOutput},
			},
		},
	}

	sanitized := sanitizeMessages(messages)
	if got := sanitized[1].Content[0].Content.(string); !strings.Contains(got, "[tool result summary; original output truncated]") {
		t.Fatalf("expected older tool result to be compacted, got %q", got)
	}
	if got := sanitized[len(sanitized)-1].Content[0].Content.(string); got != newOutput {
		t.Fatalf("expected latest tool result to remain intact, got %q", got)
	}
}

func TestAutoCompactThresholdByProvider(t *testing.T) {
	if got := autoCompactThreshold(&AnthropicProvider{}); got != 256_000 {
		t.Fatalf("expected anthropic threshold 256000, got %d", got)
	}
	if got := autoCompactThreshold(&OpenAIProvider{}); got != 96_000 {
		t.Fatalf("expected openai threshold 96000, got %d", got)
	}
}
