package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/taoai/yu/internal/cmdproxy"
	"github.com/taoai/yu/internal/config"
	"github.com/taoai/yu/internal/sandbox"
	"github.com/taoai/yu/internal/snapshot"

	"github.com/spf13/cobra"
)

// Known AI coding agent CLIs.
var knownAgentCLIs = []struct {
	Name    string
	Binary  string
	Display string
}{
	{"claude", "claude", "Claude Code"},
	{"codex", "codex", "Codex"},
	{"gemini", "gemini", "Gemini CLI"},
}

func main() {
	var (
		configFile string
		servePort  int
		allowNet   []string
	)

	rootCmd := &cobra.Command{
		Use:   "yu <dir> [-- <command...>]",
		Short: "Secure sandbox for AI agents",
		Long:  "Yu runs AI agents in a sandbox with credential isolation, auto-bypass permissions, and auto-snapshot.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			dashIdx := cmd.ArgsLenAtDash()

			var command []string
			if dashIdx >= 0 {
				command = args[dashIdx:]
				if len(command) == 0 {
					return fmt.Errorf("no command specified after --")
				}
			} else {
				var err error
				command, err = detectAndPrompt()
				if err != nil {
					return err
				}
			}

			absDir, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("resolving directory: %w", err)
			}
			if info, err := os.Stat(absDir); err != nil || !info.IsDir() {
				return fmt.Errorf("not a directory: %s", absDir)
			}

			cfg, err := config.Load(absDir, configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if len(allowNet) > 0 {
				cfg.Network.AllowExtra = append(cfg.Network.AllowExtra, allowNet...)
			}
			if servePort > 0 {
				cfg.Server.Port = servePort
			}

			sb, err := sandbox.New(absDir, command, cfg)
			if err != nil {
				return fmt.Errorf("creating sandbox: %w", err)
			}
			return sb.Run()
		},
	}

	rootCmd.Flags().StringVarP(&configFile, "config", "c", "", "config file (default: .yu/config.yaml)")
	rootCmd.Flags().IntVar(&servePort, "serve", 0, "enable server mode on this port")
	rootCmd.Flags().StringSliceVar(&allowNet, "allow-net", nil, "additional allowed network domains")

	rootCmd.AddCommand(configCmd())
	rootCmd.AddCommand(snapshotsCmd())
	rootCmd.AddCommand(rollbackCmd())
	rootCmd.AddCommand(shimCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// detectAndPrompt scans for installed agent CLIs and asks the user which to run.
func detectAndPrompt() ([]string, error) {
	type found struct {
		binary  string
		display string
		path    string
	}
	var available []found

	for _, agent := range knownAgentCLIs {
		if path, err := exec.LookPath(agent.Binary); err == nil {
			available = append(available, found{
				binary:  agent.Binary,
				display: agent.Display,
				path:    path,
			})
		}
	}

	if len(available) == 0 {
		return nil, fmt.Errorf("no AI agents found in PATH.\nInstall one of: claude, codex, gemini\nOr specify a command: yu . -- <command>")
	}

	// Only one agent — just use it
	if len(available) == 1 {
		fmt.Printf("[yu] Detected %s (%s)\n", available[0].display, available[0].path)
		return []string{available[0].binary}, nil
	}

	fmt.Println("[yu] Detected agents:")
	for i, a := range available {
		fmt.Printf("  %d) %s (%s)\n", i+1, a.display, a.path)
	}
	fmt.Println()
	fmt.Print("Choose [1]: ")

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	choice := 1
	if input != "" {
		var err error
		choice, err = strconv.Atoi(input)
		if err != nil || choice < 1 || choice > len(available) {
			return nil, fmt.Errorf("invalid choice: %s", input)
		}
	}

	return []string{available[choice-1].binary}, nil
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "init [dir]",
		Short: "Initialize .yu/ in directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.Init(resolveDir(args))
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list [dir]",
		Short: "Show merged configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(resolveDir(args), "")
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

	// inject subcommand
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

			return config.AddInjectRule(".", upstream, path, headerMap)
		},
	}
	injectCmd.Flags().String("upstream", "", "upstream URL (e.g. https://api.company.com)")
	injectCmd.Flags().String("path", "", "local proxy path prefix (e.g. /company-api)")
	injectCmd.Flags().StringSlice("header", nil, "header to inject (e.g. \"Authorization: Bearer ${TOKEN}\")")
	cmd.AddCommand(injectCmd)

	// inject-rm subcommand
	cmd.AddCommand(&cobra.Command{
		Use:   "inject-rm <path>",
		Short: "Remove an inject rule by path prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.RemoveInjectRule(".", args[0])
		},
	})

	return cmd
}

func snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots [dir]",
		Short: "List available snapshots",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := resolveDir(args)
			cfg, _ := config.Load(dir, "")
			s := snapshot.New(dir, cfg.Snapshot.Keep)
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

func rollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <id> [dir]",
		Short: "Rollback to a snapshot",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid snapshot ID: %s", args[0])
			}
			dir := resolveDir(args[1:])
			cfg, _ := config.Load(dir, "")
			s := snapshot.New(dir, cfg.Snapshot.Keep)
			if err := s.Rollback(id); err != nil {
				return err
			}
			fmt.Printf("Rolled back to snapshot #%d\n", id)
			return nil
		},
	}
}

// resolveDir returns the directory from args[0] or cwd if not provided.
func resolveDir(args []string) string {
	if len(args) > 0 && args[0] != "" {
		abs, err := filepath.Abs(args[0])
		if err == nil {
			return abs
		}
		return args[0]
	}
	dir, _ := os.Getwd()
	return dir
}
