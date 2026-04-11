package copilot

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// CopilotEndpoint is the OpenAI-compatible chat completions endpoint.
const CopilotEndpoint = "https://api.githubcopilot.com"

// copilotTokenResponse is the response from the Copilot token exchange.
type copilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// TokenManager handles Copilot JWT token exchange and auto-refresh.
type TokenManager struct {
	oauthToken string

	mu        sync.Mutex
	jwt       string
	expiresAt time.Time
}

// NewTokenManager creates a token manager with the given OAuth access token.
func NewTokenManager(oauthToken string) *TokenManager {
	return &TokenManager{oauthToken: oauthToken}
}

// Token returns a valid Copilot JWT token, refreshing if needed.
func (tm *TokenManager) Token() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Return cached token if still valid (with 60s buffer)
	if tm.jwt != "" && time.Now().Before(tm.expiresAt.Add(-60*time.Second)) {
		return tm.jwt, nil
	}

	return tm.refresh()
}

func (tm *TokenManager) refresh() (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/copilot_internal/v2/token", nil)
	req.Header.Set("Authorization", "token "+tm.oauthToken)
	req.Header.Set("User-Agent", "Yu/1.0")
	req.Header.Set("Editor-Version", "Yu/1.0")
	req.Header.Set("Editor-Plugin-Version", "yu/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot token exchange: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("copilot token exchange: status %d: %s", resp.StatusCode, string(body))
	}

	var result copilotTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding copilot token: %w", err)
	}

	if result.Token == "" {
		return "", fmt.Errorf("empty copilot token — check Copilot subscription")
	}

	tm.jwt = result.Token
	tm.expiresAt = time.Unix(result.ExpiresAt, 0)

	return tm.jwt, nil
}
