package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// --- Input Collection via Bubble Tea ---
//
// Strategy: bubbletea is used ONLY for collecting user input. Each time we need
// input, we create a short-lived tea.Program, collect the text, and exit.
// Agent turns run with direct stdout access — no pipe, no conflicts.
//
// This avoids the deadlock problem of running agentTurn inside a tea.Cmd
// (agentTurn writes stdout via fmt.Printf, which requires bubbletea's message
// loop to consume, but bubbletea is waiting for the Cmd to finish).

// inputResult is returned by collectInput.
type inputResult struct {
	text   string
	editor bool // true if user pressed Ctrl+G (open editor)
	quit   bool // true if user pressed Ctrl+C
}

type (
	editorDoneMsg  struct{ text string }
	editorErrorMsg struct{ err error }
)

// collectInput runs a short-lived bubbletea program to get user input.
// Returns the input text, or signals for editor/quit.
func collectInput(modelName string, bgCount int) inputResult {
	ta := textarea.New()
	ta.Placeholder = "Message... (Enter send, Shift+Enter newline, /help)"
	ta.Focus()
	ta.CharLimit = 0
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j")

	m := inputModel{
		textarea:  ta,
		modelName: modelName,
		bgCount:   bgCount,
	}

	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return inputResult{quit: true}
	}

	fm := finalModel.(inputModel)
	return fm.result
}

// inputModel is the bubbletea model for input collection only.
type inputModel struct {
	textarea  textarea.Model
	modelName string
	bgCount   int
	result    inputResult
	width     int
}

func (m inputModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textarea.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.result = inputResult{quit: true}
			return m, tea.Quit

		case tea.KeyEsc:
			// ESC while typing = cancel input (clear textarea)
			m.textarea.Reset()
			return m, nil

		case tea.KeyCtrlG:
			m.result = inputResult{editor: true, text: m.textarea.Value()}
			return m, tea.Quit

		case tea.KeyEnter:
			text := strings.TrimSpace(m.textarea.Value())
			if text == "" {
				return m, nil
			}
			m.result = inputResult{text: text}
			return m, tea.Quit
		}

	case editorDoneMsg:
		if msg.text != "" {
			m.result = inputResult{text: msg.text}
			return m, tea.Quit
		}
		return m, nil

	case editorErrorMsg:
		fmt.Fprintf(os.Stderr, "Editor error: %v\n", msg.err)
		return m, nil
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m inputModel) View() string {
	var b strings.Builder

	// Prompt label
	prompt := fmt.Sprintf(" %s%s%s ", boldCyan, shortModel(m.modelName), reset)
	if m.bgCount > 0 {
		prompt += fmt.Sprintf("%s[%d bg]%s ", dim, m.bgCount, reset)
	}
	b.WriteString(prompt + "\n")
	b.WriteString(m.textarea.View())

	return b.String()
}

