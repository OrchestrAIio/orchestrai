/**
 * In-process stand-in for backend-go: the REST routes, /mcp list_projects,
 * the device-flow endpoints and the /ws ticket handshake the bridge uses,
 * with the same payload shapes. Tests script what the "agent" streams back.
 */
import { randomUUID } from 'node:crypto';
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import { WebSocketServer, type WebSocket } from 'ws';

export interface MockMessage {
  id: string;
  conversation_id: string;
  author_user_id: string | null;
  role: string;
  content: string;
  metadata: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface MockConversation {
  id: string;
  project_id: string;
  user_id: string;
  title: string;
  archived: boolean;
  visibility: string;
  created_at: string;
  updated_at: string;
  access_role: string;
  owner_name: string;
  participant_count: number;
  can_post: boolean;
}

export interface RecordedRequest {
  method: string;
  path: string;
  headers: http.IncomingHttpHeaders;
  body: unknown;
}

export interface ChatSend {
  projectId: string;
  body: { message: string; conversation_id: string; conversation_history?: unknown[]; message_id?: string; attachments?: unknown[] };
}

export type ChatResponder = (send: ChatSend, backend: MockBackend) => { status: number; body: unknown } | void;

let clock = Date.parse('2026-10-19T10:00:00.000Z');
function nextTimestamp(): string {
  clock += 1000;
  return new Date(clock).toISOString();
}

export class MockBackend {
  readonly users = new Map<string, { id: string; name: string; email: string }>();
  readonly projects: Array<{ id: string; name: string; description: string; role: string; owner_id: string }> = [];
  readonly conversations: MockConversation[] = [];
  readonly messages: MockMessage[] = [];
  readonly requests: RecordedRequest[] = [];
  readonly stoppedRuns: string[] = [];
  readonly revokedKeys: string[] = [];
  readonly apiKeys: Array<{ id: string; name: string; key_prefix: string; key: string }> = [];
  readonly wsConnections: Array<{ projectId: string; userId: string; authorization: string | undefined; socket: WebSocket }> = [];
  validKeys = new Map<string, string>(); // key -> userId
  chatResponder: ChatResponder | null = null;
  /** Device flow: polls answered 428 before the key is issued. */
  devicePendingPolls = 1;
  deviceMode: 'ok' | 'denied' | 'unavailable' = 'ok';
  mcpEnabled = true;
  private devicePolls = 0;
  private server: http.Server;
  private wss: WebSocketServer;
  url = '';

  constructor() {
    this.server = http.createServer((req, res) => {
      void this.handle(req, res);
    });
    this.wss = new WebSocketServer({ noServer: true });
    this.server.on('upgrade', (req, socket, head) => {
      const url = new URL(req.url ?? '/', 'http://localhost');
      const ticket = url.searchParams.get('ticket') ?? '';
      const projectId = url.searchParams.get('project_id') ?? '';
      const userId = Buffer.from(ticket.split('.')[0] ?? '', 'base64url').toString('utf8');
      // Mirrors backend-go: a Bearer header is validated as a Better Auth session,
      // so an ork_ key there must be rejected.
      const auth = req.headers.authorization;
      if (url.pathname !== '/ws' || !ticket.endsWith('.sig') || !userId || !projectId || (auth && auth.includes('ork_'))) {
        socket.write('HTTP/1.1 401 Unauthorized\r\n\r\n');
        socket.destroy();
        return;
      }
      this.wss.handleUpgrade(req, socket, head, (ws) => {
        this.wsConnections.push({ projectId, userId, authorization: auth, socket: ws });
        ws.send(JSON.stringify({ type: 'connection_established', project_id: projectId, data: { user_id: userId } }));
        ws.on('message', (raw) => {
          const msg = JSON.parse(raw.toString()) as { type?: string };
          if (msg.type === 'heartbeat') ws.send(JSON.stringify({ type: 'heartbeat_ack', data: {} }));
        });
        ws.on('close', () => {
          const i = this.wsConnections.findIndex((c) => c.socket === ws);
          if (i >= 0) this.wsConnections.splice(i, 1);
        });
      });
    });
  }

