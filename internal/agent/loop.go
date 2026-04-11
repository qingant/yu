package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	"/jobs", "/logs", "/kill", "/rollback", "/stats",
	"/remember", "/memory", "/forget",
}

// stats tracks token usage for the current run.
type stats struct {
	totalInput      atomic.Int64
	totalOutput     atomic.Int64
	totalCacheRead  atomic.Int64
	totalCacheWrite atomic.Int64
	turns           atomic.Int64
}

// ANSI helpers
const (
	dim       = "\033[2m"
	bold      = "\033[1m"
	red       = "\033[31m"
	green     = "\033[32m"
	yellow    = "\033[33m"
	cyan      = "\033[36m"
	boldGreen = "\033[1;32m"
	boldCyan  = "\033[1;36m"
	boldRed   = "\033[1;31m"
	reset     = "\033[0m"
)

// spinner frames
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Main is the entry point for `yu agent-loop`. Must be called inside a sandbox.
func Main() {
	if os.Getenv("YU_SANDBOX") != "1" {
		fmt.Fprintln(os.Stderr, "yu agent-loop must be run inside a yu sandbox.")
		fmt.Fprintln(os.Stderr, "Use: yu agent")
		os.Exit(1)
	}

	projectDir := os.Getenv("YU_PROJECT_DIR")
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	wsDir := os.Getenv("YU_WORKSPACE_DIR")

	model := os.Getenv("YU_MODEL")
	if model == "" {
		model = loadActiveModel(wsDir)
	}
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

	// Background process manager
	tmpDir := os.TempDir()
	bgManager := NewBgManager(projectDir, filepath.Join(tmpDir, "yu-bg"), func(p *BgProcess) {
		status := "exited"
		if p.ExitCode != 0 {
			status = fmt.Sprintf("failed (exit %d)", p.ExitCode)
		}
		fmt.Printf("\n%s[bg #%d %s] %s%s\n", dim, p.ID, status, truncCmd(p.Command, 40), reset)
	})

	executor := &ToolExecutor{
		ProjectDir:  projectDir,
		BashTimeout: 120 * time.Second,
		BgManager:   bgManager,
	}
	tools := toolDefs()
	var st stats

	// Signal handling
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

	// Session resolution
	execPrompt := os.Getenv("YU_EXEC_PROMPT")
	var session *Session
	if os.Getenv("YU_NEW_SESSION") == "1" || execPrompt != "" {
		session = NewSession(model)
	} else if sid := os.Getenv("YU_SESSION"); sid != "" {
		loaded, err := LoadSession(wsDir, sid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Session %s not found, starting new\n", sid)
			session = NewSession(model)
		} else {
			session = loaded
		}
	} else {
		session = resolveSession(wsDir, model)
	}
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

	// --- Exec mode: one-shot, no interactive UI ---
	if execPrompt != "" {
		session.Messages = append(session.Messages, Message{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: execPrompt}},
		})
		_, err := agentTurn(ctx, provider, system, &session.Messages, tools, executor, &st)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
		st.turns.Add(1)
		syncStats(&st, session, wsDir)
		bgManager.StopAll()
		return
	}

	// --- Interactive mode ---
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            promptString(model, bgManager.RunningCount()),
		HistoryFile:       historyFile(wsDir),
		AutoComplete:      newCompleter(projectDir),
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "readline init failed: %v\n", err)
		os.Exit(1)
	}
	defer rl.Close()

	// Welcome banner
	printWelcome(model, projectDir, session)

	for {
		rl.SetPrompt(promptString(model, bgManager.RunningCount()))
		input, err := readMultiLine(rl, model, bgManager)
		if err != nil {
			if err == readline.ErrInterrupt {
				continue
			}
			if err == io.EOF {
				break
			}
			break
		}
		if input == "" {
			continue
		}

		// Slash commands
		if strings.HasPrefix(input, "/") {
			result := handleSlashCommand(input, session, projectDir, wsDir, provider, bgManager, &st)
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
				// Try active provider first (from /model picker)
				if p, ok := switchFromActiveProvider(model); ok {
					provider = p
				} else {
					// Fallback to env detection
					newKey, newBase := detectAPIConfig(model)
					if newKey == "" {
						fmt.Fprintf(os.Stderr, "No API key found for model %s\n", model)
					} else {
						provider = NewProvider(model, newKey, newBase, maxTokens)
					}
				}
				saveActiveModel(wsDir, model)
				fmt.Printf("\nSwitched to %s%s%s\n", bold, model, reset)
			}
			if result.handled {
				continue
			}
		}

		// !cmd — direct shell execution, output added to conversation for model context
		if strings.HasPrefix(input, "!") {
			shellCmd := strings.TrimPrefix(input, "!")
			shellCmd = strings.TrimSpace(shellCmd)
			if shellCmd != "" {
				output := execDirectCommand(shellCmd, projectDir)
				// Add to conversation so the model can see it
				session.Messages = append(session.Messages, Message{
					Role: "user",
					Content: []ContentBlock{
						{Type: "text", Text: fmt.Sprintf("[User ran shell command: %s]\n\n%s", shellCmd, output)},
					},
				})
				if wsDir != "" {
					session.Save(wsDir)
				}
				continue
			}
		}

		// User message — expand @file references
		_, expanded := expandAtFiles(input, projectDir)
		session.Messages = append(session.Messages, Message{
			Role: "user",
			Content: []ContentBlock{
				{Type: "text", Text: expanded},
			},
		})

		// Agent turn with spinner
		prevTokens := st.totalInput.Load() + st.totalOutput.Load()
		turnStart := time.Now()

		// Per-turn context: Esc cancels just this turn (not the whole session)
		turnCtx, turnCancel := context.WithCancel(ctx)
		stopEscWatcher := make(chan struct{})
		go watchEsc(turnCancel, stopEscWatcher)

		lastInput, turnErr := agentTurn(turnCtx, provider, system, &session.Messages, tools, executor, &st)

		close(stopEscWatcher)
		turnCancel()

		if turnErr != nil {
			if turnCtx.Err() != nil {
				fmt.Fprintf(os.Stderr, "\n%s↩ Interrupted%s\n", yellow, reset)
				// Remove the last user message so the turn can be retried
				if len(session.Messages) > 0 && session.Messages[len(session.Messages)-1].Role == "user" {
					session.Messages = session.Messages[:len(session.Messages)-1]
				}
			} else {
				fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", boldRed, turnErr, reset)
			}
		}

		elapsed := time.Since(turnStart)
		st.turns.Add(1)
		newTokens := st.totalInput.Load() + st.totalOutput.Load()
		turnTokens := newTokens - prevTokens
		cacheRead := st.totalCacheRead.Load()
		printTurnStats(turnTokens, cacheRead, elapsed)

		// Update session + global stats, auto-save
		syncStats(&st, session, wsDir)

		// Auto-compact when context is getting large
		autoCompact(lastInput, session, provider)
	}
}

