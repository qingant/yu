package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/taoai/yu/internal/cloud"
	"github.com/taoai/yu/internal/cmdproxy"
	"github.com/taoai/yu/internal/config"
	"github.com/taoai/yu/internal/netproxy"
	"github.com/taoai/yu/internal/snapshot"
)

// knownAgents maps command names to their permission bypass flags
// and config directories (relative to real HOME) that need to be
// symlinked into the sandbox HOME so the agent can read its own config.
type agentInfo struct {
	BypassFlags []string
	FlagsAtEnd  bool     // if true, append flags at end instead of after command[0]
	ConfigDirs  []string // relative to $HOME, dirs get symlinked
	ConfigFiles []string // relative to $HOME, individual files get symlinked
}

var knownAgents = map[string]agentInfo{
	"claude": {
		BypassFlags: []string{"--dangerously-skip-permissions"},
		ConfigDirs:  []string{".claude"},
		ConfigFiles: []string{".claude.json"},
	},
	"codex": {
		BypassFlags: []string{"--dangerously-bypass-approvals-and-sandbox"},
		ConfigDirs:  []string{".codex"},
	},
	"gemini": {
		BypassFlags: nil,
		ConfigDirs:  []string{".config/gemini"},
	},
	"yu": {
		// Built-in agent — no bypass flags, no config dirs needed
	},
}

// Sandbox manages the lifecycle of a sandboxed agent process.
type Sandbox struct {
	ID         string
	ProjectDir string
	Command    []string
	Config     *config.Config
	TmpDir     string // /tmp/yu-<id>/
	cmdDaemon    *cmdproxy.Daemon
	watcher      *snapshot.Watcher
	cloudSession *cloud.Session
	apiProxy   *netproxy.APIProxy
	apiAddr    string
	dummyKeys  map[string]string // env var name → dummy value
}

// New creates a new sandbox instance.
func New(projectDir string, command []string, cfg *config.Config) (*Sandbox, error) {
	id := randomID()
	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("yu-%s", id))

	// Auto-inject bypass flags for known agents
	command = injectAgentFlags(command)

	return &Sandbox{
		ID:         id,
		ProjectDir: projectDir,
		Command:    command,
		Config:     cfg,
		TmpDir:     tmpDir,
	}, nil
}

