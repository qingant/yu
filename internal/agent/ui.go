package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// --- agentTurnCmd wraps agentTurn as a tea.ExecCommand ---
//
// When bubbletea receives this via tea.Exec, it:
//  1. Releases the terminal (restores cooked mode, stops reading stdin)
//  2. Calls Run() — which runs agentTurn with direct stdin/stdout
//  3. Restores the terminal (raw mode, resumes TUI)
//
// This means agentTurn's fmt.Printf, spinner, tool output, ask_user stdin
// reads all work exactly as before. Zero changes to existing code.

type agentTurnCmd struct {
	ctx      context.Context
	cancel   context.CancelFunc
	provider Provider
	system   []SystemBlock
	messages *[]Message
	tools    []ToolDef
	executor *ToolExecutor
	st       *stats

	// Results
	lastInput   int
	elapsed     time.Duration
	err         error
	interrupted bool

	// stdin/stdout/stderr set by bubbletea
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (c *agentTurnCmd) SetStdin(r io.Reader)  { c.stdin = r }
func (c *agentTurnCmd) SetStdout(w io.Writer)  { c.stdout = w }
func (c *agentTurnCmd) SetStderr(w io.Writer)  { c.stderr = w }

func (c *agentTurnCmd) Run() error {
	// Restore os.Stdin/Stdout so fmt.Printf and bufio.Scanner work
	if f, ok := c.stdin.(*os.File); ok {
		os.Stdin = f
	}
	if f, ok := c.stdout.(*os.File); ok {
		os.Stdout = f
	}

	// Listen for ESC/Ctrl+C in a goroutine (terminal is in cooked mode,
	// but bubbletea released it — we can read raw if we set raw mode)
	// Actually: bubbletea restores the original terminal state. If we want
	// ESC to cancel, we need raw mode. But ask_user tool also reads stdin.
	// For now, Ctrl+C generates SIGINT which the sandbox ignores (v1.0.53),
	// and the agent's signal handler will catch it. This is good enough.

	turnStart := time.Now()
	c.lastInput, c.err = agentTurn(c.ctx, c.provider, c.system, c.messages, c.tools, c.executor, c.st)
	c.elapsed = time.Since(turnStart)
	c.interrupted = c.ctx.Err() != nil
	return nil // always return nil — we handle errors in the callback
}

// --- Tea messages ---

type turnDoneMsg struct {
	lastInput   int
	elapsed     time.Duration
	err         error
	interrupted bool
}

type editorResultMsg struct{ text string }
type editorErrMsg struct{ err error }

// --- Model ---

type uiModel struct {
	textarea  textarea.Model
	modelName string
	bgCount   int
	width     int

	// Business context
	session   *Session
	provider  Provider
	system    []SystemBlock
	tools     []ToolDef
	executor  *ToolExecutor
	bgManager *BgManager
	st        *stats
	projectDir string
	wsDir      string
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
	ta.Placeholder = "Message... (Enter send, Shift+Enter newline, /help)"
	ta.Focus()
	ta.CharLimit = 0
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j")

	return uiModel{
		textarea:   ta,
		modelName:  modelName,
		session:    session,
		provider:   provider,
		system:     system,
		tools:      tools,
		executor:   executor,
		bgManager:  bgManager,
		st:         st,
		projectDir: projectDir,
		wsDir:      wsDir,
		maxTokens:  maxTokens,
		ctx:        ctx,
	}
}

func (m uiModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textarea.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.bgManager.StopAll()
			return m, tea.Quit

		case tea.KeyEsc:
			m.textarea.Reset()
			return m, nil

		case tea.KeyCtrlG:
			return m, m.openEditor(m.textarea.Value())

		case tea.KeyEnter:
			text := strings.TrimSpace(m.textarea.Value())
			if text == "" {
				return m, nil
			}
			m.textarea.Reset()
			return m.submit(text)
		}

	case turnDoneMsg:
		// Agent turn finished — handle results and update state
		m.handleTurnDone(msg)
		m.bgCount = m.bgManager.RunningCount()
		return m, nil

	case editorResultMsg:
		if msg.text != "" {
			m.textarea.SetValue(msg.text)
		}
		return m, nil

	case editorErrMsg:
		fmt.Fprintf(os.Stderr, "Editor error: %v\n", msg.err)
		return m, nil
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m *uiModel) submit(input string) (tea.Model, tea.Cmd) {
	// Slash commands — run directly (they print to stdout, which is fine
	// because tea.Exec will release terminal for the next turn)
	if strings.HasPrefix(input, "/") {
		// Use tea.Exec to release terminal for slash command output
		return *m, tea.Exec(&slashCmd{
			input: input, session: m.session, projectDir: m.projectDir,
			wsDir: m.wsDir, provider: m.provider, bgManager: m.bgManager,
			st: m.st, modelName: m.modelName, maxTokens: m.maxTokens,
		}, func(err error) tea.Msg {
			return slashDoneMsg{}
		})
	}

	// !cmd
	if strings.HasPrefix(input, "!") {
		shellCmd := strings.TrimPrefix(input, "!")
		shellCmd = strings.TrimSpace(shellCmd)
		if shellCmd != "" {
			return *m, tea.Exec(&shellExecCmd{
				shellCmd: shellCmd, projectDir: m.projectDir,
				session: m.session, wsDir: m.wsDir,
			}, func(err error) tea.Msg {
				return slashDoneMsg{}
			})
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

	// Start agent turn via tea.Exec — bubbletea releases terminal
	turnCtx, turnCancel := context.WithCancel(m.ctx)
	cmd := &agentTurnCmd{
		ctx: turnCtx, cancel: turnCancel,
		provider: m.provider, system: m.system,
		messages: &m.session.Messages, tools: m.tools,
		executor: m.executor, st: m.st,
	}

	return *m, tea.Exec(cmd, func(err error) tea.Msg {
		turnCancel()
		return turnDoneMsg{
			lastInput:   cmd.lastInput,
			elapsed:     cmd.elapsed,
			err:         cmd.err,
			interrupted: cmd.interrupted,
		}
	})
}

func (m *uiModel) handleTurnDone(msg turnDoneMsg) {
	if msg.err != nil {
		if msg.interrupted {
			fmt.Fprintf(os.Stderr, "\n%s↩ Interrupted%s\n", yellow, reset)
			if len(m.session.Messages) > 0 && m.session.Messages[len(m.session.Messages)-1].Role == "user" {
				m.session.Messages = m.session.Messages[:len(m.session.Messages)-1]
			}
		} else {
			fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", boldRed, msg.err, reset)
		}
	} else {
		m.st.turns.Add(1)
		cacheRead := m.st.totalCacheRead.Load()
		printTurnStats(int64(msg.lastInput), cacheRead, msg.elapsed)
	}
	syncStats(m.st, m.session, m.wsDir)
	autoCompact(msg.lastInput, m.session, m.provider)
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

func (m uiModel) View() string {
	var b strings.Builder
	prompt := fmt.Sprintf(" %s%s%s ", boldCyan, shortModel(m.modelName), reset)
	if m.bgCount > 0 {
		prompt += fmt.Sprintf("%s[%d bg]%s ", dim, m.bgCount, reset)
	}
	b.WriteString(prompt + "\n")
	b.WriteString(m.textarea.View())
	return b.String()
}

// --- slashCmd wraps slash command execution as ExecCommand ---

type slashCmd struct {
	input      string
	session    *Session
	projectDir string
	wsDir      string
	provider   Provider
	bgManager  *BgManager
	st         *stats
	modelName  string
	maxTokens  int
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

type slashDoneMsg struct{}

func (c *slashCmd) SetStdin(r io.Reader)  { c.stdin = r }
func (c *slashCmd) SetStdout(w io.Writer)  { c.stdout = w }
func (c *slashCmd) SetStderr(w io.Writer)  { c.stderr = w }

func (c *slashCmd) Run() error {
	if f, ok := c.stdin.(*os.File); ok {
		os.Stdin = f
	}
	if f, ok := c.stdout.(*os.File); ok {
		os.Stdout = f
	}

	result := handleSlashCommand(c.input, c.session, c.projectDir, c.wsDir, c.provider, c.bgManager, c.st)
	if result.newSession {
		*c.session = *NewSession(c.modelName)
		fmt.Println("New session started.")
	}
	if result.resumeID != "" {
		loaded, err := LoadSession(c.wsDir, result.resumeID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading session: %v\n", err)
		} else {
			*c.session = *loaded
			if c.session.Model != "" {
				c.modelName = c.session.Model
			}
			turns := countUserTurns(c.session.Messages)
			fmt.Printf("Resumed: %s (%d turns)\n\n", c.session.Title, turns)
			printSessionHistory(c.session.Messages)
		}
	}
	if result.switchModel != "" {
		c.modelName = result.switchModel
		c.session.Model = c.modelName
		if p, ok := switchFromActiveProvider(c.modelName); ok {
			_ = p // provider will be picked up on next turn
		} else {
			newKey, _ := detectAPIConfig(c.modelName)
			if newKey == "" {
				fmt.Fprintf(os.Stderr, "No API key found for model %s\n", c.modelName)
			}
		}
		saveActiveModel(c.wsDir, c.modelName)
		fmt.Printf("\nSwitched to %s%s%s\n", bold, c.modelName, reset)
	}
	return nil
}

// --- shellExecCmd wraps !command execution as ExecCommand ---

type shellExecCmd struct {
	shellCmd   string
	projectDir string
	session    *Session
	wsDir      string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

func (c *shellExecCmd) SetStdin(r io.Reader)  { c.stdin = r }
func (c *shellExecCmd) SetStdout(w io.Writer)  { c.stdout = w }
func (c *shellExecCmd) SetStderr(w io.Writer)  { c.stderr = w }

func (c *shellExecCmd) Run() error {
	if f, ok := c.stdin.(*os.File); ok {
		os.Stdin = f
	}
	if f, ok := c.stdout.(*os.File); ok {
		os.Stdout = f
	}
	output := execDirectCommand(c.shellCmd, c.projectDir)
	c.session.Messages = append(c.session.Messages, Message{
		Role: "user",
		Content: []ContentBlock{
			{Type: "text", Text: fmt.Sprintf("[User ran shell command: %s]\n\n%s", c.shellCmd, stripControlChars(output))},
		},
	})
	if c.wsDir != "" {
		c.session.Save(c.wsDir)
	}
	return nil
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

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
	}
}
