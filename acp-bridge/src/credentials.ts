import { spawn } from 'node:child_process';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import type { Logger } from './log.js';
import { errorMessage } from './log.js';

/** One signed-in identity, keyed by the API base URL it belongs to. */
export interface StoredCredential {
  apiUrl: string;
  apiKey: string;
  user: { id: string; name: string; email: string } | null;
  /** How the key was obtained; only device-flow keys are revoked on logout. */
  source: 'device_flow' | 'manual';
  createdAt: string;
}

interface CredentialsFile {
  version: 1;
  credentials: Record<string, StoredCredential>;
}

export interface CommandResult {
  code: number;
  stdout: string;
  stderr: string;
}

/** Runs a program with stdin; never through a shell. Swappable for tests. */
export type CommandRunner = (command: string, args: string[], stdin?: string) => Promise<CommandResult>;

export const runCommand: CommandRunner = (command, args, stdin) =>
  new Promise((resolve) => {
    let stdout = '';
    let stderr = '';
    let settled = false;
    const finish = (result: CommandResult) => {
      if (!settled) {
        settled = true;
        resolve(result);
      }
    };
    let child;
    try {
      child = spawn(command, args, { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
    } catch (err) {
      finish({ code: 127, stdout: '', stderr: errorMessage(err) });
      return;
    }
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      finish({ code: 124, stdout, stderr: `${stderr}\ntimed out` });
    }, 10_000);
    child.stdout.on('data', (d: Buffer) => (stdout += d.toString('utf8')));
    child.stderr.on('data', (d: Buffer) => (stderr += d.toString('utf8')));
    child.on('error', (err) => {
      clearTimeout(timer);
      finish({ code: 127, stdout, stderr: errorMessage(err) });
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      finish({ code: code ?? 1, stdout, stderr });
    });
    child.stdin.on('error', () => undefined);
    child.stdin.end(stdin ?? '');
  });

const KEYCHAIN_SERVICE = 'orchestrai-acp';

/**
 * OS keychain backends that keep the secret out of argv (visible to other
 * local users in the process list):
 *  - macOS: `security -i` reads its command (including -w <secret>) from stdin.
 *  - Linux: `secret-tool store` reads the secret from stdin (libsecret / GNOME
 *    Keyring / KWallet via the Secret Service API).
 * Windows has no stdin-safe built-in CLI, so it uses the 0600-equivalent file
 * in %APPDATA% (protected by the user profile ACL).
 */
export interface KeychainBackend {
  readonly name: string;
  get(account: string): Promise<string | null>;
  set(account: string, secret: string): Promise<boolean>;
  remove(account: string): Promise<void>;
}

