package agent

import (
	"regexp"
	"strings"
)

// TermRenderer processes streaming markdown text and renders it for the terminal.
// Call Feed() with text chunks as they arrive. Call Flush() when the block is done.
type TermRenderer struct {
	lineBuf     strings.Builder // incomplete line accumulator
	tableBuf    []string        // buffered table rows
	inCodeBlock bool
	codeLang    string
}

// NewTermRenderer creates a renderer.
func NewTermRenderer() *TermRenderer {
	return &TermRenderer{}
}

// Feed processes a chunk of streaming text. May print immediately or buffer.
func (r *TermRenderer) Feed(text string) {
	r.lineBuf.WriteString(text)

	// Process complete lines
	for {
		content := r.lineBuf.String()
		idx := strings.IndexByte(content, '\n')
		if idx < 0 {
			break
		}
		line := content[:idx]
		r.lineBuf.Reset()
		r.lineBuf.WriteString(content[idx+1:])
		r.processLine(line)
	}
}

// Flush outputs any remaining buffered content.
func (r *TermRenderer) Flush() {
	// Flush remaining partial line
	if r.lineBuf.Len() > 0 {
		r.processLine(r.lineBuf.String())
		r.lineBuf.Reset()
	}
	r.flushTable()
	if r.inCodeBlock {
		outPrintf("%s└─%s\n", dim, reset)
		r.inCodeBlock = false
	}
}

func (r *TermRenderer) processLine(line string) {
	// Code block toggle
	if strings.HasPrefix(line, "```") {
		r.flushTable()
		if r.inCodeBlock {
			outPrintf("%s└─%s\n", dim, reset)
			r.inCodeBlock = false
		} else {
			r.inCodeBlock = true
			r.codeLang = strings.TrimPrefix(line, "```")
			label := r.codeLang
			if label == "" {
				label = "code"
			}
			outPrintf("%s┌─ %s%s\n", dim, label, reset)
		}
		return
	}

	// Inside code block
	if r.inCodeBlock {
		outPrintf("%s│%s %s\n", dim, reset, line)
		return
	}

	// Table line detection
	if isTableLine(line) {
		r.tableBuf = append(r.tableBuf, line)
		return
	}

	// Flush any buffered table before other content
	r.flushTable()

	// Headers
	if strings.HasPrefix(line, "# ") {
		outPrintf("\n%s%s%s\n", bold, strings.TrimPrefix(line, "# "), reset)
		return
	}
	if strings.HasPrefix(line, "## ") {
		outPrintf("\n%s%s%s\n", bold, strings.TrimPrefix(line, "## "), reset)
		return
	}
	if strings.HasPrefix(line, "### ") {
		outPrintf("%s%s%s\n", bold, strings.TrimPrefix(line, "### "), reset)
		return
	}

	// Unordered list
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		outPrintf("  • %s\n", renderInline(line[2:]))
		return
	}
	// Indented list
	if strings.HasPrefix(line, "  - ") || strings.HasPrefix(line, "  * ") {
		outPrintf("    ◦ %s\n", renderInline(line[4:]))
		return
	}

	// Numbered list — keep as-is but render inline
	if matched, _ := regexp.MatchString(`^\d+\. `, line); matched {
		outPrintf("  %s\n", renderInline(line))
		return
	}

	// Horizontal rule
	trimmed := strings.TrimSpace(line)
	if trimmed == "---" || trimmed == "***" || trimmed == "___" {
		outPrintf("%s────────────────────────────────%s\n", dim, reset)
		return
	}

	// Regular text
	outPrintf("%s\n", renderInline(line))
}

// flushTable renders a buffered table with aligned columns.
func (r *TermRenderer) flushTable() {
	if len(r.tableBuf) == 0 {
		return
	}

	// Parse rows into cells
	var rows [][]string
	var separatorIdx int = -1
	for i, line := range r.tableBuf {
		cells := parseTableRow(line)
		if isSeparatorRow(line) {
			separatorIdx = i
			continue
		}
		rows = append(rows, cells)
	}

	if len(rows) == 0 {
		r.tableBuf = nil
		return
	}

	// Calculate column widths
	numCols := 0
	for _, row := range rows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	widths := make([]int, numCols)
	for _, row := range rows {
		for i, cell := range row {
			w := displayWidth(cell)
			if w > widths[i] {
				widths[i] = w
			}
		}
	}

	// Cap column width
	for i := range widths {
		if widths[i] > 40 {
			widths[i] = 40
		}
		if widths[i] < 2 {
			widths[i] = 2
		}
	}

	// Render
	totalWidth := 1
	for _, w := range widths {
		totalWidth += w + 3 // " cell " + "|"
	}

	// Top border
	outPrintf("  %s┌", dim)
	for i, w := range widths {
		outPrint(strings.Repeat("─", w+2))
		if i < len(widths)-1 {
			outPrint("┬")
		}
	}
	outPrintf("┐%s\n", reset)

	for rowIdx, row := range rows {
		outPrintf("  %s│%s", dim, reset)
		for i := 0; i < numCols; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			w := displayWidth(cell)
			if w > widths[i] {
				cell = truncateToWidth(cell, widths[i])
				w = widths[i]
			}
			pad := widths[i] - w
			if rowIdx == 0 && separatorIdx > 0 {
				// Header row — bold
				outPrintf(" %s%s%s%s", bold, cell, reset, strings.Repeat(" ", pad))
			} else {
				outPrintf(" %s%s", cell, strings.Repeat(" ", pad))
			}
			outPrintf(" %s│%s", dim, reset)
		}
		outPrintln()

		// Separator after header
		if rowIdx == 0 && separatorIdx > 0 {
			outPrintf("  %s├", dim)
			for i, w := range widths {
				outPrint(strings.Repeat("─", w+2))
				if i < len(widths)-1 {
					outPrint("┼")
				}
			}
			outPrintf("┤%s\n", reset)
		}
	}

	// Bottom border
	outPrintf("  %s└", dim)
	for i, w := range widths {
		outPrint(strings.Repeat("─", w+2))
		if i < len(widths)-1 {
			outPrint("┴")
		}
	}
	outPrintf("┘%s\n", reset)

	r.tableBuf = nil
}

func isTableLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|")
}

func isSeparatorRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.ReplaceAll(trimmed, "|", "")
	trimmed = strings.ReplaceAll(trimmed, "-", "")
	trimmed = strings.ReplaceAll(trimmed, ":", "")
	trimmed = strings.TrimSpace(trimmed)
	return trimmed == ""
}

func parseTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// renderInline applies inline markdown formatting.
func renderInline(text string) string {
	// Bold: **text** or __text__
	text = replaceInlinePattern(text, "**", bold, reset)
	text = replaceInlinePattern(text, "__", bold, reset)

	// Inline code: `text`
	text = replaceInlinePattern(text, "`", "\033[36m", "\033[0m") // cyan for inline code

	return text
}

func replaceInlinePattern(text, delim, open, close string) string {
	for {
		start := strings.Index(text, delim)
		if start < 0 {
			break
		}
		end := strings.Index(text[start+len(delim):], delim)
		if end < 0 {
			break
		}
		end += start + len(delim)
		inner := text[start+len(delim) : end]
		text = text[:start] + open + inner + close + text[end+len(delim):]
	}
	return text
}

func displayWidth(s string) int {
	w := 0
	runes := []rune(s)
	for i, r := range runes {
		if isZeroWidth(r) {
			continue
		}
		if isWideRune(r) {
			w += 2
		} else if isEmojiPresentation(r, runes, i) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isZeroWidth returns true for characters that don't occupy terminal columns.
func isZeroWidth(r rune) bool {
	return r == 0xFE0F || // variation selector-16 (emoji presentation)
		r == 0xFE0E || // variation selector-15 (text presentation)
		(r >= 0x200B && r <= 0x200F) || // zero-width space, joiners, marks
		r == 0x2060 || // word joiner
		r == 0xFEFF || // BOM / zero-width no-break space
		(r >= 0x0300 && r <= 0x036F) || // combining diacritical marks
		(r >= 0x1AB0 && r <= 0x1AFF) || // combining marks extended
		(r >= 0x20D0 && r <= 0x20FF) // combining marks for symbols
}

// isEmojiPresentation returns true for emoji that render as wide (2-col) in terminals.
// Checks if followed by variation selector FE0F, or is in a range that defaults to emoji presentation.
func isEmojiPresentation(r rune, runes []rune, idx int) bool {
	// If followed by FE0F, it's emoji presentation → wide
	if idx+1 < len(runes) && runes[idx+1] == 0xFE0F {
		return true
	}
	// Emoji that default to wide presentation
	return (r >= 0x1F300 && r <= 0x1F9FF) || // Misc Symbols & Pictographs, Emoticons, etc.
		(r >= 0x2600 && r <= 0x27BF) || // Misc Symbols, Dingbats
		(r >= 0x1FA00 && r <= 0x1FAFF) // Symbols & Pictographs Extended-A
}

// isWideRune returns true for CJK and other East Asian wide characters
// that occupy 2 columns in a terminal.
func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) || // Hangul Jamo
		r == 0x2329 || r == 0x232A || // angle brackets
		(r >= 0x2E80 && r <= 0x303E) || // CJK radicals, Kangxi, ideographic
		(r >= 0x3040 && r <= 0x33BF) || // Hiragana, Katakana, Bopomofo, CJK compat
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Unified Ext A
		(r >= 0x4E00 && r <= 0xA4CF) || // CJK Unified, Yi
		(r >= 0xA960 && r <= 0xA97C) || // Hangul Jamo Extended-A
		(r >= 0xAC00 && r <= 0xD7A3) || // Hangul Syllables
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility Ideographs
		(r >= 0xFE30 && r <= 0xFE6F) || // CJK Compatibility Forms
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth Forms
		(r >= 0xFFE0 && r <= 0xFFE6) || // Fullwidth Signs
		(r >= 0x20000 && r <= 0x2FFFD) || // CJK Ext B-F
		(r >= 0x30000 && r <= 0x3FFFD) // CJK Ext G+
}

func truncateToWidth(s string, maxWidth int) string {
	if displayWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 1 {
		return "…"
	}
	w := 0
	runes := []rune(s)
	for i, r := range runes {
		if isZeroWidth(r) {
			continue
		}
		rw := 1
		if isWideRune(r) {
			rw = 2
		} else if isEmojiPresentation(r, runes, i) {
			rw = 2
		}
		if w+rw > maxWidth-1 {
			return string(runes[:i]) + "…"
		}
		w += rw
	}
	return s
}
