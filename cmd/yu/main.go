package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/taoai/yu/internal/agent"
	"github.com/taoai/yu/internal/cloud"
	"github.com/taoai/yu/internal/cmdproxy"
	"github.com/taoai/yu/internal/config"
	"github.com/taoai/yu/internal/copilot"
	"github.com/taoai/yu/internal/sandbox"
	"github.com/taoai/yu/internal/snapshot"

	"github.com/spf13/cobra"
)

var version = "dev"

// Global project directory flag — resolved before any subcommand runs.
var projectDir string

func main() {
	rootCmd := &cobra.Command{
		Use:   "yu",
		Short: "Fast AI coding agent with built-in sandbox",
		Long:  "Yu is a fast AI coding agent. Use 'yu agent' for the built-in agent, 'yu wrap' to sandbox external agents.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Skip path resolution for hidden/internal commands
			if cmd.Name() == "shim" || cmd.Name() == "agent-loop" {
				return nil
			}
			return resolveProjectDir()
		},
	}

	// Global flags available to ALL subcommands
	rootCmd.PersistentFlags().StringVarP(&projectDir, "path", "C", "", "project directory (default: current directory)")
	// -c as alias for -C
	rootCmd.PersistentFlags().StringVarP(&projectDir, "directory", "c", "", "project directory (alias for -C)")

	rootCmd.AddCommand(agentCmd())
	rootCmd.AddCommand(wrapCmd())
	rootCmd.AddCommand(configCmd())
	rootCmd.AddCommand(snapshotsCmd())
	rootCmd.AddCommand(rollbackCmd())
	rootCmd.AddCommand(shimCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(pairCmd())
	rootCmd.AddCommand(agentLoopCmd())
	rootCmd.AddCommand(githubCopilotCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func resolveProjectDir() error {
	if projectDir == "" {
		var err error
		projectDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getting current directory: %w", err)
		}
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("resolving directory: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	projectDir = abs
	return nil
}

// --- yu agent ---

func agentCmd() *cobra.Command {
	var (
		modelFlag    string
		providerFlag string
		sessionFlag  string
		newSession   bool
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Start the built-in AI agent (interactive)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(modelFlag, providerFlag, sessionFlag, newSession, "")
		},
	}

	cmd.Flags().StringVarP(&modelFlag, "model", "m", "", "model to use")
	cmd.Flags().StringVarP(&providerFlag, "provider", "p", "", "provider (anthropic/openai/custom)")
	cmd.Flags().StringVarP(&sessionFlag, "session", "s", "", "resume specific session ID")
	cmd.Flags().BoolVar(&newSession, "new", false, "force new session")

	// yu agent exec "prompt"
	execCmd := &cobra.Command{
		Use:   "exec [prompt]",
		Short: "Execute a one-shot prompt and exit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := ""
			if len(args) > 0 {
				prompt = args[0]
			}

			// Read from file
			fileFlag, _ := cmd.Flags().GetString("file")
			if fileFlag != "" {
				data, err := os.ReadFile(fileFlag)
				if err != nil {
					return fmt.Errorf("reading prompt file: %w", err)
				}
				prompt = string(data)
			}

			// Read from stdin if no prompt and not a terminal
			if prompt == "" {
				stat, _ := os.Stdin.Stat()
				if stat.Mode()&os.ModeCharDevice == 0 {
					data, err := os.ReadFile("/dev/stdin")
					if err != nil {
						return fmt.Errorf("reading stdin: %w", err)
					}
					prompt = strings.TrimSpace(string(data))
				}
			}

			if prompt == "" {
				return fmt.Errorf("no prompt provided. Usage: yu agent exec \"prompt\" or echo \"prompt\" | yu agent exec")
			}

			return runAgent(modelFlag, providerFlag, "", true, prompt)
		},
	}
	execCmd.Flags().StringP("file", "f", "", "read prompt from file")
	execCmd.Flags().StringVarP(&modelFlag, "model", "m", "", "model to use")
	execCmd.Flags().StringVarP(&providerFlag, "provider", "p", "", "provider")

	cmd.AddCommand(execCmd)
	return cmd
}

func runAgent(model, provider, session string, newSession bool, execPrompt string) error {
	cfg, err := config.Load(projectDir, "")
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Build agent-loop command with flags
	yuBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	command := []string{yuBin, "agent-loop"}

	// Pass config via environment
	if model != "" {
		os.Setenv("YU_MODEL", model)
	} else if cfg.Agent.Model != "" {
		os.Setenv("YU_MODEL", cfg.Agent.Model)
	}
	if provider != "" {
		os.Setenv("YU_PROVIDER", provider)
	}
	if session != "" {
		os.Setenv("YU_SESSION", session)
	}
	if newSession {
		os.Setenv("YU_NEW_SESSION", "1")
	}
	if execPrompt != "" {
		os.Setenv("YU_EXEC_PROMPT", execPrompt)
	}

	sb, err := sandbox.New(projectDir, command, cfg)
	if err != nil {
		return fmt.Errorf("creating sandbox: %w", err)
	}
	return sb.Run()
}

// --- yu wrap ---

func wrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "wrap <command> [args...]",
		Short:              "Run an external agent in the sandbox",
		Long:               "Wraps Claude Code, Codex, Gemini CLI, or any command in Yu's sandbox.",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(projectDir, "")
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			sb, err := sandbox.New(projectDir, args, cfg)
			if err != nil {
				return fmt.Errorf("creating sandbox: %w", err)
			}
			return sb.Run()
		},
	}
}

