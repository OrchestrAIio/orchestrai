/**
 * stdout carries the ACP JSON-RPC stream, so every human-readable line goes to
 * stderr (editors show it in their agent log panel).
 */
export interface Logger {
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
  debug(message: string): void;
}

export function createLogger(debugEnabled: boolean, sink: (line: string) => void = defaultSink): Logger {
  const write = (level: string, message: string) => sink(`[orchestrai-acp] ${level} ${message}`);
  return {
    info: (m) => write('info', m),
    warn: (m) => write('warn', m),
    error: (m) => write('error', m),
    debug: (m) => {
      if (debugEnabled) write('debug', m);
    },
  };
}

function defaultSink(line: string): void {
  process.stderr.write(`${line}\n`);
}

export const silentLogger: Logger = {
  info: () => undefined,
  warn: () => undefined,
  error: () => undefined,
  debug: () => undefined,
};

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
