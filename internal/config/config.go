package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the merged configuration from global + project .yu/
type Config struct {
	Network  NetworkConfig  `yaml:"network"`
	Snapshot SnapshotConfig `yaml:"snapshot"`
	Server   ServerConfig   `yaml:"server"`
	Commands CommandsConfig `yaml:"commands"`
	Env      EnvConfig      `yaml:"env"`

	// Credentials holds key-value pairs loaded from .yu/env
	// Injected into the command proxy executor, never into the sandbox.
	Credentials map[string]string `yaml:"-"`

	// ProjectDir is the resolved project directory path.
	ProjectDir string `yaml:"-"`
}

type EnvConfig struct {
	// Keep is a list of extra env var names to pass through to the sandbox
	// (in addition to the built-in whitelist).
	Keep []string `yaml:"keep"`
}

type NetworkConfig struct {
	Inject     []InjectRule `yaml:"inject"`
	Deny       []string     `yaml:"deny"`
	Allow      []string     `yaml:"allow"`
	AllowExtra []string     `yaml:"-"` // from CLI flags
}

type InjectRule struct {
	Upstream string            `yaml:"upstream"` // real target URL, e.g. "https://internal-api.company.com"
	Path     string            `yaml:"path"`     // local proxy path prefix, e.g. "/company-api"
	Headers  map[string]string `yaml:"headers"`  // headers to force-set (supports ${ENV_VAR} expansion)
}

