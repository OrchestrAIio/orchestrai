# @orchestrai/acp

The OrchestrAI agent in any [Agent Client Protocol](https://agentclientprotocol.com) editor —
Zed, JetBrains IDEs (AI Assistant), Neovim (CodeCompanion, avante.nvim), Emacs (agent-shell) and
VS Code ACP extensions. It is a small local stdio process that connects the editor to your team's
OrchestrAI conversations: you prompt the agent from the editor, see its reply stream in, follow the
runs it starts, and see what teammates post in shared conversations.

```bash
npx -y @orchestrai/acp            # what editors launch (speaks ACP on stdin/stdout)
npx -y @orchestrai/acp login      # sign in from a terminal
npx -y @orchestrai/acp status     # which API, key and user are in use
npx -y @orchestrai/acp logout     # forget and revoke the editor key
```

Requires Node.js 20.11 or newer.

## Sign in

Pick one:

- **Browser sign-in (recommended).** When the editor asks you to authenticate, choose
  **Sign in with OrchestrAI**. Your browser opens OrchestrAI with a one-time code; approve it and
  the editor is connected. From a terminal, `npx -y @orchestrai/acp login` does the same.
  The bridge receives an API key named "OrchestrAI Live — acp on &lt;device&gt;"; you can revoke it
  any time in **Settings → API keys**.
- **API key.** Create a key in **Settings → API keys** and set `ORCHESTRAI_API_KEY=ork_…` in the
  agent's environment (see the editor configs below). It takes precedence over a stored sign-in.

Where the key is kept: the OS keychain (macOS Keychain; Secret Service via `secret-tool` on Linux),
otherwise `~/.config/orchestrai/credentials.json` with mode 0600 (`%APPDATA%\orchestrai` on
Windows). The secret is passed to keychain tools on stdin, never on the command line.

## Zed

Settings (`zed: open settings`):

```json
{
  "agent_servers": {
    "OrchestrAI": {
      "type": "custom",
      "command": "npx",
      "args": ["-y", "@orchestrai/acp"],
      "env": {
        "ORCHESTRAI_PROJECT_ID": "optional — pin new threads to one project"
      }
    }
  }
}
```

Or install **OrchestrAI** from the ACP Registry (`zed: acp registry`) once it is listed. Start a
thread from the Agent Panel's new-thread menu. Past and shared conversations appear in Thread
History (session list / import).

## JetBrains (IntelliJ IDEA, PyCharm, WebStorm, GoLand, Rider, …)

AI Assistant → AI Chat → ⋯ → **Add Custom Agent** opens `~/.jetbrains/acp.json`:

```json
{
  "default_mcp_settings": {},
  "agent_servers": {
    "OrchestrAI": {
      "command": "npx",
      "args": ["-y", "@orchestrai/acp"],
      "env": {
        "ORCHESTRAI_API_KEY": "ork_…"
      }
    }
  }
}
```

JetBrains recommends the full path to the executable (`which npx`). ACP agents are not supported
under WSL. Once listed, the agent can also be installed from **Settings → Tools → AI Assistant →
Agents → Install from registry**.

## Neovim / Emacs / others

Point the client's ACP agent command at `npx -y @orchestrai/acp` (CodeCompanion `adapters.acp`,
avante.nvim `acp_providers`, agent-shell `agent-shell-make-agent-config`).

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `ORCHESTRAI_API_KEY` | — | `ork_…` API key; overrides the stored sign-in |
| `ORCHESTRAI_API_URL` | `https://orchestrai-api-production.up.railway.app` | API base URL (https; http only for localhost) |
| `ORCHESTRAI_WEB_URL` | `https://app.orchestrai.io` | Web app, used for approval / billing / conversation links |
| `ORCHESTRAI_PROJECT_ID` | — | Create every new conversation in this project (skips the picker) |
| `ORCHESTRAI_KEYCHAIN` | on | `0` stores the key in the credentials file instead of the OS keychain |
| `ORCHESTRAI_CONFIG_DIR` | `~/.config/orchestrai` | Credentials file and the session index |
| `ORCHESTRAI_TURN_TIMEOUT_SECONDS` | `600` | End a prompt turn after this long without any agent output (the run keeps going) |
| `ORCHESTRAI_NO_BROWSER` | — | `1` prints the sign-in link instead of opening a browser |
| `ORCHESTRAI_ACP_DEBUG` | — | `1` writes diagnostics to stderr (the editor's agent log) |

## How it maps onto OrchestrAI

| ACP | OrchestrAI |
|---|---|
| `initialize` | Advertises `loadSession`, `session/list`, `session/resume`, `session/close`, `logout`, embedded context, and the auth methods |
| `authenticate` (`orchestrai-browser-login`) | Device flow: `POST /api/v1/editor/connect/start`, browser approval at `/live/connect`, `POST /api/v1/editor/connect/poll` |
| terminal auth (`orchestrai-terminal-login`, when the client supports it) | Runs `orchestrai-acp login` |
| `logout` | Forgets the key; revokes it (`DELETE /api/v1/api-keys/:id`) when the bridge minted it |
| `session/new` | Chooses the project (env, the only project, or a picker); the conversation is created on the first prompt, titled from it |
| `session/set_config_option` (`project`) | Picks the project before the first prompt |
| `session/prompt` | Same path as the web chat: store the user message, `POST /api/v1/projects/:id/chat/message`, stream the reply over `/ws`, persist the reply |
| `agent_message_chunk` | Agent tokens (conversation stream, then any run it starts) |
| `tool_call` / `tool_call_update` | A run the agent starts, and each tool / phase event of that run |
| `plan` | The run lifecycle: analyze → plan → human approval → execute → verify |
| approvals | A read-only approval card with a link to the web app. **Approvals cannot be granted from the editor** — no ACP permission request is ever sent and there is no approve path |
| `session/cancel` | Stops streaming; `POST /api/v1/projects/:id/agent-runs/:runId/stop` for runs the turn started |
| `session/list` | Your conversations, ones shared with you, and ones shared with the project, across your projects |
| `session/load` | Replays the conversation, including teammates' turns |
| `session/resume` | Reattaches without replay |
| embedded context (`resource` blocks) | Sent as chat attachments (≤5 files, ≤10 MB) |

The WebSocket authenticates with a 60-second ticket from `POST /api/v1/ws/ticket` (the `/ws`
handshake does not accept API keys directly). The server only streams events from conversations
you can see.

## Teammates' turns

ACP has no multi-author messages, so the bridge renders another person's message as a
`user_message_chunk` whose text starts with their name — **"**Dana:** restarted the pods"** —
and carries the author in `_meta.orchestrai` (`authorUserId`, `authorName`, `fromTeammate`).
Agent replies are `agent_message_chunk`s whoever prompted them.

- **On load** (`session/load`): the full history is replayed in order, teammates labelled.
- **While the thread is open and idle**: a teammate's new message appears as a labelled user
  message, and the agent's reply to them streams in live (the copy their browser saves is not shown
  twice). When the client advertises `session.notices`, a notice "Dana posted in “Prod outage”"
  is also sent. While your own prompt is running, teammates' messages are held and shown right after
  your turn ends.
