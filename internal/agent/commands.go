package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// commandResult tells the main loop what to do after a command.
type commandResult struct {
	handled         bool   // command was recognized
	newSession      bool   // start a new session
	resumeID        string // resume this session ID
	switchModel     string // change model
	switchProvider  string // change provider
	switchReasoning string // change reasoning effort
}

// handleSlashCommand processes user input starting with "/".
// Returns what action the loop should take.
func handleSlashCommand(input string, session *Session, projectDir, wsDir string, provider Provider, bgm *BgManager, st *stats) commandResult {
	parts := strings.Fields(input)
	cmd := parts[0]

	switch cmd {
	case "/exit", "/quit":
		outPrintln("Goodbye.")
		os.Exit(0)

	case "/clear":
		session.Messages = nil
		session.CompactSummary = ""
		outPrintln("Conversation cleared.")

	case "/new":
		return commandResult{handled: true, newSession: true}

	case "/sessions", "/ls":
		sessions := ListSessions(wsDir)
		if len(sessions) == 0 {
			outPrintln("No saved sessions.")
			return commandResult{handled: true}
		}
		outPrintln()
		for i, s := range sessions {
			age := formatAge(s.UpdatedAt)
			outPrintf("  \033[1m%d)\033[0m %s \033[2m(%s, %d turns, %s)\033[0m\n",
				i+1, s.Title, s.Model, s.Turns, age)
		}
		outPrintln()

	case "/resume":
		sessions := ListSessions(wsDir)
		if len(sessions) == 0 {
			outPrintln("No saved sessions.")
			return commandResult{handled: true}
		}
		if len(parts) > 1 {
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 1 || n > len(sessions) {
				outPrintf("Invalid session number. Use 1-%d\n", len(sessions))
				return commandResult{handled: true}
			}
			return commandResult{handled: true, resumeID: sessions[n-1].ID}
		}
		// Interactive selection
		outPrintln()
		var labels []string
		for _, s := range sessions {
			age := formatAge(s.UpdatedAt)
			labels = append(labels, fmt.Sprintf("%s %s(%s, %s)%s", s.Title, dim, s.Model, age, reset))
		}
		selected := uiSelect(labels)
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
		if chosenModel, chosenProvider, ok := pickModel(session.Model); ok {
			return commandResult{handled: true, switchModel: chosenModel, switchProvider: chosenProvider.Key}
		}

	case "/reasoning":
		if len(parts) > 1 {
			effort := normalizeReasoningEffort(parts[1])
			if effort == "" {
				outPrintln("Usage: /reasoning [low|medium|high]")
				return commandResult{handled: true}
			}
			return commandResult{handled: true, switchReasoning: effort}
		}
		selected := uiSelectAt(reasoningEffortOptions(session.ReasoningEffort), reasoningEffortIndex(session.ReasoningEffort))
		if selected == "" || selected == selectBack {
			return commandResult{handled: true}
		}
		effort := normalizeReasoningEffort(stripANSI(selected))
		if effort == "" {
			return commandResult{handled: true}
		}
		return commandResult{handled: true, switchReasoning: effort}

	case "/compact":
		compactConversation(session, provider)

	case "/remember":
		if len(parts) < 2 {
			outPrintln("Usage: /remember <text to remember>")
			return commandResult{handled: true}
		}
		text := strings.TrimPrefix(input, "/remember ")
		appendMemory(wsDir, text)
		outPrintln("Remembered.")

	case "/memory":
		showMemory(wsDir)

	case "/forget":
		clearMemory(wsDir)
		outPrintln("Memory cleared.")

	case "/jobs", "/bg":
		procs := bgm.List()
		if len(procs) == 0 {
			outPrintln("No background processes.")
		} else {
			outPrintln()
			for _, p := range procs {
				outPrintf("  %s\n", p.FormatStatus())
			}
			outPrintln()
		}

	case "/logs":
		if len(parts) < 2 {
			outPrintln("Usage: /logs <id> [lines]")
			return commandResult{handled: true}
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			outPrintln("Invalid process ID")
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
			errPrintf("%v\n", err)
		} else {
			outPrintln(logs)
		}

	case "/kill":
		if len(parts) < 2 {
			outPrintln("Usage: /kill <id>")
			return commandResult{handled: true}
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			outPrintln("Invalid process ID")
			return commandResult{handled: true}
		}
		if err := bgm.Stop(id); err != nil {
			errPrintf("%v\n", err)
		} else {
			outPrintf("Stopped #%d\n", id)
		}

	case "/copilot-login":
		outPrintln("Run outside the sandbox: yu github-copilot login")

	case "/copilot-logout":
		outPrintln("Run outside the sandbox: yu github-copilot logout")

	case "/rollback":
		doRollback(projectDir)

	case "/init":
		initProjectContract(projectDir)

	case "/stats":
		printStats(st, session, wsDir)

	case "/help":
		printHelp()

	default:
		outPrintf("Unknown command: %s (type /help)\n", cmd)
	}

	return commandResult{handled: true}
}