  async start(): Promise<string> {
    await new Promise<void>((resolve) => this.server.listen(0, '127.0.0.1', resolve));
    const { port } = this.server.address() as AddressInfo;
    this.url = `http://127.0.0.1:${port}`;
    return this.url;
  }

  async stop(): Promise<void> {
    for (const c of this.wsConnections) c.socket.terminate();
    this.wss.close();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }

  // ------------------------------------------------------------ fixtures

  addUser(name: string, email: string): { id: string; key: string } {
    const id = `user-${name.toLowerCase()}`;
    const key = `ork_${randomUUID().replace(/-/g, '')}`;
    this.users.set(id, { id, name, email });
    this.validKeys.set(key, id);
    this.apiKeys.push({ id: `key-${randomUUID()}`, name: 'test key', key_prefix: key.slice(0, 12), key });
    return { id, key };
  }

  addProject(name: string, ownerId: string, role = 'owner'): string {
    const id = `proj${this.projects.length + 1}`;
    this.projects.push({ id, name, description: `${name} project`, role, owner_id: ownerId });
    return id;
  }

  addConversation(projectId: string, ownerId: string, title: string, opts: Partial<MockConversation> = {}): MockConversation {
    const ts = nextTimestamp();
    const conv: MockConversation = {
      id: `conv-${randomUUID().slice(0, 8)}`,
      project_id: projectId,
      user_id: ownerId,
      title,
      archived: false,
      visibility: 'private',
      created_at: ts,
      updated_at: ts,
      access_role: 'owner',
      owner_name: this.users.get(ownerId)?.name ?? '',
      participant_count: 0,
      can_post: true,
      ...opts,
    };
    this.conversations.push(conv);
    return conv;
  }

  addMessage(conversationId: string, role: string, content: string, author: string | null, metadata: Record<string, unknown> | null = null): MockMessage {
    const ts = nextTimestamp();
    const msg: MockMessage = {
      id: `msg-${randomUUID().slice(0, 8)}`,
      conversation_id: conversationId,
      author_user_id: author,
      role,
      content,
      metadata,
      created_at: ts,
      updated_at: ts,
    };
    this.messages.push(msg);
    return msg;
  }

  messagesOf(conversationId: string): MockMessage[] {
    return this.messages.filter((m) => m.conversation_id === conversationId);
  }

  // ------------------------------------------------------------ streaming

  /** Broadcast like BroadcastToProject (all sockets on the project). */
  broadcast(projectId: string, type: string, data: Record<string, unknown>): void {
    const envelope = JSON.stringify({ type, project_id: projectId, run_id: data.run_id, data, timestamp: new Date().toISOString() });
    for (const c of this.wsConnections) if (c.projectId === projectId) c.socket.send(envelope);
  }

  tokens(projectId: string, runId: string, delta: string, finish = false): void {
    this.broadcast(projectId, 'tokens', {
      run_id: runId,
      tokens: { delta, timestamp: new Date().toISOString(), ...(finish ? { finish_reason: 'stop' } : {}) },
    });
  }

  status(projectId: string, runId: string, status: string, extra: Record<string, unknown> = {}): void {
    this.broadcast(projectId, 'agent_status', { run_id: runId, status, message: '', progress: 0, error: '', ...extra });
  }

  conversationUpdated(projectId: string, conversationId: string, kind: string, messageId: string, actorId: string): void {
    this.broadcast(projectId, 'conversation_updated', {
      run_id: conversationId,
      conversation_id: conversationId,
      kind,
      message_id: messageId,
      actor_id: actorId,
    });
  }

  // ------------------------------------------------------------ HTTP

