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

// program is the global Bubble Tea program reference.
// Used by agentTurn to print output above the TUI input area.
var program *tea.Program

// uiPrint prints a line above the TUI input area (or to stdout if no TUI).
func uiPrint(s string) {
	if program != nil {
		program.Println(s)
	} else {
		fmt.Println(s)
	}
}

// uiPrintf prints formatted text above the TUI input area.
func uiPrintf(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	// Strip trailing newline — program.Println adds one
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if program != nil {
		program.Println(s)
	} else {
		fmt.Println(s)
	}
}

// --- Tea Messages ---

type (
	turnDoneMsg struct {
		lastInput int
		elapsed   time.Duration
	}
	turnErrorMsg struct {
		err         error
		interrupted bool
	}
	editorResultMsg struct{ text string }
	editorErrorMsg  struct{ err error }
	submitMsg       struct{ text string }
)

// --- App State ---

type uiState int

const (
	uiIdle uiState = iota
	uiThinking
)

// --- Model ---

type uiModel struct {
	textarea textarea.Model
	spinner  spinner.Model
	state    uiState
	width    int

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
	model      string
	maxTokens  int
	ctx        context.Context
}

func newUIModel(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
	ctx context.Context,
) uiModel {
	ta := textarea.New()
	ta.Placeholder = "Message... (Enter send, Shift+Enter newline, /help commands)"
	ta.Focus()
	ta.CharLimit = 0
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j")

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return uiModel{
		textarea:   ta,
		spinner:    sp,
		state:      uiIdle,
		session:    session,
		provider:   provider,
		system:     system,
		tools:      tools,
		executor:   executor,
		bgManager:  bgManager,
		st:         st,
		projectDir: projectDir,
		wsDir:      wsDir,
		model:      modelName,
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
		m.textarea.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		if m.state != uiIdle {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case submitMsg:
		return m.handleSubmit(msg.text)

	case turnDoneMsg:
		m.state = uiIdle
		m.turnCancel = nil

		// Stats line
		uiPrintf("\n  %s  %s  %s\n",
			randomEmoji(),
			formatTokens(int64(msg.lastInput)),
			formatDuration(msg.elapsed))

		m.st.turns.Add(1)
		syncStats(m.st, m.session, m.wsDir)
		autoCompact(msg.lastInput, m.session, m.provider)
		return m, nil

	case turnErrorMsg:
		m.state = uiIdle
		m.turnCancel = nil
		if msg.interrupted {
			uiPrintf("\n%s↩ Interrupted%s\n", yellow, reset)
			if len(m.session.Messages) > 0 && m.session.Messages[len(m.session.Messages)-1].Role == "user" {
				m.session.Messages = m.session.Messages[:len(m.session.Messages)-1]
			}
		} else {
			uiPrintf("\n%sError: %v%s\n", boldRed, msg.err, reset)
		}
		return m, nil

	case editorResultMsg:
		if msg.text != "" {
			m.textarea.SetValue(msg.text)
		}
		return m, nil

	case editorErrorMsg:
		uiPrintf("%sEditor error: %v%s", red, msg.err, reset)
		return m, nil
	}

	// Pass remaining messages to textarea
	if m.state == uiIdle {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		if m.state != uiIdle {
			if m.turnCancel != nil {
				m.turnCancel()
			}
			return m, nil
		}
		m.bgManager.StopAll()
		return m, tea.Quit

	case tea.KeyEsc:
		if m.state != uiIdle {
			if m.turnCancel != nil {
				m.turnCancel()
			}
			return m, nil
		}
		return m, nil

	case tea.KeyCtrlG:
		if m.state == uiIdle {
			return m, m.openEditor()
		}
		return m, nil

	case tea.KeyEnter:
		if m.state != uiIdle {
			return m, nil
		}
		input := strings.TrimSpace(m.textarea.Value())
		if input == "" {
			return m, nil
		}
		m.textarea.Reset()
		// Process via a Cmd so the View updates first (shows cleared textarea)
		return m, func() tea.Msg { return submitMsg{text: input} }
	}

	if m.state == uiIdle {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m uiModel) handleSubmit(input string) (tea.Model, tea.Cmd) {
	// Slash commands
	if strings.HasPrefix(input, "/") {
		result := handleSlashCommand(input, m.session, m.projectDir, m.wsDir, m.provider, m.bgManager, m.st)
		if result.newSession {
			m.session = NewSession(m.model)
			m.system = buildSystemPrompt(m.projectDir, findMemoryFile(m.wsDir))
			uiPrint("New session started.")
		}
		if result.resumeID != "" {
			loaded, err := LoadSession(m.wsDir, result.resumeID)
			if err != nil {
				uiPrintf("Error loading session: %v", err)
			} else {
				m.session = loaded
				if m.session.Model != "" && m.session.Model != m.model {
					m.model = m.session.Model
					newKey, newBase := detectAPIConfig(m.model)
					if newKey != "" {
						m.provider = NewProvider(m.model, newKey, newBase, m.maxTokens)
					}
				}
				turns := countUserTurns(m.session.Messages)
				uiPrintf("Resumed: %s (%d turns)", m.session.Title, turns)
				printSessionHistory(m.session.Messages)
			}
		}
		if result.switchModel != "" {
			m.model = result.switchModel
			m.session.Model = m.model
			if p, ok := switchFromActiveProvider(m.model); ok {
				m.provider = p
			} else {
				newKey, newBase := detectAPIConfig(m.model)
				if newKey == "" {
					uiPrintf("No API key found for model %s", m.model)
				} else {
					m.provider = NewProvider(m.model, newKey, newBase, m.maxTokens)
				}
			}
			saveActiveModel(m.wsDir, m.model)
			uiPrintf("Switched to %s%s%s", bold, m.model, reset)
		}
		return m, nil
	}

	// !cmd — direct shell execution
	if strings.HasPrefix(input, "!") {
		shellCmd := strings.TrimPrefix(input, "!")
		shellCmd = strings.TrimSpace(shellCmd)
		if shellCmd != "" {
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
		}
		return m, nil
	}

	// User message
	_, expanded := expandAtFiles(input, m.projectDir)
	m.session.Messages = append(m.session.Messages, Message{
		Role: "user",
		Content: []ContentBlock{
			{Type: "text", Text: expanded},
		},
	})

	// Start agent turn
	m.state = uiThinking
	turnCtx, turnCancel := context.WithCancel(m.ctx)
	m.turnCancel = turnCancel

	return m, func() tea.Msg {
		turnStart := time.Now()
		lastInput, err := agentTurn(turnCtx, m.provider, m.system, &m.session.Messages, m.tools, m.executor, m.st)
		elapsed := time.Since(turnStart)
		if err != nil {
			return turnErrorMsg{err: err, interrupted: turnCtx.Err() != nil}
		}
		return turnDoneMsg{lastInput: lastInput, elapsed: elapsed}
	}
}

func (m uiModel) openEditor() tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}
	tmp, err := os.CreateTemp("", "yu-input-*.md")
	if err != nil {
		return func() tea.Msg { return editorErrorMsg{err: err} }
	}
	current := m.textarea.Value()
	if current != "" {
		tmp.WriteString(current + "\n")
	}
	tmp.Close()

	c := exec.Command(editor, tmp.Name())
	return tea.ExecProcess(c, func(err error) tea.Msg {
		defer os.Remove(tmp.Name())
		if err != nil {
			return editorErrorMsg{err: err}
		}
		data, err := os.ReadFile(tmp.Name())
		if err != nil {
			return editorErrorMsg{err: err}
		}
		return editorResultMsg{text: strings.TrimSpace(string(data))}
	})
}

