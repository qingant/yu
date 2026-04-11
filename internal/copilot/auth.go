package copilot

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitHub OAuth App client ID used by copilot.vim / Copilot CLI.
const githubClientID = "Iv1.b507a08c87ecfe98"

// deviceCodeResponse is the response from POST /login/device/code.
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// tokenResponse is the response from POST /login/oauth/access_token.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// savedAuth is persisted to ~/.config/yu/copilot.json.
type savedAuth struct {
	AccessToken string `json:"access_token"`
	User        string `json:"user,omitempty"`
}

// Login performs the GitHub OAuth device flow.
// It prints a user code, waits for browser authorization, and saves the token.
// printFn is called for user-visible output.
func Login(printFn func(string)) (string, error) {
	// Step 1: Request device and user verification codes
	body := fmt.Sprintf(`{"client_id":"%s","scope":"read:user"}`, githubClientID)
	req, _ := http.NewRequest("POST", "https://github.com/login/device/code", strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Yu/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting device code: %w", err)
	}
	defer resp.Body.Close()

	var dc deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return "", fmt.Errorf("decoding device code: %w", err)
	}

	if dc.UserCode == "" {
		return "", fmt.Errorf("GitHub returned empty user code")
	}

	printFn(fmt.Sprintf("\n  Your code: %s\n", dc.UserCode))
	printFn(fmt.Sprintf("  Open: %s\n", dc.VerificationURI))
	printFn("  Waiting for authorization...\n")

	// Step 2: Poll for access token
	interval := dc.Interval
	if interval < 5 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval) * time.Second)

		tokenBody := fmt.Sprintf(
			`{"client_id":"%s","device_code":"%s","grant_type":"urn:ietf:params:oauth:grant-type:device_code"}`,
			githubClientID, dc.DeviceCode,
		)
		treq, _ := http.NewRequest("POST", "https://github.com/login/oauth/access_token", strings.NewReader(tokenBody))
		treq.Header.Set("Accept", "application/json")
		treq.Header.Set("Content-Type", "application/json")
		treq.Header.Set("User-Agent", "Yu/1.0")

		tresp, err := http.DefaultClient.Do(treq)
		if err != nil {
			continue
		}

		var tr tokenResponse
		json.NewDecoder(tresp.Body).Decode(&tr)
		tresp.Body.Close()

		switch tr.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		case "expired_token":
			return "", fmt.Errorf("device code expired, please try again")
		case "access_denied":
			return "", fmt.Errorf("authorization denied by user")
		case "":
			// Success
			if tr.AccessToken == "" {
				return "", fmt.Errorf("empty access token in response")
			}

			// Get username
			user := getGitHubUser(tr.AccessToken)

			// Save token
			if err := saveAuth(tr.AccessToken, user); err != nil {
				return "", fmt.Errorf("saving token: %w", err)
			}

			return user, nil
		default:
			return "", fmt.Errorf("OAuth error: %s: %s", tr.Error, tr.ErrorDesc)
		}
	}

	return "", fmt.Errorf("timed out waiting for authorization")
}

// getGitHubUser fetches the authenticated user's login name.
func getGitHubUser(token string) string {
	req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("User-Agent", "Yu/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var u struct {
		Login string `json:"login"`
	}
	json.NewDecoder(resp.Body).Decode(&u)
	return u.Login
}

// LoadToken returns the saved OAuth access token, or "" if not logged in.
func LoadToken() string {
	path := authFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var auth savedAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return ""
	}
	return auth.AccessToken
}

// IsLoggedIn returns true if a Copilot OAuth token is saved.
func IsLoggedIn() bool {
	return LoadToken() != ""
}

// Logout removes the saved token.
func Logout() error {
	path := authFilePath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func saveAuth(token, user string) error {
	path := authFilePath()
	os.MkdirAll(filepath.Dir(path), 0700)

	data, _ := json.MarshalIndent(savedAuth{AccessToken: token, User: user}, "", "  ")
	return os.WriteFile(path, data, 0600)
}

func authFilePath() string {
	// Prefer XDG, fallback to ~/.config
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "yu", "copilot.json")
}

// ValidateToken checks if the saved OAuth token can obtain a Copilot session token.
func ValidateToken() (string, error) {
	token := LoadToken()
	if token == "" {
		return "", fmt.Errorf("not logged in")
	}

	req, _ := http.NewRequest("GET", "https://api.github.com/copilot_internal/v2/token", nil)
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("User-Agent", "Yu/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
		User  string `json:"user"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Token == "" {
		return "", fmt.Errorf("no Copilot subscription found")
	}

	user := result.User
	if user == "" {
		// Read from saved auth
		data, _ := os.ReadFile(authFilePath())
		var auth savedAuth
		json.Unmarshal(data, &auth)
		user = auth.User
	}

	return user, nil
}
