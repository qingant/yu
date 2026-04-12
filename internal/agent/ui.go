package agent

import (
	"bufio"
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

// --- Approach ---
//
// stdout is hijacked to an os.Pipe before bubbletea starts.
// A background goroutine reads from the pipe and sends outputMsg to bubbletea
// via program.Send() (non-blocking, no deadlock).
// agentTurn runs in a plain goroutine (not tea.Cmd), writing to the hijacked
// stdout as usual. bubbletea's View always shows the output + input area.

// --- Messages ---

type (
	outputMsg    string // a line of output from stdout pipe
	turnDoneMsg  struct {
		lastInput   int
		elapsed     time.Duration
		err         error
		interrupted bool
	}
	slashDoneMsg   struct{}
	editorResultMsg struct{ text string }
	editorErrMsg    struct{ err error }
)

// --- Model ---

type appState int

const (
	stateIdle appState = iota
	stateWorking
)

type uiModel struct {
	textarea  textarea.Model
	spinner   spinner.Model
	state     appState
	output    strings.Builder // accumulated output lines
	width     int
	height    int

	// Turn cancellation
	turnCancel context.CancelFunc

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

	// stdout pipe (for cleanup)
	pipeWriter  *os.File
	origStdout  *os.File
	program     *tea.Program
}

func newUIModel(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
	ctx context.Context,
) uiModel {
	ta := textarea.New()
	ta.Placeholder = "Message... (Enter send, Ctrl+J newline, /help)"
	ta.Focus()
	ta.CharLimit = 0
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return uiModel{
		textarea:   ta,
		spinner:    sp,
		state:      stateIdle,
		session:    session,
		provider:   provider,
		system:     system,
		tools:      tools,
		executor:   executor,
		bgManager:  bgManager,
		st:         st,
		projectDir: projectDir,
		wsDir:      wsDir,
		modelName:  modelName,
		maxTokens:  maxTokens,
		ctx:        ctx,
	}
}

func (m uiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spinner.Tick)
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.textarea.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		if m.state == stateWorking {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case outputMsg:
		m.output.WriteString(string(msg))
		m.output.WriteByte('\n')
		return m, nil

	case turnDoneMsg:
		m.state = stateIdle
		m.turnCancel = nil
		if msg.err != nil {
			if msg.interrupted {
				m.output.WriteString(fmt.Sprintf("\n%s↩ Interrupted%s\n", yellow, reset))
				if len(m.session.Messages) > 0 && m.session.Messages[len(m.session.Messages)-1].Role == "user" {
					m.session.Messages = m.session.Messages[:len(m.session.Messages)-1]
				}
			} else {
				m.output.WriteString(fmt.Sprintf("\n%sError: %v%s\n", boldRed, msg.err, reset))
			}
		} else {
			m.st.turns.Add(1)
			cacheRead := m.st.totalCacheRead.Load()
			// Stats line
			m.output.WriteString(fmt.Sprintf("\n  %s  %s  %s\n",
				randomEmoji(),
				formatTokens(int64(msg.lastInput)),
				formatDuration(msg.elapsed)))
			_ = cacheRead
		}
		syncStats(m.st, m.session, m.wsDir)
		autoCompact(msg.lastInput, m.session, m.provider)
		return m, nil

	case slashDoneMsg:
		m.state = stateIdle
		return m, nil

	case editorResultMsg:
		if msg.text != "" {
			m.textarea.SetValue(msg.text)
		}
		return m, nil

	case editorErrMsg:
		m.output.WriteString(fmt.Sprintf("Editor error: %v\n", msg.err))
		return m, nil
	}

	// Pass to textarea when idle
	if m.state == stateIdle {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)

		// Auto-grow textarea
		lines := strings.Count(m.textarea.Value(), "\n") + 1
		h := lines + 1
		if h < 2 {
			h = 2
		}
		if h > 12 {
			h = 12
		}
		m.textarea.SetHeight(h)

		return m, cmd
	}

	return m, nil
}

