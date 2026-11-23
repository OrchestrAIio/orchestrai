import assert from 'node:assert/strict';
import { statSync } from 'node:fs';
import path from 'node:path';
import { after, afterEach, before, beforeEach, describe, it } from 'node:test';
import * as acp from '@agentclientprotocol/sdk';
import { connectHarness, tempDir, testConfig, type Harness } from './helpers/harness.js';
import { MockBackend, sleep, waitFor } from './helpers/mock-backend.js';

let backend: MockBackend;
let tmp: { dir: string; cleanup: () => void };
let harness: Harness | null = null;
let alice: { id: string; key: string };
let bob: { id: string; key: string };

const later = (fn: () => void, ms = 20) => setTimeout(fn, ms);

before(async () => {
  backend = new MockBackend();
  await backend.start();
});

after(async () => {
  await backend.stop();
});

beforeEach(() => {
  backend.projects.length = 0;
  backend.conversations.length = 0;
  backend.messages.length = 0;
  backend.requests.length = 0;
  backend.stoppedRuns.length = 0;
  backend.chatResponder = null;
  alice = backend.addUser('Alice', 'alice@example.com');
  bob = backend.addUser('Bob', 'bob@example.com');
  tmp = tempDir();
});

afterEach(() => {
  harness?.close();
  harness = null;
  tmp.cleanup();
});

async function newSession(h: Harness): Promise<string> {
  const res = await h.ctx.request(acp.methods.agent.session.new, { cwd: process.cwd(), mcpServers: [] });
  return res.sessionId;
}

