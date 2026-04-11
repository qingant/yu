package agent

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// Bracketed paste escape sequences.
var (
	pasteStart   = []byte("\033[200~")
	pasteEnd     = []byte("\033[201~")
	pasteEnable  = []byte("\033[?2004h")
	pasteDisable = []byte("\033[?2004l")
)

// pasteNewline is a Unicode placeholder (U+2028 LINE SEPARATOR)
// used to replace \n in non-bracketed paste so readline doesn't treat it as Enter.
const pasteNewline = '\u2028'

// pasteStdin wraps stdin to support multi-line paste.
//
// Bracketed paste: entire content is buffered. readline sees \r (auto-submit).
// readMultiLine picks up the content via TakePaste(). Zero visual noise.
//
// Non-bracketed fallback: \n is replaced with U+2028 (in raw mode, Enter
// sends \r, so \n is always from paste). readline gets a single line.
type pasteStdin struct {
	real         io.Reader
	mu           sync.Mutex
	buf          []byte       // leftover bytes from last read
	inPaste      bool
	matchBuf     []byte       // partial match buffer for escape sequences
	pasteContent bytes.Buffer // content during active bracketed paste
	completed    string       // paste ready for pickup
	sendSubmit   bool         // next Read returns \r
}

func newPasteStdin() *pasteStdin {
	os.Stdout.Write(pasteEnable)
	return &pasteStdin{real: os.Stdin}
}

func (p *pasteStdin) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// After paste completed, send \r so readline submits
	if p.sendSubmit {
		p.sendSubmit = false
		b[0] = '\r'
		return 1, nil
	}

	// Drain leftover buffer
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	// Read loop: during paste, keep reading until paste ends
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

		// Paste just completed → save post-marker data, return \r
		if p.sendSubmit {
			if len(out) > 0 {
				p.buf = append(p.buf, out...)
			}
			p.sendSubmit = false
			b[0] = '\r'
			return 1, nil
		}

		// Mid-paste or partial escape sequence: loop for more
		if len(out) == 0 && (p.inPaste || len(p.matchBuf) > 0) {
			continue
		}

		// Non-bracketed fallback: \n → U+2028
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

			if matchResult := matchSeq(remaining, pasteStart); matchResult == matchFull {
				p.inPaste = true
				p.pasteContent.Reset()
				i += len(pasteStart)
				continue
			} else if matchResult == matchPartial {
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}

			if matchResult := matchSeq(remaining, pasteEnd); matchResult == matchFull {
				p.inPaste = false
				p.completed = p.pasteContent.String()
				p.pasteContent.Reset()
				p.sendSubmit = true
				i += len(pasteEnd)
				continue
			} else if matchResult == matchPartial {
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}
		}

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

// TakePaste returns and clears completed paste content.
func (p *pasteStdin) TakePaste() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.completed
	p.completed = ""
	return s
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
	matchPartial
	matchFull
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

// replacePasteNewlines replaces \n with U+2028.
// In raw mode, Enter sends \r, so \n is always from paste.
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
			i++
		} else {
			out.WriteByte(data[i])
		}
	}
	return out.Bytes()
}

// Inject prepends data into the buffer.
func (p *pasteStdin) Inject(data []byte) {
	if len(data) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(data, p.buf...)
}
