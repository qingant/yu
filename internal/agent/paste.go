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

// pasteStdin wraps the real stdin to support multi-line paste.
//
// Two modes:
//  1. Bracketed paste (terminal sends \033[200~ ... \033[201~):
//     The entire paste is buffered internally. readline only sees \r (submit).
//     readMultiLine picks up the paste via TakePaste(). No character-by-character
//     redraw, no visual noise.
//  2. Non-bracketed fallback: \n bytes are replaced with U+2028 placeholders
//     (in raw mode, Enter sends \r, so \n is always from paste). readline sees
//     a single line with placeholders, which are restored to \n after submit.
type pasteStdin struct {
	real            io.Reader
	mu              sync.Mutex
	buf             []byte // leftover bytes from last read
	inPaste         bool
	matchBuf        []byte       // partial match buffer for escape sequences
	seenPasteMarker bool         // true once we've seen \033[200~
	pasteContent    bytes.Buffer // content during active bracketed paste
	completedPaste  string       // paste ready for pickup by readMultiLine
	sendSubmit      bool         // true = next Read returns \r to trigger readline submit
}

// newPasteStdin creates a paste-aware stdin wrapper and enables bracketed
// paste mode on the terminal.
func newPasteStdin() *pasteStdin {
	os.Stdout.Write(pasteEnable)
	return &pasteStdin{real: os.Stdin}
}

func (p *pasteStdin) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// After a bracketed paste completed, send \r to make readline submit.
	if p.sendSubmit {
		p.sendSubmit = false
		b[0] = '\r'
		return 1, nil
	}

	// Drain leftover buffer first
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	// Read loop: during bracketed paste, keep reading stdin internally
	// until the paste ends (avoids returning 0 bytes which confuses bufio).
	for {
		n, err := p.real.Read(b)
		if n == 0 {
			return n, err
		}

		data := b[:n]

		if len(p.matchBuf) > 0 {
			data = append(p.matchBuf, data...)
			p.matchBuf = nil
		}

		out := p.process(data)

		// Paste just completed → trigger readline submit
		if p.sendSubmit {
			if len(out) > 0 {
				p.buf = append(p.buf, out...)
			}
			p.sendSubmit = false
			b[0] = '\r'
			return 1, nil
		}

		// Mid-paste: content was absorbed, loop to read more from stdin
		if p.inPaste && len(out) == 0 {
			continue
		}

		// Non-bracketed fallback: \n is always from paste in raw mode
		if !p.inPaste {
			out = replacePasteNewlines(out)
		}

		n = copy(b, out)
		if n < len(out) {
			p.buf = append(p.buf, out[n:]...)
		}
		return n, err
	}
}

func (p *pasteStdin) process(data []byte) []byte {
	var out bytes.Buffer
	i := 0
	for i < len(data) {
		if data[i] == '\033' {
			remaining := data[i:]

			// Check for paste start: \033[200~
			if matchResult := matchSeq(remaining, pasteStart); matchResult == matchFull {
				p.inPaste = true
				p.seenPasteMarker = true
				p.pasteContent.Reset()
				i += len(pasteStart)
				continue
			} else if matchResult == matchPartial {
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}

			// Check for paste end: \033[201~
			if matchResult := matchSeq(remaining, pasteEnd); matchResult == matchFull {
				p.inPaste = false
				p.completedPaste = p.pasteContent.String()
				p.pasteContent.Reset()
				p.sendSubmit = true
				i += len(pasteEnd)
				continue
			} else if matchResult == matchPartial {
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}
		}

		// Inside bracketed paste: buffer content directly (preserve newlines)
		if p.inPaste {
			p.pasteContent.WriteByte(data[i])
			i++
			continue
		}

		out.WriteByte(data[i])
		i++
	}
	return out.Bytes()
}

// TakePaste returns and clears any completed bracketed paste content.
// Called by readMultiLine after readline returns.
func (p *pasteStdin) TakePaste() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	paste := p.completedPaste
	p.completedPaste = ""
	return paste
}

func (p *pasteStdin) Close() error {
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
	if bytes.Equal(data, seq[:len(data)]) {
		return matchPartial
	}
	return matchNone
}

func writeRune(buf *bytes.Buffer, r rune) {
	buf.WriteRune(r)
}

// replacePasteNewlines replaces \n with U+2028 placeholders.
// In raw mode (during readline), Enter sends \r and paste newlines are \n.
// So \n is ALWAYS from paste, never from Enter — safe to replace unconditionally.
// Also handles \r\n (Windows line endings in paste) as a single placeholder.
func replacePasteNewlines(data []byte) []byte {
	if !bytes.Contains(data, []byte{'\n'}) {
		return data
	}
	var out bytes.Buffer
	out.Grow(len(data) + 8)
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			writeRune(&out, pasteNewline)
		} else if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			writeRune(&out, pasteNewline)
			i++ // skip the \n
		} else {
			out.WriteByte(data[i])
		}
	}
	return out.Bytes()
}

// Inject prepends data into the pasteStdin buffer so the next Read returns it.
func (p *pasteStdin) Inject(data []byte) {
	if len(data) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(data, p.buf...)
}
