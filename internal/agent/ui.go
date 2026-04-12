package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// globalProgram is the running tea.Program reference for goroutine sends.
var globalProgram *tea.Program

// --- Messages ---

type (
	outputMsg      string               // chunk from stdout pipe
	tickMsg        time.Time            // 1-second tick for status bar
	initMsg        struct{}             // print session history after TUI starts
	slashSelectMsg struct{ cmd string } // user picked from slash menu
	modelSwitchMsg struct {
		model           string
		providerKey     string
		reasoningEffort string
	}
	turnDoneMsg struct {
		lastInput   int
		elapsed     time.Duration
		err         error
		interrupted bool
	}
	interactMsg struct{ req interactRequest } // tool wants user input
)

// --- State ---

type appState int

const (
	stateIdle     appState = iota
	stateWorking           // agent turn or slash command running
	stateInteract          // waiting for user input (ask_user / select)
	stateHelp              // showing help overlay
)

// --- Model ---

type uiModel struct {
	textarea  textarea.Model
	spinner   spinner.Model
	state     appState
	output    *strings.Builder // accumulated output (pointer to survive value copies)
	width     int
	height    int
	now       time.Time
	turnStart time.Time // when current turn started

	// Interaction state
	interactReq    *interactRequest
	interactCursor int
	interactInput  textarea.Model // for free text input
	interactFilter string

	// Business context
	session         *Session
	provider        Provider
	system          []SystemBlock
	tools           []ToolDef
	executor        *ToolExecutor
	bgManager       *BgManager
	st              *stats
	projectDir      string
	wsDir           string
	modelName       string
	maxTokens       int
	reasoningEffort string
	ctx             context.Context
	cancel          context.CancelFunc

	// Turn cancellation
	turnCancel context.CancelFunc
}

func newUIModel(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int, reasoningEffort string,
) uiModel {
	ta := textarea.New()
	ta.Placeholder = "Message... (Enter send, Ctrl+J newline, /help)"
	ta.Focus()
	ta.CharLimit = 0
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")

	ia := textarea.New()
	ia.Placeholder = "Type your answer..."
	ia.CharLimit = 0
	ia.SetHeight(1)
	ia.ShowLineNumbers = false
	ia.KeyMap.InsertNewline.SetKeys("ctrl+j")

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	ctx, cancel := context.WithCancel(context.Background())
	output := &strings.Builder{}

	// Seed welcome content into output buffer
	writeWelcome(output, modelName, projectDir, session)

	return uiModel{
		textarea:        ta,
		spinner:         sp,
		state:           stateIdle,
		output:          output,
		projectDir:      projectDir,
		wsDir:           wsDir,
		modelName:       modelName,
		maxTokens:       maxTokens,
		reasoningEffort: reasoningEffort,
		session:         session,
		provider:        provider,
		system:          system,
		tools:           tools,
		executor:        executor,
		bgManager:       bgManager,
		st:              st,
		ctx:             ctx,
		cancel:          cancel,
		interactInput:   ia,
		now:             time.Now(),
	}
}

func writeWelcome(buf *strings.Builder, model, projectDir string, session *Session) {
	buf.WriteString("\n")
	buf.WriteString(fmt.Sprintf("    ○\n"))
	buf.WriteString(fmt.Sprintf("   /|\\      %s  Yu%s%s (愚)%s\n", boldCyan, reset, dim, reset))
	buf.WriteString(fmt.Sprintf("    |       %s  live each day as if it's the last%s\n", dim, reset))
	buf.WriteString(fmt.Sprintf("   / \\\n"))
	buf.WriteString("\n")

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

	maxW := 0
	for _, line := range info {
		if len(line) > maxW {
			maxW = len(line)
		}
	}
	maxW += 2

	buf.WriteString(fmt.Sprintf("  %s╭%s╮%s\n", dim, strings.Repeat("─", maxW), reset))
	for _, line := range info {
		pad := maxW - len(line)
		if pad < 0 {
			pad = 0
		}
		buf.WriteString(fmt.Sprintf("  %s│%s%s%s%s│%s\n", dim, reset, line, strings.Repeat(" ", pad), dim, reset))
	}
	buf.WriteString(fmt.Sprintf("  %s╰%s╯%s\n", dim, strings.Repeat("─", maxW), reset))

	if len(session.Messages) == 0 {
		buf.WriteString(fmt.Sprintf("\n  %sType /help • Ctrl+J newline • Ctrl+G editor • Ctrl+L clear%s\n", dim, reset))
	}
	buf.WriteString("\n")
}

