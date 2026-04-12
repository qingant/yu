package agent

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// interactRequest is sent from tool/command goroutines to the UI.
type interactRequest struct {
	Question string   // text to display
	Options  []string // nil = free text input, non-nil = selection
	StartIdx int
	Response chan string
}

// interactFn is called to interact with the user. Blocks until response.
type interactFn func(req interactRequest) string

// globalInteract is set when TUI is active. arrowSelect and stdin reads
// use this to route through bubbletea instead of direct terminal I/O.
var (
	globalInteract   interactFn
	globalInteractMu sync.Mutex
)

func setGlobalInteract(fn interactFn) {
	globalInteractMu.Lock()
	defer globalInteractMu.Unlock()
	globalInteract = fn
}

func getGlobalInteract() interactFn {
	globalInteractMu.Lock()
	defer globalInteractMu.Unlock()
	return globalInteract
}

// uiSelect is a drop-in replacement for arrowSelect that routes through
// the TUI when available, falls back to arrowSelect otherwise.
func uiSelect(options []string) string {
	fn := getGlobalInteract()
	if fn == nil {
		return arrowSelect(options)
	}
	return fn(interactRequest{
		Question: "",
		Options:  options,
		StartIdx: 0,
		Response: make(chan string, 1),
	})
}

// uiSelectAt is a drop-in replacement for arrowSelectAt.
func uiSelectAt(options []string, startIdx int) string {
	fn := getGlobalInteract()
	if fn == nil {
		return arrowSelectAt(options, startIdx)
	}
	return fn(interactRequest{
		Question: "",
		Options:  options,
		StartIdx: startIdx,
		Response: make(chan string, 1),
	})
}

// uiInput asks for free text input through the TUI.
func uiInput(question string) string {
	fn := getGlobalInteract()
	if fn == nil {
		// Fallback: direct stdin
		outPrintf("  %s\n  > ", question)
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		return strings.TrimSpace(answer)
	}
	return fn(interactRequest{
		Question: question,
		Response: make(chan string, 1),
	})
}
