/**
 * The OrchestrAI ACP agent: wires ACP requests (@agentclientprotocol/sdk) to
 * BridgeSession instances backed by the OrchestrAI API.
 */
import { Readable, Writable } from 'node:stream';
import * as acp from '@agentclientprotocol/sdk';
import {
  RequestError,
  type AgentContext,
  type AuthMethod,
  type ClientCapabilities,
  type InitializeResponse,
  type ListSessionsResponse,
  type SessionInfo,
  type SessionUpdate,
} from '@agentclientprotocol/sdk';
import { ApiError, OrchestraiApi, userIdFromTicket, type Project } from './api.js';
import {
  createCredentialStore,
  DEVICE_AUTH_METHOD_ID,
  resolveApiKey,
  signInWithDeviceFlow,
  signOut,
  TERMINAL_AUTH_METHOD_ID,
  userAgent,
} from './auth.js';
import { PACKAGE_VERSION, type BridgeConfig } from './config.js';
import type { CredentialStore } from './credentials.js';
import { DeviceFlowError, type DeviceFlowOptions } from './device-flow.js';
import type { Logger } from './log.js';
import { errorMessage } from './log.js';
import { BridgeSession, PROJECT_CONFIG_ID } from './session.js';
import { encodeSessionId, newSessionId, SessionIndex } from './session-ids.js';
import { StreamHub, type WebSocketFactory } from './stream.js';

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface BridgeOptions {
  config: BridgeConfig;
  log: Logger;
  store?: CredentialStore;
  fetchImpl?: FetchLike;
  wsFactory?: WebSocketFactory;
  /** Device-flow overrides for tests (sleep, browser opener). */
  deviceFlow?: { sleep?: DeviceFlowOptions['sleep']; open?: (url: string) => boolean };
  /** argv used to launch this process; terminal auth re-runs it with `login`. */
  argv0?: string[];
}

interface AuthState {
  api: OrchestraiApi;
  hub: StreamHub;
  userId: string | null | undefined;
  projects: { at: number; list: Project[] } | null;
}

const PROJECT_CACHE_MS = 60_000;
const LIST_PAGE = 50;

export class OrchestraiAcpAgent {
  private readonly store: CredentialStore;
  private readonly index: SessionIndex;
  private client: AgentContext | null = null;
  private clientCaps: ClientCapabilities = {};
  private clientName = 'acp-client';
  private auth: AuthState | null = null;
  private sessions = new Map<string, BridgeSession>();

  constructor(private readonly opts: BridgeOptions) {
    this.store = opts.store ?? createCredentialStore(opts.config, opts.log);
    this.index = new SessionIndex(opts.config.configDir);
  }

  /** Builds the ACP app (handlers registered, not yet connected). */
  app(): acp.AgentApp {
    return acp
      .agent({ name: 'orchestrai' })
      .onConnect((conn) => {
        this.client = conn.client;
        void conn.closed.finally(() => this.shutdown());
      })
      .onRequest('initialize', (ctx) => this.initialize(ctx.params))
      .onRequest('authenticate', (ctx) => this.authenticate(ctx.params.methodId, ctx.signal))
      .onRequest('logout', () => this.logout())
      .onRequest('session/new', async () => {
        const auth = await this.requireAuth();
        const sessionId = newSessionId();
        const session = this.createSession(sessionId, auth);
        try {
          const configOptions = await session.initNew();
          return { sessionId, configOptions };
        } catch (err) {
          this.dropSession(sessionId);
          throw this.toRequestError(err);
        }
      })
      .onRequest('session/load', async (ctx) => {
        await this.openExisting(ctx.params.sessionId, true);
        const session = this.sessions.get(ctx.params.sessionId);
        return { configOptions: session?.configOptions() ?? null };
      })
      .onRequest('session/resume', async (ctx) => {
        await this.openExisting(ctx.params.sessionId, false);
        const session = this.sessions.get(ctx.params.sessionId);
        return { configOptions: session?.configOptions() ?? null };
      })
      .onRequest('session/list', (ctx) => this.listSessions(ctx.params.cursor ?? null, ctx.params.cwd ?? null))
      .onRequest('session/close', (ctx) => {
        this.dropSession(ctx.params.sessionId);
        return {};
      })
      .onRequest('session/set_config_option', async (ctx) => {
        const session = this.requireSession(ctx.params.sessionId);
        if (ctx.params.configId !== PROJECT_CONFIG_ID || !('value' in ctx.params) || typeof ctx.params.value !== 'string') {
          throw RequestError.invalidParams({ configId: ctx.params.configId }, 'Unknown config option.');
        }
        try {
          return { configOptions: await session.setProject(ctx.params.value) };
        } catch (err) {
          throw this.toRequestError(err);
        }
      })
      .onRequest('session/prompt', async (ctx) => {
        const session = this.requireSession(ctx.params.sessionId);
        try {
          const stopReason = await session.prompt(ctx.params.prompt);
          return { stopReason };
        } catch (err) {
          throw this.toRequestError(err);
        }
      })
      .onNotification('session/cancel', async (ctx) => {
        await this.sessions.get(ctx.params.sessionId)?.cancel();
      });
  }