func (m uiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spinner.Tick, tickEvery(), func() tea.Msg { return initMsg{} })
}

func tickEvery() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// --- Update ---

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.textarea.SetWidth(msg.Width)
		m.interactInput.SetWidth(msg.Width)
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		if isClockTicking() {
			playTick()
		}
		return m, tickEvery()

	case spinner.TickMsg:
		// Always update spinner to keep tick chain alive
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case outputMsg:
		// No longer used — relay prints directly via program.Println()
		return m, nil

	case slashSelectMsg:
		m.state = stateIdle
		m.interactReq = nil
		if msg.cmd != "" {
			return m.submit(msg.cmd)
		}
		return m, nil

	case modelSwitchMsg:
		if msg.providerKey != "" {
			if resolved, ok := lookupProvider(msg.providerKey); ok {
				activeProvider = &resolved
				m.provider = NewProviderWithProtocol(resolved.Protocol, msg.model, resolved.APIKey, resolved.BaseURL, m.maxTokens, effectiveReasoningEffort(msg.reasoningEffort, m.reasoningEffort))
				m.session.Provider = resolved.Key
				saveActiveProvider(m.wsDir, resolved.Key)
			}
		}
		if msg.model != "" {
			m.modelName = msg.model
			m.session.Model = msg.model
			saveActiveModel(m.wsDir, msg.model)
		}
		if msg.reasoningEffort != "" {
			m.reasoningEffort = msg.reasoningEffort
			m.session.ReasoningEffort = msg.reasoningEffort
			saveActiveReasoningEffort(m.wsDir, msg.reasoningEffort)
			if activeProvider != nil {
				m.provider = NewProviderWithProtocol(activeProvider.Protocol, m.modelName, activeProvider.APIKey, activeProvider.BaseURL, m.maxTokens, m.reasoningEffort)
			}
		}
		return m, nil

	case initMsg:
		// Print session history through pipe (goes to relay → program.Println)
		if len(m.session.Messages) > 0 {
			go func() {
				printSessionHistory(m.session.Messages)
			}()
		}
		return m, nil

	case interactMsg:
		m.state = stateInteract
		m.interactReq = &msg.req
		m.interactCursor = msg.req.StartIdx
		m.interactFilter = ""
		if msg.req.Options == nil {
			m.interactInput.Reset()
			m.interactInput.Focus()
		} else if len(msg.req.Options) > 0 {
			if m.interactCursor < 0 {
				m.interactCursor = 0
			}
			if m.interactCursor >= len(msg.req.Options) {
				m.interactCursor = len(msg.req.Options) - 1
			}
		}
		return m, nil

	case turnDoneMsg:
		m.state = stateIdle
		m.turnCancel = nil
		// Note: stats/errors were already printed via fmt (through pipe)
		// by runAgentTurn before sending this message. Just do bookkeeping.
		if msg.err != nil {
			if msg.interrupted {
				if len(m.session.Messages) > 0 && m.session.Messages[len(m.session.Messages)-1].Role == "user" {
					m.session.Messages = m.session.Messages[:len(m.session.Messages)-1]
				}
			}
		} else {
			m.st.turns.Add(1)
		}
		syncStats(m.st, m.session, m.wsDir)
		autoCompact(msg.lastInput, m.session, m.provider)
		m.textarea.Focus()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Pass to active input
	switch m.state {
	case stateIdle:
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		// Auto-grow: minimum 2, +1 per extra newline
		lines := strings.Count(m.textarea.Value(), "\n") + 2
		if lines < 2 {
			lines = 2
		}
		if lines > 12 {
			lines = 12
		}
		m.textarea.SetHeight(lines)
		return m, cmd
	case stateInteract:
		if m.interactReq != nil && m.interactReq.Options == nil {
			var cmd tea.Cmd
			m.interactInput, cmd = m.interactInput.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// --- Help overlay ---
	if m.state == stateHelp {
		m.state = stateIdle
		return m, nil
	}

	// --- Interact state ---
	if m.state == stateInteract && m.interactReq != nil {
		return m.handleInteractKey(msg)
	}

	// --- Working state: can type, ESC/Ctrl+C cancels, Enter blocked ---
	if m.state == stateWorking {
		if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyEsc {
			if m.turnCancel != nil {
				m.turnCancel()
			}
			return m, nil
		}
		if msg.Type == tea.KeyEnter {
			return m, nil // don't submit while working
		}
		// Pass other keys to textarea so user can type ahead
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}

	// --- Idle state ---
	switch msg.Type {
	case tea.KeyCtrlC:
		m.bgManager.StopAll()
		m.cancel()
		return m, tea.Quit

	case tea.KeyEsc:
		m.textarea.Reset()
		return m, nil

	case tea.KeyTab:
		return m.completeSlashCmd()

	case tea.KeyCtrlL:
		return m, tea.ClearScreen

	case tea.KeyCtrlG:
		return m, m.openEditor(m.textarea.Value())

	case tea.KeyEnter:
		text := strings.TrimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		m.textarea.Reset()
		m.textarea.SetHeight(2)
		return m.submit(text)

	case tea.KeyRunes:
		// "/" on empty input → show slash command menu
		if len(msg.Runes) == 1 && msg.Runes[0] == '/' && m.textarea.Value() == "" {
			return m.showSlashMenu()
		}
		// "?" on empty input → show help overlay
		if len(msg.Runes) == 1 && msg.Runes[0] == '?' && m.textarea.Value() == "" {
			return m.showHelp()
		}
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m uiModel) handleInteractKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	req := m.interactReq

	if req.Options != nil {
		matches := m.filteredInteractIndices()
		if len(matches) == 0 {
			m.interactCursor = 0
		} else if m.interactCursor >= len(matches) {
			m.interactCursor = len(matches) - 1
		}

		// Selection mode
		switch msg.Type {
		case tea.KeyEnter:
			if len(matches) == 0 {
				return m, nil
			}
			selected := req.Options[matches[m.interactCursor]]
			req.Response <- selected
			m.state = stateWorking
			m.interactReq = nil
			m.interactFilter = ""
			m.output.WriteString(fmt.Sprintf("  %s✓ %s%s\n", green, selected, reset))
			return m, nil

		case tea.KeyEsc, tea.KeyCtrlC:
			req.Response <- ""
			m.state = stateWorking
			m.interactReq = nil
			m.interactFilter = ""
			return m, nil

		case tea.KeyUp:
			if m.interactCursor > 0 {
				m.interactCursor--
			}
			return m, nil

		case tea.KeyDown:
			if m.interactCursor < len(matches)-1 {
				m.interactCursor++
			}
			return m, nil

		case tea.KeyBackspace, tea.KeyCtrlH:
			if m.interactFilter != "" {
				r := []rune(m.interactFilter)
				m.interactFilter = string(r[:len(r)-1])
				m.interactCursor = 0
			}
			return m, nil

		case tea.KeyRunes:
			if len(msg.Runes) > 0 {
				m.interactFilter += string(msg.Runes)
				m.interactCursor = 0
			}
			return m, nil
		}
		return m, nil
	}

	// Free text input mode
	switch msg.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(m.interactInput.Value())
		req.Response <- text
		m.state = stateWorking
		m.interactReq = nil
		return m, nil

	case tea.KeyEsc, tea.KeyCtrlC:
		req.Response <- ""
		m.state = stateWorking
		m.interactReq = nil
		return m, nil
	}

	var cmd tea.Cmd
	m.interactInput, cmd = m.interactInput.Update(msg)
	return m, cmd
}

