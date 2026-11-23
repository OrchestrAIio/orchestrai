/**
 * One ACP session ↔ one OrchestrAI conversation.
 *
 * Sending a turn mirrors the web chat (web-qwik ChatPage handleSend):
 *   1. store the user bubble     POST /projects/:pid/conversations/:cid/messages {role:"user"}
 *   2. hand it to the agent      POST /projects/:pid/chat/message (credit gated → 402)
 *   3. stream the reply over the project WebSocket (tokens keyed by the
 *      conversation id, then by any run the agent spawns) and persist it as the
 *      web client does (assistant message created on the first token, patched
 *      as it grows), because the sending client owns reply persistence.
 *
 * Approvals are rendered read-only with a link to the web app. There is no
 * approve path here, by design: an editor-side agent could otherwise be steered
 * into approving its own plan.
 */
import { randomUUID } from 'node:crypto';
import { RequestError, type ContentBlock, type SessionConfigOption, type SessionUpdate, type StopReason } from '@agentclientprotocol/sdk';
import { ApiError, ApiUnreachableError, type ChatMessage, type Conversation, type OrchestraiApi, type Project } from './api.js';
import { billingDenialFrom, billingMessage } from './billing.js';
import type { BridgeConfig } from './config.js';
import type { Logger } from './log.js';
import { errorMessage } from './log.js';
import {
  approvalText,
  AWAITING_STATUSES,
  chatUrl,
  displayName,
  interactionText,
  messageToUpdates,
  phaseFromStatus,
  planEntries,
  promptToMessage,
  runTitle,
  runToolCallId,
  TERMINAL_STATUSES,
  text,
  toolStatusFor,
  type RenderContext,
  type RunPhase,
} from './render.js';
import type { SessionIndex } from './session-ids.js';
import type { ProjectStream, StreamHub, WsEnvelope } from './stream.js';

export const PROJECT_CONFIG_ID = 'project';
const NO_PROJECT_VALUE = '__choose__';
const HISTORY_TURNS = 30;
const PERSIST_THROTTLE_MS = 1500;
const STREAM_CONNECT_TIMEOUT_MS = 15_000;
const SYNC_DEBOUNCE_MS = 400;
const SYNC_WINDOW = 25;
const MAX_ATTACHMENTS = 5;
const MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;

export interface SessionDeps {
  api: OrchestraiApi;
  hub: StreamHub;
  config: BridgeConfig;
  log: Logger;
  index: SessionIndex;
  /** Sends a session/update for this session. */
  emit: (update: SessionUpdate) => Promise<void>;
  clientName: string;
  supportsNotices: boolean;
  listProjects: () => Promise<Project[]>;
  whoAmI: () => Promise<string | null>;
}

interface RunState {
  status: string;
  phase: RunPhase;
  announced: boolean;
  serverMessageId: string | null;
  approvalShown: boolean;
  toolCalls: Set<string>;
}

interface Turn {
  id: string;
  runIds: Set<string>;
  replyText: string;
  replyAcpId: string;
  replyServerId: string | null;
  replyCreating: Promise<void> | null;
  replyDirty: boolean;
  persistTimer: NodeJS.Timeout | null;
  conversationalDone: boolean;
  graceTimer: NodeJS.Timeout | null;
  idleTimer: NodeJS.Timeout | null;
  finished: boolean;
  reason: StopReason;
  resolve: (reason: StopReason) => void;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

export class BridgeSession {
  projectId: string | null;
  conversationId: string | null = null;
  conversation: Conversation | null = null;

  private projects: Project[] = [];
  private stream: ProjectStream | null = null;
  private unsubscribe: (() => void) | null = null;
  private myUserId: string | null = null;
  private names = new Map<string, string>();
  private namesLoadedAt = 0;
  private seen = new Set<string>();
  private knownCount = 0;
  private transcript: Array<{ role: 'user' | 'assistant'; content: string }> = [];
  private turn: Turn | null = null;
  private runs = new Map<string, RunState>();
  private foreignReplyId: string | null = null;
  private skipForeignAssistant = 0;
  private syncTimer: NodeJS.Timeout | null = null;
  private syncQueued = false;
  private syncing: Promise<void> | null = null;
  private pendingPrompt: ContentBlock[] | null = null;
  private disposed = false;
  private eventQueue: Promise<void> = Promise.resolve();

  constructor(
    readonly sessionId: string,
    private readonly deps: SessionDeps,
  ) {
    this.projectId = deps.config.projectId;
  }

  // ---------------------------------------------------------------- lifecycle

  /** session/new: pick the project when it is unambiguous. */
  async initNew(): Promise<SessionConfigOption[]> {
    this.myUserId = await this.deps.whoAmI();
    if (!this.projectId) {
      this.projects = await this.deps.listProjects();
      if (this.projects.length === 1 && this.projects[0]) this.projectId = this.projects[0].id;
    }
    return this.configOptions();
  }

