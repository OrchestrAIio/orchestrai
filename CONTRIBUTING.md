# Contributing

Thanks for helping improve the OrchestrAI clients.

## How changes land

This repository is a published copy. The clients are developed alongside the OrchestrAI platform in a private repository, and every change there is copied here automatically. So we don't merge pull requests here directly. Instead:

1. Open a pull request here as usual. CI runs the tests.
2. A maintainer reviews it. Once it's approved, they apply your commits to the private repository with your authorship kept.
3. The next sync brings your change here, and the pull request is closed with a link to the sync commit.

Your name stays on the commit, and the change ships in the next release.

## Before you open a pull request

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Keep each pull request to one change, and include tests.
- By contributing, you agree that your contribution is licensed under the [MIT licence](LICENSE).

## Running the tests

| Directory | Commands |
|---|---|
| `cli/` | `go vet ./... && go test -race ./...` and `node --test npm/test/` |
| `acp-bridge/` | `npm ci && npm test` |
| Plugin (repository root) | `claude plugin validate --strict . && claude plugin validate --strict ./plugins/orchestrai` |

The CLI needs Go 1.25 or later, and the ACP bridge needs Node 20.11 or later.

## Security-sensitive areas

Some changes get extra review:

- In the CLI: the path-safety checks in `cli/internal/syncer/`, the rules for which hosts receive an API key in `cli/internal/cli/`, and credential storage in `cli/internal/creds/`.
- In the bridge: which runs it adopts in `acp-bridge/src/session.ts`.

If you find a weakness, report it privately as described in [SECURITY.md](SECURITY.md).