func (m uiModel) filteredInteractIndices() []int {
	if m.interactReq == nil || m.interactReq.Options == nil {
		return nil
	}
	if m.interactFilter == "" {
		indices := make([]int, len(m.interactReq.Options))
		for i := range m.interactReq.Options {
			indices[i] = i
		}
		return indices
	}

	filter := strings.ToLower(m.interactFilter)
	var prefix []int
	var contains []int
	for i, opt := range m.interactReq.Options {
		label := strings.ToLower(stripANSI(opt))
		switch {
		case strings.HasPrefix(label, filter):
			prefix = append(prefix, i)
		case strings.Contains(label, filter):
			contains = append(contains, i)
		}
	}
	return append(prefix, contains...)
}

func (m *uiModel) submit(input string) (tea.Model, tea.Cmd) {
	// Print user message through pipe → relay → program.Println (preserved in scroll history)
	ts := time.Now().Format("15:04:05")
	fmt.Printf("\n%syu%s %s%s %s%s>%s %s\n\n", cyan, reset, dim, ts, shortModel(m.modelName), reset, reset, input)

	if input == "/clock" {
		toggleClock()
		if isClockTicking() {
			m.output.WriteString("🔔 Clock ticking on\n")
		} else {
			m.output.WriteString("🔕 Clock ticking off\n")
		}
		return *m, nil
	}

	// Slash commands
	if strings.HasPrefix(input, "/") {
		m.state = stateWorking
		go m.runSlashCommand(input)
		return *m, nil
	}

	// !cmd
	if strings.HasPrefix(input, "!") {
		shellCmd := strings.TrimPrefix(input, "!")
		shellCmd = strings.TrimSpace(shellCmd)
		if shellCmd != "" {
			m.state = stateWorking
			go m.runShellCommand(shellCmd)
		}
		return *m, nil
	}

	// User message
	_, expanded := expandAtFiles(input, m.projectDir)
	m.session.Messages = append(m.session.Messages, Message{
		Role: "user",
		Content: []ContentBlock{
			{Type: "text", Text: expanded},
		},
	})

	m.state = stateWorking
	m.turnStart = time.Now()
	turnCtx, turnCancel := context.WithCancel(m.ctx)
	m.turnCancel = turnCancel
	go func() {
		fmt.Printf("%s⟳ thinking...%s\n", dim, reset)
		m.runAgentTurn(turnCtx, turnCancel)
	}()

	return *m, nil
}

