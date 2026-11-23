import type { BridgeConfig } from './config.js';
import { PACKAGE_NAME, PACKAGE_VERSION } from './config.js';
import { CredentialStore, platformKeychain, type StoredCredential } from './credentials.js';
import {
  openBrowser,
  pollDeviceAuthorization,
  startDeviceAuthorization,
  type DeviceAuthorization,
  type DeviceFlowOptions,
} from './device-flow.js';
import { OrchestraiApi } from './api.js';
import type { Logger } from './log.js';
import { errorMessage } from './log.js';

export const DEVICE_AUTH_METHOD_ID = 'orchestrai-browser-login';
export const TERMINAL_AUTH_METHOD_ID = 'orchestrai-terminal-login';

export type ResolvedKey = { apiKey: string; source: 'env' | 'stored'; credential: StoredCredential | null };

export function userAgent(clientName?: string): string {
  return `${PACKAGE_NAME}/${PACKAGE_VERSION}${clientName ? ` (${clientName})` : ''} node/${process.versions.node}`;
}

export function createCredentialStore(config: BridgeConfig, log: Logger): CredentialStore {
  return new CredentialStore(config.configDir, config.useKeychain ? platformKeychain() : null, log);
}

/** ORCHESTRAI_API_KEY wins; otherwise the key stored by a previous sign-in. */
export async function resolveApiKey(config: BridgeConfig, store: CredentialStore): Promise<ResolvedKey | null> {
  if (config.envApiKey) return { apiKey: config.envApiKey, source: 'env', credential: null };
  const stored = await store.load(config.apiUrl);
  return stored ? { apiKey: stored.apiKey, source: 'stored', credential: stored } : null;
}

export interface SignInHooks {
  /** Shown to the person: the code and the URL to approve it at. */
  onCode(auth: DeviceAuthorization, browserOpened: boolean): void;
}

/** Runs the device flow end to end and persists the key. */
export async function signInWithDeviceFlow(
  config: BridgeConfig,
  store: CredentialStore,
  hooks: SignInHooks,
  opts: { signal?: AbortSignal; fetchImpl?: DeviceFlowOptions['fetchImpl']; sleep?: DeviceFlowOptions['sleep']; open?: (url: string) => boolean } = {},
): Promise<{ credential: StoredCredential; storedIn: 'keychain' | 'file' }> {
  const flowOpts: DeviceFlowOptions = {
    apiUrl: config.apiUrl,
    client: 'acp',
    signal: opts.signal,
    fetchImpl: opts.fetchImpl,
    sleep: opts.sleep,
  };
  const auth = await startDeviceAuthorization(flowOpts);
  const opened = (opts.open ?? openBrowser)(auth.verification_uri_complete);
  hooks.onCode(auth, opened);
  const result = await pollDeviceAuthorization(flowOpts, auth);
  const credential: StoredCredential = {
    apiUrl: config.apiUrl,
    apiKey: result.apiKey,
    user: result.user,
    source: 'device_flow',
    createdAt: new Date().toISOString(),
  };
  const storedIn = await store.save(credential);
  return { credential, storedIn };
}

/**
 * Logout: forget the stored key and, for keys this bridge minted through the
 * device flow, revoke it server-side (matched by key prefix). Keys supplied via
 * ORCHESTRAI_API_KEY are the user's own and are never revoked.
 */
export async function signOut(
  config: BridgeConfig,
  store: CredentialStore,
  log: Logger,
  fetchImpl?: (input: string, init?: RequestInit) => Promise<Response>,
): Promise<{ revoked: boolean; hadCredential: boolean }> {
  const stored = await store.load(config.apiUrl);
  let revoked = false;
  if (stored && stored.source === 'device_flow') {
    try {
      const api = new OrchestraiApi(config.apiUrl, stored.apiKey, userAgent(), fetchImpl);
      const prefix = stored.apiKey.slice(0, 12);
      const keys = await api.listApiKeys();
      const match = keys.find((k) => k.key_prefix === prefix);
      if (match) {
        await api.revokeApiKey(match.id);
        revoked = true;
      }
    } catch (err) {
      log.warn(`Could not revoke the editor API key server-side (${errorMessage(err)}); revoke it in Settings → API keys.`);
    }
  }
  await store.clear(config.apiUrl);
  return { revoked, hadCredential: stored !== null };
}
