# OrchestrAI

Open-source clients for [OrchestrAI](https://orchestrai.io), the AI agent platform for engineering teams. This repository holds the `orchestrai` CLI, the Agent Client Protocol bridge for Zed and JetBrains, and the Claude Code plugin. It also hosts the CLI's release downloads and the issue tracker for every OrchestrAI client.

| Directory | What it is | Install |
|---|---|---|
| [`cli/`](cli/) | `orchestrai` CLI (alias `ork`): sign in, sync your team's rules, skills and MCP servers into your AI tools, use the team library, run workflows, ask the agent | `npm i -g @orchestrai/cli` |
| [`acp-bridge/`](acp-bridge/) | Runs the OrchestrAI agent in any Agent Client Protocol editor (Zed, JetBrains) | `npx @orchestrai/acp` |
| [`plugins/orchestrai/`](plugins/orchestrai/) | Claude Code plugin: the team agent, approvals, managed resources and shared context over MCP | `/plugin marketplace add OrchestrAIio/orchestrai` |

All three talk to the OrchestrAI API. You need an OrchestrAI account, and the Free plan works.

## Quick start

```bash
npm i -g @orchestrai/cli
orchestrai login                 # browser sign-in; creates an API key for this device
orchestrai init && orchestrai sync
```

`orchestrai sync` writes your team's shared setup into AGENTS.md, CLAUDE.md, `.mcp.json`, and the config files for Cursor, VS Code and Codex.

You can also download a CLI binary for your platform from [Releases](https://github.com/OrchestrAIio/orchestrai/releases) and check it against the `orchestrai_<version>_checksums.txt` file in the same release. To build from source, run `go install github.com/OrchestrAIio/orchestrai/cli@latest`.

Claude Code:

```text
/plugin marketplace add OrchestrAIio/orchestrai
/plugin install orchestrai@orchestrai
```

## Editor extension: OrchestrAI Live

Live co-coding, team chat with the OrchestrAI agent, and a shared library of rules, skills, workflows and MCP servers for your team. It works in VS Code, Cursor, Windsurf, Kiro and VSCodium.

- [VS Code Marketplace](https://marketplace.visualstudio.com/items?itemName=orchestrai.orchestrai-live)
- [Open VSX](https://open-vsx.org/extension/orchestrai/orchestrai-live) (Cursor, Windsurf, Kiro, VSCodium)

The extension is free to use but isn't open source. Report its bugs here.

## Contributing and support

- Bugs and feature requests: [open an issue](https://github.com/OrchestrAIio/orchestrai/issues/new).
- Pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).
- Security reports: see [SECURITY.md](SECURITY.md). Please don't open a public issue.
- Everything else: support@orchestrai.io

## Licence

Everything in this repository is [MIT](LICENSE). The OrchestrAI service, platform and editor extension are covered by the [OrchestrAI Terms of Service](https://app.orchestrai.io/terms).
