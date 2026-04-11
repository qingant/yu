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
//     Content is buffered during paste. When paste ends, newlines are replaced
//     with U+2028 and the content is released to readline. The user sees the
//     text and can edit before pressing Enter.
//  2. Non-bracketed fallback: \n bytes are replaced with U+2028 placeholders
//     (in raw mode, Enter sends \r, so \n is always from paste).
//
// In both cases, readMultiLine restores U+2028 → \n after readline returns.
type pasteStdin struct {
	real         io.Reader
	mu           sync.Mutex
	buf          []byte       // leftover bytes from last read
	inPaste      bool
	matchBuf     []byte       // partial match buffer for escape sequences
	pasteContent bytes.Buffer // content during active bracketed paste
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

	// Drain leftover buffer first
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	// Read loop: during bracketed paste, keep reading stdin internally
	// until the paste ends, then release all content at once.
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

		// Mid-paste or partial escape sequence: loop to read more
		if len(out) == 0 && (p.inPaste || len(p.matchBuf) > 0) {
			continue
		}

		// Replace \n with placeholder (works for both bracketed and
		// non-bracketed paste — in raw mode \n is never Enter)
		out = replacePasteNewlines(out)

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
				// Release buffered paste content to output
				out.Write(p.pasteContent.Bytes())
				p.pasteContent.Reset()
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