  /** Runs over stdin/stdout until the client disconnects. */
  async serveStdio(): Promise<void> {
    const stream = acp.ndJsonStream(
      Writable.toWeb(process.stdout) as WritableStream<Uint8Array>,
      Readable.toWeb(process.stdin) as ReadableStream<Uint8Array>,
    );
    const connection = this.app().connect(stream);
    await connection.closed;
  }

  // ------------------------------------------------------------------ handlers

  private initialize(params: acp.InitializeRequest): InitializeResponse {
    this.clientCaps = params.clientCapabilities ?? {};
    this.clientName = params.clientInfo?.title || params.clientInfo?.name || 'acp-client';
    // `type: "agent"` is the protocol default; it is sent explicitly because the
    // ACP Registry's auth check looks for it.
    const deviceMethod = { type: 'agent' } as Record<string, unknown>;
    const authMethods: AuthMethod[] = [
      {
        ...deviceMethod,
        id: DEVICE_AUTH_METHOD_ID,
        name: 'Sign in with OrchestrAI',
        description:
          'Opens OrchestrAI in your browser to approve this editor. Or set ORCHESTRAI_API_KEY to an API key from Settings → API keys.',
      },
    ];
    if (this.clientCaps.auth?.terminal) {
      authMethods.push({
        type: 'terminal',
        id: TERMINAL_AUTH_METHOD_ID,
        name: 'Sign in from a terminal',
        description: 'Shows a one-time code and opens OrchestrAI in your browser.',
        args: ['login'],
      });
    }
    return {
      protocolVersion: acp.PROTOCOL_VERSION,
      agentInfo: { name: 'orchestrai', title: 'OrchestrAI', version: PACKAGE_VERSION },
      agentCapabilities: {
        loadSession: true,
        promptCapabilities: { image: false, audio: false, embeddedContext: true },
        sessionCapabilities: { list: {}, resume: {}, close: {} },
        auth: { logout: {} },
      },
      authMethods,
    };
  }

  private async authenticate(methodId: string, signal: AbortSignal): Promise<Record<string, never>> {
    if (methodId === TERMINAL_AUTH_METHOD_ID) {
      throw RequestError.invalidParams({ methodId }, 'Terminal sign-in runs as a separate process; the client must not call authenticate with it.');
    }
    if (methodId !== DEVICE_AUTH_METHOD_ID) {
      throw RequestError.invalidParams({ methodId }, `Unknown auth method ${methodId}.`);
    }
    try {
      const { credential, storedIn } = await signInWithDeviceFlow(
        this.opts.config,
        this.store,
        {
          onCode: (auth, opened) => {
            this.opts.log.info(
              `${opened ? 'Opened your browser. ' : ''}Approve this editor at ${auth.verification_uri} with code ${auth.user_code}`,
            );
          },
        },
        { signal, fetchImpl: this.opts.fetchImpl, sleep: this.opts.deviceFlow?.sleep, open: this.opts.deviceFlow?.open },
      );
      this.opts.log.info(
        `Signed in${credential.user?.email ? ` as ${credential.user.email}` : ''}; key stored in the ${storedIn === 'keychain' ? 'OS keychain' : 'credentials file'}.`,
      );
      this.resetAuth();
      return {};
    } catch (err) {
      if (err instanceof DeviceFlowError) {
        throw new RequestError(-32000, err.message, { reason: err.reason });
      }
      throw this.toRequestError(err);
    }
  }

