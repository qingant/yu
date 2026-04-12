package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// slashCommands defines all available commands for tab completion.
var slashCommands = []string{
	"/help", "/exit", "/quit",
	"/clear", "/compact", "/new",
	"/sessions", "/resume",
	"/model", "/init",
	"/reasoning",
	"/jobs", "/logs", "/kill", "/rollback", "/stats",
	"/remember", "/memory", "/forget",
	"/copilot-login", "/copilot-logout",
	"/clock",
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
		errPrintln("yu agent-loop must be run inside a yu sandbox.")
		errPrintln("Use: yu agent")
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

	// Background process manager
	tmpDir := os.TempDir()
	bgManager := NewBgManager(projectDir, filepath.Join(tmpDir, "yu-bg"), func(p *BgProcess) {
		status := "exited"
		if p.ExitCode != 0 {
			status = fmt.Sprintf("failed (exit %d)", p.ExitCode)
		}
		outPrintf("\n%s[bg #%d %s] %s%s\n", dim, p.ID, status, truncCmd(p.Command, 40), reset)
	})

	executor := &ToolExecutor{
		ProjectDir:  projectDir,
		BashTimeout: 120 * time.Second,
		BgManager:   bgManager,
	}
	tools := toolDefs()
	var st stats

	// Parent process watchdog — if sandbox exits, we must exit too.
	// When parent dies on macOS/Linux, ppid becomes 1 (launchd/init).
	parentPID := os.Getppid()
	go func() {
		for {
			time.Sleep(2 * time.Second)
			if os.Getppid() != parentPID {
				bgManager.StopAll()
				os.Exit(0)
			}
		}
	}()

	// Session resolution
	execPrompt := os.Getenv("YU_EXEC_PROMPT")
	var session *Session
	if os.Getenv("YU_NEW_SESSION") == "1" || execPrompt != "" {
		session = NewSession(model)
	} else if sid := os.Getenv("YU_SESSION"); sid != "" {
		loaded, err := LoadSession(wsDir, sid)
		if err != nil {
			errPrintf("Session %s not found, starting new\n", sid)
			session = NewSession(model)
		} else {
			session = loaded
		}
	} else {
		session = resolveSession(wsDir, model)
	}
	providerKey := os.Getenv("YU_PROVIDER")
	if providerKey == "" && session.Provider != "" {
		providerKey = session.Provider
	}
	if providerKey == "" {
		providerKey = loadActiveProvider(wsDir)
	}
	reasoningEffort := normalizeReasoningEffort(os.Getenv("YU_REASONING_EFFORT"))
	if reasoningEffort == "" {
		reasoningEffort = normalizeReasoningEffort(session.ReasoningEffort)
	}
	if reasoningEffort == "" {
		reasoningEffort = loadActiveReasoningEffort(wsDir)
	}
	if reasoningEffort == "" {
		reasoningEffort = "medium"
	}
	if session.Model != "" && session.Model != model {
		model = session.Model
	}
	resolvedProvider, ok := detectProviderConfig(model, providerKey)
	if !ok {
		errPrintln("No API key found.")
		errPrintln("Set ANTHROPIC_AUTH_TOKEN or OPENAI_API_KEY in your shell environment,")
		errPrintln("or add it to your .yu/env file (yu config set ANTHROPIC_AUTH_TOKEN sk-ant-...)")
		os.Exit(1)
	}
	activeProvider = &resolvedProvider
	session.Provider = resolvedProvider.Key
	session.ReasoningEffort = reasoningEffort
	saveActiveProvider(wsDir, resolvedProvider.Key)
	saveActiveReasoningEffort(wsDir, reasoningEffort)

	maxTokens := 8192
	provider := NewProviderWithProtocol(
		resolvedProvider.Protocol,
		model,
		resolvedProvider.APIKey,
		resolvedProvider.BaseURL,
		maxTokens,
		reasoningEffort,
	)

	// System prompt
	memoryFile := findMemoryFile(wsDir)
	system := buildSystemPrompt(projectDir, memoryFile)

	// --- Exec mode: one-shot, no interactive UI ---
	if execPrompt != "" {
		session.Messages = append(session.Messages, Message{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: execPrompt}},
		})
		execCtx := context.Background()
		_, err := agentTurn(execCtx, provider, system, &session.Messages, tools, executor, &st)
		if err != nil {
			errPrintf("Error: %v\n", err)
			os.Exit(1)
		}
		outPrintln()
		st.turns.Add(1)
		syncStats(&st, session, wsDir)
		bgManager.StopAll()
		return
	}

	// --- Interactive mode ---

	RunInteractive(session, provider, system, tools, executor, bgManager, &st,
		projectDir, wsDir, model, maxTokens, reasoningEffort)
	bgManager.StopAll()
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

