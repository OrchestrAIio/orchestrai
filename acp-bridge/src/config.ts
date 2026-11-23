import os from 'node:os';
import path from 'node:path';

export const PACKAGE_NAME = '@orchestrai/acp';
export const PACKAGE_VERSION = '0.1.0';

/** Production API host (backend-go on Railway; see web-qwik/vercel.json rewrites). */
export const DEFAULT_API_URL = 'https://orchestrai-api-production.up.railway.app';
/** Production web app (FRONTEND_URL in production). */
export const DEFAULT_WEB_URL = 'https://app.orchestrai.io';

export interface BridgeConfig {
  /** Base URL of the OrchestrAI API (no trailing slash). */
  apiUrl: string;
  /** Base URL of the OrchestrAI web app, used for approval / billing links. */
  webUrl: string;
  /** Project pinned by ORCHESTRAI_PROJECT_ID; otherwise chosen per session. */
  projectId: string | null;
  /** API key from ORCHESTRAI_API_KEY (takes precedence over stored credentials). */
  envApiKey: string | null;
  /** Directory holding credentials.json and sessions.json. */
  configDir: string;
  /** Use the OS keychain (macOS Keychain / libsecret) when available. */
  useKeychain: boolean;
  /** Upper bound on one prompt turn while nothing arrives from the agent. */
  turnIdleTimeoutMs: number;
  /** Wait after the conversational reply finishes for a spawned run to show up. */
  runGraceMs: number;
  /** Write diagnostics to stderr. */
  debug: boolean;
}

function trimSlash(value: string): string {
  return value.replace(/\/+$/, '');
}

function validHttpUrl(name: string, raw: string): string {
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new Error(`${name} is not a valid URL: ${raw}`);
  }
  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') {
    throw new Error(`${name} must be an http(s) URL: ${raw}`);
  }
  if (
    parsed.protocol === 'http:' &&
    !['localhost', '127.0.0.1', '[::1]', '::1'].includes(parsed.hostname)
  ) {
    throw new Error(`${name} must use https (plain http is only allowed for localhost): ${raw}`);
  }
  return trimSlash(parsed.toString());
}

function positiveNumber(name: string, raw: string | undefined, fallback: number): number {
  if (raw === undefined || raw.trim() === '') return fallback;
  const value = Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${name} must be a positive number, got: ${raw}`);
  }
  return value;
}

export function defaultConfigDir(env: NodeJS.ProcessEnv = process.env): string {
  if (env.ORCHESTRAI_CONFIG_DIR) return env.ORCHESTRAI_CONFIG_DIR;
  if (process.platform === 'win32' && env.APPDATA) {
    return path.join(env.APPDATA, 'orchestrai');
  }
  const base = env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config');
  return path.join(base, 'orchestrai');
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): BridgeConfig {
  const apiKey = env.ORCHESTRAI_API_KEY?.trim() || null;
  if (apiKey && !apiKey.startsWith('ork_')) {
    throw new Error('ORCHESTRAI_API_KEY must be an OrchestrAI API key (it starts with "ork_").');
  }
  const keychainFlag = (env.ORCHESTRAI_KEYCHAIN ?? '').trim().toLowerCase();
  return {
    apiUrl: validHttpUrl('ORCHESTRAI_API_URL', env.ORCHESTRAI_API_URL?.trim() || DEFAULT_API_URL),
    webUrl: validHttpUrl('ORCHESTRAI_WEB_URL', env.ORCHESTRAI_WEB_URL?.trim() || DEFAULT_WEB_URL),
    projectId: env.ORCHESTRAI_PROJECT_ID?.trim() || null,
    envApiKey: apiKey,
    configDir: defaultConfigDir(env),
    useKeychain: !['0', 'false', 'off', 'no'].includes(keychainFlag),
    turnIdleTimeoutMs:
      positiveNumber('ORCHESTRAI_TURN_TIMEOUT_SECONDS', env.ORCHESTRAI_TURN_TIMEOUT_SECONDS, 600) *
      1000,
    runGraceMs: positiveNumber('ORCHESTRAI_RUN_GRACE_MS', env.ORCHESTRAI_RUN_GRACE_MS, 2500),
    debug: ['1', 'true', 'yes'].includes((env.ORCHESTRAI_ACP_DEBUG ?? '').toLowerCase()),
  };
}
