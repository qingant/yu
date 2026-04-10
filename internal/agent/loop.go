package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chzyer/readline"
)

// slashCommands defines all available commands for tab completion.
var slashCommands = []string{
	"/help", "/exit", "/quit",
	"/clear", "/compact", "/new",
	"/sessions", "/resume",
	"/model", "/init",
	"/jobs", "/logs", "/kill",
	"/remember", "/memory", "/forget",
}

// stats tracks token usage across turns.
type stats struct {
	totalInput  atomic.Int64
	totalOutput atomic.Int64
}

// Main is the entry point for `yu agent-loop`. Must be called inside a sandbox.
func Main() {
	if os.Getenv("YU_SANDBOX") != "1" {
		fmt.Fprintln(os.Stderr, "yu agent-loop must be run inside a yu sandbox.")
		fmt.Fprintln(os.Stderr, "Use: yu <dir>")
		os.Exit(1)
	}

	projectDir := os.Getenv("YU_PROJECT_DIR")
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	wsDir := os.Getenv("YU_WORKSPACE_DIR")

	model := os.Getenv("YU_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	apiKey, baseURL := detectAPIConfig(model)
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "No API key found.")
		fmt.Fprintln(os.Stderr, "Set ANTHROPIC_AUTH_TOKEN or OPENAI_API_KEY in your shell environment,")
		fmt.Fprintln(os.Stderr, "or add it to your .yu/env file (yu config set ANTHROPIC_AUTH_TOKEN sk-ant-...)")
		os.Exit(1)
	}

	maxTokens := 8192
	provider := NewProvider(model, apiKey, baseURL, maxTokens)

	// Background process manager with exit notifications
	tmpDir := os.TempDir()
	bgManager := NewBgManager(projectDir, filepath.Join(tmpDir, "yu-bg"), func(p *BgProcess) {
		status := "exited"
		if p.ExitCode != 0 {
			status = fmt.Sprintf("failed (exit %d)", p.ExitCode)
		}
		fmt.Printf("\n\033[2m[bg #%d %s] %s\033[0m\n", p.ID, status, truncCmd(p.Command, 40))
	})

	executor := &ToolExecutor{
		ProjectDir:  projectDir,
		BashTimeout: 120 * time.Second,
		BgManager:   bgManager,
	}
	tools := toolDefs()
	var st stats

	// Signal handling — also clean up background processes
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
		bgManager.StopAll()
		fmt.Println()
		os.Exit(0)
	}()

	// Session — if resumed, sync model + provider to what the session had
	session := resolveSession(wsDir, model)
	if session.Model != "" && session.Model != model {
		model = session.Model
		newKey, newBase := detectAPIConfig(model)
		if newKey != "" {
			provider = NewProvider(model, newKey, newBase, maxTokens)
		}
	}

	// System prompt
	memoryFile := findMemoryFile(wsDir)
	system := buildSystemPrompt(projectDir, memoryFile)

	// Readline with tab completion
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            promptString(model, bgManager.RunningCount()),
		HistoryFile:       historyFile(wsDir),
		AutoComplete:      newCompleter(),
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "readline init failed: %v\n", err)
		os.Exit(1)
	}
	defer rl.Close()

	// Welcome
	fmt.Printf("\033[1;36myu\033[0m \033[2m%s\033[0m\n", shortModel(model))
	fmt.Printf("\033[2m%s\033[0m\n", projectDir)
	if len(session.Messages) > 0 {
		turns := countUserTurns(session.Messages)
		fmt.Printf("\033[2mResumed: %s (%d turns)\033[0m\n\n", session.Title, turns)
		printSessionHistory(session.Messages)
	}
	fmt.Println()

	for {
		rl.SetPrompt(promptString(model, bgManager.RunningCount()))
		input, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt {
				continue
			}
			if err == io.EOF {
				break
			}
			break
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		// Slash commands
		if strings.HasPrefix(input, "/") {
			result := handleSlashCommand(input, session, projectDir, wsDir, provider, bgManager)
			if result.newSession {
				session = NewSession(model)
				system = buildSystemPrompt(projectDir, findMemoryFile(wsDir))
				fmt.Println("New session started.")
			}
			if result.resumeID != "" {
				loaded, err := LoadSession(wsDir, result.resumeID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error loading session: %v\n", err)
				} else {
					session = loaded
					if session.Model != "" && session.Model != model {
						model = session.Model
						newKey, newBase := detectAPIConfig(model)
						if newKey != "" {
							provider = NewProvider(model, newKey, newBase, maxTokens)
						}
					}
					turns := countUserTurns(session.Messages)
					fmt.Printf("Resumed: %s (%d turns)\n\n", session.Title, turns)
					printSessionHistory(session.Messages)
				}
			}
			if result.switchModel != "" {
				model = result.switchModel
				session.Model = model
				newKey, newBase := detectAPIConfig(model)
				if newKey == "" {
					fmt.Fprintf(os.Stderr, "No API key found for model %s\n", model)
				} else {
					provider = NewProvider(model, newKey, newBase, maxTokens)
					fmt.Printf("Switched to \033[1m%s\033[0m\n", model)
				}
			}
			if result.handled {
				continue
			}
		}

		// User message
		session.Messages = append(session.Messages, Message{
			Role: "user",
			Content: []ContentBlock{
				{Type: "text", Text: input},
			},
		})

		// Agent turn
		prevTokens := st.totalInput.Load() + st.totalOutput.Load()
		turnStart := time.Now()
		err = agentTurn(ctx, provider, system, &session.Messages, tools, executor, &st)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n\033[1;31mError: %v\033[0m\n", err)
		}
		elapsed := time.Since(turnStart)
		newTokens := st.totalInput.Load() + st.totalOutput.Load()
		turnTokens := newTokens - prevTokens
		now := time.Now().Format("15:04:05")
		fmt.Printf("\n\n\033[2m%s  %s  %s  %s\033[0m\n\n", randomEmoji(), formatTokens(turnTokens), formatDuration(elapsed), now)

		// Auto-save
		if wsDir != "" {
			session.Save(wsDir)
		}
	}
}

