/**
 * Typed client for the OrchestrAI REST API (backend-go), authenticated with an
 * `ork_` API key. Routes and payloads mirror backend-go:
 *  - chat_handlers.go       /api/v1/projects/:pid/conversations[/:cid/messages]
 *  - chat_sharing.go        /api/v1/projects/:pid/conversations/:cid/participants
 *  - chat_message_handler.go POST /api/v1/projects/:pid/chat/message (credit gated, 402)
 *  - api_server_stubs.go    POST /api/v1/projects/:pid/agent-runs/:runId/stop
 *  - ws_ticket.go           POST /api/v1/ws/ticket
 *  - api_keys.go            GET/DELETE /api/v1/api-keys
 *  - mcp_inbound.go         POST /mcp (orchestrai_list_projects: owned + member projects)
 */

/**
 * Names this client to the backend (live_plan.go requestSource): turns from
 * the ACP bridge are non-web, so the agent never approves runs from chat text.
 */
export const CLIENT_HEADER = 'X-OrchestrAI-Client';
export const CLIENT_NAME = 'acp';

export interface Project {
  id: string;
  name: string;
  description: string;
  /** owner | admin | developer | member | viewer … */
  role: string;
}

export interface Conversation {
  id: string;
  project_id: string;
  user_id: string;
  title: string;
  archived: boolean;
  visibility: string;
  created_at: string;
  updated_at: string;
  access_role?: string;
  owner_name?: string;
  participant_count: number;
  can_post: boolean;
}

export interface ChatMessage {
  id: string;
  conversation_id: string;
  author_user_id?: string | null;
  role: string;
  content: string;
  metadata?: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface Participant {
  user_id: string;
  name: string;
  email: string;
  role: string;
}

export interface ChatAttachment {
  name: string;
  mime_type: string;
  size_bytes: number;
  content_base64: string;
}

export interface SendChatMessagePayload {
  message: string;
  conversation_id: string;
  conversation_history?: Array<{ role: string; content: string }>;
  message_id?: string;
  attachments?: ChatAttachment[];
}

export interface ApiKeyInfo {
  id: string;
  name: string;
  key_prefix: string;
}

/** A non-2xx API answer, with the backend's error envelope normalised. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly code: string | null,
    readonly body: Record<string, unknown> | null,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/** Network failure or timeout reaching the API. */
export class ApiUnreachableError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ApiUnreachableError';
  }
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * backend-go answers errors in two shapes: `{"error": "msg", "code": "X"}`
 * (most handlers) and `{"error": {"code": "X", "message": "msg"}}` (SendError).
 */
export function parseErrorBody(status: number, body: unknown, fallback: string): ApiError {
  if (isRecord(body)) {
    const err = body.error;
    if (isRecord(err)) {
      const code = typeof err.code === 'string' ? err.code : null;
      const message = typeof err.message === 'string' ? err.message : fallback;
      return new ApiError(status, message, code, body);
    }
    const message =
      typeof err === 'string' ? err : typeof body.message === 'string' ? body.message : fallback;
    const code = typeof body.code === 'string' ? body.code : null;
    return new ApiError(status, message, code, body);
  }
  return new ApiError(status, fallback, null, null);
}

const REQUEST_TIMEOUT_MS = 30_000;

export class OrchestraiApi {
  private mcpRequestId = 0;

