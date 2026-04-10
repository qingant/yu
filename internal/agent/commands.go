package agent

import (
	"bufio"
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
		var idx int
		if len(parts) > 1 {
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 1 || n > len(sessions) {
				fmt.Printf("Invalid session number. Use 1-%d\n", len(sessions))
				return commandResult{handled: true}
			}
			idx = n - 1
		} else {
			// Show list and prompt
			for i, s := range sessions {
				age := formatAge(s.UpdatedAt)
				fmt.Printf("  %d) %s \033[2m(%s, %s)\033[0m\n", i+1, s.Title, s.Model, age)
			}
			fmt.Print("\nResume [1]: ")
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(answer)
			if answer == "" {
				idx = 0
			} else {
				n, err := strconv.Atoi(answer)
				if err != nil || n < 1 || n > len(sessions) {
					fmt.Println("Cancelled.")
					return commandResult{handled: true}
				}
				idx = n - 1
			}
		}
		return commandResult{handled: true, resumeID: sessions[idx].ID}

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
  /exit              Exit`)
}

// modelInfo represents a model from the API.
type modelInfo struct {
	ID       string
	Display  string
	Provider string // "anthropic" or "openai"
}

// cached model list
var cachedModels []modelInfo

func pickModel(current string) string {
	fmt.Print("\033[2mFetching models...\033[0m")
	models := fetchAvailableModels()
	// Clear the "Fetching..." line
	fmt.Print("\r\033[K")

	if len(models) == 0 {
		fmt.Println("No models found. Type model name directly: /model <name>")
		return ""
	}

	fmt.Printf("\n  Current: \033[1m%s\033[0m\n\n", current)
	for i, m := range models {
		marker := "  "
		if m.ID == current {
			marker = "\033[1;32m>\033[0m "
		}
		label := m.Display
		if label == "" {
			label = m.ID
		}
		fmt.Printf("  %s%d) %-35s \033[2m%s\033[0m\n", marker, i+1, label, m.Provider)
	}
	fmt.Printf("\n  Pick [1-%d] or type model name: ", len(models))

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return ""
	}

	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(models) {
		chosen := models[n-1].ID
		if chosen == current {
			fmt.Println("  (already using this model)")
			return ""
		}
		return chosen
	}

	if answer == current {
		fmt.Println("  (already using this model)")
		return ""
	}
	return answer
}

// fetchAvailableModels queries all configured API endpoints for model lists.
func fetchAvailableModels() []modelInfo {
	if cachedModels != nil {
		return cachedModels
	}

	var models []modelInfo

	// Anthropic
	anthropicKey := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if anthropicKey == "" {
		anthropicKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	anthropicBase := os.Getenv("ANTHROPIC_BASE_URL")
	if anthropicBase == "" {
		anthropicBase = "https://api.anthropic.com"
	}
	if anthropicKey != "" {
		if m := fetchAnthropicModels(anthropicBase, anthropicKey); len(m) > 0 {
			models = append(models, m...)
		}
	}

	// OpenAI
	openaiKey := os.Getenv("OPENAI_API_KEY")
	openaiBase := os.Getenv("OPENAI_BASE_URL")
	if openaiBase == "" {
		openaiBase = "https://api.openai.com"
	}
	if openaiKey != "" {
		if m := fetchOpenAIModels(openaiBase, openaiKey); len(m) > 0 {
			models = append(models, m...)
		}
	}

	cachedModels = models
	return models
}

func fetchAnthropicModels(baseURL, apiKey string) []modelInfo {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

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
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []modelInfo
	for _, m := range result.Data {
		id := m.ID
		// API returns "anthropic/claude-xxx" — strip the org prefix
		if idx := strings.LastIndex(id, "/"); idx >= 0 {
			id = id[idx+1:]
		}
		models = append(models, modelInfo{
			ID:       id,
			Display:  m.DisplayName,
			Provider: "anthropic",
		})
	}
	return models
}

func fetchOpenAIModels(baseURL, apiKey string) []modelInfo {
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
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []modelInfo
	for _, m := range result.Data {
		models = append(models, modelInfo{
			ID:       m.ID,
			Display:  m.ID,
			Provider: "openai",
		})
	}
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
