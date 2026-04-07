# Yu

**Your AI agent can `git push`, but it has never seen your SSH key.**

Yu sandboxes AI coding agents so they can use credentials without holding them. No permission popups. Auto-snapshot for fearless rollback.

Works with Claude Code, Codex, Gemini CLI, or any command.

> Apple Silicon Mac only. Linux planned.

## Install

```bash
curl -fsSL https://github.com/qingant/yu/releases/latest/download/yu-darwin-arm64 -o /usr/local/bin/yu && chmod +x /usr/local/bin/yu
brew install fswatch  # for auto-snapshot
```

<details>
<summary>Build from source</summary>

```bash
brew install go fswatch
git clone https://github.com/qingant/yu.git && cd yu
go build -o /usr/local/bin/yu ./cmd/yu/
```
</details>

## Usage

```bash
yu . -- claude                   # Claude Code in sandbox
yu . -- codex exec "prompt"      # Codex in sandbox
yu .                             # auto-detect agents
yu . -- bash                     # test sandbox yourself
```

## Protection

| Threat | Status |
|---|---|
| Package reads `~/.ssh/id_ed25519` | **Blocked** — `~/.ssh` invisible inside sandbox |
| MCP server reads `AWS_SECRET_ACCESS_KEY` | **Blocked** — env var doesn't exist inside sandbox |
| Agent exfiltrates API key | **Blocked** — only dummy keys in sandbox, real keys injected by proxy |
| Prompt injection runs `cat ~/.aws/credentials` | **Blocked** — file invisible inside sandbox |
| Supply chain attack steals `GH_TOKEN` | **Blocked** — env stripped, git works through credential proxy |
| Agent breaks your code | **Auto-rollback** — snapshot before every risky operation |
| Permission fatigue ("Allow?" x 100) | **Eliminated** — sandbox is security, no prompts needed |

## Snapshots

Automatic, behavior-driven — no timers:

- Before risky commands (`git push`, `ssh`, `gh deploy`)
- When file activity settles (~15s quiet)
- On change volume threshold (50+ files)

Uses APFS clone: instant, copy-on-write, near-zero overhead.

```
$ yu snapshots
#0   22:17:31  [init]               baseline
#1   22:18:05  [quiet]              3 files: src/main.go, README.md, +1 more
#2   22:19:30  [pre-command:git]    1 files: src/main.go

$ yu rollback 0
```

## How It Works

**Filesystem** — macOS `sandbox-exec` hides everything except the project directory. No containers.

**Env vars** — Default-deny whitelist. Secrets (`KEY`, `TOKEN`, `SECRET`, `PASSWORD`) get dummy values.

**API proxy** — For custom `BASE_URL` setups (e.g. LiteLLM), a localhost reverse proxy swaps dummy keys for real ones. No MITM, no certificates.

**Command proxy** — `git`, `ssh`, `gh`, `aws` intercepted by shims. Real commands run outside sandbox with credentials from `.yu/env`.

**Permission bypass** — Agents launch with `--dangerously-skip-permissions` (Claude) / `--dangerously-bypass-approvals-and-sandbox` (Codex). The sandbox is the security boundary.

## Configuration

```bash
yu config init                     # create .yu/ with templates
yu config set GIT_SSH_COMMAND "ssh -i ~/.ssh/id_ed25519"
yu config set GH_TOKEN ghp_xxxxx
yu config inject \                 # proxy inject rule
  --upstream "https://api.company.com" \
  --path "/company" \
  --header "Authorization: Bearer \${TOKEN}"
yu config list                     # show merged config
```

Config lives in `.yu/` (project) and `~/.config/yu/` (global). Invisible to the agent.

<details>
<summary>.yu/config.yaml reference</summary>

```yaml
snapshot:
  keep: 10
  quiet_seconds: 15
  file_threshold: 50

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

Yu implements the credential isolation layer from the [EaaS paper](../papers/) (Section 7.4).

## Name

Yu is named after Ma Xiaoyu, the author's son.

## License

MIT
