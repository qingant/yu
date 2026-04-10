package agent

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

// OpenAIProvider implements Provider using the OpenAI Chat Completions API.
// Compatible with OpenAI, Gemini, Ollama, vLLM, and other OpenAI-compatible endpoints.
type OpenAIProvider struct {
	Model     string
	APIKey    string
	BaseURL   string // e.g. "http://127.0.0.1:PORT/openai"
	MaxTokens int
}

func (p *OpenAIProvider) Stream(ctx context.Context, system []SystemBlock, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	// Convert from internal types to OpenAI format
	oaiMessages := convertToOpenAI(system, messages)
	oaiTools := convertToolsToOpenAI(tools)

	req := OpenAIChatRequest{
		Model:     p.Model,
		Messages:  oaiMessages,
		Tools:     oaiTools,
		MaxTokens: p.MaxTokens,
		Stream:    true,
		StreamOptions: &OpenAIStreamOptions{IncludeUsage: true},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimSuffix(p.BaseURL, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan StreamEvent, 32)
	go parseOpenAISSE(resp.Body, ch)
	return ch, nil
}

// convertToOpenAI converts internal Message format to OpenAI format.
func convertToOpenAI(system []SystemBlock, messages []Message) []OpenAIMessage {
	var oai []OpenAIMessage

	// System messages
	if len(system) > 0 {
		var sb strings.Builder
		for i, s := range system {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(s.Text)
		}
		oai = append(oai, OpenAIMessage{
			Role:    "system",
			Content: sb.String(),
		})
	}

	for _, m := range messages {
		switch m.Role {
		case "user":
			// Check if this is a tool_result message
			hasToolResult := false
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					hasToolResult = true
					break
				}
			}

			if hasToolResult {
				// Convert tool results to separate "tool" role messages
				for _, b := range m.Content {
					if b.Type == "tool_result" {
						content, _ := b.Content.(string)
						oai = append(oai, OpenAIMessage{
							Role:       "tool",
							Content:    content,
							ToolCallID: b.ToolUseID,
						})
					}
				}
			} else {
				// Regular user message — check for images
				var parts []OpenAIContentPart
				hasImage := false
				for _, b := range m.Content {
					switch b.Type {
					case "text":
						parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
					case "image":
						hasImage = true
						if b.Source != nil {
							parts = append(parts, OpenAIContentPart{
								Type: "image_url",
								ImageURL: &ImageURL{
									URL: fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data),
								},
							})
						}
					}
				}
				if hasImage {
					oai = append(oai, OpenAIMessage{Role: "user", Content: parts})
				} else {
					// Simple text message
					var text strings.Builder
					for _, b := range m.Content {
						if b.Type == "text" {
							text.WriteString(b.Text)
						}
					}
					oai = append(oai, OpenAIMessage{Role: "user", Content: text.String()})
				}
			}

		case "assistant":
			msg := OpenAIMessage{Role: "assistant"}
			var textParts strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					textParts.WriteString(b.Text)
				case "tool_use":
					args := string(b.Input)
					msg.ToolCalls = append(msg.ToolCalls, OpenAIToolCall{
						ID:   b.ID,
						Type: "function",
						Function: OpenAIFuncCall{
							Name:      b.Name,
							Arguments: args,
						},
					})
				}
			}
			if textParts.Len() > 0 {
				msg.Content = textParts.String()
			}
			oai = append(oai, msg)
		}
	}

	return oai
}

// convertToolsToOpenAI converts internal tool definitions to OpenAI format.
func convertToolsToOpenAI(tools []ToolDef) []OpenAITool {
	if len(tools) == 0 {
		return nil
	}
	oai := make([]OpenAITool, len(tools))
	for i, t := range tools {
		oai[i] = OpenAITool{
			Type: "function",
			Function: OpenAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}
	}
	return oai
}

// parseOpenAISSE parses OpenAI SSE stream and emits StreamEvent values
// that match our unified event format (same as Anthropic events).
func parseOpenAISSE(body io.ReadCloser, ch chan<- StreamEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Track state for converting OpenAI chunks → our unified events
	blockIndex := 0
	// Track tool call argument accumulation per tool call index
	toolCallArgs := make(map[int]*strings.Builder)
	toolCallIDs := make(map[int]string)
	toolCallNames := make(map[int]string)
	startedText := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			// Finalize any open tool calls
			for idx := range toolCallArgs {
				ch <- StreamEvent{
					Type:  "content_block_stop",
					Index: idx + 1, // offset by 1 since text block is 0
				}
			}
			ch <- StreamEvent{Type: "message_stop"}
			return
		}

		var chunk OpenAIChatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Handle usage
		if chunk.Usage != nil {
			ch <- StreamEvent{
				Type: "message_start",
				Message: &MessagesResponse{
					Usage: Usage{
						InputTokens:  chunk.Usage.PromptTokens,
						OutputTokens: chunk.Usage.CompletionTokens,
					},
				},
			}
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta
		if delta == nil {
			continue
		}

		// Text content
		if contentStr, ok := delta.Content.(string); ok && contentStr != "" {
			if !startedText {
				startedText = true
				ch <- StreamEvent{
					Type:         "content_block_start",
					Index:        0,
					ContentBlock: &ContentBlock{Type: "text"},
				}
			}
			ch <- StreamEvent{
				Type:  "content_block_delta",
				Index: 0,
				Delta: &StreamDelta{
					Type: "text_delta",
					Text: contentStr,
				},
			}
		}

		// Tool calls
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			if tc.ID != "" {
				toolCallIDs[idx] = tc.ID
			}
			if tc.Function.Name != "" {
				toolCallNames[idx] = tc.Function.Name

				// Close text block if open
				if startedText {
					ch <- StreamEvent{Type: "content_block_stop", Index: 0}
					startedText = false
				}

				blockIndex = idx + 1
				ch <- StreamEvent{
					Type:  "content_block_start",
					Index: blockIndex,
					ContentBlock: &ContentBlock{
						Type: "tool_use",
						ID:   toolCallIDs[idx],
						Name: tc.Function.Name,
					},
				}
				toolCallArgs[idx] = &strings.Builder{}
			}

			if tc.Function.Arguments != "" {
				if _, ok := toolCallArgs[idx]; !ok {
					toolCallArgs[idx] = &strings.Builder{}
				}
				toolCallArgs[idx].WriteString(tc.Function.Arguments)

				ch <- StreamEvent{
					Type:  "content_block_delta",
					Index: idx + 1,
					Delta: &StreamDelta{
						Type:        "input_json_delta",
						PartialJSON: tc.Function.Arguments,
					},
				}
			}
		}

		// Finish reason
		if choice.FinishReason != "" {
			if startedText {
				ch <- StreamEvent{Type: "content_block_stop", Index: 0}
			}

			stopReason := "end_turn"
			if choice.FinishReason == "tool_calls" {
				stopReason = "tool_use"
			}
			ch <- StreamEvent{
				Type:  "message_delta",
				Delta: &StreamDelta{StopReason: stopReason},
			}
		}
	}
}
