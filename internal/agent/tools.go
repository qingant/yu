package agent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/term"
)

// ToolExecutor handles tool execution for the agent.
type ToolExecutor struct {
	ProjectDir  string
	BashTimeout time.Duration
	BgManager   *BgManager
}

// toolDefs returns all tool definitions for the API.
func toolDefs() []ToolDef {
	return []ToolDef{
		{
			Name:        "bash",
			Description: "Run a shell command. Output streams in real-time. Working directory is the project root.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "The shell command to execute"},
					"timeout": {"type": "integer", "description": "Timeout in seconds (default: 120)"}
				},
				"required": ["command"]
			}`),
		},
		{
			Name:        "read_file",
			Description: "Read a file's contents. Returns line-numbered output. For images, returns base64-encoded content.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path (relative to project root or absolute)"},
					"offset": {"type": "integer", "description": "Start reading from this line number (1-based)"},
					"limit": {"type": "integer", "description": "Maximum number of lines to read"}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a file with the given content.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path (relative to project root or absolute)"},
					"content": {"type": "string", "description": "The full file content to write"}
				},
				"required": ["path", "content"]
			}`),
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact string in a file. The old_string must appear exactly once in the file. Include enough surrounding context to make it unique.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path"},
					"old_string": {"type": "string", "description": "Exact string to find (must be unique in the file)"},
					"new_string": {"type": "string", "description": "Replacement string"}
				},
				"required": ["path", "old_string", "new_string"]
			}`),
		},
		{
			Name:        "list_files",
			Description: "List files matching a glob pattern. Returns paths relative to the search directory.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Glob pattern, e.g. '**/*.go', 'src/**/*.ts'"},
					"path": {"type": "string", "description": "Directory to search in (default: project root)"}
				},
				"required": ["pattern"]
			}`),
		},
		{
			Name:        "search_files",
			Description: "Search file contents using ripgrep (regex supported). Returns matching lines with file paths and line numbers.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Search pattern (regex)"},
					"path": {"type": "string", "description": "Directory to search in (default: project root)"},
					"include": {"type": "string", "description": "File glob to include, e.g. '*.go'"}
				},
				"required": ["pattern"]
			}`),
		},
		{
			Name:        "poll",
			Description: "Run a command repeatedly at an interval until it succeeds or times out. Useful for waiting on deployments, CI, health checks, etc.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "Shell command to run each iteration"},
					"interval": {"type": "integer", "description": "Seconds between runs (default: 5)"},
					"timeout": {"type": "integer", "description": "Max total seconds to keep trying (default: 300)"},
					"success_pattern": {"type": "string", "description": "If set, succeed when stdout contains this string (otherwise succeed on exit code 0)"}
				},
				"required": ["command"]
			}`),
		},
		{
			Name:        "web_fetch",
			Description: "Fetch a URL and return its content as text. HTML is converted to readable text. Useful for checking documentation, APIs, or web pages.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"url": {"type": "string", "description": "The URL to fetch"},
					"headers": {"type": "object", "description": "Optional HTTP headers to include"}
				},
				"required": ["url"]
			}`),
		},
		{
			Name:        "background",
			Description: "Manage background processes (dev servers, watchers, etc.). Actions: 'start' launches a command, 'logs' reads output, 'stop' kills it.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["start", "logs", "stop"], "description": "Action to perform"},
					"command": {"type": "string", "description": "Shell command (for 'start')"},
					"id": {"type": "integer", "description": "Process ID (for 'logs' and 'stop')"},
					"tail": {"type": "integer", "description": "Number of log lines to return (default: 50, for 'logs')"}
				},
				"required": ["action"]
			}`),
		},
		{
			Name:        "generate_image",
			Description: "Generate an image using AI. The image is saved to the project directory and displayed in the terminal. Requires OpenAI API access.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"prompt": {"type": "string", "description": "Detailed description of the image to generate"},
					"filename": {"type": "string", "description": "Output filename (e.g. 'diagram.png'). Saved to project root."},
					"size": {"type": "string", "enum": ["1024x1024", "1536x1024", "1024x1536"], "description": "Image size (default: 1024x1024)"},
					"quality": {"type": "string", "enum": ["low", "medium", "high"], "description": "Quality level (default: medium)"}
				},
				"required": ["prompt", "filename"]
			}`),
		},
		{
			Name:        "ask_user",
			Description: "Ask the user a question, present choices, or request confirmation. Use when you need clarification or approval before proceeding.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"question": {"type": "string", "description": "The question to ask"},
					"options": {"type": "array", "items": {"type": "string"}, "description": "Options for the user to choose from (optional)"}
				},
				"required": ["question"]
			}`),
		},
		{
			Name:        "plan",
			Description: "Propose a multi-step plan for the user to review before you begin work. Use for non-trivial tasks that affect multiple files or require design decisions.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"title": {"type": "string", "description": "Plan title"},
					"steps": {"type": "array", "items": {"type": "string"}, "description": "Ordered list of steps"}
				},
				"required": ["title", "steps"]
			}`),
		},
	}
}

// ExecuteTools runs tool calls in parallel and returns results.
func (e *ToolExecutor) ExecuteTools(blocks []ContentBlock) []ContentBlock {
	results := make([]ContentBlock, len(blocks))
	var wg sync.WaitGroup
	for i, block := range blocks {
		wg.Add(1)
		go func(i int, b ContentBlock) {
			defer wg.Done()
			output, isErr := e.execute(b.Name, b.Input)
			results[i] = ContentBlock{
				Type:      "tool_result",
				ToolUseID: b.ID,
				Content:   output,
				IsError:   isErr,
			}
		}(i, block)
	}
	wg.Wait()
	return results
}

func (e *ToolExecutor) execute(name string, input json.RawMessage) (string, bool) {
	switch name {
	case "bash":
		return e.execBash(input)
	case "read_file":
		return e.execReadFile(input)
	case "write_file":
		return e.execWriteFile(input)
	case "edit_file":
		return e.execEditFile(input)
	case "list_files":
		return e.execListFiles(input)
	case "search_files":
		return e.execSearchFiles(input)
	case "poll":
		return e.execPoll(input)
	case "web_fetch":
		return e.execWebFetch(input)
	case "generate_image":
		return e.execGenerateImage(input)
	case "background":
		return e.execBackground(input)
	case "ask_user":
		return e.execAskUser(input)
	case "plan":
		return e.execPlan(input)
	default:
		return fmt.Sprintf("unknown tool: %s", name), true
	}
}

// execBash runs a command with real-time streaming output.
func (e *ToolExecutor) execBash(input json.RawMessage) (string, bool) {
	var args struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	timeout := e.BashTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}

	cmd := exec.Command("bash", "-c", args.Command)
	cmd.Dir = e.ProjectDir

	// Pipe stdout and stderr for real-time streaming
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}

	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("error starting command: %v", err), true
	}

	// Stream and collect output from both pipes
	var output strings.Builder
	var mu sync.Mutex
	var streamWg sync.WaitGroup

	streamPipe := func(r io.Reader, prefix string) {
		defer streamWg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			if output.Len() < 100_000 {
				output.WriteString(line)
				output.WriteByte('\n')
			}
			mu.Unlock()
			// Stream to terminal in real-time, indented
			fmt.Printf("    \033[2m%s%s\033[0m\n", prefix, line)
		}
	}

	streamWg.Add(2)
	go streamPipe(stdoutPipe, "")
	go streamPipe(stderrPipe, "")

	// Wait with timeout
	done := make(chan error, 1)
	go func() {
		streamWg.Wait()
		done <- cmd.Wait()
	}()

	select {
	case cmdErr := <-done:
		result := output.String()
		if cmdErr != nil {
			if exitErr, ok := cmdErr.(*exec.ExitError); ok {
				return fmt.Sprintf("%s\nExit code: %d", result, exitErr.ExitCode()), false
			}
			return fmt.Sprintf("error: %v\n%s", cmdErr, result), true
		}
		return result, false
	case <-time.After(timeout):
		cmd.Process.Kill()
		return fmt.Sprintf("%s\n... command timed out after %v", output.String(), timeout), true
	}
}

// execPoll runs a command repeatedly until success or timeout.
func (e *ToolExecutor) execPoll(input json.RawMessage) (string, bool) {
	var args struct {
		Command        string `json:"command"`
		Interval       int    `json:"interval"`
		Timeout        int    `json:"timeout"`
		SuccessPattern string `json:"success_pattern"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	interval := 5
	if args.Interval > 0 {
		interval = args.Interval
	}
	timeout := 300
	if args.Timeout > 0 {
		timeout = args.Timeout
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	attempt := 0

	for {
		attempt++
		fmt.Printf("    \033[2m[poll #%d] %s\033[0m\n", attempt, args.Command)

		cmd := exec.Command("bash", "-c", args.Command)
		cmd.Dir = e.ProjectDir
		output, err := cmd.CombinedOutput()
		result := string(output)

		// Check success
		if args.SuccessPattern != "" {
			if strings.Contains(result, args.SuccessPattern) {
				fmt.Printf("    \033[32m[poll] success on attempt #%d\033[0m\n", attempt)
				return fmt.Sprintf("Success on attempt #%d:\n%s", attempt, result), false
			}
		} else if err == nil {
			fmt.Printf("    \033[32m[poll] success on attempt #%d\033[0m\n", attempt)
			return fmt.Sprintf("Success on attempt #%d:\n%s", attempt, result), false
		}

		// Show current output
		preview := result
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		fmt.Printf("    \033[2m[poll] not yet: %s\033[0m\n", strings.ReplaceAll(strings.TrimSpace(preview), "\n", " "))

		// Check deadline
		if time.Now().Add(time.Duration(interval) * time.Second).After(deadline) {
			return fmt.Sprintf("Timed out after %d attempts (%ds). Last output:\n%s", attempt, timeout, result), true
		}

		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// execWebFetch fetches a URL and returns content as text.
func (e *ToolExecutor) execWebFetch(input json.RawMessage) (string, bool) {
	var args struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	req, err := http.NewRequest("GET", args.URL, nil)
	if err != nil {
		return fmt.Sprintf("invalid URL: %v", err), true
	}
	req.Header.Set("User-Agent", "yu-agent/1.0")
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("fetch error: %v", err), true
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 500_000))
	if err != nil {
		return fmt.Sprintf("read error: %v", err), true
	}

	contentType := resp.Header.Get("Content-Type")
	result := string(body)

	// Convert HTML to text
	if strings.Contains(contentType, "text/html") {
		result = htmlToText(result)
	}

	const maxOutput = 100_000
	if len(result) > maxOutput {
		result = result[:maxOutput] + "\n... (truncated)"
	}

	header := fmt.Sprintf("HTTP %d | %s\n---\n", resp.StatusCode, contentType)
	return header + result, resp.StatusCode >= 400
}