  constructor(
    readonly apiUrl: string,
    private readonly apiKey: string,
    private readonly userAgent: string,
    private readonly fetchImpl: FetchLike = (input, init) => fetch(input, init),
  ) {}

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<T> {
    const signals = [AbortSignal.timeout(REQUEST_TIMEOUT_MS)];
    if (signal) signals.push(signal);
    let res: Response;
    try {
      res = await this.fetchImpl(`${this.apiUrl}${path}`, {
        method,
        headers: {
          Authorization: `Bearer ${this.apiKey}`,
          Accept: 'application/json',
          'User-Agent': this.userAgent,
          [CLIENT_HEADER]: CLIENT_NAME,
          ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.any(signals),
      });
    } catch (err) {
      if (signal?.aborted) throw err;
      const reason = err instanceof Error ? err.message : String(err);
      throw new ApiUnreachableError(`Could not reach OrchestrAI at ${this.apiUrl}: ${reason}`);
    }
    const text = await res.text();
    let parsed: unknown = null;
    if (text.length > 0) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = null;
      }
    }
    if (!res.ok) {
      throw parseErrorBody(res.status, parsed, `${method} ${path} failed with HTTP ${res.status}`);
    }
    return parsed as T;
  }

  private projectPath(projectId: string): string {
    return `/api/v1/projects/${encodeURIComponent(projectId)}`;
  }

  private conversationPath(projectId: string, conversationId: string): string {
    return `${this.projectPath(projectId)}/conversations/${encodeURIComponent(conversationId)}`;
  }

  /**
   * Owned + member projects. REST `GET /api/v1/projects` only lists owned
   * projects, so this asks the inbound MCP endpoint (same API key) first and
   * falls back to REST when /mcp is disabled.
   */
  async listProjects(signal?: AbortSignal): Promise<Project[]> {
    try {
      const out = await this.callMcpTool<{ projects?: unknown }>('orchestrai_list_projects', {}, signal);
      if (Array.isArray(out.projects)) {
        return out.projects.filter(isRecord).map((p) => ({
          id: String(p.id ?? ''),
          name: String(p.name ?? 'Untitled project'),
          description: typeof p.description === 'string' ? p.description : '',
          role: typeof p.role === 'string' ? p.role : 'member',
        }));
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) throw err;
      if (signal?.aborted) throw err;
    }
    const res = await this.request<{ projects?: Array<Record<string, unknown>> | null }>(
      'GET',
      '/api/v1/projects?page_size=100',
      undefined,
      signal,
    );
    return (res.projects ?? []).map((p) => ({
      id: String(p.id ?? ''),
      name: String(p.name ?? 'Untitled project'),
      description: typeof p.description === 'string' ? p.description : '',
      role: 'owner',
    }));
  }

  /** Stateless MCP `tools/call` (backend runs the go-sdk server with Stateless + JSONResponse). */
  private async callMcpTool<T>(name: string, args: Record<string, unknown>, signal?: AbortSignal): Promise<T> {
    const signals = [AbortSignal.timeout(REQUEST_TIMEOUT_MS)];
    if (signal) signals.push(signal);
    const res = await this.fetchImpl(`${this.apiUrl}/mcp`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${this.apiKey}`,
        Accept: 'application/json, text/event-stream',
        'Content-Type': 'application/json',
        'MCP-Protocol-Version': '2025-11-25',
        'User-Agent': this.userAgent,
        [CLIENT_HEADER]: CLIENT_NAME,
      },
      body: JSON.stringify({
        jsonrpc: '2.0',
        id: ++this.mcpRequestId,
        method: 'tools/call',
        params: { name, arguments: args },
      }),
      signal: AbortSignal.any(signals),
    });
    const text = await res.text();
    if (!res.ok) {
      let parsed: unknown = null;
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = null;
      }
      throw parseErrorBody(res.status, parsed, `MCP ${name} failed with HTTP ${res.status}`);
    }
    const rpc = JSON.parse(text) as { result?: Record<string, unknown>; error?: { message?: string } };
    if (rpc.error) throw new Error(rpc.error.message ?? `MCP ${name} failed`);
    const result = rpc.result ?? {};
    if (result.isError === true) throw new Error(`MCP ${name} returned an error`);
    if (isRecord(result.structuredContent)) return result.structuredContent as T;
    const content = Array.isArray(result.content) ? result.content : [];
    const first = content.find((c): c is { type: string; text: string } => isRecord(c) && c.type === 'text');
    if (first) return JSON.parse(first.text) as T;
    throw new Error(`MCP ${name} returned no content`);
  }

  async listConversations(projectId: string, limit = 50, offset = 0, signal?: AbortSignal): Promise<Conversation[]> {
    const res = await this.request<{ conversations?: Conversation[] }>(
      'GET',
      `${this.projectPath(projectId)}/conversations?limit=${limit}&offset=${offset}`,
      undefined,
      signal,
    );
    return res.conversations ?? [];
  }

  /** No single-conversation GET exists; scan the accessible list (newest first). */
  async findConversation(projectId: string, conversationId: string, signal?: AbortSignal): Promise<Conversation | null> {
    const pageSize = 200;
    for (let offset = 0; offset < 2000; offset += pageSize) {
      const page = await this.listConversations(projectId, pageSize, offset, signal);
      const hit = page.find((c) => c.id === conversationId);
      if (hit) return hit;
      if (page.length < pageSize) return null;
    }
    return null;
  }

  createConversation(projectId: string, title: string, signal?: AbortSignal): Promise<Conversation> {
    return this.request<Conversation>('POST', `${this.projectPath(projectId)}/conversations`, { title }, signal);
  }

  async listMessages(
    projectId: string,
    conversationId: string,
    limit = 500,
    offset = 0,
    signal?: AbortSignal,
  ): Promise<ChatMessage[]> {
    const res = await this.request<{ messages?: ChatMessage[] }>(
      'GET',
      `${this.conversationPath(projectId, conversationId)}/messages?limit=${limit}&offset=${offset}`,
      undefined,
      signal,
    );
    return res.messages ?? [];
  }

  /** Every message, oldest first (the endpoint caps a page at 500). */
  async listAllMessages(projectId: string, conversationId: string, signal?: AbortSignal): Promise<ChatMessage[]> {
    const all: ChatMessage[] = [];
    const pageSize = 500;
    for (;;) {
      const page = await this.listMessages(projectId, conversationId, pageSize, all.length, signal);
      all.push(...page);
      if (page.length < pageSize) return all;
    }
  }

  createMessage(
    projectId: string,
    conversationId: string,
    message: { role: string; content: string; metadata?: Record<string, unknown> },
    signal?: AbortSignal,
  ): Promise<ChatMessage> {
    return this.request<ChatMessage>('POST', `${this.conversationPath(projectId, conversationId)}/messages`, message, signal);
  }

  updateMessage(
    projectId: string,
    conversationId: string,
    messageId: string,
    patch: { content?: string; metadata?: Record<string, unknown> },
  ): Promise<ChatMessage> {
    return this.request<ChatMessage>(
      'PATCH',
      `${this.conversationPath(projectId, conversationId)}/messages/${encodeURIComponent(messageId)}`,
      patch,
    );
  }

  async listParticipants(projectId: string, conversationId: string, signal?: AbortSignal): Promise<Participant[]> {
    const res = await this.request<{ participants?: Participant[] }>(
      'GET',
      `${this.conversationPath(projectId, conversationId)}/participants`,
      undefined,
      signal,
    );
    return res.participants ?? [];
  }

  sendChatMessage(projectId: string, payload: SendChatMessagePayload, signal?: AbortSignal): Promise<unknown> {
    return this.request('POST', `${this.projectPath(projectId)}/chat/message`, payload, signal);
  }

  stopAgentRun(projectId: string, runId: string): Promise<unknown> {
    return this.request('POST', `${this.projectPath(projectId)}/agent-runs/${encodeURIComponent(runId)}/stop`);
  }

  createWsTicket(signal?: AbortSignal): Promise<{ ticket: string; expires_in: number }> {
    return this.request('POST', '/api/v1/ws/ticket', undefined, signal);
  }

  async listApiKeys(): Promise<ApiKeyInfo[]> {
    const res = await this.request<ApiKeyInfo[] | null>('GET', '/api/v1/api-keys?limit=100');
    return Array.isArray(res) ? res : [];
  }

  revokeApiKey(keyId: string): Promise<unknown> {
    return this.request('DELETE', `/api/v1/api-keys/${encodeURIComponent(keyId)}`);
  }
}

/** The WS ticket is `base64url(userId).exp.hmac` (ws_ticket.go); the user id is not secret. */
export function userIdFromTicket(ticket: string): string | null {
  const first = ticket.split('.')[0];
  if (!first) return null;
  try {
    const decoded = Buffer.from(first, 'base64url').toString('utf8');
    return decoded.length > 0 ? decoded : null;
  } catch {
    return null;
  }
}