func printHelp() {
	outPrintln(`
Commands:
  /help              Show this help
  /init              Create Yu.md in project root
  /clear             Clear conversation
  /compact           Compress context
  /new               Start new session
  /sessions          List saved sessions
  /resume [n]        Resume a saved session
  /model [name]      Show or switch model
  /reasoning [level] Set reasoning effort (low|medium|high)
  /jobs              List background processes
  /logs <id> [n]     Show last n lines of process output
  /kill <id>         Stop a background process
  /stats             Show token usage (run, session, global)
  /rollback          Roll back project to a previous snapshot
  /remember <text>   Save a note to memory
  /memory            Show saved memory
  /forget            Clear all memory
  /copilot-login     Log in to GitHub Copilot
  /copilot-logout    Log out of GitHub Copilot
  /exit              Exit

  !<command>         Run shell command directly (output visible to model)
  @<file>            Attach file content to your message (tab to complete)
  line ending \      Continue input on next line
  ` + "```" + `...` + "```" + `           Multi-line block (start and end with ` + "```" + `)`)
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
	{ID: "gpt-5.4-mini", Display: "GPT-5.4 Mini"},
	{ID: "gpt-5.4-nano", Display: "GPT-5.4 Nano"},
	{ID: "o3-pro", Display: "o3-pro"},
	{ID: "o3", Display: "o3"},
	{ID: "o4-mini", Display: "o4-mini"},
	{ID: "o3-mini", Display: "o3-mini"},
	{ID: "gpt-4.1", Display: "GPT-4.1"},
	{ID: "gpt-4.1-mini", Display: "GPT-4.1 Mini"},
	{ID: "gpt-4.1-nano", Display: "GPT-4.1 Nano"},
}

// cached model lists (fetched from API)
var copilotModelCache []modelInfo
var customModelCache []modelInfo
var copilotModelCacheExpiresAt time.Time

var reasoningLevels = []string{"low", "medium", "high"}

type copilotModelCacheFile struct {
	CacheKey  string      `json:"cache_key"`
	FetchedAt time.Time   `json:"fetched_at"`
	Models    []modelInfo `json:"models"`
}

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

	// 3. GitHub Copilot
	copilotBase := os.Getenv("COPILOT_BASE_URL")
	if copilotBase != "" {
		providers = append(providers, providerInfo{
			Name: "GitHub Copilot", Key: "copilot",
			APIKey: "copilot", BaseURL: copilotBase, Protocol: "openai",
		})
	}

	// 4. Yu Custom (YU_BASE_URL + YU_API_KEY or YU_API_TOKEN)
	yuKey := os.Getenv("YU_API_KEY")
	if yuKey == "" {
		yuKey = os.Getenv("YU_API_TOKEN")
	}
	yuBase := os.Getenv("YU_BASE_URL")
	if yuKey != "" && yuBase != "" {
		providers = append(providers, providerInfo{
			Name: "Yu Custom", Key: "yu-custom",
			APIKey: yuKey, BaseURL: yuBase, Protocol: "openai",
		})
	}

	return providers
}

func normalizeReasoningEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return ""
	}
}