func (m *uiModel) runAgentTurn(ctx context.Context, cancel context.CancelFunc) {
	turnStart := time.Now()
	lastInput, err := agentTurn(ctx, m.provider, m.system, &m.session.Messages, m.tools, m.executor, m.st)
	elapsed := time.Since(turnStart)
	interrupted := ctx.Err() != nil
	cancel()

	// Print stats/errors through pipe (same stdout as agent output) so ordering is correct
	if err != nil {
		if interrupted {
			fmt.Fprintf(os.Stderr, "\n%s↩ Interrupted%s\n", yellow, reset)
		} else {
			fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", boldRed, err, reset)
		}
	} else {
		cacheRead := m.st.totalCacheRead.Load()
		printTurnStats(int64(lastInput), cacheRead, elapsed)
	}

	globalProgram.Send(turnDoneMsg{
		lastInput:   lastInput,
		elapsed:     elapsed,
		err:         err,
		interrupted: interrupted,
	})
}

func (m *uiModel) runSlashCommand(input string) {
	result := handleSlashCommand(input, m.session, m.projectDir, m.wsDir, m.provider, m.bgManager, m.st)
	if strings.HasPrefix(input, "/model") && (result.switchProvider != "" || result.switchModel != "") {
		model := m.modelName
		if result.switchModel != "" {
			model = result.switchModel
		}
		providerName := result.switchProvider
		if providerName == "" && m.session.Provider != "" {
			providerName = m.session.Provider
		}
		if providerName != "" {
			if resolved, ok := lookupProvider(providerName); ok {
				providerName = resolved.Name
			}
		}
		if providerName == "" {
			providerName = "unknown"
		}
		fmt.Printf("Switched to %s:%s\n", providerName, model)
		globalProgram.Send(modelSwitchMsg{model: model, providerKey: result.switchProvider})
		globalProgram.Send(turnDoneMsg{})
		return
	}
	if strings.HasPrefix(input, "/reasoning") && result.switchReasoning != "" {
		fmt.Printf("Reasoning effort: %s\n", result.switchReasoning)
		globalProgram.Send(modelSwitchMsg{reasoningEffort: result.switchReasoning})
		globalProgram.Send(turnDoneMsg{})
		return
	}
	if result.newSession {
		next := NewSession(m.modelName)
		next.Provider = m.session.Provider
		next.ReasoningEffort = m.session.ReasoningEffort
		if activeProvider != nil {
			next.Provider = activeProvider.Key
		}
		*m.session = *next
		m.system = buildSystemPrompt(m.projectDir, findMemoryFile(m.wsDir))
	}
	if result.resumeID != "" {
		loaded, err := LoadSession(m.wsDir, result.resumeID)
		if err == nil {
			*m.session = *loaded
			if m.session.Model != "" {
				m.modelName = m.session.Model
			}
			if effort := normalizeReasoningEffort(m.session.ReasoningEffort); effort != "" {
				m.reasoningEffort = effort
				saveActiveReasoningEffort(m.wsDir, effort)
			}
			if resolved, ok := detectProviderConfig(m.modelName, m.session.Provider); ok {
				activeProvider = &resolved
				m.provider = NewProviderWithProtocol(resolved.Protocol, m.modelName, resolved.APIKey, resolved.BaseURL, m.maxTokens, m.reasoningEffort)
				m.session.Provider = resolved.Key
				saveActiveProvider(m.wsDir, resolved.Key)
			}
			turns := countUserTurns(m.session.Messages)
			fmt.Printf("Resumed: %s (%d turns)\n\n", m.session.Title, turns)
			printSessionHistory(m.session.Messages)
		}
	}
	if result.switchProvider != "" {
		if resolved, ok := lookupProvider(result.switchProvider); ok {
			activeProvider = &resolved
			m.provider = NewProviderWithProtocol(resolved.Protocol, m.modelName, resolved.APIKey, resolved.BaseURL, m.maxTokens, m.reasoningEffort)
			m.session.Provider = resolved.Key
			saveActiveProvider(m.wsDir, resolved.Key)
		}
	}
	if result.switchModel != "" {
		m.modelName = result.switchModel
		m.session.Model = m.modelName
		if result.switchProvider != "" {
			if activeProvider != nil {
				m.provider = NewProviderWithProtocol(activeProvider.Protocol, m.modelName, activeProvider.APIKey, activeProvider.BaseURL, m.maxTokens, m.reasoningEffort)
			}
		} else if p, ok := switchFromActiveProvider(m.modelName, m.reasoningEffort); ok {
			m.provider = p
			if activeProvider != nil {
				m.session.Provider = activeProvider.Key
				saveActiveProvider(m.wsDir, activeProvider.Key)
			}
		} else {
			resolved, ok := detectProviderConfig(m.modelName, m.session.Provider)
			if ok {
				activeProvider = &resolved
				m.provider = NewProviderWithProtocol(resolved.Protocol, m.modelName, resolved.APIKey, resolved.BaseURL, m.maxTokens, m.reasoningEffort)
				m.session.Provider = resolved.Key
				saveActiveProvider(m.wsDir, resolved.Key)
			}
		}
		saveActiveModel(m.wsDir, m.modelName)
	}
	if result.switchReasoning != "" {
		m.reasoningEffort = result.switchReasoning
		m.session.ReasoningEffort = result.switchReasoning
		saveActiveReasoningEffort(m.wsDir, result.switchReasoning)
		if activeProvider != nil {
			m.provider = NewProviderWithProtocol(activeProvider.Protocol, m.modelName, activeProvider.APIKey, activeProvider.BaseURL, m.maxTokens, m.reasoningEffort)
		}
	}
	globalProgram.Send(turnDoneMsg{}) // reuse to signal done
}

