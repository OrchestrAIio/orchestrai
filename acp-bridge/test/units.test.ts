import assert from 'node:assert/strict';
import { readFileSync, statSync } from 'node:fs';
import path from 'node:path';
import { afterEach, beforeEach, describe, it } from 'node:test';
import { ApiError, CLIENT_HEADER, OrchestraiApi, parseErrorBody, userIdFromTicket } from '../src/api.js';
import { resolveApiKey } from '../src/auth.js';
import { billingDenialFrom, billingMessage, formatLimit } from '../src/billing.js';
import { loadConfig } from '../src/config.js';
import { CredentialStore, linuxSecretTool, macKeychain, type CommandRunner, type KeychainBackend, type StoredCredential } from '../src/credentials.js';
import { DeviceFlowError, pollDeviceAuthorization, stableDeviceName, startDeviceAuthorization } from '../src/device-flow.js';
import { silentLogger } from '../src/log.js';
import { interactionText, messageToUpdates, promptToMessage } from '../src/render.js';
import { encodeSessionId, parseSessionId, SessionIndex } from '../src/session-ids.js';
import { tempDir, testConfig } from './helpers/harness.js';

let tmp: { dir: string; cleanup: () => void };
beforeEach(() => {
  tmp = tempDir();
});
afterEach(() => tmp.cleanup());

const cred = (apiUrl: string, key = 'ork_0123456789abcdef'): StoredCredential => ({
  apiUrl,
  apiKey: key,
  user: { id: 'u1', name: 'Alice', email: 'a@example.com' },
  source: 'device_flow',
  createdAt: '2026-10-19T00:00:00.000Z',
});

describe('config', () => {
  it('defaults to production hosts and validates inputs', () => {
    const c = loadConfig({ ORCHESTRAI_CONFIG_DIR: '/x' });
    assert.equal(c.apiUrl, 'https://orchestrai-api-production.up.railway.app');
    assert.equal(c.webUrl, 'https://app.orchestrai.io');
    assert.equal(c.projectId, null);
    assert.throws(() => loadConfig({ ORCHESTRAI_API_KEY: 'sk-123' }), /ork_/);
    assert.throws(() => loadConfig({ ORCHESTRAI_API_URL: 'http://evil.example.com' }), /https/);
    assert.equal(loadConfig({ ORCHESTRAI_API_URL: 'http://localhost:8080/' }).apiUrl, 'http://localhost:8080');
    assert.equal(loadConfig({ ORCHESTRAI_KEYCHAIN: '0' }).useKeychain, false);
  });
});