describe('session/new + session/prompt', () => {
  it('creates the conversation on first prompt, streams tokens and persists both sides like the web chat', async () => {
    const projectId = backend.addProject('Platform', alice.id);
    backend.chatResponder = ({ body }) => {
      later(() => {
        backend.tokens(projectId, body.conversation_id, 'Your VPC ');
        backend.tokens(projectId, body.conversation_id, 'has 3 subnets.', true);
      });
    };
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = await newSession(harness);
    assert.match(sessionId, /^new-/);

    const res = await harness.ctx.request(acp.methods.agent.session.prompt, {
      sessionId,
      prompt: [{ type: 'text', text: 'How many subnets does prod have?' }],
    });
    assert.equal(res.stopReason, 'end_turn');
    assert.equal(harness.textOf(sessionId), 'Your VPC has 3 subnets.');

    const conv = backend.conversations[0];
    assert.ok(conv);
    assert.equal(conv.title, 'How many subnets does prod have?');
    const send = backend.requests.find((r) => r.path === `/api/v1/projects/${projectId}/chat/message`);
    assert.ok(send);
    const sendBody = send.body as { message: string; conversation_id: string; conversation_history: unknown[]; message_id: string };
    assert.equal(sendBody.message, 'How many subnets does prod have?');
    assert.equal(sendBody.conversation_id, conv.id);
    assert.deepEqual(sendBody.conversation_history, [{ role: 'user', content: 'How many subnets does prod have?' }]);
    assert.equal(send.headers.authorization, `Bearer ${alice.key}`);

    await waitFor(() => backend.messagesOf(conv.id).some((m) => m.role === 'assistant' && m.content === 'Your VPC has 3 subnets.'), 3000, 'reply persisted');
    const [user] = backend.messagesOf(conv.id);
    assert.equal(user?.role, 'user');
    assert.equal(user?.id, sendBody.message_id);
    assert.equal((user?.metadata as { source?: string }).source, 'acp');

    // The WebSocket used a ticket and no Authorization header.
    assert.ok(backend.requests.some((r) => r.path === '/api/v1/ws/ticket'));
    const info = harness.updatesFor(sessionId).find((u) => u.sessionUpdate === 'session_info_update');
    assert.ok(info);
  });

  it('follows a spawned run: tool calls, plan, read-only approval card with a web link, never a permission request', async () => {
    const projectId = backend.addProject('Platform', alice.id);
    backend.chatResponder = ({ body }) => {
      const conv = body.conversation_id;
      later(() => {
        backend.tokens(projectId, conv, 'Starting a run to create the bucket.', true);
        backend.status(projectId, 'run-42', 'pending', { conversation_id: conv, phase_detail: 'Analyzing' });
        backend.broadcast(projectId, 'tool_execution', {
          run_id: 'run-42',
          tool_event: { run_id: 'run-42', tool_name: 'plan_generation', status: 'completed', phase: 'planning', message: 'Generated execution plan with 2 step(s)' },
        });
        backend.status(projectId, 'run-42', 'running', {
          conversation_id: conv,
          metadata: { plan_content: '**Execution Plan** — 2 step(s), risk low' },
        });
        backend.status(projectId, 'run-42', 'awaiting_approval', {
          conversation_id: conv,
          message: 'Waiting for your approval to proceed.',
          metadata: {
            type: 'approval_required',
            approval: { id: 'approval-run-42', run_id: 'run-42', operation: 'Create S3 bucket', risk_level: 'low', affected_resources: ['s3:logs-bucket'], estimated_cost: '$0.02/mo' },
          },
        });
      });
    };
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key, runGraceMs: 400 }));
    const sessionId = await newSession(harness);
    const res = await harness.ctx.request(acp.methods.agent.session.prompt, {
      sessionId,
      prompt: [{ type: 'text', text: 'Create a logs bucket' }],
    });
    assert.equal(res.stopReason, 'end_turn');

    const updates = harness.updatesFor(sessionId);
    const runCall = updates.find((u) => u.sessionUpdate === 'tool_call' && u.toolCallId === 'orchestrai-run-run-42');
    assert.ok(runCall, 'run announced as a tool call');
    assert.ok(updates.some((u) => u.sessionUpdate === 'tool_call' && u.toolCallId.includes('plan_generation')));
    const plans = updates.filter((u) => u.sessionUpdate === 'plan');
    assert.ok(plans.length > 0);
    const lastPlan = plans.at(-1);
    assert.ok(lastPlan && lastPlan.sessionUpdate === 'plan');
    assert.equal(lastPlan.entries.find((e) => e.content.startsWith('Human approval'))?.status, 'in_progress');

    const textOut = harness.textOf(sessionId);
    assert.match(textOut, /Execution Plan/);
    assert.match(textOut, /Approval required/);
    assert.match(textOut, /only be approved by a person in the OrchestrAI web app/);
    assert.match(textOut, new RegExp(`https://app\\.example\\.test/approvals\\?project=${projectId}`));
    assert.match(textOut, /s3:logs-bucket/);
    assert.equal(harness.permissionRequests.length, 0);
    // The run is recorded in the transcript like the web app does.
    const conv = backend.conversations[0];
    assert.ok(conv);
    await waitFor(() => backend.messagesOf(conv.id).some((m) => m.role === 'run' && (m.metadata as { status?: string }).status === 'awaiting_approval'), 3000, 'run row');
  });

  it('turns a clarification into a question the user answers by typing', async () => {
    const projectId = backend.addProject('Platform', alice.id);
    backend.chatResponder = ({ body }) => {
      later(() =>
        backend.status(projectId, body.conversation_id, 'clarification', {
          metadata: {
            interaction: {
              id: 'q1',
              kind: 'clarification',
              prompt: 'Which region?',
              options: [
                { id: 'a', label: 'us-east-1', value: 'us-east-1' },
                { id: 'b', label: 'eu-west-1', value: 'eu-west-1' },
              ],
            },
          },
        }),
      );
    };
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = await newSession(harness);
    const res = await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'Create a cluster' }] });
    assert.equal(res.stopReason, 'end_turn');
    const out = harness.textOf(sessionId);
    assert.match(out, /Which region\?/);
    assert.match(out, /1\. \*\*us-east-1\*\*/);
    assert.match(out, /2\. \*\*eu-west-1\*\*/);
  });

  it('session/cancel ends the turn as cancelled and stops the spawned run', async () => {
    const projectId = backend.addProject('Platform', alice.id);
    backend.chatResponder = ({ body }) => {
      later(() => {
        backend.tokens(projectId, body.conversation_id, 'On it.', true);
        backend.status(projectId, 'run-7', 'running', { conversation_id: body.conversation_id });
      });
    };
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = await newSession(harness);
    const pending = harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'Deploy the API' }] });
    const h = harness;
    await waitFor(() => h.updatesFor(sessionId).some((u) => u.sessionUpdate === 'tool_call'), 3000, 'run started');
    await harness.ctx.notify(acp.methods.agent.session.cancel, { sessionId });
    const res = await pending;
    assert.equal(res.stopReason, 'cancelled');
    await waitFor(() => backend.stoppedRuns.includes('run-7'), 3000, 'stop called');
    assert.ok(backend.requests.some((r) => r.path === `/api/v1/projects/${projectId}/agent-runs/run-7/stop` && r.method === 'POST'));
  });
});