function quoteForSecurityInteractive(value: string): string {
  return `"${value.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
}

export function macKeychain(run: CommandRunner): KeychainBackend {
  return {
    name: 'macOS Keychain',
    async get(account) {
      const res = await run('security', ['find-generic-password', '-s', KEYCHAIN_SERVICE, '-a', account, '-w']);
      return res.code === 0 ? res.stdout.replace(/\n$/, '') : null;
    },
    async set(account, secret) {
      const cmd = `add-generic-password -U -s ${quoteForSecurityInteractive(KEYCHAIN_SERVICE)} -a ${quoteForSecurityInteractive(account)} -w ${quoteForSecurityInteractive(secret)}\n`;
      const res = await run('security', ['-i'], cmd);
      return res.code === 0 && !/error|failed/i.test(res.stderr);
    },
    async remove(account) {
      await run('security', ['delete-generic-password', '-s', KEYCHAIN_SERVICE, '-a', account]);
    },
  };
}

export function linuxSecretTool(run: CommandRunner): KeychainBackend {
  return {
    name: 'Secret Service (secret-tool)',
    async get(account) {
      const res = await run('secret-tool', ['lookup', 'service', KEYCHAIN_SERVICE, 'account', account]);
      return res.code === 0 && res.stdout.length > 0 ? res.stdout.replace(/\n$/, '') : null;
    },
    async set(account, secret) {
      const res = await run(
        'secret-tool',
        ['store', '--label=OrchestrAI ACP credentials', 'service', KEYCHAIN_SERVICE, 'account', account],
        secret,
      );
      return res.code === 0;
    },
    async remove(account) {
      await run('secret-tool', ['clear', 'service', KEYCHAIN_SERVICE, 'account', account]);
    },
  };
}

export function platformKeychain(run: CommandRunner = runCommand): KeychainBackend | null {
  if (process.platform === 'darwin') return macKeychain(run);
  if (process.platform === 'linux') return linuxSecretTool(run);
  return null;
}

function isStoredCredential(value: unknown): value is StoredCredential {
  if (typeof value !== 'object' || value === null) return false;
  const v = value as Record<string, unknown>;
  return typeof v.apiUrl === 'string' && typeof v.apiKey === 'string' && v.apiKey.startsWith('ork_');
}

/**
 * Credential storage: OS keychain when available, else
 * <configDir>/credentials.json with mode 0600 (directory 0700).
 */
export class CredentialStore {
  private readonly filePath: string;

  constructor(
    private readonly configDir: string,
    private readonly keychain: KeychainBackend | null,
    private readonly log: Logger,
  ) {
    this.filePath = path.join(configDir, 'credentials.json');
  }

  get credentialsPath(): string {
    return this.filePath;
  }

  async load(apiUrl: string): Promise<StoredCredential | null> {
    if (this.keychain) {
      try {
        const raw = await this.keychain.get(apiUrl);
        if (raw) {
          const parsed: unknown = JSON.parse(raw);
          if (isStoredCredential(parsed)) return parsed;
        }
      } catch (err) {
        this.log.debug(`keychain read failed (${this.keychain.name}): ${errorMessage(err)}`);
      }
    }
    const file = await this.readFile();
    const entry = file.credentials[apiUrl];
    return entry && isStoredCredential(entry) ? entry : null;
  }

  /** Returns where the credential was stored. */
  async save(credential: StoredCredential): Promise<'keychain' | 'file'> {
    if (this.keychain) {
      try {
        if (await this.keychain.set(credential.apiUrl, JSON.stringify(credential))) {
          // Never leave a stale plaintext copy next to a keychain entry.
          await this.removeFromFile(credential.apiUrl);
          return 'keychain';
        }
      } catch (err) {
        this.log.debug(`keychain write failed (${this.keychain.name}): ${errorMessage(err)}`);
      }
      this.log.warn(`OS keychain unavailable; storing the API key in ${this.filePath} (mode 0600).`);
    }
    const file = await this.readFile();
    file.credentials[credential.apiUrl] = credential;
    await this.writeFile(file);
    return 'file';
  }

  async clear(apiUrl: string): Promise<void> {
    if (this.keychain) {
      try {
        await this.keychain.remove(apiUrl);
      } catch (err) {
        this.log.debug(`keychain delete failed: ${errorMessage(err)}`);
      }
    }
    await this.removeFromFile(apiUrl);
  }

  private async removeFromFile(apiUrl: string): Promise<void> {
    const file = await this.readFile();
    if (file.credentials[apiUrl]) {
      delete file.credentials[apiUrl];
      await this.writeFile(file);
    }
  }

  private async readFile(): Promise<CredentialsFile> {
    let raw: string;
    try {
      raw = await fs.readFile(this.filePath, 'utf8');
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') return { version: 1, credentials: {} };
      throw err;
    }
    try {
      const parsed = JSON.parse(raw) as Partial<CredentialsFile>;
      if (parsed && typeof parsed === 'object' && parsed.credentials && typeof parsed.credentials === 'object') {
        return { version: 1, credentials: parsed.credentials };
      }
    } catch {
      this.log.warn(`${this.filePath} is not valid JSON; ignoring it.`);
    }
    return { version: 1, credentials: {} };
  }

  private async writeFile(file: CredentialsFile): Promise<void> {
    await fs.mkdir(this.configDir, { recursive: true, mode: 0o700 });
    const tmp = `${this.filePath}.${process.pid}.${Date.now()}.tmp`;
    await fs.writeFile(tmp, `${JSON.stringify(file, null, 2)}\n`, { mode: 0o600 });
    await fs.chmod(tmp, 0o600);
    await fs.rename(tmp, this.filePath);
    await fs.chmod(this.filePath, 0o600);
  }
}