// promptString builds a single-line prompt with optional bg process indicator.
func promptString(model string, bgCount int) string {
	bg := ""
	if bgCount > 0 {
		bg = fmt.Sprintf(" \033[33m[%d bg]\033[0m", bgCount)
	}
	return fmt.Sprintf("\033[1;32myu\033[0m \033[2m%s\033[0m%s> ", shortModel(model), bg)
}

func shortModel(model string) string {
	// claude-sonnet-4-6 → sonnet-4
	// gpt-4o → gpt-4o
	parts := strings.Split(model, "-")
	if len(parts) >= 3 && parts[0] == "claude" {
		// claude-{name}-{version}-{date} → {name}-{version}
		return parts[1] + "-" + parts[2]
	}
	return model
}

func formatTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d tokens", n)
	}
	return fmt.Sprintf("%.1fk tokens", float64(n)/1000)
}

func newCompleter() *readline.PrefixCompleter {
	items := make([]readline.PrefixCompleterInterface, len(slashCommands))
	for i, cmd := range slashCommands {
		items[i] = readline.PcItem(cmd)
	}
	return readline.NewPrefixCompleter(items...)
}

func historyFile(wsDir string) string {
	if wsDir == "" {
		return ""
	}
	os.MkdirAll(wsDir, 0700)
	return filepath.Join(wsDir, "history")
}

// resolveSession decides whether to resume or create a new session.
func resolveSession(wsDir, model string) *Session {
	if wsDir == "" {
		return NewSession(model)
	}

	latest := LoadLatestSession(wsDir)
	if latest == nil || len(latest.Messages) == 0 {
		return NewSession(model)
	}

	// If the latest session is recent (< 1 hour), offer to resume
	if time.Since(latest.UpdatedAt) < time.Hour {
		turns := countUserTurns(latest.Messages)
		fmt.Printf("Recent session: \033[1m%s\033[0m (%d turns, %s)\n",
			latest.Title, turns, formatAge(latest.UpdatedAt))
		fmt.Print("Resume? [Y/n]: ")

		// Use basic stdin for this one prompt (before readline init)
		var answer string
		fmt.Scanln(&answer)
		answer = strings.TrimSpace(strings.ToLower(answer))

		if answer == "" || answer == "y" || answer == "yes" {
			return latest
		}
	}

	return NewSession(model)
}

// printSessionHistory replays past messages so the user sees the conversation context.
func printSessionHistory(messages []Message) {
	for _, m := range messages {
		switch m.Role {
		case "user":
			// Only print text messages, skip tool_result
			for _, b := range m.Content {
				if b.Type == "text" {
					fmt.Printf("\033[1;32myu>\033[0m %s\n\n", truncateHistory(b.Text, 200))
				}
			}
		case "assistant":
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					fmt.Printf("%s\n\n", truncateHistory(b.Text, 500))
				case "tool_use":
					fmt.Printf("\033[2m[%s]\033[0m\n", b.Name)
				}
			}
		}
	}
	fmt.Println("\033[2m--- end of history ---\033[0m")
	fmt.Println()
}