  private async logout(): Promise<Record<string, never>> {
    for (const id of [...this.sessions.keys()]) this.dropSession(id);
    const result = await signOut(this.opts.config, this.store, this.opts.log, this.opts.fetchImpl);
    if (this.opts.config.envApiKey) {
      this.opts.log.warn('ORCHESTRAI_API_KEY is set in the environment; unset it to fully sign out.');
    }
    this.opts.log.info(result.revoked ? 'Signed out and revoked the editor API key.' : 'Signed out.');
    this.resetAuth();
    return {};
  }

  private async openExisting(sessionId: string, replay: boolean): Promise<void> {
    const auth = await this.requireAuth();
    const ref = await this.index.resolve(sessionId);
    if (!ref) throw RequestError.resourceNotFound(`orchestrai://sessions/${sessionId}`);
    this.dropSession(sessionId);
    const session = this.createSession(sessionId, auth);
    try {
      await session.open(ref, replay);
    } catch (err) {
      this.dropSession(sessionId);
      throw this.toRequestError(err);
    }
  }

  /**
   * Lists the conversations the user can open: their own, ones shared with
   * them, and ones shared with the whole project (backend access rules). The
   * cursor walks projects, then pages within a project. `cwd` is not a filter:
   * OrchestrAI conversations belong to projects, not folders.
   */
  private async listSessions(cursor: string | null, cwd: string | null): Promise<ListSessionsResponse> {
    const auth = await this.requireAuth();
    let state = { p: 0, o: 0 };
    if (cursor) {
      try {
        const parsed = JSON.parse(Buffer.from(cursor, 'base64url').toString('utf8')) as { p?: unknown; o?: unknown };
        if (typeof parsed.p === 'number' && typeof parsed.o === 'number' && parsed.p >= 0 && parsed.o >= 0) {
          state = { p: parsed.p, o: parsed.o };
        } else {
          throw new Error('bad cursor');
        }
      } catch {
        throw RequestError.invalidParams({ cursor }, 'Invalid session list cursor.');
      }
    }
    let projects: Project[];
    try {
      projects = this.opts.config.projectId
        ? [{ id: this.opts.config.projectId, name: '', description: '', role: '' }]
        : await this.projects(auth);
    } catch (err) {
      throw this.toRequestError(err);
    }
    const multiProject = projects.length > 1;
    const sessions: SessionInfo[] = [];
    while (state.p < projects.length && sessions.length < LIST_PAGE) {
      const project = projects[state.p];
      if (!project) break;
      let page;
      try {
        page = await auth.api.listConversations(project.id, LIST_PAGE, state.o);
      } catch (err) {
        if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          state = { p: state.p + 1, o: 0 };
          continue;
        }
        throw this.toRequestError(err);
      }
      for (const c of page) {
        const ref = { projectId: project.id, conversationId: c.id };
        const shared = c.access_role && c.access_role !== 'owner';
        const suffix = [shared && c.owner_name ? `shared by ${c.owner_name}` : '', multiProject && project.name ? project.name : '']
          .filter(Boolean)
          .join(' · ');
        sessions.push({
          sessionId: (await this.index.aliasFor(ref)) ?? encodeSessionId(ref),
          cwd: cwd ?? process.cwd(),
          title: suffix ? `${c.title} — ${suffix}` : c.title,
          updatedAt: c.updated_at,
          _meta: {
            orchestrai: {
              projectId: project.id,
              projectName: project.name || null,
              conversationId: c.id,
              accessRole: c.access_role ?? null,
              ownerName: c.owner_name ?? null,
              visibility: c.visibility,
              participantCount: c.participant_count,
              canPost: c.can_post,
            },
          },
        });
      }
      state = page.length < LIST_PAGE ? { p: state.p + 1, o: 0 } : { p: state.p, o: state.o + page.length };
    }
    const more = state.p < projects.length;
    return {
      sessions,
      nextCursor: more ? Buffer.from(JSON.stringify(state), 'utf8').toString('base64url') : null,
    };
  }

  // ------------------------------------------------------------------ helpers

  private async requireAuth(): Promise<AuthState> {
    if (this.auth) return this.auth;
    const resolved = await resolveApiKey(this.opts.config, this.store);
    if (!resolved) {
      throw RequestError.authRequired(
        { authMethods: [DEVICE_AUTH_METHOD_ID] },
        'Sign in to OrchestrAI (or set ORCHESTRAI_API_KEY).',
      );
    }
    const ua = userAgent(this.clientName);
    const api = new OrchestraiApi(this.opts.config.apiUrl, resolved.apiKey, ua, this.opts.fetchImpl);
    this.auth = { api, hub: new StreamHub(api, this.opts.log, ua, this.opts.wsFactory), userId: undefined, projects: null };
    return this.auth;
  }

  private resetAuth(): void {
    this.auth?.hub.closeAll();
    this.auth = null;
  }

  private async projects(auth: AuthState): Promise<Project[]> {
    if (auth.projects && Date.now() - auth.projects.at < PROJECT_CACHE_MS) return auth.projects.list;
    const list = await auth.api.listProjects();
    auth.projects = { at: Date.now(), list };
    return list;
  }

  private createSession(sessionId: string, auth: AuthState): BridgeSession {
    const session = new BridgeSession(sessionId, {
      api: auth.api,
      hub: auth.hub,
      config: this.opts.config,
      log: this.opts.log,
      index: this.index,
      emit: (update) => this.emit(sessionId, update),
      clientName: this.clientName,
      supportsNotices: this.clientCaps.session?.notices != null,
      listProjects: () => this.projects(auth),
      whoAmI: async () => {
        if (auth.userId !== undefined) return auth.userId;
        try {
          const { ticket } = await auth.api.createWsTicket();
          auth.userId = userIdFromTicket(ticket);
        } catch (err) {
          if (err instanceof ApiError && err.status === 401) throw err;
          this.opts.log.debug(`could not resolve the signed-in user: ${errorMessage(err)}`);
          auth.userId = null;
        }
        return auth.userId;
      },
    });
    this.sessions.set(sessionId, session);
    return session;
  }

  private requireSession(sessionId: string): BridgeSession {
    const session = this.sessions.get(sessionId);
    if (!session) throw RequestError.resourceNotFound(`orchestrai://sessions/${sessionId}`);
    return session;
  }

  private dropSession(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    if (!session) return;
    session.dispose();
    this.sessions.delete(sessionId);
  }

  private async emit(sessionId: string, update: SessionUpdate): Promise<void> {
    if (!this.client || !this.sessions.has(sessionId)) return;
    try {
      await this.client.notify(acp.methods.client.session.update, { sessionId, update });
    } catch (err) {
      this.opts.log.debug(`session/update failed: ${errorMessage(err)}`);
    }
  }

  private toRequestError(err: unknown): unknown {
    if (err instanceof RequestError) return err;
    if (err instanceof ApiError) {
      if (err.status === 401) {
        this.resetAuth();
        return RequestError.authRequired({ authMethods: [DEVICE_AUTH_METHOD_ID] }, 'Your OrchestrAI API key was rejected. Sign in again.');
      }
      if (err.status === 404) return RequestError.resourceNotFound();
      return new RequestError(-32000, err.message, { status: err.status, code: err.code });
    }
    return RequestError.internalError(undefined, errorMessage(err));
  }

  private shutdown(): void {
    for (const id of [...this.sessions.keys()]) this.dropSession(id);
    this.auth?.hub.closeAll();
  }
}
