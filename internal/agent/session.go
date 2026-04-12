package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TokenStats tracks token usage.
type TokenStats struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read,omitempty"`
	CacheWrite   int64 `json:"cache_write,omitempty"`
	Turns        int   `json:"turns"`
}

// Session holds a conversation that can be persisted to disk.
type Session struct {
	ID             string     `json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Model          string     `json:"model"`
	Provider       string     `json:"provider,omitempty"`
	Title          string     `json:"title"`
	Messages       []Message  `json:"messages"`
	CompactSummary string     `json:"compact_summary,omitempty"`
	Stats          TokenStats `json:"stats"`
}

// SessionInfo is a lightweight summary for listing sessions.
type SessionInfo struct {
	ID        string
	Title     string
	Model     string
	UpdatedAt time.Time
	Turns     int // number of user messages
}

// NewSession creates a fresh session.
func NewSession(model string) *Session {
	return &Session{
		ID:        sessionID(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Model:     model,
	}
}

// Save persists the session to the workspace sessions directory.
func (s *Session) Save(wsDir string) error {
	dir := filepath.Join(wsDir, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	s.UpdatedAt = time.Now()

	// Auto-generate title from first user message if empty
	if s.Title == "" {
		s.Title = s.autoTitle()
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(dir, s.ID+".json")
	return os.WriteFile(path, data, 0600)
}

// LoadSession reads a session from disk by ID.
// It sanitizes all message content to strip ANSI escapes and control characters
// that may have been persisted by older versions.
func LoadSession(wsDir, id string) (*Session, error) {
	path := filepath.Join(wsDir, "sessions", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	sanitizeSessionMessages(&s)
	return &s, nil
}

// sanitizeSessionMessages strips control characters from all message content
// in a session. This cleans up tool results (and other text) that were saved
// with ANSI escapes by older versions.
func sanitizeSessionMessages(s *Session) {
	for i := range s.Messages {
		for j := range s.Messages[i].Content {
			b := &s.Messages[i].Content[j]
			// Clean text blocks
			if b.Text != "" {
				b.Text = stripControlChars(b.Text)
			}
			// Clean tool_result content (stored as string)
			if b.Type == "tool_result" {
				if content, ok := b.Content.(string); ok && content != "" {
					b.Content = stripControlChars(content)
				}
			}
		}
	}
}

// LoadLatestSession returns the most recently updated session, or nil.
func LoadLatestSession(wsDir string) *Session {
	sessions := ListSessions(wsDir)
	if len(sessions) == 0 {
		return nil
	}
	// sessions are sorted newest-first
	s, err := LoadSession(wsDir, sessions[0].ID)
	if err != nil {
		return nil
	}
	return s
}

// ListSessions returns all saved sessions, sorted by UpdatedAt descending.
func ListSessions(wsDir string) []SessionInfo {
	dir := filepath.Join(wsDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var infos []SessionInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil {
			continue
		}

		turns := 0
		for _, m := range s.Messages {
			if m.Role == "user" {
				// Don't count tool_result-only messages as user turns
				for _, b := range m.Content {
					if b.Type == "text" {
						turns++
						break
					}
				}
			}
		}

		infos = append(infos, SessionInfo{
			ID:        s.ID,
			Title:     s.Title,
			Model:     s.Model,
			UpdatedAt: s.UpdatedAt,
			Turns:     turns,
		})
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
	})
	return infos
}

// DeleteSession removes a session file.
func DeleteSession(wsDir, id string) error {
	path := filepath.Join(wsDir, "sessions", id+".json")
	return os.Remove(path)
}

func (s *Session) autoTitle() string {
	for _, m := range s.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && b.Text != "" {
				title := b.Text
				// Truncate to first line, max 60 chars
				if idx := strings.IndexByte(title, '\n'); idx >= 0 {
					title = title[:idx]
				}
				if len(title) > 60 {
					title = title[:57] + "..."
				}
				return title
			}
		}
	}
	return "untitled"
}

func sessionID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GlobalStats sums stats across all saved sessions.
func GlobalStats(wsDir string) TokenStats {
	var total TokenStats
	sessions := ListSessions(wsDir)
	for _, info := range sessions {
		s, err := LoadSession(wsDir, info.ID)
		if err != nil {
			continue
		}
		total.InputTokens += s.Stats.InputTokens
		total.OutputTokens += s.Stats.OutputTokens
		total.CacheRead += s.Stats.CacheRead
		total.CacheWrite += s.Stats.CacheWrite
		total.Turns += s.Stats.Turns
	}
	return total
}