func reasoningEffortIndex(current string) int {
	current = normalizeReasoningEffort(current)
	for i, effort := range reasoningLevels {
		if effort == current {
			return i
		}
	}
	return 1
}

func reasoningEffortOptions(current string) []string {
	current = normalizeReasoningEffort(current)
	if current == "" {
		current = "medium"
	}
	options := make([]string, 0, len(reasoningLevels))
	for _, effort := range reasoningLevels {
		label := effort
		if effort == current {
			label = label + " " + dim + "(current)" + reset
		}
		options = append(options, label)
	}
	return options
}

func normalizeProviderKey(key string) string {
	switch strings.TrimSpace(strings.ToLower(key)) {
	case "":
		return ""
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	case "copilot", "github-copilot", "github_copilot":
		return "copilot"
	case "custom", "yu-custom", "yu_custom":
		return "yu-custom"
	default:
		return key
	}
}

func lookupProvider(key string) (providerInfo, bool) {
	key = normalizeProviderKey(key)
	for _, p := range detectProviders() {
		if p.Key == key {
			return p, true
		}
	}
	return providerInfo{}, false
}

// pickModel runs the two-level provider → model selection.
// Esc/q in model list goes back to provider. Esc/q in provider list cancels.
func pickModel(current string) (string, providerInfo, bool) {
	providers := detectProviders()
	if len(providers) == 0 {
		outPrintln("  No providers detected. Set API keys in environment or .yu/env")
		return "", providerInfo{}, false
	}

	var providerLabels []string
	for _, p := range providers {
		providerLabels = append(providerLabels, p.Name)
	}

	outPrintf("\n  Current: %s%s%s\n", bold, current, reset)

	for {
		// Step 1: Pick provider
		outPrintf("\n  %sProvider:%s\n", bold, reset)
		selectedLabel := uiSelect(providerLabels)
		if selectedLabel == "" || selectedLabel == selectBack {
			return "", providerInfo{}, false // exit
		}

		var chosen *providerInfo
		for i, l := range providerLabels {
			if l == selectedLabel {
				chosen = &providers[i]
				break
			}
		}
		if chosen == nil {
			return "", providerInfo{}, false
		}

		// Step 2: Get models
		if chosen.Key == "yu-custom" {
			outPrintf("  %sFetching models...%s\n", dim, reset)
		}
		models := modelsForProvider(*chosen)

		if len(models) == 0 {
			outPrintf("  No models available for %s\n", chosen.Name)
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

		outPrintf("\n  %sModel:%s  %s(u: back, q: exit)%s\n", bold, reset, dim, reset)
		selected := uiSelectAt(labels, currentIdx)
		if selected == selectBack {
			continue // back to provider
		}
		if selected == "" {
			return "", providerInfo{}, false // exit
		}

		for i, l := range labels {
			if l == selected {
				activeProvider = chosen
				return models[i].ID, *chosen, true
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
	case "copilot":
		return fetchCopilotModels(p.BaseURL, p.APIKey)
	case "yu-custom":
		return fetchCustomModels(p.BaseURL, p.APIKey)
	}
	return nil
}

func fetchCustomModels(baseURL, apiKey string) []modelInfo {
	if customModelCache != nil {
		return customModelCache
	}

	url := buildOpenAIURL(baseURL, "/models")
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

func fetchCopilotModels(baseURL, apiKey string) []modelInfo {
	cacheKey := copilotModelCacheKey(baseURL, apiKey)
	if copilotModelCache != nil && time.Now().Before(copilotModelCacheExpiresAt) {
		return copilotModelCache
	}
	if models, fetchedAt := loadCopilotModelCache(cacheKey); len(models) > 0 && time.Since(fetchedAt) < 24*time.Hour {
		copilotModelCache = models
		copilotModelCacheExpiresAt = fetchedAt.Add(24 * time.Hour)
		return models
	}

	url := buildOpenAIURL(baseURL, "/models")
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return loadStaleCopilotModels(cacheKey)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return loadStaleCopilotModels(cacheKey)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return loadStaleCopilotModels(cacheKey)
	}

	var result struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Capabilities struct {
				Type string `json:"type"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []modelInfo
	for _, m := range result.Data {
		// Only show chat-capable models
		if m.Capabilities.Type != "" && m.Capabilities.Type != "chat" {
			continue
		}
		display := m.ID
		if m.Name != "" {
			display = m.Name
		}
		models = append(models, modelInfo{ID: m.ID, Display: display})
	}
	models = validateCopilotModels(baseURL, apiKey, models)
	if len(models) == 0 {
		return loadStaleCopilotModels(cacheKey)
	}
	copilotModelCache = models
	copilotModelCacheExpiresAt = time.Now().Add(24 * time.Hour)
	saveCopilotModelCache(cacheKey, models)
	return models
}

func validateCopilotModels(baseURL, apiKey string, models []modelInfo) []modelInfo {
	if len(models) <= 1 {
		var valid []modelInfo
		for _, model := range models {
			ok, err := probeCopilotModel(baseURL, apiKey, model.ID)
			if err != nil {
				valid = append(valid, model)
				continue
			}
			if ok {
				valid = append(valid, model)
			}
		}
		return valid
	}

	const maxConcurrentProbes = 6
	keep := make([]bool, len(models))
	sem := make(chan struct{}, maxConcurrentProbes)
	var wg sync.WaitGroup

	for i, model := range models {
		wg.Add(1)
		go func(i int, model modelInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ok, err := probeCopilotModel(baseURL, apiKey, model.ID)
			if err != nil {
				// Keep the model on ambiguous/transient errors so temporary upstream
				// issues don't wipe the list. Only explicit unsupported responses
				// should remove a model.
				keep[i] = true
				return
			}
			keep[i] = ok
		}(i, model)
	}
	wg.Wait()

	valid := make([]modelInfo, 0, len(models))
	for i, model := range models {
		if keep[i] {
			valid = append(valid, model)
		}
	}
	return valid
}

func probeCopilotModel(baseURL, apiKey, model string) (bool, error) {
	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
		"stream":                false,
		"max_completion_tokens": 1,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequest("POST", buildOpenAIURL(baseURL, "/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}

	data, _ := io.ReadAll(resp.Body)
	bodyText := strings.ToLower(string(data))
	if strings.Contains(bodyText, "model_not_supported") ||
		strings.Contains(bodyText, "unsupported_api_for_model") ||
		strings.Contains(bodyText, "requested model is not supported") ||
		strings.Contains(bodyText, "not accessible via the /chat/completions endpoint") {
		return false, nil
	}

	return false, fmt.Errorf("probe status %d", resp.StatusCode)
}

func copilotModelCachePath() string {
	wsDir := os.Getenv("YU_WORKSPACE_DIR")
	if wsDir == "" {
		home, _ := os.UserHomeDir()
		wsDir = filepath.Join(home, ".yu")
	}
	return filepath.Join(wsDir, "copilot-model-cache.json")
}

func copilotModelCacheKey(baseURL, apiKey string) string {
	sum := sha256.Sum256([]byte(baseURL + "\n" + apiKey))
	return hex.EncodeToString(sum[:])
}

func loadCopilotModelCache(cacheKey string) ([]modelInfo, time.Time) {
	path := copilotModelCachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}
	}
	var cache copilotModelCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, time.Time{}
	}
	if cache.CacheKey != "" && cache.CacheKey != cacheKey {
		return nil, time.Time{}
	}
	return cache.Models, cache.FetchedAt
}

func loadStaleCopilotModels(cacheKey string) []modelInfo {
	models, fetchedAt := loadCopilotModelCache(cacheKey)
	if len(models) == 0 {
		return nil
	}
	copilotModelCache = models
	if fetchedAt.IsZero() {
		copilotModelCacheExpiresAt = time.Now().Add(time.Hour)
	} else {
		copilotModelCacheExpiresAt = fetchedAt.Add(24 * time.Hour)
	}
	return models
}

func saveCopilotModelCache(cacheKey string, models []modelInfo) {
	path := copilotModelCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	data, err := json.MarshalIndent(copilotModelCacheFile{
		CacheKey:  cacheKey,
		FetchedAt: time.Now(),
		Models:    models,
	}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0600)
}

// --- Stats ---

func printStats(st *stats, session *Session, wsDir string) {
	// Current run
	runIn := st.totalInput.Load()
	runOut := st.totalOutput.Load()
	runCache := st.totalCacheRead.Load()
	runTurns := int(st.turns.Load())

	outPrintf("\n  %sThis run:%s\n", bold, reset)
	outPrintf("    Turns:    %d\n", runTurns)
	outPrintf("    Input:    %s\n", formatTokens(runIn))
	outPrintf("    Output:   %s\n", formatTokens(runOut))
	outPrintf("    Total:    %s\n", formatTokens(runIn+runOut))
	if runCache > 0 {
		outPrintf("    Cached:   %s\n", formatTokens(runCache))
	}

	// Session (including previous runs in this session)
	outPrintf("\n  %sSession:%s  %s\n", bold, reset, session.Title)
	sesIn := session.Stats.InputTokens + runIn
	sesOut := session.Stats.OutputTokens + runOut
	sesTurns := session.Stats.Turns + runTurns
	outPrintf("    Turns:    %d\n", sesTurns)
	outPrintf("    Input:    %s\n", formatTokens(sesIn))
	outPrintf("    Output:   %s\n", formatTokens(sesOut))
	outPrintf("    Total:    %s\n", formatTokens(sesIn+sesOut))

	// Global (all sessions)
	if wsDir != "" {
		global := GlobalStats(wsDir)
		// Add current unsaved run
		global.InputTokens += runIn
		global.OutputTokens += runOut
		global.Turns += runTurns
		outPrintf("\n  %sAll sessions:%s\n", bold, reset)
		outPrintf("    Sessions: %d\n", len(ListSessions(wsDir)))
		outPrintf("    Turns:    %d\n", global.Turns)
		outPrintf("    Input:    %s\n", formatTokens(global.InputTokens))
		outPrintf("    Output:   %s\n", formatTokens(global.OutputTokens))
		outPrintf("    Total:    %s\n", formatTokens(global.InputTokens+global.OutputTokens))
	}
	outPrintln()
}

// --- Rollback ---

type snapshotEntry struct {
	ID      string
	Time    string
	Trigger string
	Summary string
	Age     string
}

func doRollback(projectDir string) {
	yuBin, err := os.Executable()
	if err != nil {
		errPrintf("Cannot find yu binary: %v\n", err)
		return
	}

	// Get snapshot list
	cmd := exec.Command(yuBin, "snapshots", projectDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		errPrintf("Failed to list snapshots: %v\n%s", err, string(output))
		return
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && strings.Contains(lines[0], "No snapshots")) {
		outPrintln("  No snapshots available.")
		return
	}

	// Parse snapshot lines: "#0   10:30:00  [init]               baseline"
	var entries []snapshotEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		e := parseSnapshotLine(line)
		if e.ID != "" {
			entries = append(entries, e)
		}
	}

	if len(entries) == 0 {
		outPrintln("  No snapshots available.")
		return
	}

	// Build labels for selection — most recent first
	var labels []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		label := fmt.Sprintf("#%-3s  %-12s  %-20s  %s", e.ID, e.Age, "["+e.Trigger+"]", e.Summary)
		labels = append(labels, label)
	}

	outPrintf("\n  %sSelect snapshot to restore:%s\n", bold, reset)
	selected := uiSelect(labels)
	if selected == "" || selected == selectBack {
		return
	}

	// Extract ID from selected label
	var selectedID string
	for i, l := range labels {
		if l == selected {
			selectedID = entries[len(entries)-1-i].ID
			break
		}
	}

	// Confirm
	outPrintf("\n  %sRollback to snapshot #%s? This will overwrite current files.%s\n", yellow, selectedID, reset)
	confirm := uiSelect([]string{"Yes, rollback", "Cancel"})
	if confirm != "Yes, rollback" {
		outPrintln("  Cancelled.")
		return
	}

	// Execute rollback
	rbCmd := exec.Command(yuBin, "rollback", selectedID, projectDir)
	rbOutput, err := rbCmd.CombinedOutput()
	if err != nil {
		errPrintf("  Rollback failed: %v\n%s", err, string(rbOutput))
		return
	}
	outPrintf("  %s✓ Rolled back to snapshot #%s%s\n", green, selectedID, reset)
}

func parseSnapshotLine(line string) snapshotEntry {
	// Format: "#0   10:30:00  [init]               baseline"
	line = strings.TrimPrefix(line, "#")
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return snapshotEntry{}
	}

	id := fields[0]
	timeStr := fields[1]
	trigger := strings.Trim(fields[2], "[]")
	summary := ""
	if len(fields) > 3 {
		summary = strings.Join(fields[3:], " ")
	}

	// Compute age from time string (HH:MM:SS format)
	age := computeAge(timeStr)

	return snapshotEntry{
		ID:      id,
		Time:    timeStr,
		Trigger: trigger,
		Summary: summary,
		Age:     age,
	}
}

func computeAge(timeStr string) string {
	// Parse HH:MM:SS, assume today
	t, err := time.Parse("15:04:05", timeStr)
	if err != nil {
		return timeStr
	}
	now := time.Now()
	snapTime := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
	// If parsed time is in the future, it was yesterday
	if snapTime.After(now) {
		snapTime = snapTime.Add(-24 * time.Hour)
	}
	return formatAge(snapTime)
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
			outPrintf("Contract file already exists: %s\n", name)
			return
		}
	}

	path := filepath.Join(projectDir, "Yu.md")
	template := `# Project Instructions

<!-- Yu reads this file as project context for the built-in agent. -->
<!-- Add coding conventions, project structure notes, or task-specific instructions here. -->
`
	if err := os.WriteFile(path, []byte(template), 0644); err != nil {
		errPrintf("Error creating Yu.md: %v\n", err)
		return
	}
	outPrintf("Created %s\n", path)
}

// compactConversation summarizes old messages and replaces them with a summary.
func compactConversation(session *Session, provider Provider) {
	if len(session.Messages) < 6 {
		outPrintln("Conversation too short to compact.")
		return
	}

	// Keep last 4 messages, summarize the rest
	keepCount := 4
	toSummarize := session.Messages[:len(session.Messages)-keepCount]
	kept := session.Messages[len(session.Messages)-keepCount:]

	outPrint("\033[2mCompacting... \033[0m")

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
					fmt.Fprintf(&transcript, "[Tool result for %s]: %s\n\n", b.ToolUseID, summarizeToolResultForCompact(content))
				}
			}
		case "assistant":
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					fmt.Fprintf(&transcript, "Assistant: %s\n\n", b.Text)
				case "tool_use":
					fmt.Fprintf(&transcript, "[Tool call: %s - %s]\n\n", b.Name, summarizeToolInputForCompact(b))
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
		errPrintf("\n\033[31mCompact failed: %v\033[0m\n", err)
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

	outPrintf("done (%d messages → summary + %d recent)\n", len(toSummarize), keepCount)
}

func summarizeToolResultForCompact(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "(empty)"
	}
	lines := strings.Split(content, "\n")
	previewLines := lines
	if len(previewLines) > 12 {
		previewLines = previewLines[:12]
	}
	preview := strings.Join(previewLines, "\n")
	if len(preview) > 1200 {
		preview = preview[:1200]
	}
	if len(lines) > len(previewLines) || len(content) > len(preview) {
		return fmt.Sprintf("%s\n... (%d total lines, truncated for compaction)", strings.TrimSpace(preview), len(lines))
	}
	return preview
}

func summarizeToolInputForCompact(block ContentBlock) string {
	preview := toolInputPreview(block)
	preview = strings.TrimSpace(preview)
	if preview == "" {
		return "invoked"
	}
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	return preview
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
		errPrintf("Error writing memory: %v\n", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- %s\n", text)
}

func showMemory(wsDir string) {
	path := memoryPath(wsDir)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		outPrintln("No memory saved. Use /remember <text> to add notes.")
		return
	}
	outPrintf("\n%s\n", string(data))
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