func (m *uiModel) runShellCommand(shellCmd string) {
	output := execDirectCommand(shellCmd, m.projectDir)
	m.session.Messages = append(m.session.Messages, Message{
		Role: "user",
		Content: []ContentBlock{
			{Type: "text", Text: fmt.Sprintf("[User ran shell command: %s]\n\n%s", shellCmd, stripControlChars(output))},
		},
	})
	if m.wsDir != "" {
		m.session.Save(m.wsDir)
	}
	globalProgram.Send(turnDoneMsg{})
}

func (m *uiModel) showHelp() (tea.Model, tea.Cmd) {
	m.state = stateHelp
	return *m, nil
}

func (m *uiModel) showSlashMenu() (tea.Model, tea.Cmd) {
	m.state = stateInteract
	resp := make(chan string, 1)
	m.interactReq = &interactRequest{
		Question: "Commands:",
		Options:  slashCommands,
		Response: resp,
	}
	m.interactCursor = 0
	// When user selects, submit the command
	go func() {
		selected := <-resp
		if selected != "" {
			// Trim trailing space from command name
			selected = strings.TrimSpace(selected)
			globalProgram.Send(slashSelectMsg{cmd: selected})
		} else {
			globalProgram.Send(slashSelectMsg{cmd: ""})
		}
	}()
	return *m, nil
}