  /** session/load (replay=true) and session/resume (replay=false). */
  async open(ref: { projectId: string; conversationId: string }, replay: boolean): Promise<SessionConfigOption[]> {
    this.projectId = ref.projectId;
    this.conversationId = ref.conversationId;
    this.myUserId = await this.deps.whoAmI();
    const conversation = await this.deps.api.findConversation(ref.projectId, ref.conversationId);
    if (!conversation) {
      throw RequestError.resourceNotFound(`orchestrai://conversations/${ref.conversationId}`);
    }
    this.conversation = conversation;
    await this.loadNames(true);
    const messages = await this.deps.api.listAllMessages(ref.projectId, ref.conversationId);
    this.knownCount = messages.length;
    const ctx = this.renderContext();
    for (const msg of messages) {
      this.seen.add(msg.id);
      this.recordTranscript(msg);
      if (replay) {
        for (const update of messageToUpdates(msg, ctx)) await this.deps.emit(update);
      }
    }
    await this.deps.emit({
      sessionUpdate: 'session_info_update',
      title: conversation.title,
      updatedAt: conversation.updated_at,
    });
    if (replay && !conversation.can_post) {
      await this.deps.emit({
        sessionUpdate: 'agent_message_chunk',
        messageId: `readonly-${randomUUID()}`,
        content: text(
          `_You have view access to this conversation. Ask ${conversation.owner_name || 'its owner'} for driver access to prompt the agent here._`,
        ),
      });
    }
    this.attachStream();
    return this.configOptions();
  }

  dispose(): void {
    this.disposed = true;
    if (this.turn) this.finishTurn('cancelled');
    if (this.syncTimer) clearTimeout(this.syncTimer);
    this.unsubscribe?.();
    this.unsubscribe = null;
    if (this.stream && this.projectId) this.deps.hub.release(this.projectId);
    this.stream = null;
  }

  configOptions(): SessionConfigOption[] {
    const options: Array<{ value: string; name: string; description?: string }> = [];
    const known = this.projects.length > 0 ? this.projects : [];
    for (const p of known) options.push({ value: p.id, name: p.name, description: p.role ? `Your role: ${p.role}` : undefined });
    if (this.projectId && !options.some((o) => o.value === this.projectId)) {
      options.push({ value: this.projectId, name: this.conversation ? `Project ${this.projectId.slice(0, 8)}` : this.projectId });
    }
    if (!this.projectId) options.unshift({ value: NO_PROJECT_VALUE, name: 'Choose a project…' });
    return [
      {
        id: PROJECT_CONFIG_ID,
        name: 'OrchestrAI project',
        description: this.conversationId
          ? 'The project this conversation belongs to.'
          : 'The team project your conversation is created in.',
        type: 'select',
        currentValue: this.projectId ?? NO_PROJECT_VALUE,
        options,
      },
    ];
  }

  async setProject(value: string): Promise<SessionConfigOption[]> {
    if (value === NO_PROJECT_VALUE) return this.configOptions();
    if (this.conversationId && value !== this.projectId) {
      throw RequestError.invalidParams(
        { configId: PROJECT_CONFIG_ID },
        'The project is fixed once the conversation has started. Open a new thread to use another project.',
      );
    }
    if (this.projects.length === 0) this.projects = await this.deps.listProjects();
    if (!this.projects.some((p) => p.id === value)) {
      throw RequestError.invalidParams({ configId: PROJECT_CONFIG_ID, value }, 'Unknown OrchestrAI project.');
    }
    this.projectId = value;
    return this.configOptions();
  }

  // ---------------------------------------------------------------- prompting