// --- Input ---

// readMultiLine reads input, supporting \ continuation for multi-line.
func readMultiLine(rl *readline.Instance, model string, bgm *BgManager) (string, error) {
	line, err := rl.Readline()
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, " \t")

	// Multi-line: if line ends with \, continue reading
	var lines []string
	for strings.HasSuffix(line, "\\") {
		lines = append(lines, strings.TrimSuffix(line, "\\"))
		rl.SetPrompt(fmt.Sprintf("%s...%s ", dim, reset))
		next, err := rl.Readline()
		if err != nil {
			break
		}
		line = strings.TrimRight(next, " \t")
	}
	lines = append(lines, line)

	input := strings.TrimSpace(strings.Join(lines, "\n"))
	return input, nil
}

// expandAtFiles finds @path references in input and expands them to file content.
// Returns the original text with @path replaced by inline file content for the model.
func expandAtFiles(input, projectDir string) (display string, expanded string) {
	// Match @path tokens (not preceded by another @, not inside backticks)
	words := strings.Fields(input)
	var hasFiles bool
	var fileBlocks []string

	for _, w := range words {
		if !strings.HasPrefix(w, "@") || len(w) < 2 {
			continue
		}
		path := w[1:]
		absPath := path
		if !filepath.IsAbs(path) {
			absPath = filepath.Join(projectDir, path)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		hasFiles = true
		fileBlocks = append(fileBlocks, fmt.Sprintf("[File: %s]\n```\n%s\n```", path, string(data)))
	}

	if !hasFiles {
		return input, input
	}

	expanded = input + "\n\n" + strings.Join(fileBlocks, "\n\n")
	return input, expanded
}

// --- Welcome & History ---

// watchEsc watches for Esc key and calls cancel if pressed.
// Uses a separate goroutine with blocking read to avoid interfering
// with readline's terminal state management.
func watchEsc(cancel context.CancelFunc, stopCh <-chan struct{}) {
	ch := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 16)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				if buf[i] == 0x1B {
					ch <- struct{}{}
					return
				}
			}
		}
	}()
	select {
	case <-stopCh:
	case <-ch:
		cancel()
	}
}