func (m uiModel) completeSlashCmd() (tea.Model, tea.Cmd) {
	text := m.textarea.Value()
	if !strings.HasPrefix(text, "/") {
		return m, nil
	}
	var matches []string
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd, text) {
			matches = append(matches, cmd)
		}
	}
	if len(matches) == 1 {
		m.textarea.SetValue(matches[0] + " ")
		m.textarea.CursorEnd()
	} else if len(matches) > 1 {
		prefix := matches[0]
		for _, match := range matches[1:] {
			for !strings.HasPrefix(match, prefix) {
				prefix = prefix[:len(prefix)-1]
			}
		}
		if len(prefix) > len(text) {
			m.textarea.SetValue(prefix)
			m.textarea.CursorEnd()
		}
	}
	return m, nil
}

func (m uiModel) openEditor(prefill string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}
	tmp, err := os.CreateTemp("", "yu-input-*.md")
	if err != nil {
		return nil
	}
	if prefill != "" {
		tmp.WriteString(prefill + "\n")
	}
	tmp.Close()
	c := exec.Command(editor, tmp.Name())
	return tea.ExecProcess(c, func(err error) tea.Msg {
		defer os.Remove(tmp.Name())
		if err != nil {
			return nil
		}
		data, _ := os.ReadFile(tmp.Name())
		text := strings.TrimSpace(string(data))
		if text != "" {
			return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
		}
		return nil
	})
}

func effectiveReasoningEffort(next, current string) string {
	if effort := normalizeReasoningEffort(next); effort != "" {
		return effort
	}
	if effort := normalizeReasoningEffort(current); effort != "" {
		return effort
	}
	return "medium"
}

// --- View ---

