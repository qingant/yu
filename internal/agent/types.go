package agent

import "encoding/json"

// --- Anthropic Messages API types ---

// Message is a single turn in the conversation.
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is the union type for text, tool_use, tool_result, and image.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"` // string or []ContentBlock for images
	IsError   bool   `json:"is_error,omitempty"`

	// image (Anthropic native)
	Source *ImageSource `json:"source,omitempty"`

	// image (OpenAI format)
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageSource for Anthropic image blocks.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png", "image/jpeg", etc.
	Data      string `json:"data"`       // base64-encoded
}

// ImageURL for OpenAI image blocks.
type ImageURL struct {
	URL string `json:"url"` // "data:image/png;base64,..."
}

// SystemBlock is a system prompt section.
type SystemBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// ToolDef defines a tool the model can call.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// --- Anthropic API request/response ---

type MessagesRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []SystemBlock `json:"system,omitempty"`
	Messages  []Message     `json:"messages"`
	Tools     []ToolDef     `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
}

type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence,omitempty"`
	Usage        Usage          `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- SSE streaming types ---

// StreamEvent represents one SSE event from the Claude API.
type StreamEvent struct {
	Type string // "message_start", "content_block_start", "content_block_delta", etc.

	// message_start
	Message *MessagesResponse `json:"message,omitempty"`

	// content_block_start
	Index        int           `json:"index,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`

	// content_block_delta
	Delta *StreamDelta `json:"delta,omitempty"`

	// message_delta
	Usage *Usage `json:"usage,omitempty"`
}

type StreamDelta struct {
	Type string `json:"type"` // "text_delta" | "input_json_delta"

	// text_delta
	Text string `json:"text,omitempty"`

	// input_json_delta
	PartialJSON string `json:"partial_json,omitempty"`

	// message_delta
	StopReason string `json:"stop_reason,omitempty"`
}

// --- OpenAI Chat Completions types ---

type OpenAIChatRequest struct {
	Model               string              `json:"model"`
	Messages            []OpenAIMessage     `json:"messages"`
	Tools               []OpenAITool        `json:"tools,omitempty"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	Stream              bool                `json:"stream"`
	StreamOptions       *OpenAIStreamOptions `json:"stream_options,omitempty"`
}

type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIMessage struct {
	Role       string              `json:"role"`                    // "system" | "user" | "assistant" | "tool"
	Content    any                 `json:"content"`                 // string, []OpenAIContentPart, or nil
	ToolCalls  []OpenAIToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`  // for role=tool
}

type OpenAIContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type OpenAITool struct {
	Type     string         `json:"type"` // "function"
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type OpenAIToolCall struct {
	Index    int            `json:"index,omitempty"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function OpenAIFuncCall `json:"function"`
}

type OpenAIFuncCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type OpenAIChatResponse struct {
	ID      string         `json:"id"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

type OpenAIChoice struct {
	Index        int              `json:"index"`
	Message      *OpenAIMessage   `json:"message,omitempty"`
	Delta        *OpenAIMessage   `json:"delta,omitempty"`
	FinishReason string           `json:"finish_reason,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
