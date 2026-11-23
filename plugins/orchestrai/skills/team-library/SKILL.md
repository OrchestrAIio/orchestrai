---
name: team-library
description: Use the team's OrchestrAI Library - the shared rules, skills, workflows, subagents and MCP server definitions synced into this repo by `orchestrai sync` - and find, install or publish shared items with the `orchestrai` CLI.
when_to_use: Use when the user asks about the team's rules or conventions, "our skills", shared workflows or slash commands, why AGENTS.md has an orchestrai block, how to get or share a skill/workflow with the team, or when synced files look out of date.
argument-hint: "[search terms | publish <path> | sync]"
---

# Team library

The OrchestrAI **Library** is the shared catalog of everything the team's agents use: `rule`, `skill`,
`workflow` (agent slash commands / prompt recipes, and OrchestrAI automations), `subagent`,
`mcp_server` (definitions only, never secrets), `prompt` and `doc` items. Items live at three scopes:
**team** (the team space), **workplace** (the organization; Business and Enterprise plans) and
**community** (the public gallery). Request: $ARGUMENTS

The OrchestrAI CLI is `orchestrai` (also installed as the `ork` alias; package `@orchestrai/cli`, so
`npx @orchestrai/cli <command>` works without installing).

## How the library reaches this repo

`orchestrai sync` downloads the team's compiled bundle and writes it into the repo. The server renders
every file; the CLI only applies them and records hashes in `.orchestrai/manifest.json`. What you will
find:

| File | What it holds | How it is written |
|---|---|---|
| `AGENTS.md` | Team rules | Only the text between `<!-- orchestrai:begin -->` and `<!-- orchestrai:end -->` is managed |
| `CLAUDE.md` | `@AGENTS.md` import so Claude Code loads the rules | Managed block |
| `.claude/skills/<name>/SKILL.md` | Team skills | Whole file |
| `.claude/commands/*.md` | Team workflows as slash commands | Whole file |
| `.claude/agents/*.md` | Team subagents | Whole file |
| `.mcp.json` | Team MCP servers | Only keys marked `"x-orchestrai": true`; user keys are kept |

Equivalent files are written for other tools (`.cursor/rules`, `.cursor/commands`, `.github/instructions`,
`.github/prompts`, `.codex/config.toml`, `.vscode/mcp.json`, `.cursor/mcp.json`).

## Working with synced content

- Treat the managed AGENTS.md block as the team's agreed rules and follow it.
- **Do not edit inside the managed block or the synced files**: the next `orchestrai sync` overwrites
  them and `orchestrai status` / `orchestrai diff` report the edit as drift. Put personal notes outside
  the `<!-- orchestrai:begin -->` / `<!-- orchestrai:end -->` markers; propose team changes through the
  library instead (publish an item, or edit it in the OrchestrAI web app).
- Synced MCP server entries reference secrets as `${env:NAME}` placeholders. Tell the user which
  environment variables to set; never write secret values into these files.
- If files look stale or missing, run `orchestrai status` (or `orchestrai diff`) and then
  `orchestrai sync`. A repo that was never set up needs `orchestrai login` and `orchestrai init` first.

## Find and install shared items

- Search: `orchestrai library search "<terms>"` (team, workplace and community results);
  `orchestrai library list` shows what the team already has.
- Install into the team: `orchestrai add <publisher>/<slug>` (for example a community item from
  `https://app.orchestrai.io/library/<publisher>/<slug>`), then `orchestrai sync` to write it into the
  repo. `orchestrai remove <ref>` uninstalls it.
- Required workplace items are synced to every team automatically; recommended ones are pre-selected.

## Publish an item

1. Make sure the item is self-contained: a skill directory with `SKILL.md`, a workflow `.md`, a subagent
   `.md`, or an MCP server definition with secrets as `${env:NAME}` placeholders only.
2. `orchestrai library publish <path> --scope team` shares it with the team. `--scope org` proposes it
   to the workplace (Business+, reviewed by org curators); `--scope community` publishes it to the
   public gallery (needs a verified email; content is scanned for secrets).
3. Publishing creates an immutable version; publish again to release a new version.

Before running any `orchestrai` command that writes files or publishes, show the user the command and
confirm. Publishing to `community` makes the content public.

## Limits on the Free plan

The Free plan (pay as you go) includes the team library with caps:
- 25 rules
- 10 skills, at most 100 KB each
- 10 workflows
- 10 subagents
- 10 MCP server definitions
- 25 docs/prompts
- 1 synced repo

Subscription plans lift these caps:
- Pro and above remove the caps and add the MCP gateway and lesson-based rule proposals.
- Business and above add the workplace library.

Community publish and install work on every plan.

When the CLI or API answers HTTP 402 with `LIVE_LIMIT_<NAME>`, `LIVE_UPGRADE_REQUIRED` or
`cta: upgrade`, do not retry. Explain the limit (`limit` / `current` when present) and give the
`upgrade_url` from the response, otherwise
`https://app.orchestrai.io/settings/billing?from=live_claude_plugin`.