func (m uiModel) View() string {
	w := m.width
	if w < 40 {
		w = 80
	}

	var b strings.Builder

	// Two-line margin
	b.WriteString("\n\n")

	// Separator line with status
	barWidth := m.width
	if barWidth < 40 {
		barWidth = 80
	}
	status := fmt.Sprintf(" %s● idle%s ", green, reset)
	if m.state == stateWorking {
		elapsed := time.Since(m.turnStart)
		status = fmt.Sprintf(" %s%s working %s%s %s(ESC)%s ", yellow, m.spinner.View(), formatDuration(elapsed), reset, dim, reset)
	} else if m.state == stateInteract {
		status = fmt.Sprintf(" %s? waiting%s ", cyan, reset)
	} else if m.state == stateHelp {
		status = fmt.Sprintf(" %s? help%s ", cyan, reset)
	}
	statusLen := visibleLen(status)
	lineLen := barWidth - statusLen
	if lineLen < 4 {
		lineLen = 4
	}
	leftLine := lineLen / 2
	rightLine := lineLen - leftLine
	b.WriteString(fmt.Sprintf("%s%s%s%s%s%s%s\n", dim, strings.Repeat("─", leftLine), reset, status, dim, strings.Repeat("─", rightLine), reset))

	// --- Input area ---
	switch m.state {
	case stateIdle, stateWorking:
		b.WriteByte('\n')
		b.WriteString(m.textarea.View())
		b.WriteByte('\n')

	case stateHelp:
		helpLines := []string{
			"",
			fmt.Sprintf("  %sCommands%s", bold, reset),
			fmt.Sprintf("  %s/help              %sShow help%s", dim, reset, reset),
			fmt.Sprintf("  %s/model             %sSwitch model%s", dim, reset, reset),
			fmt.Sprintf("  %s/reasoning         %sSet reasoning effort%s", dim, reset, reset),
			fmt.Sprintf("  %s/sessions          %sList sessions%s", dim, reset, reset),
			fmt.Sprintf("  %s/resume            %sResume a session%s", dim, reset, reset),
			fmt.Sprintf("  %s/new               %sNew session%s", dim, reset, reset),
			fmt.Sprintf("  %s/compact           %sCompress context%s", dim, reset, reset),
			fmt.Sprintf("  %s/stats             %sToken usage%s", dim, reset, reset),
			fmt.Sprintf("  %s/remember <text>   %sSave to memory%s", dim, reset, reset),
			fmt.Sprintf("  %s/rollback          %sRestore snapshot%s", dim, reset, reset),
			"",
			fmt.Sprintf("  %sKeys%s", bold, reset),
			fmt.Sprintf("  %sEnter%s send  %sCtrl+J%s newline  %sCtrl+G%s editor  %sCtrl+L%s clear  %sESC%s cancel", dim, reset, dim, reset, dim, reset, dim, reset, dim, reset),
			fmt.Sprintf("  %s/%s commands  %s?%s help  %s!cmd%s shell  %s@file%s attach", dim, reset, dim, reset, dim, reset, dim, reset),
			"",
			fmt.Sprintf("  %s(press any key to close)%s", dim, reset),
			"",
		}
		for _, line := range helpLines {
			b.WriteString(line + "\n")
		}

	case stateInteract:
		if m.interactReq != nil {
			if m.interactReq.Question != "" {
				b.WriteString(fmt.Sprintf("\n  %s%s%s\n", bold, m.interactReq.Question, reset))
			}
			if m.interactReq.Options != nil {
				matches := m.filteredInteractIndices()
				filterLabel := m.interactFilter
				if filterLabel == "" {
					filterLabel = dim + "type to filter" + reset
				}
				b.WriteString(fmt.Sprintf("  %sFilter:%s %s\n", dim, reset, filterLabel))

				if len(matches) == 0 {
					b.WriteString(fmt.Sprintf("  %s(no matches)%s\n", dim, reset))
					break
				}

				maxVisible := 8
				if m.height > 0 {
					if available := m.height - 10; available > 3 && available < maxVisible {
						maxVisible = available
					}
				}
				if maxVisible < 3 {
					maxVisible = 3
				}

				start := 0
				if len(matches) > maxVisible {
					start = m.interactCursor - maxVisible/2
					if start < 0 {
						start = 0
					}
					if end := start + maxVisible; end > len(matches) {
						start = len(matches) - maxVisible
					}
				}
				end := start + maxVisible
				if end > len(matches) {
					end = len(matches)
				}

				if start > 0 {
					b.WriteString(fmt.Sprintf("  %s↑ %d more%s\n", dim, start, reset))
				}
				for i := start; i < end; i++ {
					opt := m.interactReq.Options[matches[i]]
					if i == m.interactCursor {
						b.WriteString(fmt.Sprintf("  %s❯ %s%s\n", cyan, opt, reset))
					} else {
						b.WriteString(fmt.Sprintf("    %s\n", opt))
					}
				}
				if end < len(matches) {
					b.WriteString(fmt.Sprintf("  %s↓ %d more%s\n", dim, len(matches)-end, reset))
				}
			} else {
				b.WriteString(m.interactInput.View())
				b.WriteByte('\n')
			}
		}
	}

	// Status bar (bottom)
	b.WriteByte('\n')
	providerName := "Anthropic"
	if activeProvider != nil {
		providerName = activeProvider.Name
	}
	sessionDur := formatSessionDuration(m.session)
	b.WriteString(renderStatusBar(barWidth, m.projectDir, providerName, m.modelName, "", sessionDur))

	return b.String()
}

