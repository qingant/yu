package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commandResult tells the main loop what to do after a command.
type commandResult struct {
	handled     bool   // command was recognized
	newSession  bool   // start a new session
	resumeID    string // resume this session ID
	switchModel string // change model
}

// handleSlashCommand processes user input starting with "/".
// Returns what action the loop should take.
func handleSlashCommand(input string, session *Session, projectDir, wsDir string, provider Provider, bgm *BgManager) commandResult {
	parts := strings.Fields(input)
	cmd := parts[0]

	switch cmd {
	case "/exit", "/quit":
		fmt.Println("Goodbye.")
		os.Exit(0)

	case "/clear":
		session.Messages = nil
		session.CompactSummary = ""
		fmt.Println("Conversation cleared.")

	case "/new":
		return commandResult{handled: true, newSession: true}

	case "/sessions", "/ls":
		sessions := ListSessions(wsDir)
		if len(sessions) == 0 {
			fmt.Println("No saved sessions.")
			return commandResult{handled: true}
		}
		fmt.Println()
		for i, s := range sessions {
			age := formatAge(s.UpdatedAt)
			fmt.Printf("  \033[1m%d)\033[0m %s \033[2m(%s, %d turns, %s)\033[0m\n",
				i+1, s.Title, s.Model, s.Turns, age)
		}
		fmt.Println()

	case "/resume":
		sessions := ListSessions(wsDir)
		if len(sessions) == 0 {
			fmt.Println("No saved sessions.")
			return commandResult{handled: true}
		}
		if len(parts) > 1 {
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 1 || n > len(sessions) {
				fmt.Printf("Invalid session number. Use 1-%d\n", len(sessions))
				return commandResult{handled: true}
			}
			return commandResult{handled: true, resumeID: sessions[n-1].ID}
		}
		// Interactive selection
		fmt.Println()
		var labels []string
		for _, s := range sessions {
			age := formatAge(s.UpdatedAt)
			labels = append(labels, fmt.Sprintf("%s %s(%s, %s)%s", s.Title, dim, s.Model, age, reset))
		}
		selected := arrowSelect(labels)
		if selected == "" || selected == selectBack {
			return commandResult{handled: true}
		}
		for i, l := range labels {
			if l == selected {
				return commandResult{handled: true, resumeID: sessions[i].ID}
			}
		}
		return commandResult{handled: true}

	case "/model":
		if len(parts) > 1 {
			return commandResult{handled: true, switchModel: parts[1]}
		}
		// Interactive model picker
		if chosen := pickModel(session.Model); chosen != "" {
			return commandResult{handled: true, switchModel: chosen}
		}

	case "/compact":
		compactConversation(session, provider)

	case "/remember":
		if len(parts) < 2 {
			fmt.Println("Usage: /remember <text to remember>")
			return commandResult{handled: true}
		}
		text := strings.TrimPrefix(input, "/remember ")
		appendMemory(wsDir, text)
		fmt.Println("Remembered.")

	case "/memory":
		showMemory(wsDir)

	case "/forget":
		clearMemory(wsDir)
		fmt.Println("Memory cleared.")

	case "/jobs", "/bg":
		procs := bgm.List()
		if len(procs) == 0 {
			fmt.Println("No background processes.")
		} else {
			fmt.Println()
			for _, p := range procs {
				fmt.Printf("  %s\n", p.FormatStatus())
			}
			fmt.Println()
		}

	case "/logs":
		if len(parts) < 2 {
			fmt.Println("Usage: /logs <id> [lines]")
			return commandResult{handled: true}
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid process ID")
			return commandResult{handled: true}
		}
		tail := 50
		if len(parts) >= 3 {
			if n, err := strconv.Atoi(parts[2]); err == nil {
				tail = n
			}
		}
		logs, err := bgm.Logs(id, tail)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		} else {
			fmt.Println(logs)
		}

	case "/kill":
		if len(parts) < 2 {
			fmt.Println("Usage: /kill <id>")
			return commandResult{handled: true}
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid process ID")
			return commandResult{handled: true}
		}
		if err := bgm.Stop(id); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		} else {
			fmt.Printf("Stopped #%d\n", id)
		}

	case "/init":
		initProjectContract(projectDir)

	case "/help":
		printHelp()

	default:
		fmt.Printf("Unknown command: %s (type /help)\n", cmd)
	}

	return commandResult{handled: true}
}

func printHelp() {
	fmt.Println(`
Commands:
  /help              Show this help
  /init              Create Yu.md in project root
  /clear             Clear conversation
  /compact           Compress context
  /new               Start new session
  /sessions          List saved sessions
  /resume [n]        Resume a saved session
  /model [name]      Show or switch model
  /jobs              List background processes
  /logs <id> [n]     Show last n lines of process output
  /kill <id>         Stop a background process
  /remember <text>   Save a note to memory
  /memory            Show saved memory
  /forget            Clear all memory
  /exit              Exit

  !<command>         Run shell command directly (output visible to model)
  @<file>            Attach file content to your message (tab to complete)
  line ending \      Continue input on next line (multi-line)`)
}