func printWelcome(model, projectDir string, session *Session) {
	fmt.Println()
	fmt.Printf("  %syu%s %s%s%s\n", boldCyan, reset, dim, shortModel(model), reset)
	fmt.Printf("  %s%s%s\n", dim, projectDir, reset)

	if len(session.Messages) > 0 {
		turns := countUserTurns(session.Messages)
		fmt.Printf("  %sResumed: %s (%d turns)%s\n", dim, session.Title, turns, reset)
		fmt.Println()
		printSessionHistory(session.Messages)
	} else {
		fmt.Printf("  %sType /help for commands%s\n", dim, reset)
	}
	fmt.Println()
}

func printSessionHistory(messages []Message) {
	// Build a map of tool_use_id → tool result for quick lookup
	resultByID := map[string]ContentBlock{}
	for _, m := range messages {
		if m.Role == "user" {
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					resultByID[b.ToolUseID] = b
				}
			}
		}
	}

	for _, m := range messages {
		switch m.Role {
		case "user":
			for _, b := range m.Content {
				if b.Type == "text" {
					fmt.Printf("%syu>%s %s\n\n", boldGreen, reset, b.Text)
				}
				// tool_result blocks are printed inline with their tool_use below
			}
		case "assistant":
			// Print text blocks through markdown renderer
			hasText := false
			for _, b := range m.Content {
				if b.Type == "text" && b.Text != "" {
					hasText = true
				}
			}
			if hasText {
				renderer := NewTermRenderer()
				for _, b := range m.Content {
					if b.Type == "text" && b.Text != "" {
						renderer.Feed(b.Text)
					}
				}
				renderer.Flush()
				fmt.Println()
			}
			// Print tool calls + their results
			for _, b := range m.Content {
				if b.Type == "tool_use" {
					fmt.Println()
					printToolCall(b)
					if result, ok := resultByID[b.ID]; ok {
						printToolResult(b.Name, result)
					}
				}
			}
		}
	}
	fmt.Printf("\n%s─── end of history ───%s\n\n", dim, reset)
}