// Run sets up the sandbox and executes the command.
func (s *Sandbox) Run() error {
	// Create sandbox temp directories
	if err := s.setup(); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	defer s.cleanup()

	// Init log file — all yu output goes here, not stdout
	initLog(s.TmpDir)
	defer closeLog()

	// Handle signals for graceful cleanup
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		s.cleanup()
		os.Exit(1)
	}()

	yuLogStderr("Config: %s", config.WorkspaceDir(s.ProjectDir))
	yuLogStderr("Starting sandbox %s", s.ID)

	// Start API proxy
	s.apiProxy = netproxy.NewAPIProxy()
	s.apiProxy.SetAuditFunc(func(method, url string, status int, note string) {
		yuLog("[api] %s %s → %d %s", method, url, status, note)
	})

	var err error
	s.apiAddr, err = s.apiProxy.Start()
	if err != nil {
		return fmt.Errorf("starting API proxy: %w", err)
	}
	defer s.apiProxy.Stop()
	yuLogStderr("API proxy on %s", s.apiAddr)

	s.configureKeyReplacements()

	// Scan for large directories and prompt user
	snapCfg := s.Config.Snapshot
	newExcludes := snapshot.ScanAndPrompt(s.ProjectDir, snapCfg.SizeThresholdMB, snapCfg.Exclude)
	if len(newExcludes) > 0 {
		snapCfg.Exclude = append(snapCfg.Exclude, newExcludes...)
		// Persist to config so we don't ask again
		config.SaveSnapshotExcludes(s.ProjectDir, snapCfg.Exclude)
	}
	excludeSet := snapshot.BuildExcludeSet(s.ProjectDir, snapCfg.Exclude)

	// Start snapshot watcher
	yuLogStderr("Starting snapshot watcher")
	snapper := snapshot.New(s.ProjectDir, snapCfg.Keep, excludeSet)
	s.watcher = snapshot.NewWatcher(snapper, snapCfg.QuietSeconds, snapCfg.FileThreshold, yuLog)
	if err := s.watcher.Start(); err != nil {
		yuLog("Warning: snapshot watcher failed: %v", err)
	}
	defer s.watcher.Stop()

	// Start command proxy daemon
	yuLogStderr("Starting command proxy")
	socketPath := filepath.Join(s.TmpDir, "cmdproxy.sock")
	s.cmdDaemon = cmdproxy.NewDaemon(socketPath, s.Config.Credentials, s.ProjectDir, s.TmpDir)
	s.cmdDaemon.PreCommandHook = s.watcher.PreCommand
	if err := s.cmdDaemon.Start(); err != nil {
		return fmt.Errorf("starting command proxy: %w", err)
	}
	defer s.cmdDaemon.Stop()

	// Generate shims
	shimsDir := filepath.Join(s.TmpDir, "shims")
	yuBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}
	if err := cmdproxy.GenerateShims(shimsDir, socketPath, yuBin, s.Config.Commands.Intercept); err != nil {
		return fmt.Errorf("generating shims: %w", err)
	}

	// Connect to Cloud if paired
	if cloudCfg, err := cloud.LoadConfig(); err == nil {
		agent := ""
		if len(s.Command) > 0 {
			agent = filepath.Base(s.Command[0])
		}
		session, err := cloud.StartSession(cloudCfg, agent, s.ProjectDir)
		if err != nil {
			yuLog("Cloud session failed: %v", err)
		} else {
			if err := session.Connect(); err != nil {
				yuLog("Cloud WebSocket failed: %v", err)
			} else {
				s.cloudSession = session
				defer session.Close(cloudCfg)
				yuLogStderr("Connected to Yu Cloud (session %s)", session.SessionID[:8])
			}
		}
	}

	yuLogStderr("Launching: %s", strings.Join(s.Command, " "))

	// Launch the process
	exitCode, err := s.launch()
	if err != nil {
		return err
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func (s *Sandbox) setup() error {
	dirs := []string{
		s.TmpDir,
		filepath.Join(s.TmpDir, "home"),
		filepath.Join(s.TmpDir, "tmp"),
		filepath.Join(s.TmpDir, "shims"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}

	// Symlink agent config dirs and files from real HOME into sandbox HOME
	realHome, _ := os.UserHomeDir()
	fakeHome := filepath.Join(s.TmpDir, "home")
	info := s.agentInfo()
	if info != nil {
		for _, dir := range info.ConfigDirs {
			src := filepath.Join(realHome, dir)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			dst := filepath.Join(fakeHome, dir)
			os.MkdirAll(filepath.Dir(dst), 0700)
			os.Symlink(src, dst)
		}
		for _, file := range info.ConfigFiles {
			src := filepath.Join(realHome, file)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			dst := filepath.Join(fakeHome, file)
			os.MkdirAll(filepath.Dir(dst), 0700)
			os.Symlink(src, dst)
		}
	}

	return nil
}

// agentInfo returns the agentInfo for the detected agent, or nil.
func (s *Sandbox) agentInfo() *agentInfo {
	if len(s.Command) == 0 {
		return nil
	}
	base := filepath.Base(s.Command[0])
	if info, ok := knownAgents[base]; ok {
		return &info
	}
	return nil
}

func (s *Sandbox) cleanup() {
	if s.cmdDaemon != nil {
		s.cmdDaemon.Stop()
	}

	// Check for unsaved data in fake HOME that would be lost
	s.checkOrphanedFiles()

	yuLog("Sandbox %s cleaned up", s.ID)
	os.RemoveAll(s.TmpDir)
}

// checkOrphanedFiles warns about files in fake HOME that aren't symlinks.
// These were created by the agent but will be lost when the sandbox exits.
func (s *Sandbox) checkOrphanedFiles() {
	fakeHome := filepath.Join(s.TmpDir, "home")
	entries, err := os.ReadDir(fakeHome)
	if err != nil {
		return
	}

	var orphaned []string
	for _, e := range entries {
		path := filepath.Join(fakeHome, e.Name())
		// If it's a symlink, it points to real storage — safe
		if target, err := os.Readlink(path); err == nil && target != "" {
			continue
		}
		orphaned = append(orphaned, e.Name())
	}

	if len(orphaned) > 0 {
		yuLogStderr("Warning: files created in sandbox HOME will be lost: %s", strings.Join(orphaned, ", "))
		yuLogStderr("Consider adding these to the agent's ConfigDirs/ConfigFiles in yu")
	}
}

// injectAgentFlags adds bypass flags for known agents.
func injectAgentFlags(command []string) []string {
	if len(command) == 0 {
		return command
	}
	base := filepath.Base(command[0])
	info, ok := knownAgents[base]
	if !ok {
		return command
	}
	existing := strings.Join(command, " ")
	var toInsert []string
	for _, f := range info.BypassFlags {
		if !strings.Contains(existing, f) {
			toInsert = append(toInsert, f)
		}
	}
	if len(toInsert) == 0 {
		return command
	}
	if info.FlagsAtEnd {
		// Append at end (for CLIs where flags go after subcommand)
		return append(command, toInsert...)
	}
	// Insert after command[0]
	result := make([]string, 0, len(command)+len(toInsert))
	result = append(result, command[0])
	result = append(result, toInsert...)
	result = append(result, command[1:]...)
	return result
}

// agentAPIRoute groups one or more key env vars that share the same BASE_URL.
type agentAPIRoute struct {
	KeyEnvs      []string // env vars holding API keys
	BaseEnv      string   // env var holding custom base URL
	DefaultURL   string   // default upstream if base URL not set
	PathPrefix   string   // local API proxy path
	HeaderName   string   // HTTP header to force-set with the real key
	BearerPrefix bool     // if true, prepend "Bearer " to the key value
}

var agentAPIRoutes = []agentAPIRoute{
	{
		KeyEnvs:    []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
		BaseEnv:    "ANTHROPIC_BASE_URL",
		DefaultURL: "https://api.anthropic.com",
		PathPrefix: "/anthropic",
		HeaderName: "x-api-key",
	},
	{
		KeyEnvs:      []string{"OPENAI_API_KEY"},
		BaseEnv:      "OPENAI_BASE_URL",
		DefaultURL:   "https://api.openai.com",
		PathPrefix:   "/openai",
		HeaderName:   "Authorization",
		BearerPrefix: true,
	},
	{
		KeyEnvs:    []string{"GOOGLE_API_KEY"},
		BaseEnv:    "GOOGLE_API_BASE_URL",
		DefaultURL: "https://generativelanguage.googleapis.com",
		PathPrefix: "/google",
		HeaderName: "x-goog-api-key",
	},
	{
		KeyEnvs:    []string{"GEMINI_API_KEY"},
		BaseEnv:    "GEMINI_BASE_URL",
		DefaultURL: "https://generativelanguage.googleapis.com",
		PathPrefix: "/gemini",
		HeaderName: "x-goog-api-key",
	},
}

// configureKeyReplacements sets up local API proxy routes for agent API keys.
func (s *Sandbox) configureKeyReplacements() {
	s.dummyKeys = make(map[string]string)

	for _, route := range agentAPIRoutes {
		// Collect all key replacements for this route
		var replacements []netproxy.KeyReplacement
		var keyNames []string
		for _, keyEnv := range route.KeyEnvs {
			// Check environment first, then .yu/env credentials
			realKey := os.Getenv(keyEnv)
			if realKey == "" {
				realKey = s.Config.Credentials[keyEnv]
			}
			if realKey == "" {
				continue
			}
			dummy := fmt.Sprintf("yu-%s-%s", strings.ToLower(keyEnv), randomID())
			s.dummyKeys[keyEnv] = dummy
			replacements = append(replacements, netproxy.KeyReplacement{
				Dummy: dummy,
				Real:  realKey,
			})
			keyNames = append(keyNames, keyEnv)
		}

		if len(replacements) == 0 {
			continue
		}

		// Determine upstream: custom BASE_URL → default URL (built-in agent only)
		// External agents only get proxied when user has set a custom BASE_URL,
		// so they can use their own OAuth on the official endpoint.
		// The built-in agent always goes through the proxy.
		customBase := os.Getenv(route.BaseEnv)
		upstream := customBase
		if upstream == "" {
			if !s.isBuiltinAgent() {
				continue
			}
			upstream = route.DefaultURL
		}

		// Build force header: use the first real key found
		forceHeaders := map[string]string{}
		realKey := replacements[0].Real
		if route.BearerPrefix {
			forceHeaders[route.HeaderName] = "Bearer " + realKey
		} else {
			forceHeaders[route.HeaderName] = realKey
		}

		// Register API proxy route
		s.apiProxy.Routes = append(s.apiProxy.Routes, netproxy.APIRoute{
			PathPrefix:      route.PathPrefix,
			Upstream:        upstream,
			KeyReplacements: replacements,
			ForceHeaders:    forceHeaders,
		})

		// Override BASE_URL to point to local API proxy
		s.dummyKeys[route.BaseEnv] = fmt.Sprintf("http://%s%s", s.apiAddr, route.PathPrefix)

		yuLog("API proxy: %s → %s%s → %s",
			strings.Join(keyNames, ","), s.apiAddr, route.PathPrefix, upstream)
	}

	// User-configured inject rules from .yu/config.yaml
	for _, rule := range s.Config.Network.Inject {
		if rule.Upstream == "" || rule.Path == "" {
			continue
		}
		// Expand ${ENV_VAR} references in header values using .yu/env credentials
		expandedHeaders := make(map[string]string)
		for k, v := range rule.Headers {
			expanded := os.Expand(v, func(key string) string {
				if val, ok := s.Config.Credentials[key]; ok {
					return val
				}
				return os.Getenv(key)
			})
			expandedHeaders[k] = expanded
		}

		s.apiProxy.Routes = append(s.apiProxy.Routes, netproxy.APIRoute{
			PathPrefix:   rule.Path,
			Upstream:     rule.Upstream,
			ForceHeaders: expandedHeaders,
		})

		yuLog("API proxy (config): %s → %s%s → %s",
			rule.Path, s.apiAddr, rule.Path, rule.Upstream)
	}
}

// isBuiltinAgent returns true if the command is the built-in yu agent-loop.
func (s *Sandbox) isBuiltinAgent() bool {
	if len(s.Command) < 2 {
		return false
	}
	base := filepath.Base(s.Command[0])
	return base == "yu" && s.Command[1] == "agent-loop"
}

func randomID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