// --- Provider & Model Selection ---

type providerInfo struct {
	Name     string
	Key      string
	APIKey   string
	BaseURL  string
	Protocol string // "anthropic" or "openai"
}

type modelInfo struct {
	ID      string
	Display string
}

// Fixed model lists for Anthropic and OpenAI — avoids litellm returning hundreds of unusable models.
var anthropicModels = []modelInfo{
	{ID: "claude-opus-4-6", Display: "Claude Opus 4.6"},
	{ID: "claude-sonnet-4-6", Display: "Claude Sonnet 4.6"},
	{ID: "claude-sonnet-4-5", Display: "Claude Sonnet 4.5"},
	{ID: "claude-haiku-4-5", Display: "Claude Haiku 4.5"},
}

var openaiModels = []modelInfo{
	{ID: "gpt-5.4", Display: "GPT-5.4"},
	{ID: "o3-pro", Display: "o3-pro"},
	{ID: "o3", Display: "o3"},
	{ID: "o4-mini", Display: "o4-mini"},
	{ID: "gpt-4.1", Display: "GPT-4.1"},
	{ID: "gpt-4.1-mini", Display: "GPT-4.1 Mini"},
	{ID: "gpt-4.1-nano", Display: "GPT-4.1 Nano"},
}

// cached model list for custom provider (fetched from API)
var customModelCache []modelInfo

// detectProviders returns available providers from environment.
func detectProviders() []providerInfo {
	var providers []providerInfo

	// 1. Anthropic
	aKey := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if aKey == "" {
		aKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if aKey != "" {
		base := os.Getenv("ANTHROPIC_BASE_URL")
		if base == "" {
			base = "https://api.anthropic.com"
		}
		providers = append(providers, providerInfo{
			Name: "Anthropic", Key: "anthropic",
			APIKey: aKey, BaseURL: base, Protocol: "anthropic",
		})
	}

	// 2. OpenAI
	oKey := os.Getenv("OPENAI_API_KEY")
	if oKey != "" {
		base := os.Getenv("OPENAI_BASE_URL")
		if base == "" {
			base = "https://api.openai.com"
		}
		providers = append(providers, providerInfo{
			Name: "OpenAI", Key: "openai",
			APIKey: oKey, BaseURL: base, Protocol: "openai",
		})
	}

	// 3. Yu Custom (YU_BASE_URL + YU_API_KEY)
	yuKey := os.Getenv("YU_API_KEY")
	yuBase := os.Getenv("YU_BASE_URL")
	if yuKey != "" && yuBase != "" {
		providers = append(providers, providerInfo{
			Name: "Yu Custom", Key: "yu-custom",
			APIKey: yuKey, BaseURL: yuBase, Protocol: "openai",
		})
	}

	return providers
}

// pickModel runs the two-level provider → model selection.
// Esc/q in model list goes back to provider. Esc/q in provider list cancels.
func pickModel(current string) string {
	providers := detectProviders()
	if len(providers) == 0 {
		fmt.Println("  No providers detected. Set API keys in environment or .yu/env")
		return ""
	}

	var providerLabels []string
	for _, p := range providers {
		providerLabels = append(providerLabels, p.Name)
	}

	fmt.Printf("\n  Current: %s%s%s\n", bold, current, reset)

	for {
		// Step 1: Pick provider
		fmt.Printf("\n  %sProvider:%s\n", bold, reset)
		selectedLabel := arrowSelect(providerLabels)
		if selectedLabel == "" || selectedLabel == selectBack {
			return "" // exit
		}

		var chosen *providerInfo
		for i, l := range providerLabels {
			if l == selectedLabel {
				chosen = &providers[i]
				break
			}
		}
		if chosen == nil {
			return ""
		}

		// Step 2: Get models
		if chosen.Key == "yu-custom" {
			fmt.Printf("  %sFetching models...%s", dim, reset)
		}
		models := modelsForProvider(*chosen)
		if chosen.Key == "yu-custom" {
			fmt.Print("\r\033[K")
		}

		if len(models) == 0 {
			fmt.Printf("  No models available for %s\n", chosen.Name)
			continue // back to provider
		}

		var labels []string
		currentIdx := 0
		for i, m := range models {
			label := m.Display
			if label == "" || label == m.ID {
				label = m.ID
			} else if label != m.ID {
				label = fmt.Sprintf("%-22s %s%s%s", label, dim, m.ID, reset)
			}
			labels = append(labels, label)
			if m.ID == current {
				currentIdx = i
			}
		}

		fmt.Printf("\n  %sModel:%s  %s(u: back, q: exit)%s\n", bold, reset, dim, reset)
		selected := arrowSelectAt(labels, currentIdx)
		if selected == selectBack {
			continue // back to provider
		}
		if selected == "" {
			return "" // exit
		}

		for i, l := range labels {
			if l == selected {
				activeProvider = chosen
				if models[i].ID == current {
					return ""
				}
				return models[i].ID
			}
		}
	}
}

// modelsForProvider returns the model list — fixed for anthropic/openai, fetched for custom.
func modelsForProvider(p providerInfo) []modelInfo {
	switch p.Key {
	case "anthropic":
		return anthropicModels
	case "openai":
		return openaiModels
	case "yu-custom":
		return fetchCustomModels(p.BaseURL, p.APIKey)
	}
	return nil
}

func fetchCustomModels(baseURL, apiKey string) []modelInfo {
	if customModelCache != nil {
		return customModelCache
	}

	url := strings.TrimSuffix(baseURL, "/") + "/v1/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []modelInfo
	for _, m := range result.Data {
		models = append(models, modelInfo{ID: m.ID, Display: m.ID})
	}
	customModelCache = models
	return models
}

// contractFileNames lists all recognized contract filenames for existence checks.
var contractFileNames = []string{
	"Yu.md", "yu.md", "YU.md", "yuyu.md", "Yuyu.md", "YuYu.md", "YUYU.md",
	"CLAUDE.md", "AGENTS.md", "agents.md", "GEMINI.md", "gemini.md", ".cursorrules",
}

func initProjectContract(projectDir string) {
	// Check if any contract file already exists
	for _, name := range contractFileNames {
		path := filepath.Join(projectDir, name)
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("Contract file already exists: %s\n", name)
			return
		}
	}

	path := filepath.Join(projectDir, "Yu.md")
	template := `# Project Instructions

<!-- Yu reads this file as project context for the built-in agent. -->
<!-- Add coding conventions, project structure notes, or task-specific instructions here. -->
`
	if err := os.WriteFile(path, []byte(template), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Yu.md: %v\n", err)
		return
	}
	fmt.Printf("Created %s\n", path)
}