func printWelcome(model, projectDir string, session *Session) {
	outPrintln()

	// ASCII art boy + logo
	logo := []string{
		`    ○       `,
		`   /|\      ` + boldCyan + `  Yu` + reset + dim + ` (愚)` + reset,
		`    |       ` + dim + `  live each day as if it's the last` + reset,
		`   / \      `,
	}
	for _, line := range logo {
		outPrintln(line)
	}
	outPrintln()

	// Info box
	home, _ := os.UserHomeDir()
	dir := projectDir
	if strings.HasPrefix(dir, home) {
		dir = "~" + dir[len(home):]
	}

	info := []string{
		fmt.Sprintf("  Model    %s", shortModel(model)),
		fmt.Sprintf("  Project  %s", dir),
	}

	if len(session.Messages) > 0 {
		turns := countUserTurns(session.Messages)
		info = append(info, fmt.Sprintf("  Session  %s (%d turns)", session.Title, turns))
	}

	// Find max width
	maxW := 0
	for _, line := range info {
		if len(line) > maxW {
			maxW = len(line)
		}
	}
	maxW += 2

	outPrintf("  %s╭%s╮%s\n", dim, strings.Repeat("─", maxW), reset)
	for _, line := range info {
		pad := maxW - len(line)
		if pad < 0 {
			pad = 0
		}
		outPrintf("  %s│%s%s%s%s│%s\n", dim, reset, line, strings.Repeat(" ", pad), dim, reset)
	}
	outPrintf("  %s╰%s╯%s\n", dim, strings.Repeat("─", maxW), reset)

	if len(session.Messages) > 0 {
		outPrintln()
		printSessionHistory(session.Messages)
	} else {
		outPrintf("\n  %sType /help for commands • Ctrl+J newline • Ctrl+G editor%s\n", dim, reset)
	}
	outPrintln()
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
					outPrintf("%syu>%s %s\n\n", boldGreen, reset, b.Text)
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
				outPrintln()
			}
			// Print tool calls + their results
			for _, b := range m.Content {
				if b.Type == "tool_use" {
					outPrintln()
					printToolCall(b)
					if result, ok := resultByID[b.ID]; ok {
						printToolResult(b.Name, result)
					}
				}
			}
		}
	}
	outPrintf("\n%s─── end of history ───%s\n\n", dim, reset)
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
	outPrintf("\n  %s%s  %s%s  %s  %s%s\n",
		dim, emoji, formatTokens(tokens), cache, formatDuration(elapsed), now, reset)
}

