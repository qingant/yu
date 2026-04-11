package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// execGenerateImage calls the OpenAI Images API and renders the result in terminal.
func (e *ToolExecutor) execGenerateImage(input json.RawMessage) (string, bool) {
	var args struct {
		Prompt   string `json:"prompt"`
		Filename string `json:"filename"`
		Size     string `json:"size"`
		Quality  string `json:"quality"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	// Defaults
	if args.Size == "" {
		args.Size = "1024x1024"
	}
	if args.Quality == "" {
		args.Quality = "medium"
	}

	// Need OpenAI credentials
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	if apiKey == "" {
		return "OpenAI API key not available. Set OPENAI_API_KEY to use image generation.", true
	}

	fmt.Printf("  %s⟳ generating image...%s", dim, reset)

	// Call OpenAI Images API
	reqBody := map[string]any{
		"model":           "gpt-image-1",
		"prompt":          args.Prompt,
		"n":               1,
		"size":            args.Size,
		"quality":         args.Quality,
		"output_format":   "png",
	}
	body, _ := json.Marshal(reqBody)

	url := buildOpenAIURL(baseURL, "/images/generations")
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		fmt.Print("\r\033[K")
		return fmt.Sprintf("error: %v", err), true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Print("\r\033[K")
		return fmt.Sprintf("error: %v", err), true
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Print("\r\033[K") // clear "generating..." line

	if resp.StatusCode != 200 {
		return fmt.Sprintf("API error %d: %s", resp.StatusCode, string(respBody)), true
	}

	// Parse response — gpt-image-1 returns b64_json by default
	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}

	if len(result.Data) == 0 {
		return "no image returned", true
	}

	// Get image bytes
	var imgData []byte
	if result.Data[0].B64JSON != "" {
		imgData, err = base64.StdEncoding.DecodeString(result.Data[0].B64JSON)
		if err != nil {
			return fmt.Sprintf("decode error: %v", err), true
		}
	} else if result.Data[0].URL != "" {
		// Download from URL
		imgResp, err := http.Get(result.Data[0].URL)
		if err != nil {
			return fmt.Sprintf("download error: %v", err), true
		}
		defer imgResp.Body.Close()
		imgData, err = io.ReadAll(imgResp.Body)
		if err != nil {
			return fmt.Sprintf("download error: %v", err), true
		}
	} else {
		return "no image data in response", true
	}

	// Save to file
	outPath := filepath.Join(e.ProjectDir, args.Filename)
	os.MkdirAll(filepath.Dir(outPath), 0755)
	if err := os.WriteFile(outPath, imgData, 0644); err != nil {
		return fmt.Sprintf("write error: %v", err), true
	}

	// Render in terminal
	renderImageInTerminal(imgData, args.Filename)

	return fmt.Sprintf("Image saved to %s (%d bytes, %s)", args.Filename, len(imgData), args.Size), false
}

// renderImageInTerminal displays an image inline if the terminal supports it.
func renderImageInTerminal(data []byte, name string) {
	term := os.Getenv("TERM_PROGRAM")

	switch {
	case term == "iTerm.app" || term == "WezTerm" || os.Getenv("TERM") == "xterm-ghostty":
		// iTerm2 / WezTerm / Ghostty inline image protocol
		renderITerm2Image(data, name)
	case term == "Apple_Terminal":
		// macOS Terminal.app — no inline images, open externally
		fmt.Printf("  %s(image saved: %s)%s\n", dim, name, reset)
	default:
		// Unknown terminal — try iTerm2 protocol (many terminals support it)
		renderITerm2Image(data, name)
	}
}

// renderITerm2Image uses the iTerm2 inline image protocol.
// Format: ESC ] 1337 ; File=[args] : base64data ST
func renderITerm2Image(data []byte, name string) {
	b64 := base64.StdEncoding.EncodeToString(data)
	// width=auto;height=auto limits to terminal size
	fmt.Printf("\n  \033]1337;File=name=%s;size=%d;inline=1;width=60;preserveAspectRatio=1:%s\a\n",
		base64.StdEncoding.EncodeToString([]byte(name)),
		len(data),
		b64,
	)
}