// htmlToText extracts readable text from HTML.
func htmlToText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	var b strings.Builder
	var extract func(*html.Node)
	extract = func(n *html.Node) {
		// Skip script, style, head
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "nav", "footer":
				return
			case "p", "br", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr":
				b.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				b.WriteString(text)
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			extract(c)
		}
	}
	extract(doc)
	return strings.TrimSpace(b.String())
}

func (e *ToolExecutor) execBackground(input json.RawMessage) (string, bool) {
	var args struct {
		Action  string `json:"action"`
		Command string `json:"command"`
		ID      int    `json:"id"`
		Tail    int    `json:"tail"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	if e.BgManager == nil {
		return "background processes not available", true
	}

	switch args.Action {
	case "start":
		if args.Command == "" {
			return "command is required for 'start'", true
		}
		p, err := e.BgManager.Start(args.Command)
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return fmt.Sprintf("Started background process #%d (pid %d): %s", p.ID, p.Pid, p.Command), false

	case "logs":
		if args.ID == 0 {
			return "id is required for 'logs'", true
		}
		logs, err := e.BgManager.Logs(args.ID, args.Tail)
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		if logs == "" {
			return "(no output yet)", false
		}
		return logs, false

	case "stop":
		if args.ID == 0 {
			return "id is required for 'stop'", true
		}
		if err := e.BgManager.Stop(args.ID); err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return fmt.Sprintf("Stopped process #%d", args.ID), false

	default:
		return fmt.Sprintf("unknown action: %s (use start, logs, stop)", args.Action), true
	}
}

func (e *ToolExecutor) execReadFile(input json.RawMessage) (string, bool) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	path := e.resolvePath(args.Path)

	if isImageFile(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("error reading file: %v", err), true
		}
		mediaType := detectMediaType(path)
		b64 := base64.StdEncoding.EncodeToString(data)
		return fmt.Sprintf("[image: %s, %d bytes, media_type: %s]\nbase64:%s", filepath.Base(path), len(data), mediaType, b64), false
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	lineNum := 0
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 2000
	}

	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if lineNum >= offset+limit {
			lines = append(lines, fmt.Sprintf("... (%d+ lines, showing %d-%d)", lineNum, offset, offset+limit-1))
			break
		}
		lines = append(lines, fmt.Sprintf("%d\t%s", lineNum, scanner.Text()))
	}

	if len(lines) == 0 {
		return "(empty file)", false
	}
	return strings.Join(lines, "\n"), false
}

func (e *ToolExecutor) execWriteFile(input json.RawMessage) (string, bool) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	path := e.resolvePath(args.Path)
	os.MkdirAll(filepath.Dir(path), 0755)

	if err := os.WriteFile(path, []byte(args.Content), 0644); err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), args.Path), false
}

func (e *ToolExecutor) execEditFile(input json.RawMessage) (string, bool) {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	path := e.resolvePath(args.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err), true
	}

	content := string(data)
	count := strings.Count(content, args.OldString)
	if count == 0 {
		return "old_string not found in file", true
	}
	if count > 1 {
		return fmt.Sprintf("old_string found %d times — must be unique. Add more surrounding context.", count), true
	}

	newContent := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		return fmt.Sprintf("error writing file: %v", err), true
	}

	printEditDiff(args.Path, args.OldString, args.NewString)
	return fmt.Sprintf("Edited %s", args.Path), false
}

func (e *ToolExecutor) execListFiles(input json.RawMessage) (string, bool) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	dir := e.ProjectDir
	if args.Path != "" {
		dir = e.resolvePath(args.Path)
	}

	var matches []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" || name == "venv" || name == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		matched, _ := filepath.Match(args.Pattern, filepath.Base(path))
		if !matched {
			if strings.Contains(args.Pattern, "*") {
				suffix := strings.TrimPrefix(args.Pattern, "**")
				suffix = strings.TrimPrefix(suffix, "/")
				if suffix != "" {
					matched, _ = filepath.Match(suffix, filepath.Base(path))
				}
			}
		}
		if matched {
			matches = append(matches, rel)
		}
		if len(matches) >= 1000 {
			return fmt.Errorf("too many matches")
		}
		return nil
	})

	if len(matches) == 0 {
		return "no files found", false
	}
	return strings.Join(matches, "\n"), false
}

func (e *ToolExecutor) execSearchFiles(input json.RawMessage) (string, bool) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	dir := e.ProjectDir
	if args.Path != "" {
		dir = e.resolvePath(args.Path)
	}

	cmdArgs := []string{"-n", "--no-heading", "--color=never"}
	if args.Include != "" {
		cmdArgs = append(cmdArgs, "-g", args.Include)
	}
	cmdArgs = append(cmdArgs, args.Pattern, dir)

	var cmd *exec.Cmd
	if rgPath, err := exec.LookPath("rg"); err == nil {
		cmd = exec.Command(rgPath, cmdArgs...)
	} else {
		grepArgs := []string{"-rn", "--color=never"}
		if args.Include != "" {
			grepArgs = append(grepArgs, "--include="+args.Include)
		}
		grepArgs = append(grepArgs, args.Pattern, dir)
		cmd = exec.Command("grep", grepArgs...)
	}

	output, _ := cmd.CombinedOutput()
	result := string(output)

	const maxOutput = 50_000
	if len(result) > maxOutput {
		result = result[:maxOutput] + "\n... (output truncated)"
	}

	if result == "" {
		return "no matches found", false
	}
	return result, false
}

func (e *ToolExecutor) execAskUser(input json.RawMessage) (string, bool) {
	var args struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	fmt.Printf("\n  \033[1;33m%s\033[0m\n", args.Question)

	if len(args.Options) > 0 {
		selected := arrowSelect(args.Options)
		if selected == "" {
			return "(cancelled)", false
		}
		return selected, false
	}

	// No options — free text input
	fmt.Print("  > ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	return strings.TrimSpace(answer), false
}

func (e *ToolExecutor) execPlan(input json.RawMessage) (string, bool) {
	var args struct {
		Title string   `json:"title"`
		Steps []string `json:"steps"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	fmt.Printf("\n  \033[1;36m%s\033[0m\n", args.Title)
	for i, step := range args.Steps {
		fmt.Printf("  %d. %s\n", i+1, step)
	}
	fmt.Println()

	selected := arrowSelect([]string{"Approve", "Reject", "Edit"})
	switch selected {
	case "Approve":
		return "Plan approved. Proceed with implementation.", false
	case "", "Reject":
		return "Plan rejected by user.", false
	default:
		return fmt.Sprintf("User response: %s", selected), false
	}
}

// Selection return values
const selectBack = "\x00back" // sentinel: user wants to go back (u key)

// arrowSelect renders an interactive list. Arrow keys to move, Enter to select.
// Returns "" on q/Esc (exit), selectBack on u (go back), or the selected option.
func arrowSelect(options []string) string {
	return arrowSelectAt(options, 0)
}

// arrowSelectAt renders an interactive list starting at the given index.
func arrowSelectAt(options []string, startIdx int) string {
	if len(options) == 0 {
		return ""
	}

	oldState, err := rawModeOn()
	if err != nil {
		return fallbackSelect(options)
	}
	defer rawModeOff(oldState)

	cursor := startIdx
	if cursor < 0 || cursor >= len(options) {
		cursor = 0
	}
	renderList(options, cursor)

	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			break
		}

		vl := visibleLines(len(options))
		switch {
		case n == 1 && buf[0] == 13: // Enter
			clearLines(vl)
			fmt.Printf("  \033[32m✓ %s\033[0m\n", options[cursor])
			return options[cursor]

		case n == 1 && (buf[0] == 3 || buf[0] == 'q'): // Ctrl+C or q → exit
			clearLines(vl)
			return ""

		case n == 1 && buf[0] == 27: // Esc → exit
			clearLines(vl)
			return ""

		case n == 1 && buf[0] == 'u': // u → go back
			clearLines(vl)
			return selectBack

		case n == 3 && buf[0] == 27 && buf[1] == 91: // Arrow keys
			switch buf[2] {
			case 65: // Up
				if cursor > 0 {
					cursor--
				}
			case 66: // Down
				if cursor < len(options)-1 {
					cursor++
				}
			}
			clearLines(vl)
			renderList(options, cursor)
		}
	}

	return options[cursor]
}

