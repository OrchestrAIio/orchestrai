# OrchestrAI CLI (`orchestrai`, alias `ork`)

One static binary that connects a repository to its OrchestrAI team space:
it writes the team's rules, skills, slash commands, subagents and MCP servers
into every AI tool's config (Claude Code, Cursor, VS Code / Copilot, Codex),
keeps them in sync, installs and publishes library items, runs automations,
asks the team agent and opens live rooms.

## Install

```bash
npm install -g @orchestrai/cli     # installs both `orchestrai` and `ork`
npx @orchestrai/cli add acme/pr-review
```

The npm package downloads the binary for your platform from the GitHub
release `ork-v<version>` and verifies its sha256 against
`orchestrai_<version>_checksums.txt` before installing it. Without npm, download
`orchestrai_<version>_<os>_<arch>` from the release, check it with
`sha256sum -c`, put it on your `PATH`, and optionally `ln -s orchestrai ork`.

Wrapper environment variables: `ORCHESTRAI_CLI_DOWNLOAD_BASE` (mirror URL),
`ORCHESTRAI_CLI_BINARY_PATH` (use an existing binary),
`ORCHESTRAI_CLI_SKIP_DOWNLOAD` (skip the postinstall download).

## Quick start

```bash
orchestrai login          # opens the browser; approve the code shown
orchestrai init           # pick the team space; writes .orchestrai/config.json
orchestrai sync           # writes AGENTS.md, CLAUDE.md, MCP configs, …
git add .orchestrai AGENTS.md CLAUDE.md .mcp.json .cursor .vscode .codex .github .claude
```

`add` and `init` take you through the missing steps on the way: on a
terminal they sign you in (browser device flow), offer to create a team space
when you have none, let you pick one when you have several, bind the
repository and then carry on. Without a terminal (CI, pipes) they never
prompt; they print the exact commands to run instead, using the name you ran
the CLI as (`npx @orchestrai/cli …` under npx). `--yes` makes them choose
without asking: the only team space, or a new one named after the repository
when you have none.

Commit `.orchestrai/config.json` (project id and API URL, no secrets) and
`.orchestrai/manifest.json` so teammates and CI share the binding.

## Commands

| Command | What it does |
|---|---|
| `login [--api-url] [--no-browser] [--device-name]` | Device-flow sign-in. The API key goes to the OS keychain, or to `~/.config/orchestrai/credentials.json` (0600) when no keychain is available. |
| `logout [--keep-key]` | Revokes the key on the server (unless `--keep-key`) and forgets it locally. |
| `whoami [--json]` | Shows the account, where the key is stored, and whether it still works. |
| `init [--project <id> \| --create-space <name>] [--yes] [--tools …] [--force]` | Binds the repo (git root) to a team space. Signs you in first when needed (on a terminal). Without `--project` it uses your only team space, lets you pick or create one on a terminal, or with `--yes` creates one named after the repository when you have none. |
| `sync [--check] [--dry-run] [--tools claude,cursor,vscode,codex,copilot] [--json]` | Applies the team context bundle. |
| `status [--json] [--offline] [--exit-code]` | Local drift (edited managed content) and whether the server has a newer bundle. |
| `diff [--tools …] [--exit-code]` | Unified diff of what `sync` would change. |
| `add <publisher/slug\|item-id>[@version] [--no-sync] [--yes] [--create-space <name>]` | Installs a library item into the team space, then syncs. On a fresh machine or repository it signs in and binds the repository first (see Quick start). |
| `remove <publisher/slug\|item-id\|install-id> [--no-sync]` | Uninstalls it, then syncs (its files are removed if unchanged). |
| `library list [--scope team\|org\|community] [--kind …]` | Lists library items (default scope: team). |
| `library search <query> [--scope …]` | Searches (default scope: community). |
| `library show <ref> [--content] [--json]` | Shows an item and its files. |
| `library publish <path> [--scope team\|org\|community] [--kind …] [--name] [--slug] [--version] [--item <id>] [--dry-run]` | Publishes a skill directory (`SKILL.md`), a command / agent / rule / prompt `.md` file, an MCP server `.json`, or a doc URL. The kind is inferred when `--kind` is omitted. |
| `workflows list` / `workflows run <name\|id> [--input k=v] [--input k:=<json>] [--input-json '{…}'] [--wait]` | Lists and runs OrchestrAI automations in the team space. |
| `ask "<question>" [--conversation <id>] [--timeout 10m]` | Sends the question to the team agent and streams the reply. Use `-` to read the question from stdin. |
| `rooms list` / `rooms open <id> [--editor vscode\|cursor\|windsurf\|vscodium\|browser] [--print]` | Lists live co-coding rooms or opens one in the editor extension or the browser. |
| `version` | Prints the version. |

Every project-scoped command accepts `--project <id>` and `--api-url <url>`.

### Publishing safety

`library publish` scans every file before anything is uploaded and refuses if
it finds a secret: private keys, cloud/provider tokens (AWS, GitHub, GitLab,
Slack, OpenAI, Anthropic, OpenRouter, Google, Stripe, npm, Hugging Face,
OrchestrAI), JWTs, credentials in URLs, hard-coded `api_key = "…"` style
assignments, and literal values in an MCP server's `env`/`headers`. Use
`${env:VAR}` placeholders instead. A false positive can be allowed by adding
`orchestrai:allow-secret` on that line. Files are limited to 1 MiB each, 5 MiB
and 200 files per item; symlinks are refused.

## What `sync` writes

The server compiles the bundle (`GET /api/v1/projects/:id/context/bundle`);
the CLI only applies its `files[]`. Typical targets:

