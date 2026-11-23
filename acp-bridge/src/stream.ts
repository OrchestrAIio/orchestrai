/**
 * Project WebSocket (backend-go websocket.go): `GET /ws?project_id=…&ticket=…`.
 *
 * `/ws` validates a Better Auth session when an Authorization header is sent,
 * so an `ork_` key there is rejected. The bridge instead mints a 60-second HMAC
 * ticket with the API key (`POST /api/v1/ws/ticket`) and connects WITHOUT an
 * Authorization header, which takes the ticket path in WebSocketAuth.
 * Broadcasts are filtered server-side by the conversation audience
 * (stream_audience.go), so the bridge only ever receives what the user may see.
 */
import WebSocket from 'ws';
import { userIdFromTicket, type OrchestraiApi } from './api.js';
import type { Logger } from './log.js';
import { errorMessage } from './log.js';

export interface WsEnvelope {
  type: string;
  run_id?: string;
  project_id?: string;
  data?: Record<string, unknown> | null;
  timestamp?: string;
}

export type WsListener = (msg: WsEnvelope) => void;

export interface WebSocketLike {
  on(event: 'open', cb: () => void): void;
  on(event: 'message', cb: (data: WebSocket.RawData) => void): void;
  on(event: 'close', cb: (code: number, reason: Buffer) => void): void;
  on(event: 'error', cb: (err: Error) => void): void;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  readonly readyState: number;
}

export type WebSocketFactory = (url: string, headers: Record<string, string>) => WebSocketLike;

const defaultFactory: WebSocketFactory = (url, headers) => new WebSocket(url, { headers, handshakeTimeout: 15_000 });

const HEARTBEAT_MS = 25_000;
const MAX_BACKOFF_MS = 30_000;

export class ProjectStream {
  private socket: WebSocketLike | null = null;
  private listeners = new Set<WsListener>();
  private stateListeners = new Set<(connected: boolean) => void>();
  private closed = false;
  private attempt = 0;
  private heartbeat: NodeJS.Timeout | null = null;
  private reconnectTimer: NodeJS.Timeout | null = null;
  private readyPromise: Promise<void> | null = null;
  private resolveReady: (() => void) | null = null;
  private _userId: string | null = null;
  private _connected = false;

  constructor(
    readonly projectId: string,
    private readonly api: OrchestraiApi,
    private readonly log: Logger,
    private readonly userAgent: string,
    private readonly factory: WebSocketFactory = defaultFactory,
  ) {}

  get userId(): string | null {
    return this._userId;
  }

  get connected(): boolean {
    return this._connected;
  }

  subscribe(listener: WsListener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  onConnectionChange(listener: (connected: boolean) => void): () => void {
    this.stateListeners.add(listener);
    return () => this.stateListeners.delete(listener);
  }

  /** Resolves once the socket is open (first connection), rejects if the first ticket fails. */
  start(): Promise<void> {
    if (!this.readyPromise) {
      this.readyPromise = new Promise<void>((resolve, reject) => {
        this.resolveReady = resolve;
        this.connect().catch((err) => {
          this.readyPromise = null;
          reject(err);
        });
      });
    }
    return this.readyPromise;
  }

  private wsUrl(ticket: string): string {
    const url = new URL(this.api.apiUrl);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    url.pathname = `${url.pathname.replace(/\/$/, '')}/ws`;
    url.search = '';
    url.searchParams.set('project_id', this.projectId);
    url.searchParams.set('ticket', ticket);
    return url.toString();
  }

  private async connect(): Promise<void> {
    if (this.closed) return;
    const { ticket } = await this.api.createWsTicket();
    this._userId = userIdFromTicket(ticket) ?? this._userId;
    if (this.closed) return;
    // No Authorization header on purpose (see module comment).
    const socket = this.factory(this.wsUrl(ticket), { 'User-Agent': this.userAgent });
    this.socket = socket;
    socket.on('open', () => {
      this.attempt = 0;
      this._connected = true;
      this.log.debug(`ws connected for project ${this.projectId}`);
      this.startHeartbeat();
      for (const l of this.stateListeners) l(true);
      if (this.resolveReady) {
        this.resolveReady();
        this.resolveReady = null;
      }
    });
    socket.on('message', (raw) => {
      let msg: WsEnvelope;
      try {
        msg = JSON.parse(raw.toString()) as WsEnvelope;
      } catch {
        return;
      }
      if (!msg || typeof msg.type !== 'string') return;
      for (const l of this.listeners) {
        try {
          l(msg);
        } catch (err) {
          this.log.error(`ws listener failed: ${errorMessage(err)}`);
        }
      }
    });
    socket.on('error', (err) => {
      this.log.debug(`ws error for project ${this.projectId}: ${err.message}`);
    });
    socket.on('close', (code) => {
      this.stopHeartbeat();
      const wasConnected = this._connected;
      this._connected = false;
      if (this.socket === socket) this.socket = null;
      if (wasConnected) for (const l of this.stateListeners) l(false);
      if (this.closed) return;
      this.log.debug(`ws closed (${code}) for project ${this.projectId}; reconnecting`);
      this.scheduleReconnect();
    });
  }

  private scheduleReconnect(): void {
    if (this.closed || this.reconnectTimer) return;
    const delay = Math.min(MAX_BACKOFF_MS, 500 * 2 ** this.attempt) + Math.floor(Math.random() * 250);
    this.attempt += 1;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect().catch((err) => {
        this.log.warn(`ws reconnect failed for project ${this.projectId}: ${errorMessage(err)}`);
        this.scheduleReconnect();
      });
    }, delay);
    this.reconnectTimer.unref?.();
  }

  private startHeartbeat(): void {
    this.stopHeartbeat();
    this.heartbeat = setInterval(() => {
      if (this.socket && this.socket.readyState === WebSocket.OPEN) {
        try {
          this.socket.send(JSON.stringify({ type: 'heartbeat' }));
        } catch (err) {
          this.log.debug(`ws heartbeat failed: ${errorMessage(err)}`);
        }
      }
    }, HEARTBEAT_MS);
    this.heartbeat.unref?.();
  }

  private stopHeartbeat(): void {
    if (this.heartbeat) clearInterval(this.heartbeat);
    this.heartbeat = null;
  }

  close(): void {
    this.closed = true;
    this.stopHeartbeat();
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    this.socket?.close(1000, 'bridge closing');
    this.socket = null;
    this.listeners.clear();
    this.stateListeners.clear();
  }
}

/** One socket per project, shared by every ACP session on it. */
export class StreamHub {
  private streams = new Map<string, { stream: ProjectStream; refs: number }>();

  constructor(
    private readonly api: OrchestraiApi,
    private readonly log: Logger,
    private readonly userAgent: string,
    private readonly factory?: WebSocketFactory,
  ) {}

  acquire(projectId: string): ProjectStream {
    const existing = this.streams.get(projectId);
    if (existing) {
      existing.refs += 1;
      return existing.stream;
    }
    const stream = new ProjectStream(projectId, this.api, this.log, this.userAgent, this.factory);
    this.streams.set(projectId, { stream, refs: 1 });
    return stream;
  }

  release(projectId: string): void {
    const entry = this.streams.get(projectId);
    if (!entry) return;
    entry.refs -= 1;
    if (entry.refs <= 0) {
      entry.stream.close();
      this.streams.delete(projectId);
    }
  }

  closeAll(): void {
    for (const { stream } of this.streams.values()) stream.close();
    this.streams.clear();
  }
}