// --- yu agent-loop (hidden, spawned by sandbox) ---

func agentLoopCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "agent-loop",
		Hidden: true,
		Run: func(cmd *cobra.Command, args []string) {
			agent.Main()
		},
	}
}

// --- yu config ---

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Initialize workspace config",
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.Init(projectDir)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show merged configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(projectDir, "")
			if err != nil {
				return err
			}
			cfg.Print()
			return nil
		},
	})

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			global, _ := cmd.Flags().GetBool("global")
			return config.Set(args[0], args[1], global)
		},
	}
	setCmd.Flags().Bool("global", false, "set in global config (~/.config/yu/)")
	cmd.AddCommand(setCmd)

	injectCmd := &cobra.Command{
		Use:   "inject",
		Short: "Add an API proxy inject rule",
		RunE: func(cmd *cobra.Command, args []string) error {
			upstream, _ := cmd.Flags().GetString("upstream")
			path, _ := cmd.Flags().GetString("path")
			headers, _ := cmd.Flags().GetStringSlice("header")

			if upstream == "" || path == "" {
				return fmt.Errorf("--upstream and --path are required")
			}

			headerMap := make(map[string]string)
			for _, h := range headers {
				parts := strings.SplitN(h, ":", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid header format %q, expected Key: Value", h)
				}
				headerMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}

			return config.AddInjectRule(projectDir, upstream, path, headerMap)
		},
	}
	injectCmd.Flags().String("upstream", "", "upstream URL")
	injectCmd.Flags().String("path", "", "local proxy path prefix")
	injectCmd.Flags().StringSlice("header", nil, "header to inject")
	cmd.AddCommand(injectCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "inject-rm <path>",
		Short: "Remove an inject rule by path prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.RemoveInjectRule(projectDir, args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "intercept-add <command>",
		Short: "Add a command to the proxy intercept list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.AddIntercept(projectDir, args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "intercept-rm <command>",
		Short: "Remove a command from the proxy intercept list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.RemoveIntercept(projectDir, args[0])
		},
	})

	return cmd
}

// --- yu snapshots ---

func snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots",
		Short: "List available snapshots",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.Load(projectDir, "")
			s := snapshot.New(projectDir, cfg.Snapshot.Keep, nil)
			snaps := s.List()
			if len(snaps) == 0 {
				fmt.Println("No snapshots found.")
				return nil
			}
			for _, snap := range snaps {
				summary := snap.Summary
				if summary == "" {
					summary = "-"
				}
				fmt.Printf("#%-3d %s  %-20s %s\n", snap.ID, snap.CreatedAt.Format("15:04:05"), "["+snap.Trigger+"]", summary)
			}
			return nil
		},
	}
}

// --- yu rollback ---

func rollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <id>",
		Short: "Rollback to a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid snapshot ID: %s", args[0])
			}
			cfg, _ := config.Load(projectDir, "")
			s := snapshot.New(projectDir, cfg.Snapshot.Keep, nil)
			if err := s.Rollback(id); err != nil {
				return err
			}
			fmt.Printf("Rolled back to snapshot #%d\n", id)
			return nil
		},
	}
}

// --- other commands ---

func shimCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "shim",
		Hidden:             true,
		DisableFlagParsing: true,
		Run: func(cmd *cobra.Command, args []string) {
			cmdproxy.RunShim(args)
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print yu version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("yu " + version)
		},
	}
}

func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair",
		Short: "Pair this machine with the Yu app",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cloud.Pair()
		},
	}
}

func githubCopilotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github-copilot",
		Short: "Manage GitHub Copilot authentication",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Log in to GitHub Copilot via OAuth device flow",
		RunE: func(cmd *cobra.Command, args []string) error {
			if copilot.IsLoggedIn() {
				user, err := copilot.ValidateToken()
				if err == nil {
					fmt.Printf("Already logged in as %s.\n", user)
					return nil
				}
				fmt.Println("Token expired, re-authenticating...")
			}

			user, err := copilot.Login(func(s string) { fmt.Print(s) })
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}
			fmt.Printf("\n✓ Logged in as %s\n", user)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "logout",
		Short: "Log out of GitHub Copilot",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !copilot.IsLoggedIn() {
				fmt.Println("Not logged in.")
				return nil
			}
			if err := copilot.Logout(); err != nil {
				return err
			}
			fmt.Println("Logged out.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show GitHub Copilot authentication status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !copilot.IsLoggedIn() {
				fmt.Println("Not logged in. Run: yu github-copilot login")
				return nil
			}
			user, err := copilot.ValidateToken()
			if err != nil {
				fmt.Printf("Token invalid: %v\n", err)
				return nil
			}
			fmt.Printf("Logged in as %s (Copilot active)\n", user)
			return nil
		},
	})

	return cmd
}