describe('Free plan (pay as you go) 402 handling', () => {
  it('zero balance → buy-credits agent message, not an error', async () => {
    backend.addProject('Platform', alice.id);
    backend.chatResponder = () => ({
      status: 402,
      body: { error: 'Insufficient credits. Purchase more credits to continue.', code: 'INSUFFICIENT_CREDITS', cta: 'buy_credits', upgrade_url: '/settings/billing', buy_credits_url: '/settings/billing?buy=credits' },
    });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = await newSession(harness);
    const res = await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'hello' }] });
    assert.equal(res.stopReason, 'end_turn');
    assert.equal(
      harness.textOf(sessionId),
      'The OrchestrAI agent runs on credits — buy credits: https://app.example.test/settings/billing?buy=credits&from=live_acp (or upgrade to Pro for included credits: https://app.example.test/settings/billing?from=live_acp)',
    );
  });

  it('today\'s backend body (no cta / buy_credits_url) still yields the buy-credits message', async () => {
    backend.addProject('Platform', alice.id);
    backend.chatResponder = () => ({
      status: 402,
      body: { error: 'Insufficient credits. Purchase more credits to continue.', code: 'INSUFFICIENT_CREDITS', balance: 0, upgrade_url: '/settings/billing' },
    });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = await newSession(harness);
    await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'hello' }] });
    assert.match(harness.textOf(sessionId), /^The OrchestrAI agent runs on credits — buy credits: https:\/\/app\.example\.test\/settings\/billing\?buy=credits&from=live_acp/);
  });

  it('plan limit → upgrade message', async () => {
    backend.addProject('Platform', alice.id);
    backend.chatResponder = () => ({
      status: 402,
      body: { error: 'Free team spaces allow 1 concurrent room', code: 'LIVE_LIMIT_ROOMS', cta: 'upgrade', limit: 1, current: 1, upgrade_url: 'https://app.example.test/settings/billing?from=live_rooms' },
    });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = await newSession(harness);
    await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'hello' }] });
    assert.equal(
      harness.textOf(sessionId),
      // The server message already states the number: no usage note is added.
      'Free team spaces allow 1 concurrent room. Upgrade your OrchestrAI plan to lift this limit: https://app.example.test/settings/billing?from=live_rooms',
    );
  });
});