func truncateHistory(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// --- Turn Stats ---

func printTurnStats(tokens int64, cacheRead int64, elapsed time.Duration) {
	now := time.Now().Format("15:04:05")
	emoji := randomEmoji()
	cache := ""
	if cacheRead > 0 {
		cache = fmt.Sprintf("  cached:%s", formatTokens(cacheRead))
	}
	fmt.Printf("\n\n  %s%s  %s%s  %s  %s%s\n\n",
		dim, emoji, formatTokens(tokens), cache, formatDuration(elapsed), now, reset)
}

// --- Prompt ---

func promptString(model string, bgCount int) string {
	bg := ""
	if bgCount > 0 {
		bg = fmt.Sprintf(" %s[%d bg]%s", yellow, bgCount, reset)
	}
	return fmt.Sprintf("%syu%s %s%s%s%s❯ ", boldGreen, reset, dim, shortModel(model), reset, bg)
}

func shortModel(model string) string {
	parts := strings.Split(model, "-")
	if len(parts) >= 3 && parts[0] == "claude" {
		return parts[1] + "-" + parts[2]
	}
	return model
}

// --- Agent Turn ---

// agentTurn returns (lastInputTokens, error). lastInputTokens is the input size of the final API call.
func agentTurn(ctx context.Context, provider Provider, system []SystemBlock, messages *[]Message, tools []ToolDef, executor *ToolExecutor, st *stats) (int, error) {
	for {
		// Start spinner while waiting for first token
		spinner := startSpinner("thinking")
		sanitized := sanitizeMessages(*messages)
		ch, err := provider.Stream(ctx, system, sanitized, tools)
		if err != nil {
			spinner.Stop()
			return 0, err
		}

		response, err := processStream(ch, spinner)
		if err != nil {
			return 0, err
		}

		st.totalInput.Add(int64(response.InputTokens))
		st.totalOutput.Add(int64(response.OutputTokens))
		st.totalCacheRead.Add(int64(response.CacheReadTokens))
		st.totalCacheWrite.Add(int64(response.CacheWriteTokens))

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
			return response.InputTokens, nil
		}

		// Display tool calls with visual separation
		fmt.Println()
		for _, tc := range toolCalls {
			printToolCall(tc)
		}

		// Execute tools in parallel
		results := executor.ExecuteTools(toolCalls)

		// Display results
		for i, result := range results {
			printToolResult(toolCalls[i].Name, result)
		}

		// Send tool results back
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

// --- Tool Display ---

func printToolCall(tc ContentBlock) {
	name := tc.Name
	preview := toolInputPreview(tc)

	// Different icons for different tool types
	icon := "●"
	switch name {
	case "bash":
		icon = "$"
	case "read_file":
		icon = "◇"
	case "write_file", "edit_file":
		icon = "✎"
	case "list_files", "search_files":
		icon = "⌕"
	case "web_fetch":
		icon = "⊕"
	case "background":
		icon = "◐"
	case "poll":
		icon = "↻"
	case "ask_user", "plan":
		icon = "?"
	}

	fmt.Printf("  %s%s %s%s %s%s%s\n", cyan, icon, name, reset, dim, preview, reset)
}

func printToolResult(name string, result ContentBlock) {
	content, _ := result.Content.(string)
	if content == "" {
		return
	}

	// edit_file already prints its own diff
	if name == "edit_file" && !result.IsError {
		return
	}

	// Limit preview
	preview := content
	lines := strings.Split(preview, "\n")
	if len(lines) > 15 {
		preview = strings.Join(lines[:15], "\n") + fmt.Sprintf("\n    %s… (%d more lines)%s", dim, len(lines)-15, reset)
	}

	if result.IsError {
		fmt.Printf("  %s✗ %s%s\n", red, preview, reset)
	} else {
		// Indent tool output
		indented := "    " + strings.ReplaceAll(preview, "\n", "\n    ")
		fmt.Printf("%s%s%s\n", dim, indented, reset)
	}
}

func toolInputPreview(tc ContentBlock) string {
	switch tc.Name {
	case "bash":
		var args struct {
			Command string `json:"command"`
		}
		json.Unmarshal(tc.Input, &args)
		return args.Command
	case "read_file", "write_file", "edit_file":
		var args struct {
			Path string `json:"path"`
		}
		json.Unmarshal(tc.Input, &args)
		return args.Path
	case "search_files":
		var args struct {
			Pattern string `json:"pattern"`
		}
		json.Unmarshal(tc.Input, &args)
		return args.Pattern
	case "list_files":
		var args struct {
			Pattern string `json:"pattern"`
		}
		json.Unmarshal(tc.Input, &args)
		return args.Pattern
	case "web_fetch":
		var args struct {
			URL string `json:"url"`
		}
		json.Unmarshal(tc.Input, &args)
		return args.URL
	case "background":
		var args struct {
			Action  string `json:"action"`
			Command string `json:"command"`
			ID      int    `json:"id"`
		}
		json.Unmarshal(tc.Input, &args)
		if args.Command != "" {
			return args.Action + " " + args.Command
		}
		if args.ID > 0 {
			return fmt.Sprintf("%s #%d", args.Action, args.ID)
		}
		return args.Action
	}
	s := string(tc.Input)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// --- Spinner ---

type spinnerState struct {
	label   string
	stop    chan struct{}
	stopped chan struct{}
}

func startSpinner(label string) *spinnerState {
	s := &spinnerState{
		label:   label,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go func() {
		defer close(s.stopped)
		i := 0
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				// Clear spinner line
				fmt.Printf("\r\033[K")
				return
			case <-ticker.C:
				frame := spinnerFrames[i%len(spinnerFrames)]
				fmt.Printf("\r  %s%s %s%s", cyan, frame, s.label, reset)
				i++
			}
		}
	}()
	return s
}

func (s *spinnerState) Stop() {
	select {
	case <-s.stop:
		// already stopped
	default:
		close(s.stop)
	}
	<-s.stopped
}

// --- Stream Processing ---

type streamResponse struct {
	Blocks          []ContentBlock
	StopReason      string
	InputTokens     int
	OutputTokens    int
	CacheReadTokens  int
	CacheWriteTokens int
}

// processStream reads streaming events, prints text in real-time, and returns the full response.
// Stops the spinner on first text output.
func processStream(ch <-chan StreamEvent, spinner *spinnerState) (*streamResponse, error) {
	resp := &streamResponse{}
	spinnerActive := true
	renderer := NewTermRenderer()

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
				resp.CacheReadTokens = evt.Message.Usage.CacheReadInputTokens
				resp.CacheWriteTokens = evt.Message.Usage.CacheCreationInputTokens
			}

		case "content_block_start":
			if evt.ContentBlock != nil {
				blocks[evt.Index] = &blockBuilder{
					block: *evt.ContentBlock,
				}
				if spinnerActive {
					spinner.Stop()
					spinnerActive = false
				}
			}

		case "content_block_delta":
			if evt.Delta == nil {
				continue
			}
			switch evt.Delta.Type {
			case "text_delta":
				if spinnerActive {
					spinner.Stop()
					spinnerActive = false
				}
				// Feed through markdown renderer (handles tables, code, etc.)
				renderer.Feed(evt.Delta.Text)
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
				if b.block.Type == "text" {
					renderer.Flush()
				}
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
			if spinnerActive {
				spinner.Stop()
				spinnerActive = false
			}
			if evt.Delta != nil && evt.Delta.Text != "" {
				return nil, fmt.Errorf("%s", evt.Delta.Text)
			}
		}
	}

	renderer.Flush()
	if spinnerActive {
		spinner.Stop()
	}

	return resp, nil
}

