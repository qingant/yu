package agent

import (
	"bytes"
	"testing"
)

func TestPasteProcess(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    string
		inPaste bool // starting state
	}{
		{
			name:  "plain text no paste",
			input: []byte("hello world"),
			want:  "hello world",
		},
		{
			name:  "full paste with newlines",
			input: append(append(pasteStart, []byte("line1\nline2\nline3")...), pasteEnd...),
			want:  "line1\u2028line2\u2028line3",
		},
		{
			name:  "paste with CRLF",
			input: append(append(pasteStart, []byte("a\r\nb")...), pasteEnd...),
			want:  "a\u2028b",
		},
		{
			name:  "paste with bare CR",
			input: append(append(pasteStart, []byte("a\rb")...), pasteEnd...),
			want:  "a\u2028b",
		},
		{
			name:    "data while already in paste",
			input:   []byte("hello\nworld"),
			inPaste: true,
			want:    "hello\u2028world",
		},
		{
			name:  "newline outside paste not replaced",
			input: []byte("hello\nworld"),
			want:  "hello\nworld",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &pasteStdin{inPaste: tt.inPaste}
			got := p.process(tt.input)
			if !bytes.Equal(got, []byte(tt.want)) {
				t.Errorf("process() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPastePartialSequence(t *testing.T) {
	// Simulate paste arriving in two chunks via Read:
	//   chunk 1: "\033[200"        (partial paste start)
	//   chunk 2: "~hello\nworld\033[201~"  (rest of paste start + content + paste end)
	chunk1 := []byte("\033[200")
	chunk2 := append([]byte("~hello\nworld"), pasteEnd...)

	// Build a reader that returns chunk1 then chunk2
	r := &chunkedReader{chunks: [][]byte{chunk1, chunk2}}
	p := &pasteStdin{real: r}

	buf := make([]byte, 1024)

	// First Read: should return nothing (partial sequence buffered)
	n1, _ := p.Read(buf)
	if n1 != 0 {
		t.Errorf("first Read should return 0 bytes, got %d: %q", n1, buf[:n1])
	}

	// Second Read: should return processed content
	n2, _ := p.Read(buf)
	got := string(buf[:n2])
	want := "hello\u2028world"
	if got != want {
		t.Errorf("second Read = %q, want %q", got, want)
	}
}

// chunkedReader returns pre-set chunks on each Read call.
type chunkedReader struct {
	chunks [][]byte
	idx    int
}

func (r *chunkedReader) Read(b []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, nil
	}
	n := copy(b, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func TestPasteStdinRead(t *testing.T) {
	// Simulate a paste arriving through Read
	pasteData := append(append(pasteStart, []byte("line1\nline2")...), pasteEnd...)
	p := &pasteStdin{real: bytes.NewReader(pasteData)}

	buf := make([]byte, 1024)
	n, err := p.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	got := string(buf[:n])
	want := "line1\u2028line2"
	if got != want {
		t.Errorf("Read = %q, want %q", got, want)
	}
}

func TestRestorePasteNewlines(t *testing.T) {
	input := "line1\u2028line2\u2028line3"
	got := restorePasteNewlines(input)
	want := "line1\nline2\nline3"
	if got != want {
		t.Errorf("restorePasteNewlines = %q, want %q", got, want)
	}
}

func TestMatchSeq(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		seq  []byte
		want matchType
	}{
		{"full match", pasteStart, pasteStart, matchFull},
		{"partial match", []byte("\033[20"), pasteStart, matchPartial},
		{"no match", []byte("abc"), pasteStart, matchNone},
		{"single ESC partial", []byte{0x1B}, pasteStart, matchPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchSeq(tt.data, tt.seq)
			if got != tt.want {
				t.Errorf("matchSeq() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplacePasteNewlines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no newlines", "hello", "hello"},
		{"LF replaced", "hello\nworld", "hello\u2028world"},
		{"trailing LF replaced", "hello\n", "hello\u2028"},
		{"CR preserved", "hello\r", "hello\r"},
		{"CRLF as one", "a\r\nb", "a\u2028b"},
		{"multi-line", "line1\nline2\nline3", "line1\u2028line2\u2028line3"},
		{"single LF", "\n", "\u2028"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replacePasteNewlines([]byte(tt.input))
			if string(got) != tt.want {
				t.Errorf("replacePasteNewlines(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNonBracketedPasteFallback(t *testing.T) {
	// Simulate a paste arriving without bracketed paste markers.
	// In raw mode, \n is always from paste (Enter sends \r).
	pasteData := []byte("line1\nline2\nline3")
	p := &pasteStdin{real: bytes.NewReader(pasteData)}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	want := "line1\u2028line2\u2028line3"
	if got != want {
		t.Errorf("non-bracketed paste: Read = %q, want %q", got, want)
	}
}

func TestNewlineReplacementAlwaysActive(t *testing.T) {
	// \n replacement works even after bracketed paste was seen,
	// because in raw mode \n is never Enter.
	paste1 := append(append(pasteStart, []byte("a\nb")...), pasteEnd...)
	raw := []byte("x\ny\n")

	combined := append(paste1, raw...)
	p := &pasteStdin{real: bytes.NewReader(combined)}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	// Bracketed paste: a\u2028b (replaced by process)
	// Raw after paste end: x\u2028y\u2028 (replaced by fallback)
	want := "a\u2028bx\u2028y\u2028"
	if got != want {
		t.Errorf("after bracketed paste: Read = %q, want %q", got, want)
	}
}

func TestInject(t *testing.T) {
	p := &pasteStdin{real: bytes.NewReader([]byte("world"))}
	p.Inject([]byte("hello "))

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	if got != "hello " {
		t.Errorf("Read after Inject = %q, want %q", got, "hello ")
	}
}