type SnapshotConfig struct {
	Keep         int `yaml:"keep"`
	QuietSeconds int `yaml:"quiet_seconds"`
	FileThreshold int `yaml:"file_threshold"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type CommandsConfig struct {
	Intercept []string `yaml:"intercept"` // command names to intercept, e.g. ["git", "ssh", "gh"]
}

// Defaults returns a Config with sensible defaults.
func Defaults() *Config {
	return &Config{
		Snapshot: SnapshotConfig{
			Keep:          10,
			QuietSeconds:  15,
			FileThreshold: 50,
		},
		Commands: CommandsConfig{
			Intercept: []string{"git", "ssh", "gh", "aws", "scp"},
		},
		Credentials: make(map[string]string),
	}
}

// Load merges global (~/.config/yu/) and project (.yu/) config.
func Load(projectDir string, configFile string) (*Config, error) {
	cfg := Defaults()
	cfg.ProjectDir = projectDir

	// Load global config
	globalDir := globalConfigDir()
	loadYAML(filepath.Join(globalDir, "config.yaml"), cfg)
	globalEnv, _ := loadEnvFile(filepath.Join(globalDir, "env"))
	for k, v := range globalEnv {
		cfg.Credentials[k] = v
	}

	// Load project config
	projectYuDir := filepath.Join(projectDir, ".yu")
	if configFile != "" {
		loadYAML(configFile, cfg)
	} else {
		loadYAML(filepath.Join(projectYuDir, "config.yaml"), cfg)
	}
	projectEnv, _ := loadEnvFile(filepath.Join(projectYuDir, "env"))
	for k, v := range projectEnv {
		cfg.Credentials[k] = v
	}

	return cfg, nil
}

// Init creates .yu/ directory with template files in the given directory.
func Init(dir string) error {
	yuDir := filepath.Join(dir, ".yu")
	if err := os.MkdirAll(yuDir, 0700); err != nil {
		return fmt.Errorf("creating .yu/: %w", err)
	}

	// Template config.yaml
	configPath := filepath.Join(yuDir, "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		template := `# Yu configuration
# See: https://github.com/taoai/yu

snapshot:
  keep: 10
  quiet_seconds: 15
  file_threshold: 50

# API proxy routes — inject credentials for specific endpoints
# Agent traffic to these is routed through a local proxy that adds headers.
# Header values support ${VAR} expansion from .yu/env.
# network:
#   inject:
#     - upstream: "https://internal-api.company.com"
#       path: "/company-api"
#       headers:
#         Authorization: "Bearer ${COMPANY_API_TOKEN}"

# Commands to intercept (defaults: git, ssh, gh, aws, scp)
# commands:
#   intercept: [git, ssh, gh, aws, scp]
`
		if err := os.WriteFile(configPath, []byte(template), 0600); err != nil {
			return fmt.Errorf("writing config.yaml: %w", err)
		}
	}

	// Template env file
	envPath := filepath.Join(yuDir, "env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		template := `# Yu credentials — injected into command proxy executor, never into sandbox
# Standard dotenv format: KEY=VALUE
#
# GIT_SSH_COMMAND=ssh -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes
# GH_TOKEN=ghp_xxxxx
# AWS_ACCESS_KEY_ID=AKIAxxxxx
# AWS_SECRET_ACCESS_KEY=xxxxx
`
		if err := os.WriteFile(envPath, []byte(template), 0600); err != nil {
			return fmt.Errorf("writing env: %w", err)
		}
	}

	// Add to .gitignore if present
	addToGitignore(dir)

	fmt.Printf("Initialized .yu/ in %s\n", dir)
	return nil
}

// Set writes a key-value pair to the env file.
func Set(key, value string, global bool) error {
	var envPath string
	if global {
		dir := globalConfigDir()
		os.MkdirAll(dir, 0700)
		envPath = filepath.Join(dir, "env")
	} else {
		envPath = filepath.Join(".yu", "env")
		os.MkdirAll(".yu", 0700)
	}

	env, _ := loadEnvFile(envPath)
	if env == nil {
		env = make(map[string]string)
	}
	env[key] = value

	return writeEnvFile(envPath, env)
}

// AddInjectRule adds an inject rule to the project config.
func AddInjectRule(dir, upstream, path string, headers map[string]string) error {
	configPath := filepath.Join(dir, ".yu", "config.yaml")
	cfg := Defaults()
	loadYAML(configPath, cfg)

	// Check for duplicate path
	for _, rule := range cfg.Network.Inject {
		if rule.Path == path {
			return fmt.Errorf("inject rule for path %q already exists, remove it first with: yu config inject-rm %s", path, path)
		}
	}

	cfg.Network.Inject = append(cfg.Network.Inject, InjectRule{
		Upstream: upstream,
		Path:     path,
		Headers:  headers,
	})

	return writeYAML(configPath, cfg)
}

// RemoveInjectRule removes an inject rule by path prefix.
func RemoveInjectRule(dir, path string) error {
	configPath := filepath.Join(dir, ".yu", "config.yaml")
	cfg := Defaults()
	loadYAML(configPath, cfg)

	found := false
	var remaining []InjectRule
	for _, rule := range cfg.Network.Inject {
		if rule.Path == path {
			found = true
		} else {
			remaining = append(remaining, rule)
		}
	}
	if !found {
		return fmt.Errorf("no inject rule found for path %q", path)
	}

	cfg.Network.Inject = remaining
	return writeYAML(configPath, cfg)
}

func writeYAML(path string, cfg *Config) error {
	os.MkdirAll(filepath.Dir(path), 0700)
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Print outputs the merged config to stdout.
func (c *Config) Print() {
	out, _ := yaml.Marshal(c)
	fmt.Println(string(out))
	if len(c.Credentials) > 0 {
		fmt.Println("# Credentials (from .yu/env, injected into command proxy):")
		for k := range c.Credentials {
			fmt.Printf("#   %s=<set>\n", k)
		}
	}
	if len(c.Env.Keep) > 0 {
		fmt.Println("# Extra env vars passed to sandbox:")
		for _, k := range c.Env.Keep {
			fmt.Printf("#   %s\n", k)
		}
	}
}

func globalConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "yu")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "yu")
}

func loadYAML(path string, cfg *Config) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // file doesn't exist, skip
	}
	yaml.Unmarshal(data, cfg)
}

func addToGitignore(dir string) {
	gitignorePath := filepath.Join(dir, ".gitignore")
	content, _ := os.ReadFile(gitignorePath)

	// Check if .yu/ is already in .gitignore
	for _, line := range splitLines(string(content)) {
		if line == ".yu/" || line == ".yu" {
			return
		}
	}

	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	if len(content) > 0 && content[len(content)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString(".yu/\n")
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
