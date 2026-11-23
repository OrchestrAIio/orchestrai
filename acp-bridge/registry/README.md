# ACP Registry submission

`orchestrai/` is the entry for [agentclientprotocol/registry](https://github.com/agentclientprotocol/registry):
`agent.json` (schema: `agent.schema.json` in that repo) and a 16×16 `currentColor` `icon.svg`.
`test/registry.test.ts` mirrors the registry's CI checks (fields, id, version pinning, icon).

## Steps

1. Publish the package: `cd clients/acp-bridge && npm publish --access public`.
   The registry CI checks that `@orchestrai/acp@<version>` exists on npm and that `license_url`
   (served by unpkg from the published tarball) returns 200.
2. Fork `agentclientprotocol/registry`, copy `registry/orchestrai/` to the fork root as `orchestrai/`.
3. Validate locally in the fork:
   ```bash
   uv run --with jsonschema .github/workflows/build_registry.py
   python3 .github/workflows/verify_agents.py --auth-check --agent orchestrai
   ```
   The auth check launches the agent and requires `authMethods` with `type: "agent"` or
   `type: "terminal"` in the `initialize` response. The bridge always returns
   `orchestrai-browser-login` (`type: "agent"`, device flow) and adds `orchestrai-terminal-login`
   (`type: "terminal"`, `args: ["login"]`) when the client advertises `auth.terminal`.
4. Open the PR. After merge the registry bumps the version automatically (hourly) from npm.

## Before submitting

- The device-flow endpoints (`/api/v1/editor/connect/start|poll` and the `/live/connect` web page)
  must be live in production; until then `authenticate` answers with "set ORCHESTRAI_API_KEY".
- Keep `version`, the `npx.package` pin and the `license_url` version in step with `package.json`
  (the registry test enforces it).
