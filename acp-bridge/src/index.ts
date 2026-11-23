#!/usr/bin/env node
/**
 * orchestrai-acp
 *   (no args)  run the ACP agent over stdio (what editors launch)
 *   login      sign in with the device flow (also the ACP terminal-auth entry)
 *   logout     forget (and revoke) the stored editor key
 *   status     show which API, key source and user this bridge would use
 */
import { OrchestraiAcpAgent } from './agent.js';
import { OrchestraiApi, userIdFromTicket } from './api.js';
import { createCredentialStore, resolveApiKey, signInWithDeviceFlow, signOut, userAgent } from './auth.js';
import { loadConfig, PACKAGE_NAME, PACKAGE_VERSION, type BridgeConfig } from './config.js';
import { DeviceFlowError } from './device-flow.js';
import { createLogger, errorMessage } from './log.js';

const HELP = `${PACKAGE_NAME} ${PACKAGE_VERSION} — OrchestrAI agent for ACP editors (Zed, JetBrains, Neovim, …)

Usage:
  orchestrai-acp            Run the ACP agent on stdio (configure this in your editor)
  orchestrai-acp login      Sign in through your browser (stores an editor API key)
  orchestrai-acp logout     Remove and revoke the stored editor API key
  orchestrai-acp status     Show the API, key source and signed-in user

Environment:
  ORCHESTRAI_API_KEY        API key (ork_…) — overrides the stored sign-in
  ORCHESTRAI_API_URL        API base URL (default https://orchestrai-api-production.up.railway.app)
  ORCHESTRAI_WEB_URL        Web app URL for links (default https://app.orchestrai.io)
  ORCHESTRAI_PROJECT_ID     Pin every new conversation to this project
  ORCHESTRAI_KEYCHAIN=0     Store the key in ~/.config/orchestrai/credentials.json instead of the OS keychain
  ORCHESTRAI_ACP_DEBUG=1    Verbose logs on stderr
`;

async function login(config: BridgeConfig): Promise<number> {
  const log = createLogger(config.debug);
  const store = createCredentialStore(config, log);
  const controller = new AbortController();
  process.once('SIGINT', () => controller.abort());
  try {
    const { credential, storedIn } = await signInWithDeviceFlow(
      config,
      store,
      {
        onCode: (auth, opened) => {
          process.stdout.write(
            `\nTo connect this editor to OrchestrAI, approve it in your browser:\n\n  ${auth.verification_uri_complete}\n\n` +
              `Code: ${auth.user_code}\n${opened ? '(Your browser should have opened; if not, open the link above.)\n' : ''}\nWaiting for approval… (Ctrl+C to cancel)\n`,
          );
        },
      },
      { signal: controller.signal },
    );
    process.stdout.write(
      `\nSigned in${credential.user?.email ? ` as ${credential.user.email}` : ''}. ` +
        `The key is stored in ${storedIn === 'keychain' ? 'your OS keychain' : store.credentialsPath}.\n`,
    );
    return 0;
  } catch (err) {
    process.stderr.write(`\nSign-in failed: ${err instanceof DeviceFlowError ? err.message : errorMessage(err)}\n`);
    return 1;
  }
}

async function logout(config: BridgeConfig): Promise<number> {
  const log = createLogger(config.debug);
  const store = createCredentialStore(config, log);
  const result = await signOut(config, store, log);
  process.stdout.write(
    result.hadCredential
      ? `Signed out${result.revoked ? ' and revoked the editor API key' : ''}.\n`
      : 'No stored sign-in for this API.\n',
  );
  if (config.envApiKey) process.stdout.write('Note: ORCHESTRAI_API_KEY is still set in your environment.\n');
  return 0;
}

async function status(config: BridgeConfig): Promise<number> {
  const log = createLogger(config.debug);
  const store = createCredentialStore(config, log);
  const resolved = await resolveApiKey(config, store);
  process.stdout.write(`API: ${config.apiUrl}\nWeb: ${config.webUrl}\n`);
  if (!resolved) {
    process.stdout.write('Not signed in. Run `orchestrai-acp login` or set ORCHESTRAI_API_KEY.\n');
    return 1;
  }
  process.stdout.write(`Key: ${resolved.apiKey.slice(0, 12)}… (${resolved.source === 'env' ? 'ORCHESTRAI_API_KEY' : 'stored sign-in'})\n`);
  const api = new OrchestraiApi(config.apiUrl, resolved.apiKey, userAgent('cli'));
  try {
    const { ticket } = await api.createWsTicket();
    const projects = await api.listProjects();
    process.stdout.write(`User id: ${userIdFromTicket(ticket) ?? 'unknown'}\nProjects: ${projects.map((p) => p.name).join(', ') || 'none'}\n`);
    return 0;
  } catch (err) {
    process.stdout.write(`The key could not be used: ${errorMessage(err)}\n`);
    return 1;
  }
}

async function main(argv: string[]): Promise<number> {
  const cmd = argv[0];
  if (cmd === '--help' || cmd === '-h' || cmd === 'help') {
    process.stdout.write(HELP);
    return 0;
  }
  if (cmd === '--version' || cmd === '-v') {
    process.stdout.write(`${PACKAGE_VERSION}\n`);
    return 0;
  }
  let config: BridgeConfig;
  try {
    config = loadConfig();
  } catch (err) {
    process.stderr.write(`orchestrai-acp: ${errorMessage(err)}\n`);
    return 2;
  }
  switch (cmd) {
    case undefined:
    case 'acp':
    case 'serve': {
      const log = createLogger(config.debug);
      const agent = new OrchestraiAcpAgent({ config, log });
      await agent.serveStdio();
      return 0;
    }
    case 'login':
      return login(config);
    case 'logout':
      return logout(config);
    case 'status':
      return status(config);
    default:
      process.stderr.write(`Unknown command: ${cmd}\n\n${HELP}`);
      return 2;
  }
}

main(process.argv.slice(2)).then(
  (code) => {
    process.exitCode = code;
  },
  (err: unknown) => {
    process.stderr.write(`orchestrai-acp: ${errorMessage(err)}\n`);
    process.exitCode = 1;
  },
);