func truncateHistory(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func countUserTurns(messages []Message) int {
	count := 0
	for _, m := range messages {
		if m.Role == "user" {
			for _, b := range m.Content {
				if b.Type == "text" {
					count++
					break
				}
			}
		}
	}
	return count
}

// agentTurn runs one complete agent turn: API call → tool execution → repeat.
func agentTurn(ctx context.Context, provider Provider, system []SystemBlock, messages *[]Message, tools []ToolDef, executor *ToolExecutor, st *stats) error {
	for {
		ch, err := provider.Stream(ctx, system, *messages, tools)
		if err != nil {
			return err
		}

		response, err := processStream(ch)
		if err != nil {
			return err
		}

		// Track tokens
		st.totalInput.Add(int64(response.InputTokens))
		st.totalOutput.Add(int64(response.OutputTokens))

		*messages = append(*messages, Message{
			Role:    "assistant",
			Content: response.Blocks,
		})

		// Extract tool_use blocks
		var toolCalls []ContentBlock
		for _, block := range response.Blocks {
			if block.Type == "tool_use" {
				toolCalls = append(toolCalls, block)
			}
		}

		if len(toolCalls) == 0 {
			return nil
		}

		// Show tool calls
		fmt.Println()
		for _, tc := range toolCalls {
			fmt.Printf("\033[2m[%s] %s\033[0m\n", tc.Name, toolInputPreview(tc))
		}

		results := executor.ExecuteTools(toolCalls)

		for i, result := range results {
			content, _ := result.Content.(string)
			preview := content
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			if result.IsError {
				fmt.Printf("\033[31m[%s error] %s\033[0m\n", toolCalls[i].Name, preview)
			} else if len(content) > 0 {
				fmt.Printf("\033[2m[%s] %s\033[0m\n", toolCalls[i].Name, preview)
			}
		}
		fmt.Println()

		var resultBlocks []ContentBlock
		for _, r := range results {
			resultBlocks = append(resultBlocks, r)
		}
		*messages = append(*messages, Message{
			Role:    "user",
			Content: resultBlocks,
		})
	}
}

func toolInputPreview(tc ContentBlock) string {
	if tc.Name == "bash" {
		var args struct {
			Command string `json:"command"`
		}
		json.Unmarshal(tc.Input, &args)
		return args.Command
	}
	s := string(tc.Input)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// streamResponse collects the full response from streaming events.
type streamResponse struct {
	Blocks       []ContentBlock
	StopReason   string
	InputTokens  int
	OutputTokens int
}

// processStream reads streaming events, prints text in real-time, and returns the full response.
func processStream(ch <-chan StreamEvent) (*streamResponse, error) {
	resp := &streamResponse{}

	type blockBuilder struct {
		block   ContentBlock
		jsonBuf strings.Builder
	}
	blocks := make(map[int]*blockBuilder)

	for evt := range ch {
		switch evt.Type {
		case "message_start":
			if evt.Message != nil {
				resp.InputTokens = evt.Message.Usage.InputTokens
			}

		case "content_block_start":
			if evt.ContentBlock != nil {
				blocks[evt.Index] = &blockBuilder{
					block: *evt.ContentBlock,
				}
			}

		case "content_block_delta":
			if evt.Delta == nil {
				continue
			}
			switch evt.Delta.Type {
			case "text_delta":
				fmt.Print(evt.Delta.Text)
				if b, ok := blocks[evt.Index]; ok {
					b.block.Text += evt.Delta.Text
				}
			case "input_json_delta":
				if b, ok := blocks[evt.Index]; ok {
					b.jsonBuf.WriteString(evt.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			if b, ok := blocks[evt.Index]; ok {
				if b.block.Type == "tool_use" && b.jsonBuf.Len() > 0 {
					b.block.Input = json.RawMessage(b.jsonBuf.String())
				}
				resp.Blocks = append(resp.Blocks, b.block)
			}

		case "message_delta":
			if evt.Delta != nil {
				resp.StopReason = evt.Delta.StopReason
			}
			if evt.Usage != nil {
				resp.OutputTokens = evt.Usage.OutputTokens
			}

		case "message_stop":
			// done

		case "error":
			if evt.Delta != nil && evt.Delta.Text != "" {
				return nil, fmt.Errorf("%s", evt.Delta.Text)
			}
		}
	}

	return resp, nil
}

// detectAPIConfig finds API key and base URL from environment.
func detectAPIConfig(model string) (apiKey, baseURL string) {
	if isAnthropicModel(model) {
		apiKey = os.Getenv("ANTHROPIC_AUTH_TOKEN")
		if apiKey == "" {
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		baseURL = os.Getenv("ANTHROPIC_BASE_URL")
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		return
	}
	apiKey = os.Getenv("OPENAI_API_KEY")
	baseURL = os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	return
}

var prettyEmojis = []string{
	"✨", "🌟", "💫", "⚡", "🔮", "🎯", "🚀", "💡",
	"🌸", "🍀", "🌊", "🔥", "❄️", "🌙", "☀️", "🌈",
	"🦋", "🐬", "🦊", "🐙", "🎪", "🎨", "🎵", "💎",
	"🧊", "🫧", "🪐", "⭐", "🌀", "🎲", "🧩", "🪄",
}

func randomEmoji() string {
	return prettyEmojis[time.Now().UnixNano()%int64(len(prettyEmojis))]
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

func findMemoryFile(wsDir string) string {
	if wsDir == "" {
		return ""
	}
	path := wsDir + "/memory.md"
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}