  async prompt(blocks: ContentBlock[]): Promise<StopReason> {
    if (this.turn) throw RequestError.invalidRequest(undefined, 'A prompt is already running in this session.');
    let blocksToSend = blocks;

    if (!this.projectId) {
      const chosen = await this.handleProjectPicker(blocks);
      if (!chosen) return 'end_turn';
      blocksToSend = chosen;
    }
    const projectId = this.projectId;
    if (!projectId) return 'end_turn';

    const { text: messageText, attachments } = promptToMessage(blocksToSend);
    if (!messageText && attachments.length === 0) {
      await this.say('Send a message for the OrchestrAI agent.');
      return 'end_turn';
    }
    if (attachments.length > MAX_ATTACHMENTS) {
      await this.say(`OrchestrAI accepts at most ${MAX_ATTACHMENTS} attached files per message; remove some and resend.`);
      return 'end_turn';
    }
    if (attachments.reduce((n, a) => n + a.size_bytes, 0) > MAX_ATTACHMENT_BYTES) {
      await this.say('Attached context is over the 10 MB limit for one message; attach less and resend.');
      return 'end_turn';
    }

    try {
      if (!this.conversationId) await this.createConversation(projectId, messageText || 'Editor conversation');
      if (this.conversation && !this.conversation.can_post) {
        await this.say(
          `You have view access to this conversation, so you can follow it here but not prompt the agent. Ask ${this.conversation.owner_name || 'its owner'} for driver access: ${chatUrl(this.deps.config.webUrl, projectId, this.conversationId ?? '')}`,
        );
        return 'end_turn';
      }
      this.attachStream();
      await this.waitForStream();
    } catch (err) {
      return this.handleSendError(err);
    }

    const conversationId = this.conversationId as string;
    const turn = this.beginTurn(conversationId);
    const content = messageText || attachments.map((a) => `(attached ${a.name})`).join('\n');

    try {
      const stored = await this.deps.api.createMessage(projectId, conversationId, {
        role: 'user',
        content,
        metadata: {
          source: 'acp',
          client: this.deps.clientName,
          ...(attachments.length > 0
            ? { attachments: attachments.map((a) => ({ name: a.name, mime_type: a.mime_type, size_bytes: a.size_bytes })) }
            : {}),
        },
      });
      this.seen.add(stored.id);
      this.knownCount += 1;
      this.transcript.push({ role: 'user', content });
      await this.deps.api.sendChatMessage(projectId, {
        message: content,
        conversation_id: conversationId,
        conversation_history: this.history(),
        message_id: stored.id,
        ...(attachments.length > 0 ? { attachments } : {}),
      });
    } catch (err) {
      try {
        const reason = await this.handleSendError(err);
        this.finishTurn(reason);
        return reason;
      } catch (fatal) {
        this.finishTurn('end_turn');
        throw fatal;
      }
    }
    if (!turn.finished) this.armIdleTimer(turn);
    return new Promise<StopReason>((resolve) => {
      if (turn.finished) {
        resolve(turn.reason);
        return;
      }
      turn.resolve = resolve;
    });
  }

  /** session/cancel: stop streaming now; ask the backend to stop spawned runs. */
  async cancel(): Promise<void> {
    const turn = this.turn;
    if (!turn || !this.projectId) return;
    const runIds = [...turn.runIds].filter((id) => id !== this.conversationId);
    this.finishTurn('cancelled');
    for (const runId of runIds) {
      try {
        await this.deps.api.stopAgentRun(this.projectId, runId);
      } catch (err) {
        if (!(err instanceof ApiError && err.status === 409)) {
          this.deps.log.warn(`Could not stop run ${runId}: ${errorMessage(err)}`);
        }
      }
    }
  }

  private async handleProjectPicker(blocks: ContentBlock[]): Promise<ContentBlock[] | null> {
    if (this.projects.length === 0) this.projects = await this.deps.listProjects();
    if (this.projects.length === 0) {
      await this.say(
        `You don't have an OrchestrAI project yet. Create a team space at ${this.deps.config.webUrl} and try again.`,
      );
      return null;
    }
    if (this.projects.length === 1 && this.projects[0]) {
      this.projectId = this.projects[0].id;
      return blocks;
    }
    const reply = promptToMessage(blocks).text.trim();
    if (this.pendingPrompt) {
      const asNumber = Number.parseInt(reply, 10);
      const byIndex = Number.isInteger(asNumber) && String(asNumber) === reply ? this.projects[asNumber - 1] : undefined;
      const byName = this.projects.find((p) => p.name.toLowerCase() === reply.toLowerCase() || p.id === reply);
      const chosen = byIndex ?? byName;
      if (chosen) {
        this.projectId = chosen.id;
        const original = this.pendingPrompt;
        this.pendingPrompt = null;
        await this.deps.emit({ sessionUpdate: 'config_option_update', configOptions: this.configOptions() });
        return original;
      }
    } else {
      this.pendingPrompt = blocks;
    }
    const list = this.projects.map((p, i) => `${i + 1}. **${p.name}**${p.role ? ` (${p.role})` : ''}`).join('\n');
    await this.say(
      `Which OrchestrAI project should this conversation live in? Reply with its number or name (or pick it in the session's "OrchestrAI project" option):\n\n${list}\n\nSet ORCHESTRAI_PROJECT_ID to skip this question.`,
    );
    return null;
  }