// --- Utilities ---

func promptForResume(latest *Session) bool {
	turns := countUserTurns(latest.Messages)
	fmt.Printf("  Recent session: %s%s%s (%d turns, %s)\n",
		bold, latest.Title, reset, turns, formatAge(latest.UpdatedAt))
	fmt.Printf("  Resume? [Y/n]: ")
	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func resolveSession(wsDir, model string) *Session {
	if wsDir == "" {
		return NewSession(model)
	}
	latest := LoadLatestSession(wsDir)
	if latest == nil || len(latest.Messages) == 0 {
		return NewSession(model)
	}
	if time.Since(latest.UpdatedAt) < time.Hour {
		if promptForResume(latest) {
			return latest
		}
	}
	return NewSession(model)
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
	// OpenAI / other OpenAI-compatible
	apiKey = os.Getenv("OPENAI_API_KEY")
	baseURL = os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	return
}

// activeProvider is set by /model picker — determines API key/base for subsequent calls.
var activeProvider *providerInfo

// switchFromActiveProvider creates a Provider using the globally selected provider context.
func switchFromActiveProvider(model string) (Provider, bool) {
	if activeProvider == nil {
		return nil, false
	}
	return NewProvider(model, activeProvider.APIKey, activeProvider.BaseURL, 8192), true
}

// execDirectCommand runs a shell command directly (for !cmd), streaming output to terminal.
func execDirectCommand(command, projectDir string) string {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = projectDir
	cmd.Stdin = os.Stdin

	// Stream output live and capture it
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		msg := fmt.Sprintf("error: %v", err)
		fmt.Println(msg)
		return msg
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		msg := fmt.Sprintf("error: %v", err)
		fmt.Println(msg)
		return msg
	}

	if err := cmd.Start(); err != nil {
		msg := fmt.Sprintf("error: %v", err)
		fmt.Println(msg)
		return msg
	}

	var output strings.Builder
	var wg sync.WaitGroup

	pipe := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println(line)
			output.WriteString(line)
			output.WriteByte('\n')
		}
	}

	wg.Add(2)
	go pipe(stdoutPipe)
	go pipe(stderrPipe)
	wg.Wait()

	cmdErr := cmd.Wait()
	if cmdErr != nil {
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			fmt.Printf("%sExit code: %d%s\n", dim, exitErr.ExitCode(), reset)
		}
	}

	return output.String()
}