  private async handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk as Buffer);
    const raw = Buffer.concat(chunks).toString('utf8');
    let body: unknown = null;
    if (raw) {
      try {
        body = JSON.parse(raw);
      } catch {
        body = raw;
      }
    }
    const url = new URL(req.url ?? '/', 'http://localhost');
    this.requests.push({ method: req.method ?? 'GET', path: url.pathname + url.search, headers: req.headers, body });
    const send = (status: number, payload?: unknown) => {
      res.writeHead(status, { 'Content-Type': 'application/json' });
      res.end(payload === undefined ? '' : JSON.stringify(payload));
    };
    const method = req.method ?? 'GET';
    const p = url.pathname;

    // Unauthenticated device flow (spec §5).
    if (p === '/api/v1/editor/connect/start' && method === 'POST') {
      if (this.deviceMode === 'unavailable') return send(404, { error: 'Not found' });
      this.devicePolls = 0;
      return send(200, {
        device_code: 'dev-code-1',
        user_code: 'ABCD-EFGH',
        verification_uri: `${this.url}/live/connect`,
        verification_uri_complete: `${this.url}/live/connect?code=ABCD-EFGH`,
        interval: 1,
        expires_in: 600,
      });
    }
    if (p === '/api/v1/editor/connect/poll' && method === 'POST') {
      if ((body as { device_code?: string })?.device_code !== 'dev-code-1') return send(400, { error: 'expired_token' });
      if (this.deviceMode === 'denied') return send(403, { error: 'access_denied' });
      this.devicePolls += 1;
      if (this.devicePolls <= this.devicePendingPolls) return send(428, { error: 'authorization_pending' });
      const { id, key } = this.addUser('Device', 'device@example.com');
      return send(200, { api_key: key, user: { id, name: 'Device', email: 'device@example.com' } });
    }

    const authz = req.headers.authorization ?? '';
    const key = authz.startsWith('Bearer ') ? authz.slice(7) : '';
    const userId = this.validKeys.get(key);
    if (!userId) {
      if (p === '/mcp') return send(401, { error: 'invalid_token' });
      return send(401, { error: { code: 'UNAUTHORIZED', message: 'Invalid API key' } });
    }

    if (p === '/mcp' && method === 'POST') {
      if (!this.mcpEnabled) return send(404, { error: 'Not found' });
      const rpc = body as { id: number; method: string; params: { name: string } };
      if (rpc.method === 'tools/call' && rpc.params.name === 'orchestrai_list_projects') {
        return send(200, {
          jsonrpc: '2.0',
          id: rpc.id,
          result: {
            content: [{ type: 'text', text: '{}' }],
            structuredContent: { projects: this.projects.map(({ id, name, description, role }) => ({ id, name, description, role })) },
          },
        });
      }
      return send(200, { jsonrpc: '2.0', id: rpc.id, error: { code: -32601, message: 'unknown tool' } });
    }

    if (p === '/api/v1/ws/ticket' && method === 'POST') {
      return send(200, { ticket: `${Buffer.from(userId).toString('base64url')}.${Math.floor(Date.now() / 1000) + 60}.sig`, expires_in: 60 });
    }
    if (p === '/api/v1/projects' && method === 'GET') {
      return send(200, { projects: this.projects.filter((x) => x.owner_id === userId).map((x) => ({ id: x.id, name: x.name, owner_id: x.owner_id })), total: 1, page: 1, page_size: 100 });
    }
    if (p === '/api/v1/api-keys' && method === 'GET') {
      return send(200, this.apiKeys.filter((k) => this.validKeys.get(k.key) === userId).map(({ id, name, key_prefix }) => ({ id, name, key_prefix, permissions: [] })));
    }
    const keyMatch = p.match(/^\/api\/v1\/api-keys\/([^/]+)$/);
    if (keyMatch && method === 'DELETE') {
      const k = this.apiKeys.find((x) => x.id === keyMatch[1]);
      if (!k) return send(404, { error: { code: 'NOT_FOUND', message: 'API key not found' } });
      this.revokedKeys.push(k.id);
      this.validKeys.delete(k.key);
      return send(200, { message: 'API key revoked' });
    }

