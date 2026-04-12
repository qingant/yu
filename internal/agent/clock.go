package agent

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"
)

// renderClock draws an ASCII alarm clock with bell on top.
// height is the total height including the bell (minimum 10).
// Returns a slice of strings, one per line, all same width.
func renderClock(t time.Time, height int) []string {
	if height < 10 {
		height = 10
	}

	// Clock face dimensions (excluding bell)
	faceHeight := height - 3 // 2 lines for bell, 1 for feet
	faceWidth := faceHeight * 2
	if faceWidth < 16 {
		faceWidth = 16
	}

	// Center of clock face
	cx := float64(faceWidth) / 2.0
	cy := float64(faceHeight) / 2.0
	rx := cx - 1.0 // radius x
	ry := cy - 0.5 // radius y

	// Canvas
	totalWidth := faceWidth + 2
	canvas := make([][]rune, height)
	for i := range canvas {
		canvas[i] = make([]rune, totalWidth)
		for j := range canvas[i] {
			canvas[i][j] = ' '
		}
	}

	// --- Bell on top (lines 0-1) ---
	bellLine0 := renderBell(totalWidth)
	bellLine1 := renderBellBase(totalWidth)
	for i, ch := range []rune(bellLine0) {
		if i < totalWidth {
			canvas[0][i] = ch
		}
	}
	for i, ch := range []rune(bellLine1) {
		if i < totalWidth {
			canvas[1][i] = ch
		}
	}

	// --- Clock face (lines 2 to 2+faceHeight-1) ---
	faceTop := 2

	// Draw circle
	for angle := 0.0; angle < 360; angle += 2 {
		rad := angle * math.Pi / 180
		x := cx + rx*math.Sin(rad)
		y := cy - ry*math.Cos(rad)
		ix := int(math.Round(x))
		iy := int(math.Round(y)) + faceTop
		if ix >= 0 && ix < totalWidth && iy >= 0 && iy < height-1 {
			canvas[iy][ix] = '·'
		}
	}

	// Draw hour markers
	for h := 1; h <= 12; h++ {
		angle := float64(h) * 30 * math.Pi / 180
		x := cx + (rx-0.5)*math.Sin(angle)
		y := cy - (ry-0.3)*math.Cos(angle)
		ix := int(math.Round(x))
		iy := int(math.Round(y)) + faceTop
		if ix >= 0 && ix < totalWidth && iy >= 0 && iy < height-1 {
			marker := '●'
			if h == 12 || h == 3 || h == 6 || h == 9 {
				marker = '◆'
			}
			canvas[iy][ix] = marker
		}
	}

	// Hour numbers at 12, 3, 6, 9
	placeText(canvas, int(cx), faceTop+1, "12", totalWidth, height)
	placeText(canvas, int(cx+rx)-1, faceTop+int(cy), "3", totalWidth, height)
	placeText(canvas, int(cx), faceTop+int(cy*2)-1, "6", totalWidth, height)
	placeText(canvas, int(cx-rx)+1, faceTop+int(cy), "9", totalWidth, height)

	// Center dot
	ciy := int(math.Round(cy)) + faceTop
	cix := int(math.Round(cx))
	if cix >= 0 && cix < totalWidth && ciy >= 0 && ciy < height-1 {
		canvas[ciy][cix] = '⊙'
	}

	// --- Hands ---
	hour := t.Hour() % 12
	minute := t.Minute()
	second := t.Second()

	// Hour hand (short, thick)
	hourAngle := (float64(hour) + float64(minute)/60.0) * 30
	drawHand(canvas, cx, cy, faceTop, hourAngle, ry*0.45, rx*0.45, '█', totalWidth, height)

	// Minute hand (medium)
	minAngle := float64(minute) * 6
	drawHand(canvas, cx, cy, faceTop, minAngle, ry*0.7, rx*0.7, '▓', totalWidth, height)

	// Second hand (long, thin)
	secAngle := float64(second) * 6
	drawHand(canvas, cx, cy, faceTop, secAngle, ry*0.85, rx*0.85, '╌', totalWidth, height)

	// --- Feet (last line) ---
	feetLine := renderFeet(totalWidth)
	lastLine := height - 1
	for i, ch := range []rune(feetLine) {
		if i < totalWidth {
			canvas[lastLine][i] = ch
		}
	}

	// Convert to strings
	lines := make([]string, height)
	for i, row := range canvas {
		lines[i] = string(row)
	}

	return lines
}