// compactConversation summarizes old messages and replaces them with a summary.
func compactConversation(session *Session, provider Provider) {
	if len(session.Messages) < 6 {
		fmt.Println("Conversation too short to compact.")
		return
	}

	// Keep last 4 messages, summarize the rest
	keepCount := 4
	toSummarize := session.Messages[:len(session.Messages)-keepCount]
	kept := session.Messages[len(session.Messages)-keepCount:]

	fmt.Print("\033[2mCompacting... \033[0m")

	// Render conversation as a text transcript, then ask the model to summarize.
	// This avoids tool_use/tool_result validation issues and preserves full context.
	var transcript strings.Builder
	for _, m := range toSummarize {
		switch m.Role {
		case "user":
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					fmt.Fprintf(&transcript, "User: %s\n\n", b.Text)
				case "tool_result":
					content, _ := b.Content.(string)
					fmt.Fprintf(&transcript, "[Tool result for %s]: %s\n\n", b.ToolUseID, content)
				}
			}
		case "assistant":
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					fmt.Fprintf(&transcript, "Assistant: %s\n\n", b.Text)
				case "tool_use":
					fmt.Fprintf(&transcript, "[Tool call: %s(%s)]\n\n", b.Name, string(b.Input))
				}
			}
		}
	}

	summaryMessages := []Message{{
		Role: "user",
		Content: []ContentBlock{{
			Type: "text",
			Text: "Here is a conversation transcript. Summarize it in a few concise paragraphs. Focus on: what was requested, key decisions made, what was accomplished, and any pending work. This summary will replace the conversation history to save context space.\n\n---\n\n" + transcript.String(),
		}},
	}}

	system := []SystemBlock{{Type: "text", Text: "You are a helpful assistant. Summarize the provided conversation concisely."}}
	ch, err := provider.Stream(context.Background(), system, summaryMessages, nil)
	if err != nil {
		fmt.Printf("\n\033[31mCompact failed: %v\033[0m\n", err)
		return
	}

	var summary strings.Builder
	for evt := range ch {
		if evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Type == "text_delta" {
			summary.WriteString(evt.Delta.Text)
		}
	}

	session.CompactSummary = summary.String()
	session.Messages = append([]Message{{
		Role: "user",
		Content: []ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[Previous conversation summary]\n\n%s\n\n[End of summary — conversation continues below]", session.CompactSummary),
		}},
	}, {
		Role: "assistant",
		Content: []ContentBlock{{
			Type: "text",
			Text: "Understood. I have the context from the previous conversation. How can I help?",
		}},
	}}, kept...)

	fmt.Printf("done (%d messages → summary + %d recent)\n", len(toSummarize), keepCount)
}

// --- Memory helpers ---

func memoryPath(wsDir string) string {
	return filepath.Join(wsDir, "memory.md")
}

func appendMemory(wsDir string, text string) {
	os.MkdirAll(wsDir, 0700)
	path := memoryPath(wsDir)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing memory: %v\n", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- %s\n", text)
}

func showMemory(wsDir string) {
	path := memoryPath(wsDir)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		fmt.Println("No memory saved. Use /remember <text> to add notes.")
		return
	}
	fmt.Printf("\n%s\n", string(data))
}

func clearMemory(wsDir string) {
	os.Remove(memoryPath(wsDir))
}

func formatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
