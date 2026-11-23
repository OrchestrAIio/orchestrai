import { mkdtempSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import * as acp from '@agentclientprotocol/sdk';
import type { ClientCapabilities, SessionNotification, SessionUpdate } from '@agentclientprotocol/sdk';
import { OrchestraiAcpAgent } from '../../src/agent.js';
import type { BridgeConfig } from '../../src/config.js';
import { CredentialStore } from '../../src/credentials.js';
import { silentLogger } from '../../src/log.js';

export function tempDir(): { dir: string; cleanup: () => void } {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'orchestrai-acp-test-'));
  return { dir, cleanup: () => rmSync(dir, { recursive: true, force: true }) };
}

export function testConfig(apiUrl: string, configDir: string, overrides: Partial<BridgeConfig> = {}): BridgeConfig {
  return {
    apiUrl,
    webUrl: 'https://app.example.test',
    projectId: null,
    envApiKey: null,
    configDir,
    useKeychain: false,
    turnIdleTimeoutMs: 5_000,
    runGraceMs: 150,
    debug: false,
    ...overrides,
  };
}

export interface Harness {
  agent: OrchestraiAcpAgent;
  ctx: acp.ClientContext;
  connection: acp.ClientConnection;
  updates: SessionNotification[];
  permissionRequests: unknown[];
  updatesFor(sessionId: string): SessionUpdate[];
  textOf(sessionId: string, kind?: SessionUpdate['sessionUpdate']): string;
  close(): void;
}

export async function connectHarness(
  config: BridgeConfig,
  opts: { clientCapabilities?: ClientCapabilities; open?: (url: string) => boolean } = {},
): Promise<Harness> {
  const agent = new OrchestraiAcpAgent({
    config,
    log: silentLogger,
    store: new CredentialStore(config.configDir, null, silentLogger),
    deviceFlow: { sleep: () => new Promise((r) => setTimeout(r, 5)), open: opts.open ?? (() => true) },
  });
  const updates: SessionNotification[] = [];
  const permissionRequests: unknown[] = [];
  const client = acp
    .client({ name: 'test-client' })
    .onNotification(acp.methods.client.session.update, (c) => {
      updates.push(c.params);
    })
    .onRequest(acp.methods.client.session.requestPermission, (c) => {
      permissionRequests.push(c.params);
      return { outcome: { outcome: 'cancelled' } };
    });
  const connection = client.connect(agent.app());
  await connection.agent.request(acp.methods.agent.initialize, {
    protocolVersion: acp.PROTOCOL_VERSION,
    clientCapabilities: opts.clientCapabilities ?? {},
    clientInfo: { name: 'test-client', version: '1.0.0' },
  });
  const updatesFor = (sessionId: string) => updates.filter((u) => u.sessionId === sessionId).map((u) => u.update);
  return {
    agent,
    ctx: connection.agent,
    connection,
    updates,
    permissionRequests,
    updatesFor,
    textOf(sessionId, kind = 'agent_message_chunk') {
      return updatesFor(sessionId)
        .filter((u) => u.sessionUpdate === kind)
        .map((u) => ('content' in u && u.content && typeof u.content === 'object' && 'text' in u.content ? String(u.content.text) : ''))
        .join('');
    },
    close: () => connection.close(),
  };
}
