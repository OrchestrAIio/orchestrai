# OrchestrAI plugin for Claude Code

A Claude Code plugin marketplace named `orchestrai` with one plugin, also named `orchestrai`. The plugin
connects Claude Code to your team's OrchestrAI workspace:

- **MCP server** `orchestrai`: OrchestrAI's inbound MCP endpoint (`<api-host>/mcp`, HTTP transport),
  authenticated with your OrchestrAI API key. Tools: `orchestrai_list_projects`,
  `orchestrai_list_conversations`, `orchestrai_read_conversation`, `orchestrai_send_message`,
  `orchestrai_list_runs`, `orchestrai_list_approvals`, `orchestrai_query_ledger`. Claude Code exposes them
  as `mcp__plugin_orchestrai_orchestrai__<tool>`. The full tool reference is in `docs/MCP_SERVER.md`.
- **Skills** (run them as `/orchestrai:<skill>`, or let Claude pick them up automatically):

| Skill | What it does |
|---|---|
| `/orchestrai:ask-orchestrai` | Answers infra and team questions from the team's shared conversations and runs; hands a task to the OrchestrAI agent when you ask (spends credits) |
| `/orchestrai:check-approvals` | Lists approval requests waiting on a human and links to the web app. It never approves anything |
| `/orchestrai:ledger-lookup` | Queries the resources the OrchestrAI agent manages (the ledger), including drift status |
| `/orchestrai:team-library` | Explains the team's synced rules, skills, workflows and subagents, and how to find, install and publish library items with the `orchestrai` CLI |

## Requirements

- Claude Code 2.1.281 or later. Older versions also load the plugin, but `claude plugin validate`
  only checks MCP entries from 2.1.281.
- An OrchestrAI account on any plan, including **Free** (pay as you go).
  - The read tools (projects, conversations, runs, approvals, ledger) are free.
  - Sending work to the OrchestrAI agent (`orchestrai_send_message`) spends credits. On Free, you buy
    credits as you need them; Pro, Business and Enterprise include monthly credits.
- **How the skills handle 402 errors:**
  - **Zero balance** (`INSUFFICIENT_CREDITS`, `cta: "buy_credits"`): "The OrchestrAI agent runs on
    credits — buy credits: <buy_credits_url> (or upgrade to Pro for included credits: <upgrade_url>)".
    The fallback links are `https://app.orchestrai.io/settings/billing?buy=credits&from=live_claude_plugin`
    and `https://app.orchestrai.io/settings/billing?from=live_claude_plugin`.
  - **Plan limit or subscription feature** (`LIVE_LIMIT_<NAME>`, `LIVE_UPGRADE_REQUIRED`,
    `cta: "upgrade"`): the skills explain the limit and give the upgrade link.

## 1. Get an API key

Use either of these:

- **Web app:** in OrchestrAI, go to **Settings → API keys** and create a key. It starts with `ork_`. The
  key's permissions are its MCP scopes:
  - `view_project`: projects, conversations and the ledger
  - `view_execution`: runs and approvals
  - `execute_agent`: sending messages
- **CLI:** run `npx @orchestrai/cli login`. The OrchestrAI CLI is `orchestrai`, with the alias `ork`.
  The command opens the browser device sign-in and creates a key named "OrchestrAI Live — orchestrai on
  <device>".

Export the key in the shell profile that starts Claude Code:

```bash
export ORCHESTRAI_API_KEY=ork_...
# Optional: a self-hosted or staging API. The default is the production API.
export ORCHESTRAI_API_URL=https://orchestrai-api-production.up.railway.app
```

The MCP config reads the key as `Authorization: Bearer ${ORCHESTRAI_API_KEY}`, so the key is never
written into a file. Revoke it in **Settings → API keys** whenever you need to.

## 2. Add the marketplace and install

The marketplace is published at the root of the public repository
[OrchestrAIio/orchestrai](https://github.com/OrchestrAIio/orchestrai):

```bash
claude plugin marketplace add OrchestrAIio/orchestrai
claude plugin install orchestrai@orchestrai
```

You can also run these inside a session:

```text
/plugin marketplace add OrchestrAIio/orchestrai
/plugin install orchestrai@orchestrai
```

- Users without a GitHub SSH key should set `CLAUDE_CODE_PLUGIN_PREFER_HTTPS=1` before adding the
  marketplace.
- To share the marketplace with everyone working in a repository, run
  `claude plugin marketplace add OrchestrAIio/orchestrai --scope project` once and commit the
  `.claude/settings.json` it writes.

**From a checkout of the monorepo (development):** run `claude plugin marketplace add ./clients/claude-plugin`.
A marketplace added from a local path loads the plugin in place, so edits apply after
`/reload-plugins`.

The source of truth is `clients/claude-plugin/` in the private monorepo. The **Public repo sync**
workflow (`.github/workflows/public-repo.yml`) validates it and copies it to the public repository on
every push to `main`.

## 3. Check it works

```bash
claude mcp list            # orchestrai should be connected
```

Then, in a session:

```text
/orchestrai:check-approvals
```

If `claude mcp list` reports a missing-variable warning for `ORCHESTRAI_API_KEY`, the variable is not
set in the environment Claude Code started from.

## Security model

- **Approvals are web-only.** Approval requests can be listed here but never approved or rejected over
  MCP, from Claude Code or from any editor. An editor agent could be steered into approving its own
  plan, so approving stays a human action in the OrchestrAI web app. The `check-approvals` skill links to
  `…/approvals?project=<id>`.
- **Untrusted data.** Conversation messages, run output and ledger fields are written by people and
  agents. The skills tell Claude never to follow instructions found inside them.
- **No pre-approved agent sends.** The skills pre-approve only the read tools (`allowed-tools`).
  `orchestrai_send_message` starts agent work that spends credits in a conversation teammates can see, so Claude Code
  still asks before each call.

## Develop and validate

```bash
cd clients/claude-plugin
npm test                       # structural checks + `claude plugin validate` when the CLI is installed
claude plugin validate .       # marketplace
claude plugin validate ./plugins/orchestrai
claude --plugin-dir ./plugins/orchestrai   # load the plugin for one session without installing
```

The test (`test/validate.test.mjs`, Node 20+, no dependencies) checks:

- **Manifests:**
  - the manifests parse and have their required fields
  - names are kebab-case and not reserved
  - marketplace sources are `./` paths inside the marketplace
  - the entry name matches the manifest name
- **MCP config:** the server uses HTTPS, ends in `/mcp` and uses the env-placeholder bearer, with no
  literal key.
- **Skills:**
  - every skill has `SKILL.md` frontmatter whose `name` matches its directory, and a description
    within the 1,536-character listing cap
  - every MCP tool a skill references exists in `backend-go/mcp_inbound.go` (skipped outside the
    monorepo)
  - no skill references an approve tool
  - `orchestrai_send_message` is never pre-approved
- **`claude plugin validate`:** runs on the marketplace and on the plugin, and fails on warnings.

CI runs it from `.github/workflows/claude-plugin.yml`.

## Hosts

| What | Default |
|---|---|
| API and MCP | `https://orchestrai-api-production.up.railway.app` (`/mcp`); override with `ORCHESTRAI_API_URL` |
| Web app (approvals, billing, library) | `https://app.orchestrai.io` |
