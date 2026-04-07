# Yu

Secure sandbox for AI coding agents. Safer and faster than running bare.

Yu wraps Claude Code, Codex, Gemini CLI (or any command) in an isolated environment where credentials are structurally unreachable — not hidden behind prompts, but absent from the agent's world entirely.

## Why

AI agents run with full access to your credentials, filesystem, and network. Any compromised dependency can exfiltrate your SSH keys, API tokens, and cloud credentials. Meanwhile, permission prompts ("Allow this tool?") create the illusion of security while destroying flow.

Yu moves the security boundary from per-action prompts to environment-level isolation. The result is both **safer** (credentials are structurally unreachable) and **faster** (no permission prompts, auto-snapshot for rollback).

## Install

```bash
go install github.com/taoai/yu/cmd/yu@latest
```

Or build from source:

```bash
git clone https://github.com/taoai/yu.git
cd yu
go build -o yu ./cmd/yu/
```

## Quick Start

```bash
# Run Claude Code in a sandbox
yu . -- claude

# Run Codex
yu . -- codex exec "your prompt"

# Auto-detect installed agents
yu .

# Test the sandbox
yu . -- bash
```

Inside the sandbox:
- `~/.ssh`, `~/.aws`, `~/.gnupg` — invisible
- `SSH_AUTH_SOCK`, `GH_TOKEN`, `AWS_*` — stripped
- API keys (when using custom BASE_URL) — replaced with dummies, real keys injected by local proxy
- `git push` — works transparently through credential proxy
- Permission prompts — auto-bypassed (sandbox is the security)
- Snapshots — automatic, behavior-driven, rollback anytime

## How It Works

### Filesystem Isolation
macOS `sandbox-exec` enforces kernel-level restrictions. Agent sees the project directory at its real path. Everything else (home directory, credentials, other projects) is hidden.

### Environment Whitelist
Default-deny. Only explicitly allowed env vars enter the sandbox. Secrets (vars containing KEY, TOKEN, SECRET, PASSWORD) get unique dummy values.

### API Key Proxy
When you use a custom `BASE_URL` (e.g., a LiteLLM proxy), Yu routes agent API calls through a localhost reverse proxy that replaces dummy keys with real ones. No MITM, no certificates. Agent never sees real keys.

### Command Proxy
Sensitive commands (`git`, `ssh`, `gh`, `aws`, `scp`) are intercepted by shims. The real command executes outside the sandbox with credentials from `.yu/env` injected. Configurable via `commands.intercept`.

### Auto-Snapshot
Behavior-driven snapshots using APFS clone (instant, copy-on-write):
- Before proxied commands (git push, etc.)
- When file activity settles (~15s quiet)
- On change volume threshold

```bash
yu snapshots                    # list with change summaries
yu rollback 3                   # restore to snapshot #3
```

### Auto-Bypass Permissions
Yu launches agents with permission bypass flags because the sandbox is the security boundary:
- Claude Code: `--dangerously-skip-permissions`
- Codex: `--dangerously-bypass-approvals-and-sandbox`

## Configuration

```bash
# Initialize config in project
yu config init

# Set credentials (written to .yu/env)
yu config set GIT_SSH_COMMAND "ssh -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes"
yu config set GH_TOKEN ghp_xxxxx

# Add API proxy inject rule
yu config inject \
  --upstream "https://internal-api.company.com" \
  --path "/company-api" \
  --header "Authorization: Bearer \${COMPANY_TOKEN}"

# View merged config
yu config list
```

Config lives in `.yu/` (project-level) and `~/.config/yu/` (global). The `.yu/` directory is invisible to the agent and auto-added to `.gitignore`.

### `.yu/env`
Standard dotenv. Credentials injected into the command proxy executor, never into the sandbox.

### `.yu/config.yaml`
```yaml
snapshot:
  keep: 10
  quiet_seconds: 15
  file_threshold: 50

network:
  inject:
    - upstream: "https://internal-api.company.com"
      path: "/company-api"
      headers:
        Authorization: "Bearer ${COMPANY_TOKEN}"

commands:
  intercept: [git, ssh, gh, aws, scp]

env:
  keep: [MY_CUSTOM_VAR]  # extra env vars to pass through
```

## Platform Support

- **macOS**: Full support (sandbox-exec, APFS snapshots, fswatch)
- **Linux**: Planned (mount namespaces, overlayfs/btrfs, inotify)

## Background

Yu implements the infrastructure layer described in the [Environment as a Service (EaaS)](../papers/) paper — specifically the proxy injection, executor sandboxing, and credential isolation patterns from Section 7.4.

## License

MIT