// autoCompactThreshold — compact when input tokens exceed this.
// Models with 1M context can afford much larger working sets.
const autoCompactThreshold = 512_000

// autoCompact triggers compaction when the context is getting too large.
func autoCompact(lastInputTokens int, session *Session, provider Provider) {
	if lastInputTokens < autoCompactThreshold {
		return
	}
	if len(session.Messages) < 6 {
		return
	}
	fmt.Printf("\n  %s⟳ Auto-compacting context (%s input)...%s\n", yellow, formatTokens(int64(lastInputTokens)), reset)
	compactConversation(session, provider)
}

// sanitizeMessages fixes message pairs before sending to the API.
// Removes tool_result blocks whose tool_use_id doesn't exist in the
// preceding assistant message. This can happen after /compact or
// session corruption.
func sanitizeMessages(messages []Message) []Message {
	result := make([]Message, 0, len(messages))

	for i, msg := range messages {
		if msg.Role == "user" {
			// Check if this message has tool_result blocks
			hasToolResult := false
			for _, b := range msg.Content {
				if b.Type == "tool_result" {
					hasToolResult = true
					break
				}
			}

			if hasToolResult && i > 0 {
				// Collect valid tool_use IDs from the previous assistant message
				validIDs := make(map[string]bool)
				prev := messages[i-1]
				if prev.Role == "assistant" {
					for _, b := range prev.Content {
						if b.Type == "tool_use" && b.ID != "" {
							validIDs[b.ID] = true
						}
					}
				}

				// Filter: keep only tool_results with valid IDs
				var filtered []ContentBlock
				for _, b := range msg.Content {
					if b.Type == "tool_result" {
						if !validIDs[b.ToolUseID] {
							continue // orphaned tool_result — skip
						}
					}
					filtered = append(filtered, b)
				}

				if len(filtered) == 0 {
					continue // entire message was orphaned tool_results — skip
				}
				result = append(result, Message{Role: msg.Role, Content: filtered})
				continue
			}
		}
		result = append(result, msg)
	}

	return result
}