| Tool | Files |
|---|---|
| All tools | `AGENTS.md` (managed block) |
| Claude Code | `CLAUDE.md` (managed block containing `@AGENTS.md`), `.mcp.json`, `.claude/skills/<name>/SKILL.md`, `.claude/commands/*.md`, `.claude/agents/*.md` |
| Cursor | `.cursor/mcp.json`, `.cursor/rules/*.mdc`, `.cursor/commands/*.md` |
| VS Code | `.vscode/mcp.json` (`servers` shape) |
| GitHub Copilot | `.github/instructions/*.instructions.md`, `.github/prompts/*.prompt.md` |
| Codex | `.codex/config.toml` (managed block) |

`--tools` (or `"tools"` in `.orchestrai/config.json`) limits the targets;
tool-agnostic files such as `AGENTS.md` are always written.

### Apply strategies

| Strategy | Behaviour |
|---|---|
| `file` | The CLI owns the whole file. |
| `managed_block` | Only the text between `<!-- orchestrai:begin -->` and `<!-- orchestrai:end -->` (`# orchestrai:begin` / `# orchestrai:end` in `.toml`) is replaced; the block is appended when absent and nothing outside it is touched. CRLF files stay CRLF. A missing or duplicated marker stops the sync with an error instead of guessing. |
| `json_merge` | Only entries under `servers` / `mcpServers` that carry `"x-orchestrai": true` are added, replaced or removed. Your own servers and other keys stay, as do key order and indentation. If you define a server with the same name yourself, yours wins (with a warning). Invalid JSON, comments (JSONC) or a non-object container stop the sync with an error. |

Path safety: bundle paths must be relative, use forward slashes, contain no
`.`/`..` segments, stay out of `.git/` and `.orchestrai/`, and may only follow
symlinks that stay inside the repository. Two bundle paths that resolve to the
same file are refused. Nothing is written if any file fails these checks.

### Manifest and drift

`.orchestrai/manifest.json` records `bundle_version`, `bundle_hash`, and for
each managed path the sha256 of the content the CLI owns:

- `file`: the whole file, CRLF normalized to LF;
- `managed_block`: the text between the marker lines, LF-normalized;
- `json_merge`: `{"<container>": {"<name>": entry}}` over the flagged entries,
  serialized with sorted keys, no whitespace and no HTML escaping.

It also keeps `entries` (`{path: {strategy, tool}}`), the `tools` selection and
the server `etag`. When the bundle stops listing a path, the CLI removes its
content only if it is unchanged since the last sync; edited files are left in
place with a warning (and reported as drift). Empty directories left behind
are removed. When the working tree still matches the manifest, `sync` sends
`If-None-Match` and a `304` means nothing to do.

After a real sync the CLI posts
`POST /api/v1/projects/:id/context/sync-reports`
`{client: "ork", tool_targets, manifest_hash, drift: [{path, tool, state}]}`
(best effort; `state` is `overwritten` or `kept_modified`).

## CI

```yaml
- name: Team AI context is in sync
  run: npx @orchestrai/cli sync --check
  env:
    ORCHESTRAI_API_KEY: ${{ secrets.ORCHESTRAI_API_KEY }}
```

`sync --check` writes nothing and exits `1` when the repository differs from
the team bundle (`--json` prints the planned changes). Create the key under
Settings → API keys with `view_project` and `manage_context`.

## Environment

| Variable | Purpose |
|---|---|
| `ORCHESTRAI_API_KEY` | API key; overrides stored credentials (CI). |
| `ORCHESTRAI_API_URL` | API base URL (default `https://orchestrai-api-production.up.railway.app`). |
| `ORCHESTRAI_PROJECT_ID` | Team space id; overrides `.orchestrai/config.json`. |
| `ORCHESTRAI_APP_URL` | Web app URL used in links (default `https://app.orchestrai.io`). |
| `ORCHESTRAI_WS_URL` | WebSocket endpoint for `ask` (default: the API URL with `ws(s)://…/ws`). |
| `ORCHESTRAI_NO_KEYRING` | Store the key in `credentials.json` instead of the OS keychain. |
| `ORCHESTRAI_CONFIG_DIR` | Directory for `credentials.json` (default `~/.config/orchestrai`, `%AppData%\orchestrai` on Windows). |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Out of sync / drift (`sync --check`, `status --exit-code`, `diff --exit-code`) |
| 2 | Usage error |
| 3 | Not signed in, key invalid or revoked, or access denied |
| 4 | Plan limit or purchase required (HTTP 402); the message includes the buy-credits or upgrade link |
| 5 | Network failure or unexpected API response |
| 6 | Refused: unsafe bundle path, malformed local file, or secrets in a publish |
| 7 | Repository not bound (`orchestrai init`) |
| 8 | Other local error |
| 9 | A workflow run or agent request finished in a failed state |
| 130 | Interrupted |

## Development

Run these from the CLI directory (`cli/` in [OrchestrAIio/orchestrai](https://github.com/OrchestrAIio/orchestrai)):

```bash
go test -race ./...          # unit, path-safety, device-flow, golden end-to-end tests
go test ./internal/cli -run TestGoldenEndToEnd -update   # refresh golden files
node --test npm/test/
CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/OrchestrAIio/orchestrai/cli/internal/cli.Version=dev" -o orchestrai .
```

Releases (maintainers): a tag `ork-v<semver>` on the OrchestrAI monorepo
tests, cross-compiles linux/darwin/windows × amd64/arm64, publishes the GitHub
release with checksums, and publishes `@orchestrai/cli` to npm (`NPM_TOKEN`).

## Licence

MIT. See [LICENSE](LICENSE).