// --- stdout pipe relay ---
//
// Reads from the pipe in a goroutine. Complete lines are printed above the
// TUI via program.Println() (safe from goroutines, NOT from Update handlers).
// Partial lines are buffered until a newline arrives.

func startOutputRelay(pr *os.File, p *tea.Program) {
	var lineBuf strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			// Filter control chars that break bubbletea
			chunk = strings.ReplaceAll(chunk, "\r", "")
			chunk = strings.ReplaceAll(chunk, "\033[K", "")
			chunk = strings.ReplaceAll(chunk, "\033[2K", "")

			lineBuf.WriteString(chunk)
			content := lineBuf.String()

			// Print all complete lines
			lastNL := strings.LastIndex(content, "\n")
			if lastNL >= 0 {
				lines := strings.Split(content[:lastNL], "\n")
				for _, line := range lines {
					p.Println(line)
				}
				lineBuf.Reset()
				if lastNL+1 < len(content) {
					lineBuf.WriteString(content[lastNL+1:])
				}
			}
		}
		if err != nil {
			// Flush remaining
			if lineBuf.Len() > 0 {
				p.Println(lineBuf.String())
			}
			return
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- Entry point ---

func RunInteractive(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int, reasoningEffort string,
) {
	m := newUIModel(session, provider, system, tools, executor, bgManager,
		st, projectDir, wsDir, modelName, maxTokens, reasoningEffort)

	// Print welcome to real stdout before hijacking (stays in scroll history)
	if m.output.Len() > 0 {
		fmt.Print(m.output.String())
		m.output.Reset()
	}

	// Hijack stdout/stderr → pipe → program.Println (in relay goroutine)
	origStdout := os.Stdout
	origStderr := os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipe error: %v\n", err)
		return
	}
	os.Stdout = pw
	os.Stderr = pw

	// Create program writing to original stdout
	p := tea.NewProgram(m, tea.WithOutput(origStdout))
	globalProgram = p

	// Set global interact function — tools/commands use this
	setGlobalInteract(func(req interactRequest) string {
		p.Send(interactMsg{req: req})
		return <-req.Response
	})

	// Relay pipe output to bubbletea
	go startOutputRelay(pr, p)

	if _, err := p.Run(); err != nil {
		os.Stdout = origStdout
		os.Stderr = origStderr
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
	}

	// Cleanup
	globalProgram = nil
	setGlobalInteract(nil)
	pw.Close()
	pr.Close()
	os.Stdout = origStdout
	os.Stderr = origStderr
}
