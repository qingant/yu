package agent

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

var (
	outputMu  sync.RWMutex
	outWriter io.Writer = os.Stdout
	errWriter io.Writer = os.Stderr
)

func setOutputWriters(out, err io.Writer) func() {
	outputMu.Lock()
	prevOut := outWriter
	prevErr := errWriter
	outWriter = out
	errWriter = err
	outputMu.Unlock()
	return func() {
		outputMu.Lock()
		outWriter = prevOut
		errWriter = prevErr
		outputMu.Unlock()
	}
}

func currentOutWriter() io.Writer {
	outputMu.RLock()
	defer outputMu.RUnlock()
	return outWriter
}

func currentErrWriter() io.Writer {
	outputMu.RLock()
	defer outputMu.RUnlock()
	return errWriter
}

func outPrint(args ...any) {
	fmt.Fprint(currentOutWriter(), args...)
}

func outPrintf(format string, args ...any) {
	fmt.Fprintf(currentOutWriter(), format, args...)
}

func outPrintln(args ...any) {
	fmt.Fprintln(currentOutWriter(), args...)
}

func errPrint(args ...any) {
	fmt.Fprint(currentErrWriter(), args...)
}

func errPrintf(format string, args ...any) {
	fmt.Fprintf(currentErrWriter(), format, args...)
}

func errPrintln(args ...any) {
	fmt.Fprintln(currentErrWriter(), args...)
}

type lineBufferedProgramWriter struct {
	mu  sync.Mutex
	p   *tea.Program
	buf strings.Builder
}

func newLineBufferedProgramWriter(p *tea.Program) *lineBufferedProgramWriter {
	return &lineBufferedProgramWriter{p: p}
}

func (w *lineBufferedProgramWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	chunk := string(b)
	chunk = strings.ReplaceAll(chunk, "\r", "")
	chunk = strings.ReplaceAll(chunk, "\033[K", "")
	chunk = strings.ReplaceAll(chunk, "\033[2K", "")
	w.buf.WriteString(chunk)

	content := w.buf.String()
	lastNL := strings.LastIndex(content, "\n")
	if lastNL < 0 {
		return len(b), nil
	}
	for _, line := range strings.Split(content[:lastNL], "\n") {
		w.p.Println(line)
	}
	w.buf.Reset()
	if lastNL+1 < len(content) {
		w.buf.WriteString(content[lastNL+1:])
	}
	return len(b), nil
}

func (w *lineBufferedProgramWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		return
	}
	w.p.Println(w.buf.String())
	w.buf.Reset()
}