describe('session/list, session/load, session/resume and teammates', () => {
  it('lists owned and shared conversations across projects', async () => {
    const p1 = backend.addProject('Platform', alice.id);
    const p2 = backend.addProject('Data', bob.id, 'member');
    backend.addConversation(p1, alice.id, 'Mine');
    backend.addConversation(p2, bob.id, 'Shared incident', { access_role: 'driver', owner_name: 'Bob', visibility: 'project' });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const res = await harness.ctx.request(acp.methods.agent.session.list, {});
    const titles = res.sessions.map((s) => s.title);
    assert.deepEqual(titles.sort(), ['Mine — Platform', 'Shared incident — shared by Bob · Data']);
    const shared = res.sessions.find((s) => s.title?.startsWith('Shared'));
    assert.ok(shared);
    assert.match(shared.sessionId, new RegExp(`^${p2}:conv-`));
    assert.equal(res.nextCursor, null);
  });

  it('load replays history with teammates labelled; resume does not replay', async () => {
    const p1 = backend.addProject('Platform', alice.id);
    const conv = backend.addConversation(p1, alice.id, 'Prod outage', { visibility: 'project', participant_count: 1 });
    backend.addMessage(conv.id, 'user', 'Why is checkout slow?', alice.id);
    backend.addMessage(conv.id, 'assistant', 'p99 latency spiked at 10:02.', alice.id);
    backend.addMessage(conv.id, 'user', 'I restarted the pods.', bob.id);
    backend.addMessage(conv.id, 'run', '', null, { runId: 'run-9', status: 'completed' });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = `${p1}:${conv.id}`;
    await harness.ctx.request(acp.methods.agent.session.load, { sessionId, cwd: process.cwd(), mcpServers: [] });
    const updates = harness.updatesFor(sessionId);
    const userTexts = updates.filter((u) => u.sessionUpdate === 'user_message_chunk').map((u) => (u.sessionUpdate === 'user_message_chunk' && u.content.type === 'text' ? u.content.text : ''));
    assert.deepEqual(userTexts, ['Why is checkout slow?', '**Bob:** I restarted the pods.']);
    const bobChunk = updates.find((u) => u.sessionUpdate === 'user_message_chunk' && u.content.type === 'text' && u.content.text.startsWith('**Bob'));
    assert.deepEqual((bobChunk?._meta as { orchestrai: { authorName: string; fromTeammate: boolean } }).orchestrai.authorName, 'Bob');
    assert.equal(harness.textOf(sessionId), 'p99 latency spiked at 10:02.');
    assert.ok(updates.some((u) => u.sessionUpdate === 'tool_call' && u.toolCallId === 'orchestrai-run-run-9' && u.status === 'completed'));

    const before = harness.updates.length;
    await harness.ctx.request(acp.methods.agent.session.resume, { sessionId, cwd: process.cwd() });
    const replayed = harness.updates.slice(before).filter((u) => u.update.sessionUpdate === 'user_message_chunk');
    assert.equal(replayed.length, 0);
  });

  it('shows a teammate turn that arrives while the editor is idle (label + notice + live agent reply, no duplicate)', async () => {
    const p1 = backend.addProject('Platform', alice.id);
    const conv = backend.addConversation(p1, alice.id, 'Prod outage', { visibility: 'project' });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }), {
      clientCapabilities: { session: { notices: {} } },
    });
    const sessionId = `${p1}:${conv.id}`;
    await harness.ctx.request(acp.methods.agent.session.load, { sessionId, cwd: process.cwd(), mcpServers: [] });
    const h = harness;
    await waitFor(() => backend.wsConnections.some((c) => c.projectId === p1), 3000, 'ws connected');

    const msg = backend.addMessage(conv.id, 'user', 'Can you check the DB connections?', bob.id);
    backend.conversationUpdated(p1, conv.id, 'message_created', msg.id, bob.id);
    await waitFor(() => h.textOf(sessionId, 'user_message_chunk').includes('**Bob:** Can you check the DB connections?'), 3000, 'teammate message');
    const notice = h.updatesFor(sessionId).find((u) => u.sessionUpdate === 'notice');
    assert.ok(notice && notice.sessionUpdate === 'notice');
    assert.equal(notice.title, 'Bob posted in “Prod outage”');

    // Bob's browser streams the agent reply and persists it.
    backend.tokens(p1, conv.id, 'Connections are at 92% ');
    await sleep(30);
    const reply = backend.addMessage(conv.id, 'assistant', 'Connections are at 92% ', bob.id);
    backend.conversationUpdated(p1, conv.id, 'message_created', reply.id, bob.id);
    backend.tokens(p1, conv.id, 'of max.', true);
    await waitFor(() => h.textOf(sessionId) === 'Connections are at 92% of max.', 3000, 'live reply');
    await sleep(700);
    assert.equal(h.textOf(sessionId), 'Connections are at 92% of max.', 'persisted reply not rendered twice');
  });

  it('a view-only shared conversation can be followed but not prompted', async () => {
    const p1 = backend.addProject('Platform', alice.id);
    const conv = backend.addConversation(p1, bob.id, 'Bob only', { access_role: 'viewer', can_post: false, owner_name: 'Bob', visibility: 'project' });
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const sessionId = `${p1}:${conv.id}`;
    await harness.ctx.request(acp.methods.agent.session.load, { sessionId, cwd: process.cwd(), mcpServers: [] });
    const res = await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'hi' }] });
    assert.equal(res.stopReason, 'end_turn');
    assert.match(harness.textOf(sessionId), /view access/);
    assert.equal(backend.requests.filter((r) => r.path.endsWith('/chat/message')).length, 0);
  });
});

