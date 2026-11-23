/**
 * Pure mapping from OrchestrAI chat data / stream events to ACP session updates.
 *
 * Teammate turns: ACP has no multi-author model. A message a teammate sent is
 * rendered as a `user_message_chunk` whose text starts with a bold author
 * label ("**Dana:** …") and whose `_meta.orchestrai` carries the author, so
 * every client shows who said it (Zed renders it as a user bubble in the
 * thread). Agent replies are `agent_message_chunk`s regardless of who prompted.
 */
import type { ContentBlock, PlanEntry, SessionUpdate, ToolCallStatus } from '@agentclientprotocol/sdk';
import type { ChatMessage } from './api.js';

export interface RenderContext {
  myUserId: string | null;
  /** user id → display name (conversation owner + participants). */
  names: Map<string, string>;
  webUrl: string;
  projectId: string;
  conversationId: string;
}

export function text(t: string): ContentBlock {
  return { type: 'text', text: t };
}

export function chatUrl(webUrl: string, projectId: string, conversationId: string): string {
  return `${webUrl}/projects/${encodeURIComponent(projectId)}/chat/${encodeURIComponent(conversationId)}`;
}

export function approvalsUrl(webUrl: string, projectId: string): string {
  return `${webUrl}/approvals?project=${encodeURIComponent(projectId)}`;
}