const maxVisible = 15 // max items visible at once

func renderList(options []string, cursor int) {
	total := len(options)
	if total <= maxVisible {
		// No scrolling needed
		for i, opt := range options {
			if i == cursor {
				fmt.Printf("  \033[36m❯ %s\033[0m\r\n", opt)
			} else {
				fmt.Printf("    %s\r\n", opt)
			}
		}
		return
	}

	// Scrolling: keep cursor centered in viewport
	start := cursor - maxVisible/2
	if start < 0 {
		start = 0
	}
	end := start + maxVisible
	if end > total {
		end = total
		start = end - maxVisible
	}

	if start > 0 {
		fmt.Printf("  \033[2m  ↑ %d more\033[0m\r\n", start)
	}
	for i := start; i < end; i++ {
		if i == cursor {
			fmt.Printf("  \033[36m❯ %s\033[0m\r\n", options[i])
		} else {
			fmt.Printf("    %s\r\n", options[i])
		}
	}
	if end < total {
		fmt.Printf("  \033[2m  ↓ %d more\033[0m\r\n", total-end)
	}
}

func visibleLines(total int) int {
	n := total
	if n > maxVisible {
		n = maxVisible
		n++ // "↓ more" line
	}
	// Add "↑ more" line if we might scroll
	if total > maxVisible {
		n++
	}
	return n
}