describe('project selection', () => {
  it('asks which project on the first prompt, then sends the original message there', async () => {
    backend.addProject('Platform', alice.id);
    const p2 = backend.addProject('Data', alice.id);
    backend.chatResponder = ({ projectId, body }) => {
      later(() => backend.tokens(projectId, body.conversation_id, 'done', true));
    };
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const created = await harness.ctx.request(acp.methods.agent.session.new, { cwd: process.cwd(), mcpServers: [] });
    const option = created.configOptions?.[0];
    assert.ok(option && option.type === 'select');
    assert.equal(option.currentValue, '__choose__');

    const sessionId = created.sessionId;
    await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: 'List my buckets' }] });
    assert.match(harness.textOf(sessionId), /Which OrchestrAI project/);
    assert.match(harness.textOf(sessionId), /2\. \*\*Data\*\*/);
    assert.equal(backend.conversations.length, 0);

    await harness.ctx.request(acp.methods.agent.session.prompt, { sessionId, prompt: [{ type: 'text', text: '2' }] });
    const send = backend.requests.find((r) => r.path.endsWith('/chat/message'));
    assert.ok(send);
    assert.equal(send.path, `/api/v1/projects/${p2}/chat/message`);
    assert.equal((send.body as { message: string }).message, 'List my buckets');

    // The new-* id now resolves after a restart (persisted mapping).
    const index = JSON.parse((await import('node:fs')).readFileSync(path.join(tmp.dir, 'sessions.json'), 'utf8')) as { sessions: Record<string, { projectId: string }> };
    assert.equal(index.sessions[sessionId]?.projectId, p2);
  });

  it('session/set_config_option picks the project', async () => {
    backend.addProject('Platform', alice.id);
    const p2 = backend.addProject('Data', alice.id);
    harness = await connectHarness(testConfig(backend.url, tmp.dir, { envApiKey: alice.key }));
    const { sessionId } = await harness.ctx.request(acp.methods.agent.session.new, { cwd: process.cwd(), mcpServers: [] });
    const res = await harness.ctx.request(acp.methods.agent.session.setConfigOption, { sessionId, configId: 'project', value: p2 });
    const option = res.configOptions[0];
    assert.ok(option && option.type === 'select');
    assert.equal(option.currentValue, p2);
  });
});

describe('authentication', () => {
  it('without a key, session/new fails with auth_required and advertises the browser sign-in', async () => {
    backend.addProject('Platform', alice.id);
    harness = await connectHarness(testConfig(backend.url, tmp.dir));
    await assert.rejects(
      harness.ctx.request(acp.methods.agent.session.new, { cwd: process.cwd(), mcpServers: [] }),
      (err: unknown) => err instanceof acp.RequestError && err.code === -32000 && /Authentication required/.test(err.message),
    );
  });

  it('authenticate runs the device flow, stores the key (0600) and unlocks sessions', async () => {
    backend.devicePendingPolls = 2;
    backend.deviceMode = 'ok';
    const opened: string[] = [];
    harness = await connectHarness(testConfig(backend.url, tmp.dir), { open: (u) => (opened.push(u), true) });
    await harness.ctx.request(acp.methods.agent.authenticate, { methodId: 'orchestrai-browser-login' });
    assert.deepEqual(opened, [`${backend.url}/live/connect?code=ABCD-EFGH`]);
    const start = backend.requests.find((r) => r.path === '/api/v1/editor/connect/start');
    assert.deepEqual((start?.body as { client: string }).client, 'acp');
    assert.equal(backend.requests.filter((r) => r.path === '/api/v1/editor/connect/poll').length, 3);

    const file = path.join(tmp.dir, 'credentials.json');
    assert.equal(statSync(file).mode & 0o777, 0o600);
    const device = [...backend.users.values()].find((u) => u.name === 'Device');
    assert.ok(device);
    backend.addProject('Device project', device.id);
    const { sessionId } = await harness.ctx.request(acp.methods.agent.session.new, { cwd: process.cwd(), mcpServers: [] });
    assert.ok(sessionId);
  });

  it('a declined sign-in surfaces as an error', async () => {
    backend.deviceMode = 'denied';
    harness = await connectHarness(testConfig(backend.url, tmp.dir));
    await assert.rejects(
      harness.ctx.request(acp.methods.agent.authenticate, { methodId: 'orchestrai-browser-login' }),
      /declined/,
    );
    backend.deviceMode = 'ok';
  });

  it('logout revokes the device-flow key server-side and forgets it', async () => {
    harness = await connectHarness(testConfig(backend.url, tmp.dir));
    await harness.ctx.request(acp.methods.agent.authenticate, { methodId: 'orchestrai-browser-login' });
    await harness.ctx.request(acp.methods.agent.logout, {});
    assert.equal(backend.revokedKeys.length, 1);
    await assert.rejects(harness.ctx.request(acp.methods.agent.session.new, { cwd: process.cwd(), mcpServers: [] }), /Authentication required/);
  });
});
