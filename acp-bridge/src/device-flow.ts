/**
 * Editor sign-in, RFC 8628 style (spec 2026-10-19 §5 "Editor sign-in"):
 *   POST /api/v1/editor/connect/start {client, device_name}
 *     → {device_code, user_code, verification_uri, verification_uri_complete, interval, expires_in}
 *   POST /api/v1/editor/connect/poll {device_code}
 *     → 428 {"error":"authorization_pending"} | {api_key: "ork_…", user: {id, name, email}}
 * Both endpoints are unauthenticated.
 */
import { spawn } from 'node:child_process';
import os from 'node:os';

export interface DeviceAuthorization {
  device_code: string;
  user_code: string;
  verification_uri: string;
  verification_uri_complete: string;
  interval: number;
  expires_in: number;
}

export interface DeviceFlowResult {
  apiKey: string;
  user: { id: string; name: string; email: string } | null;
}

export class DeviceFlowError extends Error {
  constructor(
    message: string,
    readonly reason:
      | 'unavailable'
      | 'expired'
      | 'denied'
      | 'cancelled'
      | 'invalid_response'
      | 'network'
      | 'rate_limited',
  ) {
    super(message);
    this.name = 'DeviceFlowError';
  }
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface DeviceFlowOptions {
  apiUrl: string;
  client: string;
  deviceName?: string;
  fetchImpl?: FetchLike;
  signal?: AbortSignal;
  /** Injectable sleeper for tests. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

const defaultSleep = (ms: number, signal?: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DeviceFlowError('Sign-in was cancelled.', 'cancelled'));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(new DeviceFlowError('Sign-in was cancelled.', 'cancelled'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });
  });

async function postJson(
  fetchImpl: FetchLike,
  url: string,
  body: unknown,
  signal?: AbortSignal,
): Promise<{ status: number; json: Record<string, unknown> | null }> {
  let res: Response;
  try {
    const signals = [AbortSignal.timeout(30_000)];
    if (signal) signals.push(signal);
    res = await fetchImpl(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(body),
      signal: AbortSignal.any(signals),
    });
  } catch (err) {
    if (signal?.aborted) throw new DeviceFlowError('Sign-in was cancelled.', 'cancelled');
    throw new DeviceFlowError(
      `Could not reach OrchestrAI: ${err instanceof Error ? err.message : String(err)}`,
      'network',
    );
  }
  const text = await res.text();
  let json: Record<string, unknown> | null = null;
  try {
    const parsed: unknown = text ? JSON.parse(text) : null;
    json = parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : null;
  } catch {
    json = null;
  }
  return { status: res.status, json };
}

function errorCode(json: Record<string, unknown> | null): string {
  if (!json) return '';
  const e = json.error;
  if (typeof e === 'string') return e;
  if (e && typeof e === 'object' && typeof (e as Record<string, unknown>).code === 'string') {
    return String((e as Record<string, unknown>).code).toLowerCase();
  }
  return typeof json.code === 'string' ? json.code.toLowerCase() : '';
}

/**
 * The `device_name` sent with the device-flow start: the machine's host name,
 * cleaned the way the server stores it (no control characters, trimmed, at
 * most 64 characters), or "this device". The editor extension and the CLI
 * send the same value from the same machine; the API has no separate device
 * identifier field.
 */
export function stableDeviceName(hostname: string): string {
  // eslint-disable-next-line no-control-regex
  const cleaned = hostname.replace(/[\u0000-\u001f\u007f-\u009f]/g, '').trim();
  const name = Array.from(cleaned).slice(0, 64).join('').trim();
  return name || 'this device';
}

export async function startDeviceAuthorization(opts: DeviceFlowOptions): Promise<DeviceAuthorization> {
  const fetchImpl = opts.fetchImpl ?? ((i, init) => fetch(i, init));
  const { status, json } = await postJson(
    fetchImpl,
    `${opts.apiUrl}/api/v1/editor/connect/start`,
    { client: opts.client, device_name: stableDeviceName(opts.deviceName ?? os.hostname()) },
    opts.signal,
  );
  if (status === 404 || status === 405 || status === 501) {
    throw new DeviceFlowError(
      'This OrchestrAI server does not support editor sign-in yet. Create an API key in Settings → API keys and set ORCHESTRAI_API_KEY instead.',
      'unavailable',
    );
  }
  if (status === 429) {
    throw new DeviceFlowError('Too many sign-in attempts from this network; wait a minute and retry.', 'rate_limited');
  }
  if (status < 200 || status >= 300 || !json) {
    throw new DeviceFlowError(`Editor sign-in could not start (HTTP ${status}).`, 'invalid_response');
  }
  const required = ['device_code', 'user_code', 'verification_uri'] as const;
  for (const key of required) {
    if (typeof json[key] !== 'string' || (json[key] as string).length === 0) {
      throw new DeviceFlowError(`Editor sign-in response is missing ${key}.`, 'invalid_response');
    }
  }
  const verificationUri = String(json.verification_uri);
  return {
    device_code: String(json.device_code),
    user_code: String(json.user_code),
    verification_uri: verificationUri,
    verification_uri_complete:
      typeof json.verification_uri_complete === 'string' && json.verification_uri_complete
        ? json.verification_uri_complete
        : verificationUri,
    interval: typeof json.interval === 'number' && json.interval > 0 ? json.interval : 5,
    expires_in: typeof json.expires_in === 'number' && json.expires_in > 0 ? json.expires_in : 600,
  };
}

export async function pollDeviceAuthorization(
  opts: DeviceFlowOptions,
  auth: DeviceAuthorization,
): Promise<DeviceFlowResult> {
  const fetchImpl = opts.fetchImpl ?? ((i, init) => fetch(i, init));
  const sleep = opts.sleep ?? defaultSleep;
  let intervalMs = auth.interval * 1000;
  const deadline = Date.now() + auth.expires_in * 1000;
  for (;;) {
    await sleep(intervalMs, opts.signal);
    if (Date.now() > deadline) {
      throw new DeviceFlowError('The sign-in code expired before it was approved. Start again.', 'expired');
    }
    let response: { status: number; json: Record<string, unknown> | null };
    try {
      response = await postJson(
        fetchImpl,
        `${opts.apiUrl}/api/v1/editor/connect/poll`,
        { device_code: auth.device_code },
        opts.signal,
      );
    } catch (err) {
      // A transient network blip must not abort a sign-in the user is completing.
      if (err instanceof DeviceFlowError && err.reason === 'network') continue;
      throw err;
    }
    const { status, json } = response;
    const code = errorCode(json);
    if (status >= 200 && status < 300 && json) {
      const apiKey = json.api_key;
      if (typeof apiKey !== 'string' || !apiKey.startsWith('ork_')) {
        throw new DeviceFlowError('Sign-in succeeded but no API key was returned.', 'invalid_response');
      }
      const u = json.user;
      const user =
        u && typeof u === 'object'
          ? {
              id: String((u as Record<string, unknown>).id ?? ''),
              name: String((u as Record<string, unknown>).name ?? ''),
              email: String((u as Record<string, unknown>).email ?? ''),
            }
          : null;
      return { apiKey, user };
    }
    if (status === 428 || code === 'authorization_pending') continue;
    if (status === 429 || code === 'slow_down') {
      intervalMs += 5000;
      continue;
    }
    if (code === 'access_denied' || status === 403) {
      throw new DeviceFlowError('Sign-in was declined in the browser.', 'denied');
    }
    if (code === 'expired_token' || status === 410 || status === 404) {
      throw new DeviceFlowError('The sign-in code expired before it was approved. Start again.', 'expired');
    }
    if (status >= 500) continue;
    throw new DeviceFlowError(`Editor sign-in failed (HTTP ${status}${code ? `, ${code}` : ''}).`, 'invalid_response');
  }
}

/** Opens a URL in the user's browser without a shell; failures are non-fatal. */
export function openBrowser(url: string): boolean {
  // Headless machines / CI: print the link only.
  if (['1', 'true', 'yes'].includes((process.env.ORCHESTRAI_NO_BROWSER ?? '').toLowerCase())) return false;
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return false;
  }
  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') return false;
  const target = parsed.toString();
  const [cmd, args] =
    process.platform === 'darwin'
      ? ['open', [target]]
      : process.platform === 'win32'
        ? ['rundll32', ['url.dll,FileProtocolHandler', target]]
        : ['xdg-open', [target]];
  try {
    const child = spawn(cmd, args as string[], { stdio: 'ignore', detached: true, windowsHide: true });
    child.on('error', () => undefined);
    child.unref();
    return true;
  } catch {
    return false;
  }
}