- **Runs started by teammates** in the conversation keep updating their tool call; a run that
  needs approval shows the approval card with the web link.

What editors show (October 2026):

- **Zed** applies `session/update` notifications to the open thread whether or not a prompt is
  running (routing is by session id only, `crates/agent_servers/src/acp.rs`), adds
  `user_message_chunk` as a user message and `agent_message_chunk` as an assistant message
  (`acp/session_updates.rs`), and shows `notice` updates as dismissible notices outside the
  transcript (Session Notices RFD; Zed advertises `session.notices` with ACP beta enabled). Thread
  History lists `session/list` results and imports them; Zed skips sessions without a working
  directory, so the bridge reports the editor's cwd for every conversation.
- **JetBrains AI Assistant** documents ACP chat, registry install and `acp.json`, but not how it
  renders replayed history or updates that arrive outside a prompt turn. The bridge only uses
  stable ACP v1 updates, so replayed and live teammate messages arrive as ordinary user/agent
  messages; if a client ignores updates outside a turn, the teammate's messages still show the next
  time the conversation is loaded. Notices are sent only to clients that advertise them.

## Plans and credits

OrchestrAI Live is free; the OrchestrAI agent runs on credits (Free is pay as you go; Pro and above
include credits). When the backend answers `402`:

- zero balance (`INSUFFICIENT_CREDITS` / `cta: "buy_credits"`): the agent replies
  "The OrchestrAI agent runs on credits — buy credits: &lt;link&gt; (or upgrade to Pro for included
  credits: &lt;link&gt;)";
- a plan limit or subscription feature (`LIVE_LIMIT_*`, `LIVE_UPGRADE_REQUIRED`, `cta: "upgrade"`):
  it explains the limit and links to the upgrade page.

Neither is shown as an error; the turn simply ends.

## Development

```bash
npm install
npm run typecheck
npm test          # builds, then unit + session tests (mock HTTP/WS backend) and the stdio end-to-end test
```

`registry/` holds the ACP Registry entry (see `registry/README.md`).

## Licence

MIT. See [LICENSE](LICENSE).