export function displayName(ctx: RenderContext, userId: string | null | undefined): string {
  if (!userId) return 'A teammate';
  return ctx.names.get(userId)?.trim() || 'A teammate';
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/** Clarification / safety-guard interaction → Markdown the user can answer by typing. */
export function interactionText(interaction: unknown): string {
  if (!isRecord(interaction)) return '';
  const lines: string[] = [];
  const prompt = str(interaction.prompt);
  if (prompt) lines.push(prompt);
  const groups = interaction.clarificationGroups ?? interaction.clarification_groups;
  const renderOptions = (options: unknown) => {
    if (!Array.isArray(options)) return;
    options.filter(isRecord).forEach((o, i) => {
      const label = str(o.label) || str(o.value);
      if (!label) return;
      const reply = str(o.sendText) || str(o.send_text) || str(o.value);
      const desc = str(o.description);
      lines.push(`${i + 1}. **${label}**${desc ? ` — ${desc}` : ''}${reply && reply !== label ? ` (reply: \`${reply}\`)` : ''}`);
    });
  };
  if (Array.isArray(groups) && groups.length > 1) {
    for (const g of groups.filter(isRecord)) {
      const q = str(g.question);
      if (q) lines.push('', `**${q}**`);
      renderOptions(g.options);
    }
  } else {
    renderOptions(interaction.options);
  }
  const allowFree = interaction.allowFreeText ?? interaction.allow_free_text;
  if (allowFree !== false) lines.push('', '_Reply with an option or type your own answer._');
  return lines.join('\n').trim();
}

/** Run status (agent_run / agent_status) → ACP tool-call status. */
export function toolStatusFor(status: string): ToolCallStatus {
  switch (status.toLowerCase()) {
    case 'completed':
    case 'execution_completed':
      return 'completed';
    case 'failed':
    case 'cancelled':
    case 'canceled':
    case 'rejected':
      return 'failed';
    case 'pending':
    case 'queued':
    case 'awaiting_approval':
    case 'awaiting_confirmation':
    case 'pending_confirmation':
    case 'awaiting_input':
      return 'pending';
    default:
      return 'in_progress';
  }
}

export const AWAITING_STATUSES = new Set([
  'awaiting_approval',
  'awaiting_confirmation',
  'pending_confirmation',
  'awaiting_input',
]);
export const TERMINAL_STATUSES = new Set(['completed', 'failed', 'cancelled', 'canceled']);

export function runToolCallId(runId: string): string {
  return `orchestrai-run-${runId}`;
}

export function runTitle(status: string, detail?: string): string {
  const s = status.toLowerCase();
  if (AWAITING_STATUSES.has(s)) return 'OrchestrAI run — waiting for approval in the web app';
  if (s === 'completed') return 'OrchestrAI run — completed';
  if (s === 'failed') return 'OrchestrAI run — failed';
  if (s === 'cancelled' || s === 'canceled') return 'OrchestrAI run — cancelled';
  return detail ? `OrchestrAI run — ${detail}` : 'OrchestrAI run';
}

/**
 * The run lifecycle as an ACP plan (OrchestrAI streams phases, not step lists):
 * analyze → plan → human approval (web app) → execute → verify.
 */
export type RunPhase = 'analysis' | 'planning' | 'approval' | 'execution' | 'verification' | 'done';
const PHASES: Array<{ key: Exclude<RunPhase, 'done'>; label: string }> = [
  { key: 'analysis', label: 'Analyze the request' },
  { key: 'planning', label: 'Build an execution plan' },
  { key: 'approval', label: 'Human approval in the OrchestrAI web app' },
  { key: 'execution', label: 'Execute the approved plan' },
  { key: 'verification', label: 'Verify the result' },
];

export function planEntries(phase: RunPhase): PlanEntry[] {
  const idx = phase === 'done' ? PHASES.length : PHASES.findIndex((p) => p.key === phase);
  return PHASES.map((p, i) => ({
    content: p.label,
    priority: 'medium',
    status: i < idx ? 'completed' : i === idx ? 'in_progress' : 'pending',
  }));
}

export function phaseFromStatus(status: string, toolPhase?: string): RunPhase | null {
  const s = status.toLowerCase();
  if (TERMINAL_STATUSES.has(s)) return s === 'completed' ? 'done' : null;
  if (AWAITING_STATUSES.has(s)) return 'approval';
  if (toolPhase && ['analysis', 'planning', 'execution', 'verification'].includes(toolPhase)) {
    return toolPhase as RunPhase;
  }
  if (s === 'pending' || s === 'queued' || s === 'thinking') return 'analysis';
  if (s === 'executing') return 'execution';
  return null;
}

/** Approval card (approval_required / status metadata.approval). Read-only by design. */
export function approvalText(approval: Record<string, unknown>, webUrl: string, projectId: string, conversationId: string): string {
  const lines = ['**Approval required** — this can only be approved by a person in the OrchestrAI web app.'];
  const op = str(approval.operation) || str(approval.tool_name);
  if (op) lines.push(`- Action: ${op}`);
  const risk = str(approval.risk_level);
  if (risk) lines.push(`- Risk: ${risk}`);
  const cost = approval.estimated_cost;
  if (typeof cost === 'string' && cost) lines.push(`- Estimated cost: ${cost}`);
  if (typeof cost === 'number') lines.push(`- Estimated cost: $${cost.toFixed(2)}`);
  const resources = Array.isArray(approval.affected_resources) ? approval.affected_resources.filter((r) => typeof r === 'string') : [];
  if (resources.length > 0) {
    lines.push(`- Affects: ${resources.slice(0, 8).join(', ')}${resources.length > 8 ? ` (+${resources.length - 8} more)` : ''}`);
  }
  const expires = str(approval.expires_at);
  if (expires) lines.push(`- Expires: ${expires}`);
  lines.push('', `Review and approve: ${approvalsUrl(webUrl, projectId)}`, `Conversation: ${chatUrl(webUrl, projectId, conversationId)}`);
  return lines.join('\n');
}

function messageMeta(msg: ChatMessage, ctx: RenderContext, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    orchestrai: {
      messageId: msg.id,
      conversationId: msg.conversation_id,
      authorUserId: msg.author_user_id ?? null,
      authorName: msg.author_user_id ? displayName(ctx, msg.author_user_id) : null,
      createdAt: msg.created_at,
      ...extra,
    },
  };
}

/** Converts one persisted message into the updates that replay it. */
export function messageToUpdates(msg: ChatMessage, ctx: RenderContext): SessionUpdate[] {
  const metadata = isRecord(msg.metadata) ? msg.metadata : {};
  const content = msg.content ?? '';
  switch (msg.role) {
    case 'user': {
      if (!content.trim()) return [];
      const mine = !msg.author_user_id || msg.author_user_id === ctx.myUserId;
      const body = mine ? content : `**${displayName(ctx, msg.author_user_id)}:** ${content}`;
      return [
        {
          sessionUpdate: 'user_message_chunk',
          messageId: msg.id,
          content: text(body),
          _meta: messageMeta(msg, ctx, { fromTeammate: !mine }),
        },
      ];
    }
    case 'assistant': {
      const parts: string[] = [];
      if (content.trim()) parts.push(content);
      const interaction = interactionText(metadata.interaction);
      if (interaction) parts.push(interaction);
      if (parts.length === 0) return [];
      return [
        {
          sessionUpdate: 'agent_message_chunk',
          messageId: msg.id,
          content: text(parts.join('\n\n')),
          _meta: messageMeta(msg, ctx),
        },
      ];
    }
    case 'run': {
      const runId = str(metadata.runId) || str(metadata.run_id);
      if (!runId) return [];
      const status = str(metadata.status) || 'completed';
      return [
        {
          sessionUpdate: 'tool_call',
          toolCallId: runToolCallId(runId),
          title: runTitle(status),
          kind: 'execute',
          status: toolStatusFor(status),
          content: [
            {
              type: 'content',
              content: text(`Run ${runId} — details: ${chatUrl(ctx.webUrl, ctx.projectId, ctx.conversationId)}`),
            },
          ],
          rawInput: { runId, status },
          _meta: messageMeta(msg, ctx),
        },
      ];
    }
    case 'system': {
      if (!content.trim()) return [];
      return [
        {
          sessionUpdate: 'agent_message_chunk',
          messageId: msg.id,
          content: text(`_${content}_`),
          _meta: messageMeta(msg, ctx),
        },
      ];
    }
    default:
      return [];
  }
}

/** ACP prompt blocks → message text + attachments for POST /chat/message. */
export function promptToMessage(blocks: ContentBlock[]): {
  text: string;
  attachments: Array<{ name: string; mime_type: string; size_bytes: number; content_base64: string }>;
} {
  const parts: string[] = [];
  const attachments: Array<{ name: string; mime_type: string; size_bytes: number; content_base64: string }> = [];
  for (const block of blocks) {
    switch (block.type) {
      case 'text':
        parts.push(block.text);
        break;
      case 'resource_link':
        parts.push(`[${block.title ?? block.name}](${block.uri})`);
        break;
      case 'resource': {
        const res = block.resource as Record<string, unknown>;
        const uri = str(res.uri);
        const name = uri ? decodeURIComponent(uri.split('/').pop() || 'context') : 'context';
        const mime = str(res.mimeType) || 'text/plain';
        if (typeof res.text === 'string') {
          const bytes = Buffer.from(res.text, 'utf8');
          attachments.push({ name, mime_type: mime, size_bytes: bytes.length, content_base64: bytes.toString('base64') });
          parts.push(`(attached ${name})`);
        } else if (typeof res.blob === 'string') {
          const size = Buffer.from(res.blob, 'base64').length;
          attachments.push({ name, mime_type: mime, size_bytes: size, content_base64: res.blob });
          parts.push(`(attached ${name})`);
        }
        break;
      }
      case 'image':
      case 'audio':
        break;
    }
  }
  return { text: parts.join('\n').trim(), attachments };
}