  private async createConversation(projectId: string, firstMessage: string): Promise<void> {
    const oneLine = firstMessage.replace(/\s+/g, ' ').trim();
    const title = oneLine.length > 50 ? `${oneLine.slice(0, 47)}...` : oneLine || 'Editor conversation';
    const conversation = await this.deps.api.createConversation(projectId, title);
    this.conversation = conversation;
    this.conversationId = conversation.id;
    this.seen.clear();
    this.knownCount = 0;
    await this.deps.index.remember(this.sessionId, { projectId, conversationId: conversation.id });
    await this.deps.emit({ sessionUpdate: 'session_info_update', title: conversation.title, updatedAt: conversation.updated_at });
  }

  private async handleSendError(err: unknown): Promise<StopReason> {
    const denial = billingDenialFrom(err, this.deps.config.webUrl);
    if (denial) {
      await this.say(billingMessage(denial));
      return 'end_turn';
    }
    if (err instanceof ApiError) {
      // The agent maps this to an auth_required error and drops the bad key.
      if (err.status === 401) throw err;
      if (err.code === 'CONVERSATION_READ_ONLY' || err.status === 403) {
        await this.say(`OrchestrAI refused this message: ${err.message}`);
        return 'end_turn';
      }
      if (err.status === 429) {
        await this.say(`OrchestrAI is rate limiting you right now (${err.message}). Wait a moment and resend.`);
        return 'end_turn';
      }
      await this.say(`OrchestrAI could not take this message: ${err.message}`);
      return 'end_turn';
    }
    if (err instanceof ApiUnreachableError) {
      await this.say(`${err.message}. Check your connection and resend.`);
      return 'end_turn';
    }
    if (err instanceof RequestError) throw err;
    this.deps.log.error(`prompt failed: ${errorMessage(err)}`);
    await this.say(`Something went wrong talking to OrchestrAI: ${errorMessage(err)}`);
    return 'end_turn';
  }

  // ---------------------------------------------------------------- turn state

  private beginTurn(conversationId: string): Turn {
    const turn: Turn = {
      id: randomUUID(),
      runIds: new Set([conversationId]),
      replyText: '',
      replyAcpId: `reply-${randomUUID()}`,
      replyServerId: null,
      replyCreating: null,
      replyDirty: false,
      persistTimer: null,
      conversationalDone: false,
      graceTimer: null,
      idleTimer: null,
      finished: false,
      reason: 'end_turn',
      resolve: () => undefined,
    };
    this.turn = turn;
    this.foreignReplyId = null;
    return turn;
  }

  private armIdleTimer(turn: Turn): void {
    if (turn.idleTimer) clearTimeout(turn.idleTimer);
    turn.idleTimer = setTimeout(() => {
      if (turn.finished || this.turn !== turn) return;
      const link = chatUrl(this.deps.config.webUrl, this.projectId ?? '', this.conversationId ?? '');
      void this.sayInTurn(
        turn,
        `\n\n_The agent has not said anything for a while. It may still be working; progress keeps appearing here and in the web app: ${link}_`,
      ).finally(() => this.finishTurn('end_turn'));
    }, this.deps.config.turnIdleTimeoutMs);
    turn.idleTimer.unref?.();
  }

  private finishTurn(reason: StopReason): void {
    const turn = this.turn;
    if (!turn || turn.finished) return;
    turn.finished = true;
    turn.reason = reason;
    if (turn.idleTimer) clearTimeout(turn.idleTimer);
    if (turn.graceTimer) clearTimeout(turn.graceTimer);
    this.turn = null;
    void this.flushReply(turn, reason === 'cancelled').finally(() => {
      if (turn.replyText.trim()) this.transcript.push({ role: 'assistant', content: turn.replyText });
      if (this.syncQueued) this.scheduleSync();
    });
    turn.resolve(reason);
  }

  private maybeCompleteConversational(turn: Turn): void {
    if (!turn.conversationalDone || turn.finished) return;
    const spawnedRun = [...turn.runIds].some((id) => id !== this.conversationId);
    if (spawnedRun) return;
    if (turn.graceTimer) clearTimeout(turn.graceTimer);
    turn.graceTimer = setTimeout(() => {
      const stillNoRun = ![...turn.runIds].some((id) => id !== this.conversationId);
      if (stillNoRun) this.finishTurn('end_turn');
    }, this.deps.config.runGraceMs);
    turn.graceTimer.unref?.();
  }

  private async appendTurnText(turn: Turn, delta: string): Promise<void> {
    if (!delta) return;
    turn.replyText += delta;
    turn.replyDirty = true;
    await this.deps.emit({ sessionUpdate: 'agent_message_chunk', messageId: turn.replyAcpId, content: text(delta) });
    this.schedulePersist(turn);
  }

  private async sayInTurn(turn: Turn, message: string): Promise<void> {
    await this.appendTurnText(turn, message);
  }

