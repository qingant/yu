package agent

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// Bracketed paste escape sequences.
// When enabled, the terminal wraps pasted text in these markers.
var (
	pasteStart = []byte("\033[200~")
	pasteEnd   = []byte("\033[201~")
	// Enable/disable bracketed paste mode
	pasteEnable  = []byte("\033[?2004h")
	pasteDisable = []byte("\033[?2004l")
)

// pasteNewline is a Unicode placeholder (U+2028 LINE SEPARATOR, UTF-8: E2 80 A8)
// used to replace \n inside pasted text so readline doesn't treat it as Enter.
const pasteNewline = '\u2028'

// pasteStdin wraps the real stdin to support bracketed paste.
// When the terminal sends \033[200~ ... \033[201~, newlines within the paste
// are replaced with pasteNewline (U+2028) so readline sees them as regular chars.
// After readline returns, the caller replaces U+2028 back to \n.
type pasteStdin struct {
	real     io.Reader
	mu       sync.Mutex
	buf      []byte // leftover bytes from last read
	inPaste  bool
	matchBuf []byte // partial match buffer for escape sequences
}

// newPasteStdin creates a paste-aware stdin wrapper and enables bracketed
// paste mode on the terminal.
func newPasteStdin() *pasteStdin {
	// Enable bracketed paste mode
	os.Stdout.Write(pasteEnable)
	return &pasteStdin{real: os.Stdin}
}

func (p *pasteStdin) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Drain leftover buffer first
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	// Read from real stdin
	n, err := p.real.Read(b)
	if n == 0 {
		return n, err
	}

	data := b[:n]

	// Prepend any partial match buffer from previous read
	if len(p.matchBuf) > 0 {
		data = append(p.matchBuf, data...)
		p.matchBuf = nil
	}

	// Process the data: find paste start/end markers, replace newlines
	out := p.process(data)

	n = copy(b, out)
	if n < len(out) {
		p.buf = append(p.buf, out[n:]...)
	}
	return n, err
}

func (p *pasteStdin) process(data []byte) []byte {
	var out bytes.Buffer
	i := 0
	for i < len(data) {
		// Check for escape sequence start
		if data[i] == '\033' {
			remaining := data[i:]

			// Check for paste start: \033[200~
			if matchResult := matchSeq(remaining, pasteStart); matchResult == matchFull {
				p.inPaste = true
				i += len(pasteStart)
				continue
			} else if matchResult == matchPartial {
				// Save for next Read call
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}

			// Check for paste end: \033[201~
			if matchResult := matchSeq(remaining, pasteEnd); matchResult == matchFull {
				p.inPaste = false
				i += len(pasteEnd)
				continue
			} else if matchResult == matchPartial {
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}
		}

		// Inside paste: replace \n and \r\n with placeholder
		if p.inPaste && (data[i] == '\n' || data[i] == '\r') {
			if data[i] == '\r' {
				// Skip \r, the \n following it (if any) will be replaced
				if i+1 < len(data) && data[i+1] == '\n' {
					i++ // skip \r, handle \n below
				} else {
					// Bare \r — replace with placeholder
					writeRune(&out, pasteNewline)
					i++
					continue
				}
			}
			// Replace \n with placeholder (UTF-8 encoded U+2028)
			writeRune(&out, pasteNewline)
			i++
			continue
		}

		out.WriteByte(data[i])
		i++
	}
	return out.Bytes()
}

func (p *pasteStdin) Close() error {
	// Disable bracketed paste mode
	os.Stdout.Write(pasteDisable)
	if c, ok := p.real.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type matchType int

const (
	matchNone    matchType = iota
	matchPartial           // data is a prefix of seq
	matchFull              // data starts with seq
)

func matchSeq(data, seq []byte) matchType {
	n := len(seq)
	if len(data) >= n {
		if bytes.Equal(data[:n], seq) {
			return matchFull
		}
		return matchNone
	}
	// data is shorter than seq — check if it's a prefix
	if bytes.Equal(data, seq[:len(data)]) {
		return matchPartial
	}
	return matchNone
}

func writeRune(buf *bytes.Buffer, r rune) {
	buf.WriteRune(r)
}
