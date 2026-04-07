# Yu

**Your AI agent can `git push`, but it has never seen your SSH key.**

Yu is a sandbox for AI coding agents (Claude Code, Codex, Gemini CLI). It lets agents use credentials without holding them — structurally unreachable, not just hidden behind prompts. No permission popups. Auto-snapshot for fearless rollback.

> Requires Apple Silicon Mac (macOS). Linux support planned.

## Install

```bash
brew install go fswatch  # prerequisites
go install github.com/qingant/yu/cmd/yu@latest
```

Or build from source:

```bash
git clone https://github.com/qingant/yu.git
cd yu
go build -o yu ./cmd/yu/
cp yu /usr/local/bin/
```

## Quick Start

```bash
yu . -- claude          # run Claude Code in sandbox
yu . -- codex exec "prompt"  # run Codex in sandbox
yu .                    # auto-detect installed agents
yu . -- bash            # test the sandbox yourself
```

## What Yu Protects

| Attack | Without Yu | With Yu |
|---|---|---|
| Malicious package reads `~/.ssh/id_ed25519` | Key stolen | **Blocked** — `~/.ssh` invisible to agent |
| Compromised MCP server reads `AWS_SECRET_ACCESS_KEY` | Credential leaked | **Blocked** — env var doesn't exist in sandbox |
| Agent sends API key to attacker's server | Real key exfiltrated | **Blocked** — agent only has a dummy key |
| Prompt injection tries `cat ~/.aws/credentials` | File contents exposed | **Blocked** — file doesn't exist in agent's world |
| Supply chain attack exfiltrates `GH_TOKEN` | Token stolen | **Blocked** — env var stripped, git works through credential proxy |
| Agent breaks your code | Manual recovery | **Auto-rollback** — snapshot before every risky operation |
| Permission popup fatigue ("Allow?" x100) | You click Allow reflexively | **Eliminated** — sandbox is the security, agent runs unrestricted inside |

## How It Works

### Filesystem Isolation
macOS `sandbox-exec` enforces kernel-level restrictions. Agent sees the project directory at its real path — no translation, no container. Everything else (`~/.ssh`, `~/.aws`, `~/.gnupg`, other projects) is invisible.

### Environment Whitelist
Default-deny model. Only explicitly allowed env vars enter the sandbox. Secret-looking vars (containing `KEY`, `TOKEN`, `SECRET`, `PASSWORD`) get unique dummy values. Agent config vars (`ANTHROPIC_BASE_URL`, `OPENAI_ORG_ID`) pass through.

### API Key Proxy
When you use a custom `BASE_URL` (e.g., a LiteLLM proxy), Yu routes agent API calls through a localhost reverse proxy that swaps dummy keys for real ones. No MITM, no TLS certificates, no `NODE_TLS_REJECT_UNAUTHORIZED`. Supports HTTP and WebSocket.

### Command Proxy
Sensitive commands (`git`, `ssh`, `gh`, `aws`, `scp`) are intercepted by shims in `PATH`. The real command executes outside the sandbox with credentials from `.yu/env` injected. Agent never touches credential files.

### Auto-Bypass Permissions
Yu launches agents with permission bypass flags because the sandbox is the security boundary:
- Claude Code: `--dangerously-skip-permissions`
- Codex: `--dangerously-bypass-approvals-and-sandbox`

## Auto-Snapshot

Yu takes filesystem snapshots automatically, driven by agent behavior — no timers:

| Trigger | When |
|---|---|
| **Before risky commands** | Agent is about to `git push`, `ssh`, `gh deploy` |
| **On quiet periods** | No file writes for ~15 seconds (agent between tasks) |
| **On change threshold** | 50+ files changed since last snapshot |

Snapshots use APFS clone — instant, copy-on-write, zero overhead until files change.

```bash
$ yu snapshots
#0   22:17:31  [init]               baseline
#1   22:18:05  [quiet]              3 files: src/main.go, README.md, +1 more
#2   22:19:30  [pre-command:git]    1 files: src/main.go

$ yu rollback 0    # restore to baseline
```

## Configuration

```bash
yu config init                         # create .yu/ with templates
yu config set GIT_SSH_COMMAND "ssh -i ~/.ssh/id_ed25519"
yu config set GH_TOKEN ghp_xxxxx
yu config inject \                     # API proxy inject rule
  --upstream "https://internal-api.company.com" \
  --path "/company-api" \
  --header "Authorization: Bearer \${COMPANY_TOKEN}"
yu config list                         # show merged config
```

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
  keep: [MY_CUSTOM_VAR]
```

Config lives in `.yu/` (project) and `~/.config/yu/` (global). The `.yu/` directory is invisible to the agent and auto-added to `.gitignore`.

## Background

Yu implements the infrastructure layer described in the [Environment as a Service (EaaS)](https://github.com/qingant/yu/tree/main/../papers/) paper — specifically the proxy injection, executor sandboxing, and credential isolation patterns from Section 7.4.

## License

MIT