  private schedulePersist(turn: Turn): void {
    if (!this.projectId || !this.conversationId) return;
    if (!turn.replyServerId && !turn.replyCreating) {
      const projectId = this.projectId;
      const conversationId = this.conversationId;
      const initial = turn.replyText;
      turn.replyDirty = false;
      turn.replyCreating = this.deps.api
        .createMessage(projectId, conversationId, { role: 'assistant', content: initial, metadata: { source: 'acp_reply' } })
        .then((msg) => {
          turn.replyServerId = msg.id;
          this.seen.add(msg.id);
          this.knownCount += 1;
          if (turn.replyText !== initial) turn.replyDirty = true;
        })
        .catch((err) => {
          this.deps.log.warn(`could not persist the agent reply: ${errorMessage(err)}`);
          turn.replyDirty = true;
        })
        .finally(() => {
          turn.replyCreating = null;
        });
      return;
    }
    if (turn.persistTimer) return;
    turn.persistTimer = setTimeout(() => {
      turn.persistTimer = null;
      void this.patchReply(turn);
    }, PERSIST_THROTTLE_MS);
    turn.persistTimer.unref?.();
  }

  private async patchReply(turn: Turn, metadata?: Record<string, unknown>): Promise<void> {
    if (!this.projectId || !this.conversationId || !turn.replyServerId) return;
    if (!turn.replyDirty && !metadata) return;
    turn.replyDirty = false;
    try {
      await this.deps.api.updateMessage(this.projectId, this.conversationId, turn.replyServerId, {
        content: turn.replyText,
        ...(metadata ? { metadata } : {}),
      });
    } catch (err) {
      turn.replyDirty = true;
      this.deps.log.warn(`could not update the agent reply: ${errorMessage(err)}`);
    }
  }

  private async flushReply(turn: Turn, cancelled: boolean): Promise<void> {
    if (turn.persistTimer) clearTimeout(turn.persistTimer);
    turn.persistTimer = null;
    if (turn.replyCreating) await turn.replyCreating;
    if (!this.projectId || !this.conversationId) return;
    if (!turn.replyServerId) {
      if (!turn.replyText.trim()) return;
      try {
        const msg = await this.deps.api.createMessage(this.projectId, this.conversationId, {
          role: 'assistant',
          content: turn.replyText,
          metadata: { source: 'acp_reply', ...(cancelled ? { cancelled: true } : {}) },
        });
        this.seen.add(msg.id);
        this.knownCount += 1;
      } catch (err) {
        this.deps.log.warn(`could not persist the agent reply: ${errorMessage(err)}`);
      }
      return;
    }
    await this.patchReply(turn, cancelled ? { source: 'acp_reply', cancelled: true } : undefined);
  }

  // ---------------------------------------------------------------- streaming

  private attachStream(): void {
    if (this.stream || !this.projectId || this.disposed) return;
    this.stream = this.deps.hub.acquire(this.projectId);
    // Events are applied strictly in arrival order (a run's status updates
    // must not overtake the creation of its tool call / transcript row).
    this.unsubscribe = this.stream.subscribe((msg) => {
      this.eventQueue = this.eventQueue
        .then(() => this.onEvent(msg))
        .catch((err) => this.deps.log.error(`event handling failed: ${errorMessage(err)}`));
    });
    this.stream.start().catch((err) => this.deps.log.warn(`live stream unavailable: ${errorMessage(err)}`));
  }