describe('credentials', () => {
  it('falls back to a 0600 file in a 0700 directory and keeps entries per API', async () => {
    const dir = path.join(tmp.dir, 'nested');
    const store = new CredentialStore(dir, null, silentLogger);
    assert.equal(await store.save(cred('https://a.example')), 'file');
    await store.save(cred('https://b.example', 'ork_bbbbbbbbbbbbbbbb'));
    const file = path.join(dir, 'credentials.json');
    assert.equal(statSync(file).mode & 0o777, 0o600);
    assert.equal(statSync(dir).mode & 0o777, 0o700);
    assert.equal((await store.load('https://a.example'))?.apiKey, 'ork_0123456789abcdef');
    await store.clear('https://a.example');
    assert.equal(await store.load('https://a.example'), null);
    assert.equal((await store.load('https://b.example'))?.apiKey, 'ork_bbbbbbbbbbbbbbbb');
  });

  it('prefers the keychain and never leaves a plaintext copy behind', async () => {
    const secrets = new Map<string, string>();
    const keychain: KeychainBackend = {
      name: 'fake',
      get: async (a) => secrets.get(a) ?? null,
      set: async (a, s) => (secrets.set(a, s), true),
      remove: async (a) => void secrets.delete(a),
    };
    const fileOnly = new CredentialStore(tmp.dir, null, silentLogger);
    await fileOnly.save(cred('https://a.example', 'ork_oldoldoldoldold'));
    const store = new CredentialStore(tmp.dir, keychain, silentLogger);
    assert.equal(await store.save(cred('https://a.example')), 'keychain');
    assert.equal((await store.load('https://a.example'))?.apiKey, 'ork_0123456789abcdef');
    const onDisk = JSON.parse(readFileSync(path.join(tmp.dir, 'credentials.json'), 'utf8')) as { credentials: Record<string, unknown> };
    assert.deepEqual(onDisk.credentials, {});
  });

  it('keychain CLIs receive the secret on stdin, never in argv', async () => {
    const calls: Array<{ cmd: string; args: string[]; stdin?: string }> = [];
    const run: CommandRunner = async (cmd, args, stdin) => {
      calls.push({ cmd, args, stdin });
      return { code: 0, stdout: '', stderr: '' };
    };
    await macKeychain(run).set('https://a.example', 'SECRET"X');
    await linuxSecretTool(run).set('https://a.example', 'SECRET2');
    for (const c of calls) assert.ok(!c.args.some((a) => a.includes('SECRET')), `${c.cmd} argv leaks the secret`);
    assert.match(calls[0]?.stdin ?? '', /-w "SECRET\\"X"/);
    assert.equal(calls[1]?.stdin, 'SECRET2');
  });

  it('ORCHESTRAI_API_KEY wins over the stored key', async () => {
    const store = new CredentialStore(tmp.dir, null, silentLogger);
    await store.save(cred('https://a.example'));
    const fromEnv = await resolveApiKey(testConfig('https://a.example', tmp.dir, { envApiKey: 'ork_envenvenvenvenv' }), store);
    assert.equal(fromEnv?.source, 'env');
    const stored = await resolveApiKey(testConfig('https://a.example', tmp.dir), store);
    assert.equal(stored?.source, 'stored');
    assert.equal(await resolveApiKey(testConfig('https://other.example', tmp.dir), store), null);
  });
});

describe('device flow', () => {
  const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
  const noSleep = async () => undefined;
  const auth = { device_code: 'd', user_code: 'U', verification_uri: 'https://x/live/connect', verification_uri_complete: 'https://x/live/connect?code=U', interval: 1, expires_in: 600 };

  it('backs off on slow_down and returns the key', async () => {
    const answers = [json(428, { error: 'authorization_pending' }), json(400, { error: 'slow_down' }), json(200, { api_key: 'ork_abcdefabcdefabcd', user: { id: 'u', name: 'N', email: 'e' } })];
    const sleeps: number[] = [];
    const res = await pollDeviceAuthorization(
      { apiUrl: 'https://x', client: 'acp', fetchImpl: async () => answers.shift() as Response, sleep: async (ms) => void sleeps.push(ms) },
      auth,
    );
    assert.equal(res.apiKey, 'ork_abcdefabcdefabcd');
    assert.deepEqual(sleeps, [1000, 1000, 6000]);
  });

  it('maps expired / denied / missing endpoint to typed errors', async () => {
    await assert.rejects(
      pollDeviceAuthorization({ apiUrl: 'https://x', client: 'acp', fetchImpl: async () => json(400, { error: 'expired_token' }), sleep: noSleep }, auth),
      (e: unknown) => e instanceof DeviceFlowError && e.reason === 'expired',
    );
    await assert.rejects(
      pollDeviceAuthorization({ apiUrl: 'https://x', client: 'acp', fetchImpl: async () => json(403, { error: 'access_denied' }), sleep: noSleep }, auth),
      (e: unknown) => e instanceof DeviceFlowError && e.reason === 'denied',
    );
    await assert.rejects(
      startDeviceAuthorization({ apiUrl: 'https://x', client: 'acp', fetchImpl: async () => json(404, { error: 'Not found' }) }),
      (e: unknown) => e instanceof DeviceFlowError && e.reason === 'unavailable' && /ORCHESTRAI_API_KEY/.test(e.message),
    );
  });

  it('sends the spec request body', async () => {
    let sent: unknown = null;
    const res = await startDeviceAuthorization({
      apiUrl: 'https://x',
      client: 'acp',
      deviceName: 'laptop',
      fetchImpl: async (_u, init) => {
        sent = JSON.parse(String(init?.body));
        return json(200, { device_code: 'd', user_code: 'U', verification_uri: 'https://x/live/connect' });
      },
    });
    assert.deepEqual(sent, { client: 'acp', device_name: 'laptop' });
    assert.equal(res.verification_uri_complete, 'https://x/live/connect');
    assert.equal(res.interval, 5);
  });

  it('sends the cleaned host name as a stable device name', async () => {
    assert.equal(stableDeviceName('  ada-laptop.local\n'), 'ada-laptop.local');
    assert.equal(stableDeviceName(''), 'this device');
    assert.equal(Array.from(stableDeviceName('é'.repeat(70))).length, 64);
    let sent: Record<string, string> = {};
    await startDeviceAuthorization({
      apiUrl: 'https://x',
      client: 'acp',
      deviceName: ' lap\u0007top ',
      fetchImpl: async (_u, init) => {
        sent = JSON.parse(String(init?.body));
        return json(200, { device_code: 'd', user_code: 'U', verification_uri: 'https://x/live/connect' });
      },
    });
    assert.equal(sent.device_name, 'laptop');
  });
});

describe('api helpers', () => {
  it('normalises both backend error envelopes', () => {
    const a = parseErrorBody(402, { error: 'Insufficient credits', code: 'INSUFFICIENT_CREDITS' }, 'x');
    assert.equal(a.code, 'INSUFFICIENT_CREDITS');
    assert.equal(a.message, 'Insufficient credits');
    const b = parseErrorBody(401, { error: { code: 'UNAUTHORIZED', message: 'Invalid API key' } }, 'x');
    assert.equal(b.code, 'UNAUTHORIZED');
    assert.equal(b.message, 'Invalid API key');
  });

  it('names itself to the backend as the acp client (REST and /mcp)', async () => {
    const seen: Array<Record<string, string>> = [];
    const api = new OrchestraiApi('https://x', 'ork_k', 'ua', async (_u, init) => {
      seen.push(init?.headers as Record<string, string>);
      return new Response(JSON.stringify({ conversations: [] }), { status: 200, headers: { 'content-type': 'application/json' } });
    });
    await api.listConversations('p1');
    assert.equal(CLIENT_HEADER, 'X-OrchestrAI-Client');
    assert.equal(seen[0]?.[CLIENT_HEADER], 'acp');
    assert.equal(seen[0]?.Authorization, 'Bearer ork_k');
  });

  it('reads the user id out of a WS ticket', () => {
    assert.equal(userIdFromTicket(`${Buffer.from('user-1').toString('base64url')}.123.abc`), 'user-1');
  });
});

describe('billing', () => {
  it('never resolves links outside http(s) and keeps an existing from=', () => {
    const d = billingDenialFrom(
      new ApiError(402, 'nope', 'LIVE_UPGRADE_REQUIRED', { cta: 'upgrade', upgrade_url: 'javascript:alert(1)' }),
      'https://app.example.test',
    );
    assert.ok(d);
    assert.equal(d.kind, 'upgrade');
    assert.equal(d.upgradeUrl, 'https://app.example.test/settings/billing?from=live_acp');
    assert.equal(billingMessage(d), 'nope. Upgrade your OrchestrAI plan to lift this limit: https://app.example.test/settings/billing?from=live_acp');
    assert.equal(billingDenialFrom(new ApiError(500, 'x', null, null), 'https://w'), null);
  });

  it('formats limits by unit and never repeats the server message', () => {
    assert.equal(formatLimit('LIVE_LIMIT_LIVE_ROOM_SECONDS_MONTH', 108000), '30 hours');
    assert.equal(formatLimit('LIVE_LIMIT_LIVE_ROOM_BYTES_MONTH', 3221225472), '3 GiB');
    assert.equal(formatLimit('LIVE_LIMIT_DIGEST_LOOKBACK_DAYS', 7), '7 days');
    assert.equal(formatLimit('LIVE_LIMIT_LIVE_ROOMS', 1), '1');
    const said = billingDenialFrom(
      new ApiError(402, 'This team space used its 30 hours of live rooms this month. Upgrade to Pro to lift this limit', 'LIVE_LIMIT_LIVE_ROOM_SECONDS_MONTH', {
        cta: 'upgrade',
        limit: 108000,
        current: 108000,
        upgrade_url: 'https://w/settings/billing?from=live_room_start',
      }),
      'https://w',
    );
    assert.ok(said);
    assert.equal(
      billingMessage(said),
      'This team space used its 30 hours of live rooms this month. Upgrade to Pro to lift this limit: https://w/settings/billing?from=live_room_start',
    );
    const bare = billingDenialFrom(
      new ApiError(402, 'Live room bandwidth is used up', 'LIVE_LIMIT_LIVE_ROOM_BYTES_MONTH', { cta: 'upgrade', limit: 3221225472, current: 3221225472 }),
      'https://w',
    );
    assert.ok(bare);
    assert.equal(
      billingMessage(bare),
      'Live room bandwidth is used up (3 GiB of 3 GiB used). Upgrade your OrchestrAI plan to lift this limit: https://w/settings/billing?from=live_acp',
    );
  });
});

describe('render', () => {
  const ctx = { myUserId: 'me', names: new Map([['them', 'Dana']]), webUrl: 'https://w', projectId: 'p', conversationId: 'c' };
  it('labels teammates and keeps own messages plain', () => {
    const mine = messageToUpdates({ id: '1', conversation_id: 'c', author_user_id: 'me', role: 'user', content: 'hi', created_at: '', updated_at: '' }, ctx);
    const theirs = messageToUpdates({ id: '2', conversation_id: 'c', author_user_id: 'them', role: 'user', content: 'yo', created_at: '', updated_at: '' }, ctx);
    const unknown = messageToUpdates({ id: '3', conversation_id: 'c', author_user_id: 'ghost', role: 'user', content: 'boo', created_at: '', updated_at: '' }, ctx);
    assert.equal(mine[0]?.sessionUpdate === 'user_message_chunk' && mine[0].content.type === 'text' && mine[0].content.text, 'hi');
    assert.equal(theirs[0]?.sessionUpdate === 'user_message_chunk' && theirs[0].content.type === 'text' && theirs[0].content.text, '**Dana:** yo');
    assert.equal(unknown[0]?.sessionUpdate === 'user_message_chunk' && unknown[0].content.type === 'text' && unknown[0].content.text, '**A teammate:** boo');
  });

  it('renders grouped clarifications', () => {
    const t = interactionText({
      prompt: 'Pick',
      clarificationGroups: [
        { question: 'Region?', options: [{ id: 'a', label: 'us-east-1', value: 'us-east-1' }] },
        { question: 'Size?', options: [{ id: 'b', label: 'small', value: 'small' }] },
      ],
    });
    assert.match(t, /\*\*Region\?\*\*\n1\. \*\*us-east-1\*\*/);
    assert.match(t, /\*\*Size\?\*\*/);
  });

  it('turns embedded editor context into chat attachments', () => {
    const res = promptToMessage([
      { type: 'text', text: 'Review this' },
      { type: 'resource', resource: { uri: 'file:///repo/main.tf', mimeType: 'text/x-terraform', text: 'resource "x" {}' } },
      { type: 'resource_link', name: 'README.md', uri: 'file:///repo/README.md' },
    ]);
    assert.equal(res.text, 'Review this\n(attached main.tf)\n[README.md](file:///repo/README.md)');
    assert.equal(res.attachments[0]?.name, 'main.tf');
    assert.equal(Buffer.from(res.attachments[0]?.content_base64 ?? '', 'base64').toString(), 'resource "x" {}');
  });
});

describe('session ids', () => {
  it('round-trips conversation refs and persists new-session mappings', async () => {
    const id = encodeSessionId({ projectId: 'p1', conversationId: 'c-1' });
    assert.deepEqual(parseSessionId(id), { projectId: 'p1', conversationId: 'c-1' });
    assert.equal(parseSessionId('new-123'), null);
    assert.equal(parseSessionId('a:../../etc'), null);
    const index = new SessionIndex(tmp.dir);
    await index.remember('new-abc', { projectId: 'p1', conversationId: 'c-9' });
    const reread = new SessionIndex(tmp.dir);
    assert.deepEqual(await reread.resolve('new-abc'), { projectId: 'p1', conversationId: 'c-9' });
    assert.equal(await reread.aliasFor({ projectId: 'p1', conversationId: 'c-9' }), 'new-abc');
  });
});