func drawHand(canvas [][]rune, cx, cy float64, faceTop int, angleDeg, lengthY, lengthX float64, ch rune, w, h int) {
	rad := angleDeg * math.Pi / 180
	steps := int(math.Max(lengthY, lengthX) * 2)
	for s := 1; s <= steps; s++ {
		t := float64(s) / float64(steps)
		x := cx + lengthX*t*math.Sin(rad)
		y := cy - lengthY*t*math.Cos(rad)
		ix := int(math.Round(x))
		iy := int(math.Round(y)) + faceTop
		if ix >= 0 && ix < w && iy >= 0 && iy < h-1 {
			// Don't overwrite hour markers or numbers
			existing := canvas[iy][ix]
			if existing == ' ' || existing == '·' || existing == '╌' {
				canvas[iy][ix] = ch
			}
		}
	}
}

func placeText(canvas [][]rune, x, y int, text string, w, h int) {
	runes := []rune(text)
	startX := x - len(runes)/2
	for i, ch := range runes {
		px := startX + i
		if px >= 0 && px < w && y >= 0 && y < h {
			canvas[y][px] = ch
		}
	}
}

func renderBell(width int) string {
	// Bell shape:   ♪ .━━━. ♪
	mid := width / 2
	bell := make([]rune, width)
	for i := range bell {
		bell[i] = ' '
	}
	// Two small bells on sides, dome in middle
	pattern := "♪ ╭━━━╮ ♪"
	runes := []rune(pattern)
	start := mid - len(runes)/2
	for i, ch := range runes {
		pos := start + i
		if pos >= 0 && pos < width {
			bell[pos] = ch
		}
	}
	return string(bell)
}

func renderBellBase(width int) string {
	mid := width / 2
	line := make([]rune, width)
	for i := range line {
		line[i] = ' '
	}
	pattern := "╰──┬──╯"
	runes := []rune(pattern)
	start := mid - len(runes)/2
	for i, ch := range runes {
		pos := start + i
		if pos >= 0 && pos < width {
			line[pos] = ch
		}
	}
	return string(line)
}

func renderFeet(width int) string {
	mid := width / 2
	line := make([]rune, width)
	for i := range line {
		line[i] = ' '
	}
	// Two little feet
	left := mid - 3
	right := mid + 2
	if left >= 0 && left+1 < width {
		line[left] = '╰'
		line[left+1] = '╯'
	}
	if right >= 0 && right+1 < width {
		line[right] = '╰'
		line[right+1] = '╯'
	}
	return string(line)
}

// --- Clock ticking ---

var clockTicking bool

func toggleClock() {
	clockTicking = !clockTicking
}

func isClockTicking() bool {
	return clockTicking
}

// playTick plays a pleasant tick sound (macOS).
func playTick() {
	// Alternate between two soft sounds for tick-tock
	sound := "/System/Library/Sounds/Tink.aiff"
	cmd := exec.Command("afplay", "-v", "0.3", sound)
	cmd.Start()
	// Don't wait — fire and forget
}

// tickMsg and tickEvery are defined in ui.go

// formatSessionDuration formats duration since session start.
func formatSessionDuration(session *Session) string {
	if session == nil || session.CreatedAt.IsZero() {
		return ""
	}
	d := time.Since(session.CreatedAt)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// renderStatusBar renders the bottom status bar.
func renderStatusBar(width int, projectDir, providerName, modelName, status, sessionDur string) string {
	left := fmt.Sprintf(" %s │ %s │ %s:%s │ %s",
		shortenPath(projectDir), status, providerName, shortModel(modelName), sessionDur)

	quote := "live each day as if it's the last"
	right := fmt.Sprintf("%s ", quote)

	// Pad middle
	pad := width - visibleLen(left) - visibleLen(right)
	if pad < 1 {
		pad = 1
	}

	return fmt.Sprintf("%s%s%s%s%s", dim, left, strings.Repeat(" ", pad), right, reset)
}

func shortenPath(path string) string {
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

func visibleLen(s string) int {
	// Strip ANSI escape sequences for length calculation
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}
