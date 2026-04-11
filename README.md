# Yu

**A fast AI coding agent with built-in sandbox.** 80% of Claude Code, 10x faster to start, zero config.

Yu can run as a standalone agent or sandbox existing agents (Claude Code, Codex, Gemini CLI). Single Go binary, no dependencies.

> Apple Silicon Mac only. Linux planned.

## Install

```bash
sudo curl -fsSL https://github.com/qingant/yu/releases/latest/download/yu-darwin-arm64 -o /usr/local/bin/yu && sudo chmod +x /usr/local/bin/yu
```

Update to latest:
```bash
yu update
```

<details>
<summary>Build from source</summary>

```bash
brew install go
git clone https://github.com/qingant/yu.git && cd yu
go build -o yu ./cmd/yu/
sudo mv yu /usr/local/bin/
```
</details>

## Quick Start

```bash
yu .                                 # built-in agent (default)
yu . -- claude                       # or wrap Claude Code
yu . -- codex exec "prompt"          # or wrap Codex
yu . --model gpt-5.4                 # use a specific model
```

The built-in agent gives you:

- **13 tools**: bash (streaming), file read/write/edit, search, poll, web fetch, background processes, image generation, interactive planning
- **Multi-provider**: Anthropic, OpenAI, or any OpenAI-compatible endpoint
- **Sessions**: auto-save, resume, `/compact` to compress context
- **Memory**: `/remember` persists notes across sessions
- **Snapshots**: auto-rollback with `/rollback`

## Agent Commands

```
/help              Show all commands
/model             Switch provider and model (interactive)
/rollback          Roll back to a previous snapshot
/compact           Compress conversation context
/sessions          List / resume past sessions
/init              Create Yu.md project instructions
/jobs              List background processes
/remember <text>   Save to cross-session memory

!<command>         Run shell command (output visible to model)
@<file>            Attach file to your message (tab to complete)
```

## Protection

Your agent can `git push`, but it has never seen your SSH key.

| Threat | Status |
|---|---|
| Package reads `~/.ssh/id_ed25519` | **Blocked** — invisible inside sandbox |
| MCP server reads `AWS_SECRET_ACCESS_KEY` | **Blocked** — env var doesn't exist |
| Agent exfiltrates API key | **Blocked** — only dummy keys in sandbox |
| Supply chain attack steals `GH_TOKEN` | **Blocked** — git works through credential proxy |
| Agent breaks your code | **Auto-rollback** — `/rollback` to any snapshot |
| Permission fatigue ("Allow?" x 100) | **Eliminated** — sandbox is the security boundary |

## Snapshots

Automatic, behavior-driven:

- Before risky commands (`git push`, `ssh`, `gh deploy`)
- When file activity settles (~15s quiet)
- On change volume threshold (50+ files)
- Skipped when nothing changed (no wasted space)

Uses APFS clone: instant, copy-on-write, near-zero overhead. Keeps 10 snapshots with time-bucketed retention (1 daily + 2 hourly + 7 recent).

```
$ yu snapshots
#0   22:17:31  [init]               baseline
#1   22:18:05  [quiet]              3 files: src/main.go, README.md, +1 more
#2   22:19:30  [pre-command:git]    1 files: src/main.go

$ yu rollback 1
```

Inside the agent, use `/rollback` for interactive selection.

## How It Works

**Built-in agent** runs as a separate process inside the sandbox — same isolation as external agents. Calls LLM APIs through the credential proxy. Tools execute inside sandbox-exec.

**Filesystem** — macOS `sandbox-exec` hides everything except the project directory. No containers.

**Env vars** — Default-deny whitelist. Secrets (`KEY`, `TOKEN`, `SECRET`, `PASSWORD`) get dummy values.

**API proxy** — Localhost reverse proxy swaps dummy keys for real ones. No MITM, no certificates.

**Command proxy** — `git`, `ssh`, `gh`, `aws` intercepted by shims. Real commands run outside sandbox with credentials from `.yu/env`.

**Permission bypass** — Agents launch with `--dangerously-skip-permissions` (Claude) / `--dangerously-bypass-approvals-and-sandbox` (Codex). The sandbox is the security boundary.

## Configuration

```bash
yu config init                     # create workspace config
yu config set GIT_SSH_COMMAND "ssh -i ~/.ssh/id_ed25519"
yu config set GH_TOKEN ghp_xxxxx
yu config set ANTHROPIC_AUTH_TOKEN sk-ant-xxxxx
yu config list                     # show merged config
```

Config lives in `~/.yu/workspaces/<project>/` (per-project) and `~/.config/yu/` (global). Invisible to the agent.

**Project instructions**: Create `Yu.md` in your project root (or use `/init`). Also reads `CLAUDE.md`, `AGENTS.md` if no `Yu.md` exists.

<details>
<summary>.yu/config.yaml reference</summary>

```yaml
snapshot:
  keep: 10             # max snapshots; uses time-bucketed retention
  quiet_seconds: 15
  file_threshold: 50

agent:
  model: claude-sonnet-4-6   # default model
  max_tokens: 8192
  bash_timeout: 120

network:
  inject:
    - upstream: "https://api.company.com"
      path: "/company"
      headers:
        Authorization: "Bearer ${TOKEN}"

commands:
  intercept: [git, ssh, gh, aws, scp]

env:
  keep: [MY_CUSTOM_VAR]
```
</details>

## Providers

| Provider | Env vars | Models |
|---|---|---|
| Anthropic | `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` | Claude Opus/Sonnet/Haiku 4.x |
| OpenAI | `OPENAI_API_KEY` | GPT-5.4, o3, o4-mini, GPT-4.1 |
| Custom | `YU_API_KEY` + `YU_BASE_URL` | Any OpenAI-compatible endpoint |

Use `/model` inside the agent to switch interactively.

## Background

Yu implements the credential isolation layer from the [Environment as a Service](https://blog.dreambubble.ai/en/posts/environment-as-a-service-agent-as-the-interface) paper.

## Name

Yu is named after Ma Xiaoyu, the author's son.

## License

MIT