// --- Prompt ---

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

		// Don't append an empty assistant message if the turn was interrupted
		if len(response.Blocks) == 0 && ctx.Err() != nil {
			return 0, ctx.Err()
		}

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
		outPrintln()
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

	outPrintf("  %s%s %s%s %s%s%s\n", cyan, icon, name, reset, dim, preview, reset)
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
		outPrintf("  %s✗ %s%s\n", red, preview, reset)
	} else {
		// Indent tool output
		indented := "    " + strings.ReplaceAll(preview, "\n", "\n    ")
		outPrintf("%s%s%s\n", dim, indented, reset)
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
	// In TUI mode, the spinner is rendered by bubbletea's View.
	// This goroutine is a no-op — just wait for stop signal.
	go func() {
		defer close(s.stopped)
		<-s.stop
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
	Blocks           []ContentBlock
	StopReason       string
	InputTokens      int
	OutputTokens     int
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
	outPrintf("  Recent session: %s%s%s (%d turns, %s)\n",
		bold, latest.Title, reset, turns, formatAge(latest.UpdatedAt))
	outPrintf("  Resume? [Y/n]: ")
	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func resolveSession(wsDir, model string) *Session {
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

func detectProviderConfig(model, preferredProvider string) (providerInfo, bool) {
	if p, ok := lookupProvider(preferredProvider); ok {
		return p, true
	}

	if isAnthropicModel(model) {
		if p, ok := lookupProvider("anthropic"); ok {
			return p, true
		}
	}
	if p, ok := lookupProvider("openai"); ok {
		return p, true
	}
	if p, ok := lookupProvider("copilot"); ok {
		return p, true
	}
	if p, ok := lookupProvider("yu-custom"); ok {
		return p, true
	}
	if p, ok := lookupProvider("anthropic"); ok {
		return p, true
	}
	return providerInfo{}, false
}

// activeProvider is set by /model picker — determines API key/base for subsequent calls.
var activeProvider *providerInfo

// switchFromActiveProvider creates a Provider using the globally selected provider context.
func switchFromActiveProvider(model, reasoningEffort string) (Provider, bool) {
	if activeProvider == nil {
		return nil, false
	}
	return NewProviderWithProtocol(modelProtocol(activeProvider, model), model, activeProvider.APIKey, activeProvider.BaseURL, 8192, reasoningEffort), true
}

func modelProtocol(p *providerInfo, model string) string {
	if p != nil && p.Protocol != "" {
		return p.Protocol
	}
	if isAnthropicModel(model) {
		return "anthropic"
	}
	return "openai"
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
		outPrintln(msg)
		return msg
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		msg := fmt.Sprintf("error: %v", err)
		outPrintln(msg)
		return msg
	}

	if err := cmd.Start(); err != nil {
		msg := fmt.Sprintf("error: %v", err)
		outPrintln(msg)
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
			outPrintln(line)
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
			outPrintf("%sExit code: %d%s\n", dim, exitErr.ExitCode(), reset)
		}
	}

	return output.String()
}

// autoCompact triggers compaction when the context is getting too large.
func autoCompact(lastInputTokens int, session *Session, provider Provider) {
	if lastInputTokens < autoCompactThreshold(provider) {
		return
	}
	if len(session.Messages) < 6 {
		return
	}
	outPrintf("\n  %s⟳ Auto-compacting context (%s input)...%s\n", yellow, formatTokens(int64(lastInputTokens)), reset)
	compactConversation(session, provider)
}

func autoCompactThreshold(provider Provider) int {
	switch provider.(type) {
	case *AnthropicProvider:
		return 256_000
	default:
		return 96_000
	}
}

// sanitizeMessages fixes message pairs before sending to the API.
// Handles two corruption cases from interrupted turns:
//  1. Orphaned tool_result: tool_result without matching tool_use in preceding assistant message
//  2. Orphaned tool_use: assistant has tool_use but next message lacks matching tool_result
func sanitizeMessages(messages []Message) []Message {
	result := make([]Message, 0, len(messages))

	for i, msg := range messages {
		// Skip messages with nil/empty content (from interrupted turns)
		if len(msg.Content) == 0 {
			continue
		}

		if msg.Role == "assistant" {
			// Check for tool_use blocks that lack matching tool_results
			var toolUseIDs []string
			for _, b := range msg.Content {
				if b.Type == "tool_use" && b.ID != "" {
					toolUseIDs = append(toolUseIDs, b.ID)
				}
			}

			if len(toolUseIDs) > 0 {
				// Collect tool_result IDs from the next message
				resultIDs := make(map[string]bool)
				if i+1 < len(messages) && messages[i+1].Role == "user" {
					for _, b := range messages[i+1].Content {
						if b.Type == "tool_result" {
							resultIDs[b.ToolUseID] = true
						}
					}
				}

				// Remove tool_use blocks without matching tool_result
				var filtered []ContentBlock
				for _, b := range msg.Content {
					if b.Type == "tool_use" && b.ID != "" && !resultIDs[b.ID] {
						continue // orphaned tool_use — skip
					}
					filtered = append(filtered, b)
				}

				if len(filtered) == 0 {
					continue // entire message was orphaned — skip
				}
				result = append(result, Message{Role: msg.Role, Content: filtered})
				continue
			}
		}

		if msg.Role == "user" {
			// Remove tool_result blocks without matching tool_use in preceding assistant
			hasToolResult := false
			for _, b := range msg.Content {
				if b.Type == "tool_result" {
					hasToolResult = true
					break
				}
			}

			if hasToolResult && i > 0 {
				validIDs := make(map[string]bool)
				prev := messages[i-1]
				if prev.Role == "assistant" {
					for _, b := range prev.Content {
						if b.Type == "tool_use" && b.ID != "" {
							validIDs[b.ID] = true
						}
					}
				}

				var filtered []ContentBlock
				for _, b := range msg.Content {
					if b.Type == "tool_result" && !validIDs[b.ToolUseID] {
						continue // orphaned tool_result — skip
					}
					if b.Type == "tool_result" && i < len(messages)-2 {
						if content, ok := b.Content.(string); ok {
							b.Content = compactToolResultContent(content)
						}
					}
					filtered = append(filtered, b)
				}

				if len(filtered) == 0 {
					continue
				}
				result = append(result, Message{Role: msg.Role, Content: filtered})
				continue
			}
		}

		result = append(result, msg)
	}

	return result
}

func compactToolResultContent(content string) string {
	const maxChars = 4000
	const maxLines = 80

	content = strings.TrimSpace(content)
	if content == "" {
		return content
	}

	lines := strings.Split(content, "\n")
	originalLineCount := len(lines)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	compacted := strings.Join(lines, "\n")
	if len(compacted) > maxChars {
		compacted = compacted[:maxChars]
	}
	compacted = strings.TrimSpace(compacted)
	if compacted == content && originalLineCount <= maxLines {
		return content
	}
	return fmt.Sprintf("[tool result summary; original output truncated]\n%s\n\n[truncated from %d lines]", compacted, originalLineCount)
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

func loadActiveProvider(wsDir string) string {
	if wsDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(wsDir, "active-provider"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func loadActiveReasoningEffort(wsDir string) string {
	if wsDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(wsDir, "active-reasoning-effort"))
	if err != nil {
		return ""
	}
	return normalizeReasoningEffort(strings.TrimSpace(string(data)))
}

func saveActiveModel(wsDir, model string) {
	if wsDir == "" {
		return
	}
	os.MkdirAll(wsDir, 0700)
	os.WriteFile(filepath.Join(wsDir, "active-model"), []byte(model+"\n"), 0600)
}

func saveActiveProvider(wsDir, provider string) {
	if wsDir == "" || provider == "" {
		return
	}
	os.MkdirAll(wsDir, 0700)
	os.WriteFile(filepath.Join(wsDir, "active-provider"), []byte(provider+"\n"), 0600)
}

func saveActiveReasoningEffort(wsDir, effort string) {
	effort = normalizeReasoningEffort(effort)
	if wsDir == "" || effort == "" {
		return
	}
	os.MkdirAll(wsDir, 0700)
	os.WriteFile(filepath.Join(wsDir, "active-reasoning-effort"), []byte(effort+"\n"), 0600)
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

// stripControlChars removes ANSI escape sequences and control characters from a string.
// Used to sanitize input before saving to history.
func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	runes := []rune(s)
	for i < len(runes) {
		r := runes[i]
		// Skip ANSI escape sequences: ESC [ ... final_byte
		if r == 0x1B && i+1 < len(runes) {
			i++ // skip ESC
			if i < len(runes) && runes[i] == '[' {
				i++ // skip [
				// Skip parameter bytes (0x30-0x3F), intermediate bytes (0x20-0x2F)
				for i < len(runes) && runes[i] >= 0x20 && runes[i] <= 0x3F {
					i++
				}
				// Skip intermediate bytes
				for i < len(runes) && runes[i] >= 0x20 && runes[i] <= 0x2F {
					i++
				}
				// Skip final byte (0x40-0x7E)
				if i < len(runes) && runes[i] >= 0x40 && runes[i] <= 0x7E {
					i++
				}
			} else {
				// Other escape sequence (ESC + one char), skip both
				if i < len(runes) {
					i++
				}
			}
			continue
		}
		// Keep printable chars, newlines, tabs
		if r == '\n' || r == '\t' || r >= 0x20 {
			b.WriteRune(r)
		}
		i++
	}
	return b.String()
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