// openEditorForInput opens $EDITOR with prefilled text, returns the result.
func openEditorForInput(prefill string) (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	tmp, err := os.CreateTemp("", "yu-input-*.md")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if prefill != "" {
		tmp.WriteString(prefill + "\n")
	}
	tmp.Close()

	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// RunInteractive is the main interactive loop using Bubble Tea for input.
func RunInteractive(
	session *Session, provider Provider, system []SystemBlock,
	tools []ToolDef, executor *ToolExecutor, bgManager *BgManager,
	st *stats, projectDir, wsDir, modelName string, maxTokens int,
) {
	ctx := context.Background()

	for {
		// Collect input via bubbletea
		result := collectInput(modelName, bgManager.RunningCount())

		if result.quit {
			bgManager.StopAll()
			fmt.Println()
			return
		}

		if result.editor {
			text, err := openEditorForInput(result.text)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Editor error: %v\n", err)
				continue
			}
			if text == "" {
				continue
			}
			result.text = text
		}

		input := result.text

		// Slash commands
		if strings.HasPrefix(input, "/") {
			res := handleSlashCommand(input, session, projectDir, wsDir, provider, bgManager, st)
			if res.newSession {
				session = NewSession(modelName)
				system = buildSystemPrompt(projectDir, findMemoryFile(wsDir))
				fmt.Println("New session started.")
			}
			if res.resumeID != "" {
				loaded, err := LoadSession(wsDir, res.resumeID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error loading session: %v\n", err)
				} else {
					session = loaded
					if session.Model != "" && session.Model != modelName {
						modelName = session.Model
						newKey, newBase := detectAPIConfig(modelName)
						if newKey != "" {
							provider = NewProvider(modelName, newKey, newBase, maxTokens)
						}
					}
					turns := countUserTurns(session.Messages)
					fmt.Printf("Resumed: %s (%d turns)\n\n", session.Title, turns)
					printSessionHistory(session.Messages)
				}
			}
			if res.switchModel != "" {
				modelName = res.switchModel
				session.Model = modelName
				if p, ok := switchFromActiveProvider(modelName); ok {
					provider = p
				} else {
					newKey, newBase := detectAPIConfig(modelName)
					if newKey == "" {
						fmt.Fprintf(os.Stderr, "No API key found for model %s\n", modelName)
					} else {
						provider = NewProvider(modelName, newKey, newBase, maxTokens)
					}
				}
				saveActiveModel(wsDir, modelName)
				fmt.Printf("\nSwitched to %s%s%s\n", bold, modelName, reset)
			}
			continue
		}

		// !cmd — direct shell execution
		if strings.HasPrefix(input, "!") {
			shellCmd := strings.TrimPrefix(input, "!")
			shellCmd = strings.TrimSpace(shellCmd)
			if shellCmd != "" {
				output := execDirectCommand(shellCmd, projectDir)
				session.Messages = append(session.Messages, Message{
					Role: "user",
					Content: []ContentBlock{
						{Type: "text", Text: fmt.Sprintf("[User ran shell command: %s]\n\n%s", shellCmd, stripControlChars(output))},
					},
				})
				if wsDir != "" {
					session.Save(wsDir)
				}
			}
			continue
		}

		// User message
		_, expanded := expandAtFiles(input, projectDir)
		session.Messages = append(session.Messages, Message{
			Role: "user",
			Content: []ContentBlock{
				{Type: "text", Text: expanded},
			},
		})

		// Agent turn — runs with direct stdout, no bubbletea interference.
		// Put terminal in raw mode so we can catch ESC/Ctrl+C to cancel.
		turnCtx, turnCancel := context.WithCancel(ctx)

		// Enter raw mode for key listening during turn
		oldState, rawErr := term.MakeRaw(os.Stdin.Fd())
		if rawErr == nil {
			go func() {
				buf := make([]byte, 1)
				for {
					n, err := os.Stdin.Read(buf)
					if err != nil || n == 0 {
						return
					}
					if buf[0] == 0x1b || buf[0] == 0x03 { // ESC or Ctrl+C
						turnCancel()
						return
					}
				}
			}()
		}

		turnStart := time.Now()
		lastInput, turnErr := agentTurn(turnCtx, provider, system, &session.Messages, tools, executor, st)
		elapsed := time.Since(turnStart)
		turnCancel()

		// Restore terminal
		if rawErr == nil {
			term.Restore(os.Stdin.Fd(), oldState)
		}

		if turnErr != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "\n%s↩ Interrupted%s\n", yellow, reset)
				if len(session.Messages) > 0 && session.Messages[len(session.Messages)-1].Role == "user" {
					session.Messages = session.Messages[:len(session.Messages)-1]
				}
			} else {
				fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", boldRed, turnErr, reset)
			}
		} else {
			st.turns.Add(1)
			newTokens := st.totalInput.Load() + st.totalOutput.Load()
			_ = newTokens
			cacheRead := st.totalCacheRead.Load()
			printTurnStats(int64(lastInput), cacheRead, elapsed)
		}

		syncStats(st, session, wsDir)
		autoCompact(lastInput, session, provider)
	}
}
