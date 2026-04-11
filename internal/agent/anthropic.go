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

const anthropicVersion = "2023-06-01"

// AnthropicProvider implements Provider using the Anthropic Messages API.
type AnthropicProvider struct {
	Model     string
	APIKey    string
	BaseURL   string // e.g. "http://127.0.0.1:PORT/anthropic"
	MaxTokens int
}

func (p *AnthropicProvider) Stream(ctx context.Context, system []SystemBlock, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	// Mark cache breakpoints for prompt caching:
	// - Last system block: caches all system content
	// - Last tool: caches all tool definitions
	cachedSystem := make([]SystemBlock, len(system))
	copy(cachedSystem, system)
	if len(cachedSystem) > 0 {
		cachedSystem[len(cachedSystem)-1].CacheControl = &CacheControl{Type: "ephemeral"}
	}

	cachedTools := make([]ToolDef, len(tools))
	copy(cachedTools, tools)
	if len(cachedTools) > 0 {
		cachedTools[len(cachedTools)-1].CacheControl = &CacheControl{Type: "ephemeral"}
	}

	req := MessagesRequest{
		Model:     p.Model,
		MaxTokens: p.MaxTokens,
		System:    cachedSystem,
		Messages:  messages,
		Tools:     cachedTools,
		Stream:    true,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	base := strings.TrimSuffix(p.BaseURL, "/")
	var url string
	if strings.HasSuffix(base, "/v1") {
		url = base + "/messages"
	} else {
		url = base + "/v1/messages"
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

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
	go parseAnthropicSSE(resp.Body, ch)
	return ch, nil
}

func parseAnthropicSSE(body io.ReadCloser, ch chan<- StreamEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	// SSE lines can be long (tool input JSON), increase buffer
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}

			evt := StreamEvent{Type: eventType}

			switch eventType {
			case "message_start":
				var wrapper struct {
					Message MessagesResponse `json:"message"`
				}
				if json.Unmarshal([]byte(data), &wrapper) == nil {
					evt.Message = &wrapper.Message
				}

			case "content_block_start":
				var wrapper struct {
					Index        int          `json:"index"`
					ContentBlock ContentBlock `json:"content_block"`
				}
				if json.Unmarshal([]byte(data), &wrapper) == nil {
					evt.Index = wrapper.Index
					evt.ContentBlock = &wrapper.ContentBlock
				}

			case "content_block_delta":
				var wrapper struct {
					Index int         `json:"index"`
					Delta StreamDelta `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &wrapper) == nil {
					evt.Index = wrapper.Index
					evt.Delta = &wrapper.Delta
				}

			case "content_block_stop":
				var wrapper struct {
					Index int `json:"index"`
				}
				json.Unmarshal([]byte(data), &wrapper)
				evt.Index = wrapper.Index

			case "message_delta":
				var wrapper struct {
					Delta StreamDelta `json:"delta"`
					Usage Usage       `json:"usage"`
				}
				if json.Unmarshal([]byte(data), &wrapper) == nil {
					evt.Delta = &wrapper.Delta
					evt.Usage = &wrapper.Usage
				}

			case "message_stop":
				// no payload needed

			case "ping":
				continue

			case "error":
				var wrapper struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(data), &wrapper) == nil {
					evt.Delta = &StreamDelta{Text: fmt.Sprintf("API error: %s: %s", wrapper.Error.Type, wrapper.Error.Message)}
				}
			}

			ch <- evt
		}
	}
}
