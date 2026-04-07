package sandbox

import (
	"os"
	"strings"
	"testing"

	"github.com/taoai/yu/internal/config"
	"github.com/taoai/yu/internal/netproxy"
)

func TestBuildEnvInjectsBaseURL(t *testing.T) {
	// Simulate: user has OPENAI_API_KEY but no OPENAI_BASE_URL
	os.Setenv("OPENAI_API_KEY", "sk-test-key")
	os.Unsetenv("OPENAI_BASE_URL")
	defer os.Unsetenv("OPENAI_API_KEY")

	s := &Sandbox{
		TmpDir:    "/tmp/yu-test",
		Config:    config.Defaults(),
		apiProxy:  netproxy.NewAPIProxy(),
		apiAddr:   "127.0.0.1:9999",
		dummyKeys: make(map[string]string),
	}

	// Simulate what configureKeyReplacements does
	s.dummyKeys["OPENAI_API_KEY"] = "yu-openai_api_key-abcd1234"
	s.dummyKeys["OPENAI_BASE_URL"] = "http://127.0.0.1:9999/openai"

	env := s.buildEnv()

	var foundKey, foundBase string
	for _, e := range env {
		if strings.HasPrefix(e, "OPENAI_API_KEY=") {
			foundKey = e
		}
		if strings.HasPrefix(e, "OPENAI_BASE_URL=") {
			foundBase = e
		}
	}

	if foundKey != "OPENAI_API_KEY=yu-openai_api_key-abcd1234" {
		t.Errorf("Expected dummy API key, got: %q", foundKey)
	}
	if foundBase != "OPENAI_BASE_URL=http://127.0.0.1:9999/openai" {
		t.Errorf("Expected injected BASE_URL, got: %q", foundBase)
	}

	t.Logf("OPENAI_API_KEY: %s", foundKey)
	t.Logf("OPENAI_BASE_URL: %s", foundBase)
}
