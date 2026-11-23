/**
 * ACP session ids.
 *  - An existing OrchestrAI conversation is addressed as `<projectId>:<conversationId>`
 *    (what session/list returns), so it can be loaded on any machine.
 *  - A session/new thread gets `new-<uuid>` because its conversation is only
 *    created on the first prompt (no empty "New chat" rows from opened-and-
 *    abandoned editor threads). Once created, the mapping is persisted in
 *    <configDir>/sessions.json so the editor can session/load it after a restart.
 */
import { randomUUID } from 'node:crypto';
import { promises as fs } from 'node:fs';
import path from 'node:path';

export interface ConversationRef {
  projectId: string;
  conversationId: string;
}

const UUIDISH = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;

export function encodeSessionId(ref: ConversationRef): string {
  return `${ref.projectId}:${ref.conversationId}`;
}

export function parseSessionId(sessionId: string): ConversationRef | null {
  const idx = sessionId.indexOf(':');
  if (idx <= 0) return null;
  const projectId = sessionId.slice(0, idx);
  const conversationId = sessionId.slice(idx + 1);
  if (!UUIDISH.test(projectId) || !UUIDISH.test(conversationId)) return null;
  return { projectId, conversationId };
}

export function newSessionId(): string {
  return `new-${randomUUID()}`;
}

const MAX_ENTRIES = 1000;

interface IndexFile {
  version: 1;
  sessions: Record<string, ConversationRef & { savedAt: string }>;
}

export class SessionIndex {
  private readonly filePath: string;
  private cache: IndexFile | null = null;

  constructor(private readonly configDir: string) {
    this.filePath = path.join(configDir, 'sessions.json');
  }

  async resolve(sessionId: string): Promise<ConversationRef | null> {
    const direct = parseSessionId(sessionId);
    if (direct) return direct;
    const file = await this.read();
    const entry = file.sessions[sessionId];
    return entry ? { projectId: entry.projectId, conversationId: entry.conversationId } : null;
  }

  async remember(sessionId: string, ref: ConversationRef): Promise<void> {
    if (parseSessionId(sessionId)) return;
    const file = await this.read();
    file.sessions[sessionId] = { ...ref, savedAt: new Date().toISOString() };
    const keys = Object.keys(file.sessions);
    if (keys.length > MAX_ENTRIES) {
      keys
        .sort((a, b) => (file.sessions[a]?.savedAt ?? '').localeCompare(file.sessions[b]?.savedAt ?? ''))
        .slice(0, keys.length - MAX_ENTRIES)
        .forEach((k) => delete file.sessions[k]);
    }
    await fs.mkdir(this.configDir, { recursive: true, mode: 0o700 });
    const tmp = `${this.filePath}.${process.pid}.${Date.now()}.tmp`;
    await fs.writeFile(tmp, `${JSON.stringify(file, null, 2)}\n`, { mode: 0o600 });
    await fs.rename(tmp, this.filePath);
  }

  /** Reverse lookup so session/list can return the id the editor already knows. */
  async aliasFor(ref: ConversationRef): Promise<string | null> {
    const file = await this.read();
    for (const [id, entry] of Object.entries(file.sessions)) {
      if (entry.projectId === ref.projectId && entry.conversationId === ref.conversationId) return id;
    }
    return null;
  }

  private async read(): Promise<IndexFile> {
    if (this.cache) return this.cache;
    try {
      const parsed = JSON.parse(await fs.readFile(this.filePath, 'utf8')) as Partial<IndexFile>;
      this.cache = { version: 1, sessions: parsed.sessions && typeof parsed.sessions === 'object' ? parsed.sessions : {} };
    } catch {
      this.cache = { version: 1, sessions: {} };
    }
    return this.cache;
  }
}
