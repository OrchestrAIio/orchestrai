/**
 * End to end: spawns the built bridge (`node dist/index.js`, what `npx
 * @orchestrai/acp` runs) and drives it over stdio with the ACP SDK's client
 * side, against the mock OrchestrAI backend.
 */
import assert from 'node:assert/strict';
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { Readable, Writable } from 'node:stream';
import { fileURLToPath } from 'node:url';
import { after, afterEach, before, beforeEach, describe, it } from 'node:test';
import * as acp from '@agentclientprotocol/sdk';
import type { SessionNotification } from '@agentclientprotocol/sdk';
import { tempDir } from './helpers/harness.js';
import { MockBackend, waitFor } from './helpers/mock-backend.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const BIN = path.resolve(here, '..', 'dist', 'index.js');

let backend: MockBackend;
let tmp: { dir: string; cleanup: () => void };
let children: ChildProcessWithoutNullStreams[] = [];

before(async () => {
  assert.ok(existsSync(BIN), 'run `npm run build` first (npm test does)');
  backend = new MockBackend();
  await backend.start();
});
after(async () => backend.stop());
beforeEach(() => {
  tmp = tempDir();
});
afterEach(() => {
  for (const c of children) c.kill();
  children = [];
  tmp.cleanup();
});

function spawnBridge(env: Record<string, string>, args: string[] = []): ChildProcessWithoutNullStreams {
  const child = spawn(process.execPath, [BIN, ...args], {
    env: {
      PATH: process.env.PATH ?? '',
      HOME: tmp.dir,
      ORCHESTRAI_API_URL: backend.url,
      ORCHESTRAI_WEB_URL: 'https://app.example.test',
      ORCHESTRAI_CONFIG_DIR: tmp.dir,
      ORCHESTRAI_KEYCHAIN: '0',
      ORCHESTRAI_NO_BROWSER: '1',
      ORCHESTRAI_RUN_GRACE_MS: '150',
      ...env,
    },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  children.push(child);
  return child;
}

async function withClient<T>(child: ChildProcessWithoutNullStreams, op: (ctx: acp.ClientContext, updates: SessionNotification[]) => Promise<T>): Promise<T> {
  const updates: SessionNotification[] = [];
  const stream = acp.ndJsonStream(
    Writable.toWeb(child.stdin) as WritableStream<Uint8Array>,
    Readable.toWeb(child.stdout) as ReadableStream<Uint8Array>,
  );
  return acp
    .client({ name: 'e2e' })
    .onNotification(acp.methods.client.session.update, (c) => {
      updates.push(c.params);
    })
    .onRequest(acp.methods.client.session.requestPermission, () => ({ outcome: { outcome: 'cancelled' } }))
    .connectWith(stream, (ctx) => op(ctx, updates));
}

const textOf = (updates: SessionNotification[], sessionId: string, kind: string) =>
  updates
    .filter((u) => u.sessionId === sessionId && u.update.sessionUpdate === kind)
    .map((u) => ('content' in u.update && u.update.content && 'text' in u.update.content ? String(u.update.content.text) : ''))
    .join('');

describe('bridge subprocess over stdio', () => {
  it('initialize → session/new → prompt streams the reply → session/list → session/load replays teammates', async () => {
    const alice = backend.addUser('Alice', 'alice@example.com');
    const bob = backend.addUser('Bob', 'bob@example.com');
    const projectId = backend.addProject('Platform', alice.id);
    const shared = backend.addConversation(projectId, bob.id, 'On-call handoff', { access_role: 'driver', owner_name: 'Bob', visibility: 'project' });
    backend.addMessage(shared.id, 'user', 'Disk alert on db-2 again', bob.id);
    backend.addMessage(shared.id, 'assistant', 'db-2 /var is at 91%.', bob.id);
    backend.chatResponder = ({ body }) => {
      setTimeout(() => {
        backend.tokens(projectId, body.conversation_id, 'Three ');
        backend.tokens(projectId, body.conversation_id, 'clusters.', true);
      }, 30);
    };

    const child = spawnBridge({ ORCHESTRAI_API_KEY: alice.key });
    await withClient(child, async (ctx, updates) => {
      const init = await ctx.request(acp.methods.agent.initialize, {
        protocolVersion: acp.PROTOCOL_VERSION,
        clientCapabilities: { auth: { terminal: true } },
        clientInfo: { name: 'zed', version: '1.0.0' },
      });
      assert.equal(init.agentCapabilities?.loadSession, true);
      assert.ok(init.agentCapabilities?.sessionCapabilities?.list);
      assert.deepEqual(init.authMethods?.map((m) => m.id), ['orchestrai-browser-login', 'orchestrai-terminal-login']);

      const { sessionId } = await ctx.request(acp.methods.agent.session.new, { cwd: tmp.dir, mcpServers: [] });
      const res = await ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'How many EKS clusters?' }] });
      assert.equal(res.stopReason, 'end_turn');
      assert.equal(textOf(updates, sessionId, 'agent_message_chunk'), 'Three clusters.');

      const list = await ctx.request(acp.methods.agent.session.list, { cwd: tmp.dir });
      const titles = list.sessions.map((s) => s.title);
      assert.ok(titles.includes('On-call handoff — shared by Bob'));
      assert.ok(titles.includes('How many EKS clusters?'));
      // The thread created here is listed under the id the editor already has.
      assert.ok(list.sessions.some((s) => s.sessionId === sessionId));

      const sharedId = `${projectId}:${shared.id}`;
      await ctx.request(acp.methods.agent.session.load, { sessionId: sharedId, cwd: tmp.dir, mcpServers: [] });
      assert.equal(textOf(updates, sharedId, 'user_message_chunk'), '**Bob:** Disk alert on db-2 again');
      assert.equal(textOf(updates, sharedId, 'agent_message_chunk'), 'db-2 /var is at 91%.');
    });
    const conv = backend.conversations.find((c) => c.title === 'How many EKS clusters?');
    assert.ok(conv);
    await waitFor(() => backend.messagesOf(conv.id).some((m) => m.role === 'assistant' && m.content === 'Three clusters.'), 3000, 'reply persisted');
  });

  it('reports auth_required without a key, and `orchestrai-acp login` (terminal auth) signs in through the device flow', async () => {
    backend.devicePendingPolls = 1;
    const child = spawnBridge({});
    await withClient(child, async (ctx) => {
      await ctx.request(acp.methods.agent.initialize, { protocolVersion: acp.PROTOCOL_VERSION, clientCapabilities: {} });
      await assert.rejects(ctx.request(acp.methods.agent.session.new, { cwd: tmp.dir, mcpServers: [] }), /Authentication required/);
    });

    const login = spawnBridge({}, ['login']);
    let out = '';
    login.stdout.on('data', (d: Buffer) => (out += d.toString()));
    const code = await new Promise<number | null>((resolve) => login.on('close', resolve));
    assert.equal(code, 0, out);
    assert.match(out, /Code: ABCD-EFGH/);
    assert.match(out, /Signed in as device@example\.com/);

    const device = [...backend.users.values()].find((u) => u.name === 'Device');
    assert.ok(device);
    backend.addProject('Device space', device.id);
    const again = spawnBridge({});
    await withClient(again, async (ctx) => {
      await ctx.request(acp.methods.agent.initialize, { protocolVersion: acp.PROTOCOL_VERSION, clientCapabilities: {} });
      const { sessionId } = await ctx.request(acp.methods.agent.session.new, { cwd: tmp.dir, mcpServers: [] });
      assert.match(sessionId, /^new-/);
    });
  });
});
