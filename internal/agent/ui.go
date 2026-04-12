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
	outputMsg   string    // line from stdout pipe
	tickMsg     time.Time // 1-second tick for status bar
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
)

// --- Model ---

type uiModel struct {
	textarea textarea.Model
	spinner  spinner.Model
	state    appState
	output   *strings.Builder // accumulated output (pointer to survive value copies)
	width    int
	height   int
	now      time.Time

	// Interaction state
	interactReq     *interactRequest
	interactCursor  int
	interactInput   textarea.Model // for free text input

	// Business context
	session    *Session
	provider   Provider
	system     []SystemBlock
	tools      []ToolDef
	executor   *ToolExecutor
	bgManager  *BgManager
	st         *stats
	projectDir string
	wsDir      string
	modelName  string
	maxTokens  int
	ctx        context.Context
	cancel     context.CancelFunc

	// Turn cancellation
	turnCancel context.CancelFunc

}

func newUIModel(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
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
		textarea:   ta,
		spinner:    sp,
		state:      stateIdle,
		output:     output,
		projectDir: projectDir,
		wsDir:      wsDir,
		modelName:  modelName,
		maxTokens:  maxTokens,
		session:    session,
		provider:   provider,
		system:     system,
		tools:      tools,
		executor:   executor,
		bgManager:  bgManager,
		st:         st,
		ctx:        ctx,
		cancel:     cancel,
		interactInput: ia,
		now:        time.Now(),
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
	return tea.Batch(textarea.Blink, m.spinner.Tick, tickEvery())
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
		if m.state == stateWorking {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case outputMsg:
		m.output.WriteString(string(msg))
		return m, nil

	case interactMsg:
		m.state = stateInteract
		m.interactReq = &msg.req
		m.interactCursor = 0
		if msg.req.Options == nil {
			m.interactInput.Reset()
			m.interactInput.Focus()
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
		// Auto-grow
		lines := strings.Count(m.textarea.Value(), "\n") + 1
		h := lines + 1
		if h < 2 { h = 2 }
		if h > 12 { h = 12 }
		m.textarea.SetHeight(h)
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
		m.output.Reset()
		return m, nil

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
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m uiModel) handleInteractKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	req := m.interactReq

	if req.Options != nil {
		// Selection mode
		switch msg.Type {
		case tea.KeyEnter:
			selected := req.Options[m.interactCursor]
			req.Response <- selected
			m.state = stateWorking
			m.interactReq = nil
			m.output.WriteString(fmt.Sprintf("  %s✓ %s%s\n", green, selected, reset))
			return m, nil

		case tea.KeyEsc, tea.KeyCtrlC:
			req.Response <- ""
			m.state = stateWorking
			m.interactReq = nil
			return m, nil

		case tea.KeyUp:
			if m.interactCursor > 0 {
				m.interactCursor--
			}
			return m, nil

		case tea.KeyDown:
			if m.interactCursor < len(req.Options)-1 {
				m.interactCursor++
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

func (m *uiModel) submit(input string) (tea.Model, tea.Cmd) {
	ts := time.Now().Format("15:04:05")
	m.output.WriteString(fmt.Sprintf("\n%syu %s %s>%s %s\n\n", boldGreen, ts, shortModel(m.modelName), reset, input))

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
	turnCtx, turnCancel := context.WithCancel(m.ctx)
	m.turnCancel = turnCancel
	go m.runAgentTurn(turnCtx, turnCancel)

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
	if result.newSession {
		*m.session = *NewSession(m.modelName)
		m.system = buildSystemPrompt(m.projectDir, findMemoryFile(m.wsDir))
	}
	if result.resumeID != "" {
		loaded, err := LoadSession(m.wsDir, result.resumeID)
		if err == nil {
			*m.session = *loaded
			if m.session.Model != "" {
				m.modelName = m.session.Model
			}
		}
	}
	if result.switchModel != "" {
		m.modelName = result.switchModel
		m.session.Model = m.modelName
		if p, ok := switchFromActiveProvider(m.modelName); ok {
			m.provider = p
		} else {
			newKey, newBase := detectAPIConfig(m.modelName)
			if newKey != "" {
				m.provider = NewProvider(m.modelName, newKey, newBase, m.maxTokens)
			}
		}
		saveActiveModel(m.wsDir, m.modelName)
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

// --- View ---

func (m uiModel) View() string {
	w := m.width
	if w < 40 {
		w = 80
	}

	var b strings.Builder

	// Output area — in non-alt-screen mode, bubbletea redraws from
	// its starting position. We just render the last portion of output
	// to keep the view manageable.
	outputStr := m.output.String()
	if outputStr != "" {
		b.WriteString(outputStr)
		if !strings.HasSuffix(outputStr, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n\n")

	// --- Input area ---
	switch m.state {
	case stateIdle:
		bg := ""
		if m.bgManager.RunningCount() > 0 {
			bg = fmt.Sprintf(" %s[%d bg]%s", yellow, m.bgManager.RunningCount(), reset)
		}
		prompt := fmt.Sprintf("%syu%s %s%s%s%s❯ ", boldGreen, reset, dim, shortModel(m.modelName), reset, bg)
		b.WriteString(prompt + "\n")
		b.WriteString(m.textarea.View())
		b.WriteByte('\n')

	case stateWorking:
		bg := ""
		if m.bgManager.RunningCount() > 0 {
			bg = fmt.Sprintf(" %s[%d bg]%s", yellow, m.bgManager.RunningCount(), reset)
		}
		prompt := fmt.Sprintf("%syu%s %s%s%s%s❯ ", boldGreen, reset, dim, shortModel(m.modelName), reset, bg)
		b.WriteString(fmt.Sprintf("%s  %s %sworking...%s  %s(ESC to cancel)%s\n",
			prompt, m.spinner.View(), bold, reset, dim, reset))
		b.WriteString(m.textarea.View())
		b.WriteByte('\n')

	case stateInteract:
		if m.interactReq != nil {
			if m.interactReq.Question != "" {
				b.WriteString(fmt.Sprintf("  %s%s%s\n", bold, m.interactReq.Question, reset))
			}
			if m.interactReq.Options != nil {
				for i, opt := range m.interactReq.Options {
					if i == m.interactCursor {
						b.WriteString(fmt.Sprintf("  %s❯ %s%s\n", cyan, opt, reset))
					} else {
						b.WriteString(fmt.Sprintf("    %s\n", opt))
					}
				}
			} else {
				b.WriteString(m.interactInput.View())
				b.WriteByte('\n')
			}
		}
	}

	// Status bar
	status := fmt.Sprintf("%s● idle%s", green, reset)
	if m.state == stateWorking {
		status = fmt.Sprintf("%s◉ working%s", yellow, reset)
	} else if m.state == stateInteract {
		status = fmt.Sprintf("%s? waiting%s", cyan, reset)
	}

	providerName := "Anthropic"
	if activeProvider != nil {
		providerName = activeProvider.Name
	}
	sessionDur := formatSessionDuration(m.session)
	b.WriteString(renderStatusBar(w, m.projectDir, providerName, m.modelName, status, sessionDur))

	return b.String()
}

// --- stdout pipe relay ---

// outputMsg now carries raw text chunks (not just lines).
// View splits by \n for display.

func startOutputRelay(pr *os.File, p *tea.Program) {
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			p.Send(outputMsg(string(buf[:n])))
		}
		if err != nil {
			return
		}
	}
}

// --- Entry point ---

func RunInteractive(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
) {
	m := newUIModel(session, provider, system, tools, executor, bgManager,
		st, projectDir, wsDir, modelName, maxTokens)

	// Hijack stdout → pipe → outputMsg
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipe error: %v\n", err)
		return
	}
	os.Stdout = pw

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
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
	}

	// Cleanup
	globalProgram = nil
	setGlobalInteract(nil)
	pw.Close()
	pr.Close()
	os.Stdout = origStdout
}