// --- View ---

func (m uiModel) View() string {
	var b strings.Builder

	if m.state == uiIdle {
		// Prompt label
		prompt := fmt.Sprintf(" %s%s%s ", boldCyan, shortModel(m.model), reset)
		bgCount := m.bgManager.RunningCount()
		if bgCount > 0 {
			prompt += fmt.Sprintf("%s[%d bg]%s ", dim, bgCount, reset)
		}
		b.WriteString(prompt)
		b.WriteString("\n")
		b.WriteString(m.textarea.View())
	} else {
		label := "thinking"
		if m.state == uiThinking {
			label = "thinking"
		}
		b.WriteString(fmt.Sprintf(" %s %s...", m.spinner.View(), label))
		b.WriteString(fmt.Sprintf("  %s(ESC to cancel)%s", dim, reset))
	}

	return b.String()
}

// --- Entry Point ---

// RunUI starts the Bubble Tea UI for interactive mode.
func RunUI(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newUIModel(session, provider, system, tools, executor, bgManager,
		st, projectDir, wsDir, modelName, maxTokens, ctx)

	// Print welcome before TUI starts (goes to real stdout)
	printWelcome(modelName, projectDir, session)

	// Hijack stdout: all fmt.Printf in agentTurn/tools/render goes through
	// the pipe, and we relay it to program.Println() above the TUI.
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}
	os.Stdout = pw

	p := tea.NewProgram(m, tea.WithOutput(origStdout))
	program = p

	// Background goroutine: read from pipe, relay to TUI
	go func() {
		buf := make([]byte, 4096)
		var lineBuf strings.Builder
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				// Split by newlines, print complete lines
				for i, ch := range chunk {
					if ch == '\n' {
						line := lineBuf.String()
						lineBuf.Reset()
						if program != nil {
							program.Println(line)
						} else {
							origStdout.WriteString(line + "\n")
						}
					} else if ch == '\r' {
						// Spinner \r\033[K — just flush the line for now
						if lineBuf.Len() > 0 {
							// Don't print spinner partial lines
							lineBuf.Reset()
						}
					} else {
						lineBuf.WriteByte(chunk[i])
					}
				}
			}
			if err != nil {
				// Flush remaining
				if lineBuf.Len() > 0 {
					line := lineBuf.String()
					if program != nil {
						program.Println(line)
					} else {
						origStdout.WriteString(line + "\n")
					}
				}
				return
			}
		}
	}()

	_, runErr := p.Run()

	// Restore stdout
	program = nil
	pw.Close()
	pr.Close()
	os.Stdout = origStdout

	return runErr
}