func clearLines(n int) {
	for i := 0; i < n; i++ {
		fmt.Print("\033[A\033[K")
	}
}

func fallbackSelect(options []string) string {
	for i, opt := range options {
		fmt.Printf("  %d) %s\n", i+1, opt)
	}
	fmt.Print("  > ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(answer)
	if n, err := fmt.Sscanf(answer, "%d"); err == nil && n >= 1 && n <= len(options) {
		return options[n-1]
	}
	if answer != "" {
		return answer
	}
	return options[0]
}

func (e *ToolExecutor) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(e.ProjectDir, path)
}

func isImageFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
		return true
	}
	return false
}

func detectMediaType(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "application/octet-stream"
	}
	return http.DetectContentType(data)
}

func rawJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}

func rawModeOn() (*term.State, error) {
	return term.MakeRaw(int(os.Stdin.Fd()))
}

func rawModeOff(state *term.State) {
	term.Restore(int(os.Stdin.Fd()), state)
}

// printEditDiff prints a colorized unified diff for an edit_file operation.
func printEditDiff(path, oldStr, newStr string) {
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")

	// Find common prefix/suffix lines to show minimal context
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	oldSuffix, newSuffix := len(oldLines)-1, len(newLines)-1
	for oldSuffix > prefix && newSuffix > prefix && oldLines[oldSuffix] == newLines[newSuffix] {
		oldSuffix--
		newSuffix--
	}

	// Context lines around changes
	ctxStart := prefix - 3
	if ctxStart < 0 {
		ctxStart = 0
	}
	oldEnd := oldSuffix + 4
	if oldEnd > len(oldLines) {
		oldEnd = len(oldLines)
	}
	newEnd := newSuffix + 4
	if newEnd > len(newLines) {
		newEnd = len(newLines)
	}

	fmt.Printf("    %s--- %s%s\n", dim, path, reset)
	fmt.Printf("    %s+++ %s%s\n", dim, path, reset)

	// Print context before
	for i := ctxStart; i < prefix; i++ {
		fmt.Printf("    %s %s%s\n", dim, oldLines[i], reset)
	}
	// Print removed lines
	for i := prefix; i <= oldSuffix; i++ {
		fmt.Printf("    %s-%s%s\n", red, oldLines[i], reset)
	}
	// Print added lines
	for i := prefix; i <= newSuffix; i++ {
		fmt.Printf("    %s+%s%s\n", green, newLines[i], reset)
	}
	// Print context after
	afterStart := oldSuffix + 1
	for i := afterStart; i < oldEnd; i++ {
		fmt.Printf("    %s %s%s\n", dim, oldLines[i], reset)
	}
}