func (m uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		if m.state == stateWorking {
			if m.turnCancel != nil {
				m.turnCancel()
			}
			return m, nil
		}
		m.bgManager.StopAll()
		return m, tea.Quit

	case tea.KeyEsc:
		if m.state == stateWorking {
			if m.turnCancel != nil {
				m.turnCancel()
			}
			return m, nil
		}
		m.textarea.Reset()
		return m, nil

	case tea.KeyTab:
		if m.state == stateIdle {
			return m.completeSlashCmd()
		}
		return m, nil

	case tea.KeyCtrlG:
		if m.state == stateIdle {
			return m, m.openEditor(m.textarea.Value())
		}
		return m, nil

	case tea.KeyEnter:
		if m.state != stateIdle {
			return m, nil
		}
		text := strings.TrimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		m.textarea.Reset()
		m.textarea.SetHeight(2)
		return m.submit(text)
	}

	if m.state == stateIdle {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *uiModel) submit(input string) (tea.Model, tea.Cmd) {
	// Show user input
	m.output.WriteString(fmt.Sprintf("\n%syu>%s %s\n", boldGreen, reset, input))

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

	// Start agent turn in goroutine
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
	cancel()

	m.program.Send(turnDoneMsg{
		lastInput:   lastInput,
		elapsed:     elapsed,
		err:         err,
		interrupted: ctx.Err() != nil,
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
	m.program.Send(slashDoneMsg{})
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
	m.program.Send(slashDoneMsg{})
}

func (m *uiModel) handleTurnDone(msg turnDoneMsg) {
	if msg.err != nil {
		if msg.interrupted {
			if len(m.session.Messages) > 0 && m.session.Messages[len(m.session.Messages)-1].Role == "user" {
				m.session.Messages = m.session.Messages[:len(m.session.Messages)-1]
			}
		}
	}
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
		return func() tea.Msg { return editorErrMsg{err: err} }
	}
	if prefill != "" {
		tmp.WriteString(prefill + "\n")
	}
	tmp.Close()
	c := exec.Command(editor, tmp.Name())
	return tea.ExecProcess(c, func(err error) tea.Msg {
		defer os.Remove(tmp.Name())
		if err != nil {
			return editorErrMsg{err: err}
		}
		data, err := os.ReadFile(tmp.Name())
		if err != nil {
			return editorErrMsg{err: err}
		}
		return editorResultMsg{text: strings.TrimSpace(string(data))}
	})
}

// --- View ---

func (m uiModel) View() string {
	if m.height == 0 {
		return ""
	}

	var b strings.Builder

	// Output area — scrollable history
	outputStr := m.output.String()
	outputLines := strings.Split(outputStr, "\n")

	// Calculate available height for output
	inputHeight := m.textarea.Height() + 1 // +1 for prompt line
	if m.state == stateWorking {
		inputHeight = 2 // prompt + spinner
	}
	availHeight := m.height - inputHeight - 1
	if availHeight < 1 {
		availHeight = 1
	}

	// Show last N lines of output
	if len(outputLines) > availHeight {
		outputLines = outputLines[len(outputLines)-availHeight:]
	}
	for _, line := range outputLines {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	// Pad to push input to bottom
	pad := availHeight - len(outputLines)
	for i := 0; i < pad; i++ {
		b.WriteByte('\n')
	}

	// Prompt + input area
	bg := ""
	bgCount := m.bgManager.RunningCount()
	if bgCount > 0 {
		bg = fmt.Sprintf(" %s[%d bg]%s", yellow, bgCount, reset)
	}
	prompt := fmt.Sprintf("%syu%s %s%s%s%s❯ ", boldGreen, reset, dim, shortModel(m.modelName), reset, bg)

	if m.state == stateIdle {
		b.WriteString(prompt)
		b.WriteByte('\n')
		b.WriteString(m.textarea.View())
	} else {
		b.WriteString(prompt)
		b.WriteString(fmt.Sprintf("%s %s...%s", m.spinner.View(), "working", sDim(" ESC to cancel")))
	}

	return b.String()
}

func sDim(s string) string {
	return fmt.Sprintf("%s%s%s", dim, s, reset)
}

// --- stdout pipe relay ---

func startOutputRelay(pr *os.File, p *tea.Program) {
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		p.Send(outputMsg(line))
	}
}

// --- Entry point ---

func RunInteractive(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
) {
	ctx := context.Background()
	m := newUIModel(session, provider, system, tools, executor, bgManager,
		st, projectDir, wsDir, modelName, maxTokens, ctx)

	// Hijack stdout to pipe — all fmt.Printf goes through pipe → outputMsg
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipe error: %v\n", err)
		return
	}
	os.Stdout = pw
	m.origStdout = origStdout
	m.pipeWriter = pw

	// Create program with output to original stdout (not the pipe)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithOutput(origStdout))
	m.program = p

	// Start relay goroutine
	go startOutputRelay(pr, p)

	if _, err := p.Run(); err != nil {
		os.Stdout = origStdout
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
	}

	// Restore stdout
	pw.Close()
	pr.Close()
	os.Stdout = origStdout
}
