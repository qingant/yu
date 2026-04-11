# Yu

One product, two modes.

**Yu Sandbox** — run any AI agent with credential isolation, auto-snapshots, and zero permission prompts.

**Yu Agent** — a fast built-in AI coding agent. 80% of Claude Code, 10x faster to start, single binary.

> Apple Silicon Mac only. Linux planned.

## Install

```bash
sudo curl -fsSL https://github.com/qingant/yu/releases/latest/download/yu-darwin-arm64 -o /usr/local/bin/yu && sudo chmod +x /usr/local/bin/yu
```

```bash
yu update                            # update to latest
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

---

## Yu Sandbox

Wrap any AI coding agent in a secure sandbox. The agent can use credentials without holding them.

```bash
yu wrap claude                       # Claude Code in sandbox
yu wrap codex exec "prompt"          # Codex in sandbox
yu wrap gemini                       # Gemini CLI in sandbox
yu wrap bash                         # test the sandbox yourself
```

### What gets sandboxed

| Layer | How |
|---|---|
| **Filesystem** | macOS `sandbox-exec` — denies access to home dir except project directory |
| **Env vars** | Default-deny whitelist. Secrets get dummy values. |
| **API keys** | Localhost proxy swaps dummy keys for real ones. No MITM. |
| **Commands** | `git`, `ssh`, `gh`, `aws` intercepted by shims. Credentials injected outside sandbox. |
| **Permissions** | Auto-bypass (`--dangerously-skip-permissions`). Sandbox is the boundary. |

### Protection

| Threat | Status |
|---|---|
| Package reads `~/.ssh/id_ed25519` | **Blocked** — home dir access denied, only project dir allowed |
| MCP server reads `AWS_SECRET_ACCESS_KEY` | **Blocked** — env var redacted |
| Agent exfiltrates API key | **Blocked** — only dummy keys in sandbox, real keys injected by proxy |
| Agent reads `~/Documents/` | **Blocked** — entire home dir denied except project |
| Supply chain attack steals `GH_TOKEN` | **Blocked** — git works through credential proxy |
| Agent breaks your code | **Auto-rollback** — `/rollback` to any snapshot |
| Permission fatigue ("Allow?" x 100) | **Eliminated** — sandbox is the security boundary |

### Snapshots

Automatic, behavior-driven. Uses APFS clone (instant, copy-on-write).

- Before risky commands (`git push`, `ssh`, `gh deploy`)
- When file activity settles (~15s quiet)
- On change volume threshold (50+ files)
- Skipped when nothing changed

Keeps 10 snapshots with time-bucketed retention (1 daily + 2 hourly + 7 recent).

```bash
yu snapshots                         # list
yu rollback 1                        # restore
```

---

## Yu Agent

A fast built-in AI coding agent that runs inside the sandbox.

```bash
yu agent                             # interactive
yu agent -m claude-haiku-4-5         # specify model
yu agent --new                       # force new session

yu agent exec "fix the bug"          # one-shot, exit when done
yu agent exec -f prompt.md           # from file
echo "explain this" | yu agent exec  # pipe
```

### Tools (13)

| Tool | Description |
|---|---|
| `bash` | Streaming shell commands |
| `read_file` | Read files (with image support) |
| `write_file` | Create / overwrite files |
| `edit_file` | Search-and-replace |
| `list_files` | Glob pattern matching |
| `search_files` | Ripgrep content search |
| `poll` | Recurring command with interval + timeout |
| `web_fetch` | HTTP GET with HTML-to-text |
| `background` | Start / logs / stop long-running processes |
| `generate_image` | AI image generation (OpenAI) |
| `ask_user` | Interactive questions / choices |
| `plan` | Multi-step plan with approval |

### Agent Commands

```
/help              Show all commands
/model             Switch provider and model (interactive)
/rollback          Roll back to a previous snapshot
/compact           Compress conversation context
/sessions          List / resume past sessions
/init              Create Yu.md project instructions
/jobs              List background processes
/logs <id>         Show background process output
/kill <id>         Stop a background process
/remember <text>   Save to cross-session memory
/memory            Show saved memory
/forget            Clear memory
/exit              Exit

!<command>         Run shell command (output visible to model)
@<file>            Attach file to message (tab to complete)
line ending \      Multi-line input
```

### Providers

| Provider | Env vars | Models |
|---|---|---|
| Anthropic | `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` | Claude Opus / Sonnet / Haiku 4.x |
| OpenAI | `OPENAI_API_KEY` | GPT-5.4, o3, o4-mini, GPT-4.1 |
| Custom | `YU_API_KEY` + `YU_BASE_URL` | Any OpenAI-compatible endpoint |

Use `/model` to switch interactively. Selection persists across sessions.

### Features

- **Prompt caching** — Anthropic cache_control for ~80% TTFT reduction after first message
- **Sessions** — auto-save, resume on startup, `/compact` to compress context
- **Memory** — `/remember` persists notes across sessions
- **Markdown rendering** — tables with box-drawing, code blocks, headers, lists
- **Readline** — history, tab completion for commands and @file paths

---

## Configuration

```bash
yu config init                       # create workspace config
yu config set GIT_SSH_COMMAND "ssh -i ~/.ssh/id_ed25519"
yu config set GH_TOKEN ghp_xxxxx
yu config set ANTHROPIC_AUTH_TOKEN sk-ant-xxxxx
yu config list                       # show merged config
```

Global flag `-C` (or `-c`, `--path`) sets project directory for any command:

```bash
yu -C /path/to/project agent
yu -C /path/to/project snapshots
yu -C /path/to/project wrap claude
```

Config lives in `~/.yu/workspaces/<project>/` (per-project) and `~/.config/yu/` (global). Invisible to the agent.

**Project instructions**: Create `Yu.md` in your project root (or use `/init`). Also reads `CLAUDE.md`, `AGENTS.md` if no `Yu.md` exists.

<details>
<summary>config.yaml reference</summary>

```yaml
snapshot:
  keep: 10
  quiet_seconds: 15
  file_threshold: 50

agent:
  model: claude-sonnet-4-6
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

## Background

Yu implements the credential isolation layer from the [Environment as a Service](https://blog.dreambubble.ai/en/posts/environment-as-a-service-agent-as-the-interface) paper.

## Name

Yu is named after Ma Xiaoyu, the author's son.

## License

MIT
