package netproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForceHeaders(t *testing.T) {
	// Fake upstream that echoes auth headers
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Authorization=" + r.Header.Get("Authorization") + "\n"))
		w.Write([]byte("X-Api-Key=" + r.Header.Get("X-Api-Key") + "\n"))
	}))
	defer upstream.Close()

	// API proxy with ForceHeaders
	ap := NewAPIProxy()
	ap.Routes = []APIRoute{
		{
			PathPrefix: "/openai",
			Upstream:   upstream.URL,
			ForceHeaders: map[string]string{
				"Authorization": "Bearer sk-real-key-123",
			},
		},
		{
			PathPrefix: "/anthropic",
			Upstream:   upstream.URL,
			ForceHeaders: map[string]string{
				"X-Api-Key": "sk-ant-real-key-456",
			},
		},
	}

	addr, err := ap.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Stop()

	// Test 1: OpenAI route - agent sends JWT, should be overridden
	req, _ := http.NewRequest("POST", "http://"+addr+"/openai/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.fake-jwt")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yu-Proxy-Secret", ap.Secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "Authorization=Bearer sk-real-key-123") {
		t.Errorf("OpenAI: ForceHeaders not applied.\nGot: %s", body)
	}

	// Test 2: Anthropic route - agent sends dummy, should be overridden
	req2, _ := http.NewRequest("POST", "http://"+addr+"/anthropic/v1/messages", strings.NewReader("{}"))
	req2.Header.Set("X-Api-Key", "yu-anthropic-dummy123")
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Yu-Proxy-Secret", ap.Secret)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if !strings.Contains(string(body2), "X-Api-Key=sk-ant-real-key-456") {
		t.Errorf("Anthropic: ForceHeaders not applied.\nGot: %s", body2)
	}

	t.Logf("OpenAI response: %s", body)
	t.Logf("Anthropic response: %s", body2)
}