    const m = p.match(/^\/api\/v1\/projects\/([^/]+)(\/.*)?$/);
    if (!m) return send(404, { error: 'not found' });
    const projectId = decodeURIComponent(m[1] ?? '');
    const rest = m[2] ?? '';
    if (!this.projects.some((x) => x.id === projectId)) return send(403, { error: 'You do not have access to this project' });

    if (rest === '/conversations' && method === 'GET') {
      const limit = Number(url.searchParams.get('limit') ?? 50);
      const offset = Number(url.searchParams.get('offset') ?? 0);
      const list = this.conversations
        .filter((c) => c.project_id === projectId)
        .sort((a, b) => b.updated_at.localeCompare(a.updated_at))
        .slice(offset, offset + limit);
      return send(200, { conversations: list, limit, offset });
    }
    if (rest === '/conversations' && method === 'POST') {
      const conv = this.addConversation(projectId, userId, (body as { title?: string })?.title || 'New chat');
      return send(201, conv);
    }
    if (rest === '/chat/message' && method === 'POST') {
      const sendBody = body as ChatSend['body'];
      const result = this.chatResponder?.({ projectId, body: sendBody }, this);
      if (result) return send(result.status, result.body);
      return send(202, { success: true, conversation_id: sendBody.conversation_id });
    }
    const stop = rest.match(/^\/agent-runs\/([^/]+)\/stop$/);
    if (stop && method === 'POST') {
      this.stoppedRuns.push(stop[1] ?? '');
      return send(200, { success: true, status: 'cancelling' });
    }
    const cm = rest.match(/^\/conversations\/([^/]+)(\/messages|\/participants)?(?:\/([^/]+))?$/);
    if (cm) {
      const conversationId = cm[1] ?? '';
      const conv = this.conversations.find((c) => c.id === conversationId && c.project_id === projectId);
      if (!conv) return send(404, { error: 'Conversation not found' });
      if (cm[2] === '/participants' && method === 'GET') {
        const owner = this.users.get(conv.user_id);
        const participants = [...this.users.values()]
          .filter((u) => u.id !== conv.user_id)
          .map((u) => ({ user_id: u.id, name: u.name, email: u.email, role: 'driver' }));
        return send(200, {
          participants: [...(owner ? [{ user_id: owner.id, name: owner.name, email: owner.email, role: 'owner' }] : []), ...participants],
          visibility: conv.visibility,
          your_role: 'owner',
        });
      }
      if (cm[2] === '/messages' && !cm[3] && method === 'GET') {
        const limit = Number(url.searchParams.get('limit') ?? 100);
        const offset = Number(url.searchParams.get('offset') ?? 0);
        return send(200, { messages: this.messagesOf(conversationId).slice(offset, offset + limit), limit, offset });
      }
      if (cm[2] === '/messages' && !cm[3] && method === 'POST') {
        if (!conv.can_post) return send(403, { error: 'read only', code: 'CONVERSATION_READ_ONLY' });
        const b = body as { role: string; content: string; metadata?: Record<string, unknown> };
        const msg = this.addMessage(conversationId, b.role, b.content, userId, b.metadata ?? null);
        return send(201, msg);
      }
      if (cm[2] === '/messages' && cm[3] && method === 'PATCH') {
        const msg = this.messages.find((x) => x.id === cm[3]);
        if (!msg) return send(404, { error: 'Message not found' });
        const b = body as { content?: string; metadata?: Record<string, unknown> };
        if (b.content !== undefined) msg.content = b.content;
        if (b.metadata !== undefined) msg.metadata = b.metadata;
        msg.updated_at = nextTimestamp();
        return send(200, msg);
      }
    }
    return send(404, { error: 'not found' });
  }
}

export function waitFor(predicate: () => boolean, timeoutMs = 3000, label = 'condition'): Promise<void> {
  const started = Date.now();
  return new Promise((resolve, reject) => {
    const tick = () => {
      if (predicate()) return resolve();
      if (Date.now() - started > timeoutMs) return reject(new Error(`timed out waiting for ${label}`));
      setTimeout(tick, 10);
    };
    tick();
  });
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