// syncStats updates session stats from the run counters and saves.
func syncStats(st *stats, session *Session, wsDir string) {
	session.Stats = TokenStats{
		InputTokens:  st.totalInput.Load(),
		OutputTokens: st.totalOutput.Load(),
		CacheRead:    st.totalCacheRead.Load(),
		CacheWrite:   st.totalCacheWrite.Load(),
		Turns:        int(st.turns.Load()),
	}
	if wsDir != "" {
		session.Save(wsDir)
	}
}

func loadActiveModel(wsDir string) string {
	if wsDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(wsDir, "active-model"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveActiveModel(wsDir, model string) {
	if wsDir == "" {
		return
	}
	os.MkdirAll(wsDir, 0700)
	os.WriteFile(filepath.Join(wsDir, "active-model"), []byte(model+"\n"), 0600)
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

// yuCompleter handles both /commands and @file completion.
type yuCompleter struct {
	projectDir string
	prefix     *readline.PrefixCompleter
}

func newCompleter(projectDir string) *yuCompleter {
	items := make([]readline.PrefixCompleterInterface, len(slashCommands))
	for i, cmd := range slashCommands {
		items[i] = readline.PcItem(cmd)
	}
	return &yuCompleter{
		projectDir: projectDir,
		prefix:     readline.NewPrefixCompleter(items...),
	}
}

func (c *yuCompleter) Do(line []rune, pos int) ([][]rune, int) {
	lineStr := string(line[:pos])

	// Slash command completion at start of line
	if strings.HasPrefix(lineStr, "/") {
		return c.prefix.Do(line, pos)
	}

	// @file completion — find the @token being typed
	atIdx := -1
	for i := pos - 1; i >= 0; i-- {
		ch := line[i]
		if ch == '@' {
			atIdx = i
			break
		}
		if ch == ' ' || ch == '\t' {
			break
		}
	}
	if atIdx < 0 {
		return nil, 0
	}

	partial := string(line[atIdx+1 : pos])
	matches := completeFilePath(c.projectDir, partial)
	if len(matches) == 0 {
		return nil, 0
	}

	// Return completions relative to what's already typed
	var candidates [][]rune
	for _, m := range matches {
		suffix := m[len(partial):]
		candidates = append(candidates, []rune(suffix))
	}
	return candidates, len(partial)
}

func completeFilePath(projectDir, partial string) []string {
	// Resolve the partial path
	dir := projectDir
	base := partial
	if idx := strings.LastIndex(partial, "/"); idx >= 0 {
		dir = filepath.Join(projectDir, partial[:idx])
		base = partial[idx+1:]
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var matches []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(name, base) {
			continue
		}
		rel := name
		if idx := strings.LastIndex(partial, "/"); idx >= 0 {
			rel = partial[:idx+1] + name
		}
		if e.IsDir() {
			rel += "/"
		}
		matches = append(matches, rel)
	}
	return matches
}

func historyFile(wsDir string) string {
	if wsDir == "" {
		return ""
	}
	os.MkdirAll(wsDir, 0700)
	return filepath.Join(wsDir, "history")
}

func formatTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d tok", n)
	}
	if n < 10000 {
		return fmt.Sprintf("%.1fk tok", float64(n)/1000)
	}
	return fmt.Sprintf("%.0fk tok", float64(n)/1000)
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

var prettyEmojis = []string{
	"✨", "🌟", "💫", "⚡", "🔮", "🎯", "🚀", "💡",
	"🌸", "🍀", "🌊", "🔥", "❄️", "🌙", "☀️", "🌈",
	"🦋", "🐬", "🦊", "🐙", "🎪", "🎨", "🎵", "💎",
	"🧊", "🫧", "🪐", "⭐", "🌀", "🎲", "🧩", "🪄",
}

// Use a mutex so concurrent goroutines don't both read the same nanosecond
var emojiMu sync.Mutex
var emojiIdx int

func randomEmoji() string {
	emojiMu.Lock()
	defer emojiMu.Unlock()
	e := prettyEmojis[emojiIdx%len(prettyEmojis)]
	emojiIdx++
	return e
}
