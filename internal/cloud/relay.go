package cloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

// Session represents an active session connected to Yu Cloud.
type Session struct {
	SessionID string
	WsURL     string
	conn      *websocket.Conn
	mu        sync.Mutex
	closed    bool
}

// Message is the JSON protocol between CLI and Cloud.
type Message struct {
	Type     string   `json:"type"`
	Data     string   `json:"data,omitempty"`
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Project  string   `json:"project,omitempty"`
	State    string   `json:"state,omitempty"`
	Snapshot int      `json:"snapshot,omitempty"`
	Summary  string   `json:"summary,omitempty"`
}

// StartSession registers a session with Cloud and returns it.
func StartSession(cfg *MachineConfig, agent, project string) (*Session, error) {
	body, _ := json.Marshal(map[string]string{
		"machine_id": cfg.MachineID,
		"secret":     cfg.Secret,
		"agent":      agent,
		"project":    project,
	})
	resp, err := http.Post(CloudURL+"/api/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, data)
	}

	var result struct {
		SessionID string `json:"session_id"`
		WsURL     string `json:"ws_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &Session{
		SessionID: result.SessionID,
		WsURL:     result.WsURL,
	}, nil
}

// Connect opens the WebSocket to Cloud.
func (s *Session) Connect() error {
	wsURL := s.WsURL + "?role=cli"
	// websocket.Dial needs an origin
	origin := strings.Replace(wsURL, "wss://", "https://", 1)
	origin = strings.Replace(origin, "ws://", "http://", 1)

	conn, err := websocket.Dial(wsURL, "", origin)
	if err != nil {
		return fmt.Errorf("websocket connect: %w", err)
	}
	s.conn = conn
	return nil
}

// Send sends a message to Cloud.
func (s *Session) Send(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return websocket.JSON.Send(s.conn, msg)
}

// SendOutput sends agent stdout to Cloud.
func (s *Session) SendOutput(data string) error {
	return s.Send(Message{Type: "output", Data: data})
}

// SendStatus sends status update to Cloud.
func (s *Session) SendStatus(agent, project, state string, snapshot int) error {
	return s.Send(Message{
		Type:     "status",
		Agent:    agent,
		Project:  project,
		State:    state,
		Snapshot: snapshot,
	})
}

// Receive reads a message from Cloud (blocks).
func (s *Session) Receive() (Message, error) {
	var msg Message
	err := websocket.JSON.Receive(s.conn, &msg)
	return msg, err
}

// Close ends the session.
func (s *Session) Close(cfg *MachineConfig) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	if s.conn != nil {
		s.conn.Close()
	}

	// Deregister session
	body, _ := json.Marshal(map[string]string{
		"machine_id": cfg.MachineID,
		"secret":     cfg.Secret,
	})
	req, _ := http.NewRequest("DELETE", CloudURL+"/api/sessions/"+s.SessionID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}