  private async waitForStream(): Promise<void> {
    if (!this.stream) return;
    let timer: NodeJS.Timeout | null = null;
    try {
      await Promise.race([
        this.stream.start(),
        new Promise<never>((_, reject) => {
          timer = setTimeout(
            () => reject(new ApiUnreachableError('Could not open the OrchestrAI live stream (WebSocket)')),
            STREAM_CONNECT_TIMEOUT_MS,
          );
        }),
      ]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  /** Public for tests: feed one WebSocket envelope. */
  async onEvent(msg: WsEnvelope): Promise<void> {
    if (this.disposed || !this.conversationId) return;
    const data = isRecord(msg.data) ? msg.data : {};
    switch (msg.type) {
      case 'tokens':
        await this.onTokens(data);
        return;
      case 'agent_status':
        await this.onStatus(data);
        return;
      case 'tool_execution':
        await this.onToolExecution(data);
        return;
      case 'approval_required':
        await this.onApproval(data);
        return;
      case 'conversation_updated':
        if (str(data.conversation_id) !== this.conversationId) return;
        if (str(data.kind) === 'visibility') {
          await this.refreshConversation();
          return;
        }
        if (str(data.kind) === 'message_created') this.scheduleSync();
        return;
      default:
        return;
    }
  }

  private async onTokens(data: Record<string, unknown>): Promise<void> {
    const runId = str(data.run_id);
    const tokens = isRecord(data.tokens) ? data.tokens : {};
    const delta = str(tokens.delta) || str(data.delta);
    const finish = str(tokens.finish_reason);
    const turn = this.turn;
    if (turn && turn.runIds.has(runId)) {
      this.armIdleTimer(turn);
      await this.appendTurnText(turn, delta);
      if (finish && runId === this.conversationId) {
        turn.conversationalDone = true;
        this.maybeCompleteConversational(turn);
      }
      return;
    }
    if (turn) return;
    // Out of turn: a teammate prompted the agent in this shared conversation.
    if (runId !== this.conversationId && !this.runs.has(runId)) return;
    if (delta) {
      if (!this.foreignReplyId) {
        this.foreignReplyId = `live-${randomUUID()}`;
        // Their client persists this reply; skip it when the sync sees it.
        if (runId === this.conversationId) this.skipForeignAssistant += 1;
      }
      await this.deps.emit({ sessionUpdate: 'agent_message_chunk', messageId: this.foreignReplyId, content: text(delta) });
    }
    if (finish && runId === this.conversationId) this.foreignReplyId = null;
  }

  private async onStatus(data: Record<string, unknown>): Promise<void> {
    const runId = str(data.run_id);
    const status = str(data.status).toLowerCase();
    const metadata = isRecord(data.metadata) ? data.metadata : {};
    const turn = this.turn;

    if (runId === this.conversationId) {
      if (!turn) return;
      this.armIdleTimer(turn);
      if (status === 'clarification') {
        const interaction = data.interaction ?? metadata.interaction;
        const body = interactionText(interaction);
        if (body) {
          await this.appendTurnText(turn, `${turn.replyText ? '\n\n' : ''}${body}`);
          if (this.projectId && this.conversationId && isRecord(interaction)) {
            await this.patchOrCreateInteraction(turn, interaction);
          }
        }
        this.finishTurn('end_turn');
      }
      return;
    }
    if (!runId || !status) return;

    const statusConversation = str(data.conversation_id) || str(metadata.conversation_id);
    let run = this.runs.get(runId);
    if (!run) {
      // Only runs the server attributes to this conversation are adopted:
      // guessing by timing could attach (and later cancel) another
      // conversation's run in a busy project.
      if (statusConversation !== this.conversationId) return;
      run = { status, phase: 'analysis', announced: false, serverMessageId: null, approvalShown: false, toolCalls: new Set() };
      this.runs.set(runId, run);
    }
    if (turn) {
      turn.runIds.add(runId);
      this.armIdleTimer(turn);
      if (turn.graceTimer) clearTimeout(turn.graceTimer);
    }
    run.status = status;
    const detail = str(data.phase_detail) || str(data.message);
    await this.announceRun(runId, run, status, detail, turn !== null);

    const phase = phaseFromStatus(status);
    if (phase && phase !== run.phase) {
      run.phase = phase;
      await this.deps.emit({ sessionUpdate: 'plan', entries: planEntries(phase) });
    }

    const planContent = str(metadata.plan_content);
    if (planContent && turn) await this.appendTurnText(turn, `${turn.replyText ? '\n\n' : ''}${planContent}`);

    if (AWAITING_STATUSES.has(status)) {
      const approval = isRecord(metadata.approval) ? metadata.approval : {};
      await this.showApproval(runId, run, approval);
      if (turn) this.finishTurn('end_turn');
      return;
    }
    if (TERMINAL_STATUSES.has(status)) {
      if (status === 'completed') await this.deps.emit({ sessionUpdate: 'plan', entries: planEntries('done') });
      const summary = str(data.message) || str(data.error);
      if (turn) {
        if (summary) await this.appendTurnText(turn, `${turn.replyText ? '\n\n' : ''}${summary}`);
        this.finishTurn('end_turn');
      }
    }
  }

  private async patchOrCreateInteraction(turn: Turn, interaction: Record<string, unknown>): Promise<void> {
    if (turn.replyCreating) await turn.replyCreating;
    if (turn.replyServerId) {
      await this.patchReply(turn, { source: 'acp_reply', interaction });
    }
  }

  private async announceRun(runId: string, run: RunState, status: string, detail: string, inTurn: boolean): Promise<void> {
    const toolCallId = runToolCallId(runId);
    const link = chatUrl(this.deps.config.webUrl, this.projectId ?? '', this.conversationId ?? '');
    if (!run.announced) {
      run.announced = true;
      await this.deps.emit({
        sessionUpdate: 'tool_call',
        toolCallId,
        title: runTitle(status, detail),
        kind: 'execute',
        status: toolStatusFor(status),
        content: [{ type: 'content', content: text(`OrchestrAI started run ${runId}. Follow it live: ${link}`) }],
        rawInput: { runId },
      });
      if (inTurn && this.projectId && this.conversationId) {
        try {
          const msg = await this.deps.api.createMessage(this.projectId, this.conversationId, {
            role: 'run',
            content: '',
            metadata: { runId, status, runPhase: 'thinking', intentType: 'action', source: 'acp' },
          });
          run.serverMessageId = msg.id;
          this.seen.add(msg.id);
          this.knownCount += 1;
        } catch (err) {
          this.deps.log.warn(`could not record run ${runId} in the conversation: ${errorMessage(err)}`);
        }
      }
      return;
    }
    await this.deps.emit({
      sessionUpdate: 'tool_call_update',
      toolCallId,
      title: runTitle(status, detail),
      status: toolStatusFor(status),
    });
    if (run.serverMessageId && this.projectId && this.conversationId && (AWAITING_STATUSES.has(status) || TERMINAL_STATUSES.has(status))) {
      try {
        await this.deps.api.updateMessage(this.projectId, this.conversationId, run.serverMessageId, {
          metadata: { runId, status, runPhase: TERMINAL_STATUSES.has(status) ? 'completed' : 'awaiting_approval', intentType: 'action', source: 'acp' },
        });
      } catch (err) {
        this.deps.log.debug(`run message update failed: ${errorMessage(err)}`);
      }
    }
  }

  private async showApproval(runId: string, run: RunState, approval: Record<string, unknown>): Promise<void> {
    if (run.approvalShown) return;
    run.approvalShown = true;
    const body = approvalText(approval, this.deps.config.webUrl, this.projectId ?? '', this.conversationId ?? '');
    await this.deps.emit({
      sessionUpdate: 'tool_call_update',
      toolCallId: runToolCallId(runId),
      status: 'pending',
      title: runTitle('awaiting_approval'),
      content: [{ type: 'content', content: text(body) }],
    });
    const turn = this.turn;
    if (turn) {
      await this.appendTurnText(turn, `${turn.replyText ? '\n\n' : ''}${body}`);
    } else {
      await this.deps.emit({ sessionUpdate: 'agent_message_chunk', messageId: `approval-${runId}`, content: text(body) });
      if (this.deps.supportsNotices) {
        await this.deps.emit({
          sessionUpdate: 'notice',
          severity: 'warning',
          title: 'An OrchestrAI run is waiting for approval',
          description: 'Approve or reject it in the OrchestrAI web app.',
        });
      }
    }
  }

  private async onToolExecution(data: Record<string, unknown>): Promise<void> {
    const runId = str(data.run_id);
    const run = this.runs.get(runId);
    if (!run) return;
    const ev = isRecord(data.tool_event) ? data.tool_event : {};
    const tool = str(ev.tool_name) || 'tool';
    const phase = str(ev.phase);
    const evStatus = str(ev.status).toLowerCase();
    const toolCallId = `orchestrai-tool-${runId}-${tool}-${phase || 'run'}`;
    const status = evStatus === 'completed' ? 'completed' : evStatus === 'failed' ? 'failed' : 'in_progress';
    const title = `${tool}${str(ev.message) ? ` — ${str(ev.message)}` : ''}`;
    if (this.turn) this.armIdleTimer(this.turn);
    if (!run.toolCalls.has(toolCallId)) {
      run.toolCalls.add(toolCallId);
      await this.deps.emit({
        sessionUpdate: 'tool_call',
        toolCallId,
        title,
        kind: phase === 'execution' ? 'execute' : phase === 'verification' ? 'read' : 'think',
        status,
        rawInput: { runId, tool, phase },
      });
    } else {
      await this.deps.emit({
        sessionUpdate: 'tool_call_update',
        toolCallId,
        title,
        status,
        ...(str(ev.error) ? { content: [{ type: 'content' as const, content: text(str(ev.error)) }] } : {}),
      });
    }
    const next = phaseFromStatus(run.status, phase);
    if (next && next !== run.phase && next !== 'done') {
      run.phase = next;
      await this.deps.emit({ sessionUpdate: 'plan', entries: planEntries(next) });
    }
  }

  private async onApproval(data: Record<string, unknown>): Promise<void> {
    const approval = isRecord(data.approval) ? data.approval : data;
    const runId = str(approval.run_id);
    const run = this.runs.get(runId);
    if (!run) return;
    await this.showApproval(runId, run, approval);
    if (this.turn && this.turn.runIds.has(runId)) this.finishTurn('end_turn');
  }

  // ---------------------------------------------------------------- teammates

  private scheduleSync(): void {
    if (this.turn) {
      // Our own reply rows are only known once the turn settles; sync after.
      this.syncQueued = true;
      return;
    }
    this.syncQueued = false;
    if (this.syncTimer) clearTimeout(this.syncTimer);
    this.syncTimer = setTimeout(() => {
      this.syncTimer = null;
      if (!this.syncing) {
        this.syncing = this.syncNewMessages().finally(() => {
          this.syncing = null;
        });
      } else {
        this.syncQueued = true;
        void this.syncing.then(() => this.scheduleSync());
      }
    }, SYNC_DEBOUNCE_MS);
    this.syncTimer.unref?.();
  }

  /** Public for tests: pull messages written by others and render them. */
  async syncNewMessages(): Promise<void> {
    if (!this.projectId || !this.conversationId || this.disposed) return;
    const offset = Math.max(0, this.knownCount - SYNC_WINDOW);
    let page: ChatMessage[];
    try {
      page = await this.deps.api.listMessages(this.projectId, this.conversationId, 500, offset);
    } catch (err) {
      this.deps.log.warn(`could not refresh the conversation: ${errorMessage(err)}`);
      return;
    }
    this.knownCount = offset + page.length;
    const fresh = page.filter((m) => !this.seen.has(m.id));
    if (fresh.some((m) => m.author_user_id && !this.names.has(m.author_user_id))) await this.loadNames(false);
    const ctx = this.renderContext();
    for (const msg of fresh) {
      this.seen.add(msg.id);
      if (msg.role === 'assistant' && this.skipForeignAssistant > 0) {
        this.skipForeignAssistant -= 1;
        this.recordTranscript(msg);
        continue;
      }
      if (msg.role === 'run') {
        const runId = isRecord(msg.metadata) ? str(msg.metadata.runId) : '';
        if (runId && this.runs.has(runId)) continue;
        if (runId) {
          this.runs.set(runId, {
            status: isRecord(msg.metadata) ? str(msg.metadata.status) || 'running' : 'running',
            phase: 'analysis',
            announced: true,
            serverMessageId: msg.id,
            approvalShown: false,
            toolCalls: new Set(),
          });
        }
      }
      this.recordTranscript(msg);
      for (const update of messageToUpdates(msg, ctx)) await this.deps.emit(update);
      const fromTeammate = msg.role === 'user' && msg.author_user_id && msg.author_user_id !== this.myUserId;
      if (fromTeammate && this.deps.supportsNotices) {
        const excerpt = msg.content.replace(/\s+/g, ' ').trim();
        await this.deps.emit({
          sessionUpdate: 'notice',
          severity: 'info',
          title: `${displayName(ctx, msg.author_user_id)} posted in “${this.conversation?.title ?? 'this conversation'}”`,
          description: excerpt.length > 140 ? `${excerpt.slice(0, 137)}...` : excerpt,
        });
      }
    }
  }

  private async refreshConversation(): Promise<void> {
    if (!this.projectId || !this.conversationId) return;
    try {
      const fresh = await this.deps.api.findConversation(this.projectId, this.conversationId);
      if (fresh) this.conversation = fresh;
    } catch (err) {
      this.deps.log.debug(`conversation refresh failed: ${errorMessage(err)}`);
    }
  }

  private async loadNames(force: boolean): Promise<void> {
    if (!this.projectId || !this.conversationId) return;
    if (!force && Date.now() - this.namesLoadedAt < 60_000) return;
    this.namesLoadedAt = Date.now();
    try {
      const participants = await this.deps.api.listParticipants(this.projectId, this.conversationId);
      for (const p of participants) this.names.set(p.user_id, p.name || p.email || 'A teammate');
    } catch (err) {
      this.deps.log.debug(`participants unavailable: ${errorMessage(err)}`);
    }
    if (this.conversation?.owner_name && this.conversation.user_id && !this.names.has(this.conversation.user_id)) {
      this.names.set(this.conversation.user_id, this.conversation.owner_name);
    }
  }

  // ---------------------------------------------------------------- helpers

  private renderContext(): RenderContext {
    return {
      myUserId: this.myUserId,
      names: this.names,
      webUrl: this.deps.config.webUrl,
      projectId: this.projectId ?? '',
      conversationId: this.conversationId ?? '',
    };
  }

  private recordTranscript(msg: ChatMessage): void {
    if (!msg.content.trim()) return;
    if (msg.role === 'user') this.transcript.push({ role: 'user', content: msg.content });
    else if (msg.role === 'assistant' || msg.role === 'run') this.transcript.push({ role: 'assistant', content: msg.content });
  }

  private history(): Array<{ role: string; content: string }> {
    return this.transcript.slice(-HISTORY_TURNS).map((m) => ({ role: m.role, content: m.content }));
  }

  private async say(message: string): Promise<void> {
    await this.deps.emit({ sessionUpdate: 'agent_message_chunk', messageId: `bridge-${randomUUID()}`, content: text(message) });
  }
}
