/**
 * Free (pay-as-you-go) plan handling. Spec 2026-10-19 §2/§4: 402 denials carry
 * `{error, code: "INSUFFICIENT_CREDITS" | "LIVE_UPGRADE_REQUIRED" | "LIVE_LIMIT_<NAME>",
 *   cta: "buy_credits" | "upgrade", limit, current, upgrade_url, buy_credits_url}`.
 * The bridge turns them into an agent message, never a protocol error.
 */
import { ApiError } from './api.js';

export interface BillingDenial {
  kind: 'buy_credits' | 'upgrade';
  code: string;
  message: string;
  buyCreditsUrl: string;
  upgradeUrl: string;
  limit: number | null;
  current: number | null;
}

const SURFACE = 'live_acp';

function absolutize(webUrl: string, raw: unknown, fallbackPath: string, extra: Record<string, string>): string {
  const base = `${webUrl}/`;
  let url: URL;
  try {
    url = typeof raw === 'string' && raw.trim() ? new URL(raw.trim(), base) : new URL(fallbackPath, base);
  } catch {
    url = new URL(fallbackPath, base);
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') url = new URL(fallbackPath, base);
  for (const [k, v] of Object.entries(extra)) {
    if (!url.searchParams.has(k)) url.searchParams.set(k, v);
  }
  return url.toString();
}

function num(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

export function billingDenialFrom(err: unknown, webUrl: string): BillingDenial | null {
  if (!(err instanceof ApiError) || err.status !== 402) return null;
  const body = err.body ?? {};
  const code = (err.code ?? '').toUpperCase();
  const cta = typeof body.cta === 'string' ? body.cta : '';
  const kind: BillingDenial['kind'] =
    cta === 'buy_credits' || (cta !== 'upgrade' && (code === 'INSUFFICIENT_CREDITS' || code === ''))
      ? 'buy_credits'
      : 'upgrade';
  return {
    kind,
    code: code || 'INSUFFICIENT_CREDITS',
    message: err.message,
    buyCreditsUrl: absolutize(webUrl, body.buy_credits_url, '/settings/billing', { buy: 'credits', from: SURFACE }),
    upgradeUrl: absolutize(webUrl, body.upgrade_url, '/settings/billing', { from: SURFACE }),
    limit: num(body.limit),
    current: num(body.current),
  };
}

/** Formats a limit value by the unit in its `LIVE_LIMIT_<NAME>` code: hours, GiB/MiB/KB, days or a count. */
export function formatLimit(code: string, value: number): string {
  const name = code.toUpperCase();
  const v = Math.max(0, value);
  const n = (x: number, d: number) => x.toLocaleString('en-US', { maximumFractionDigits: d });
  if (name.includes('_SECONDS')) {
    if (v >= 3600) {
      const h = Math.round((v / 3600) * 10) / 10;
      return `${n(h, 1)} ${h === 1 ? 'hour' : 'hours'}`;
    }
    const m = Math.ceil(v / 60);
    return `${m} ${m === 1 ? 'minute' : 'minutes'}`;
  }
  if (name.includes('_BYTES')) {
    if (v >= 1024 ** 3) return `${n(v / 1024 ** 3, 1)} GiB`;
    if (v >= 1024 ** 2) return `${n(v / 1024 ** 2, 1)} MiB`;
    return `${n(Math.ceil(v / 1024), 0)} KB`;
  }
  if (name.endsWith('_DAYS')) return `${n(v, 0)} ${v === 1 ? 'day' : 'days'}`;
  return n(v, 0);
}

/**
 * Agent-message text for a denial (Markdown). The server's message is used as
 * is: no usage note when it already states numbers, no second call to action
 * when it already says to upgrade.
 */
export function billingMessage(denial: BillingDenial): string {
  if (denial.kind === 'buy_credits') {
    return `The OrchestrAI agent runs on credits — buy credits: ${denial.buyCreditsUrl} (or upgrade to Pro for included credits: ${denial.upgradeUrl})`;
  }
  const reason = denial.message.trim().replace(/\.$/, '') || 'This needs a Pro or higher plan';
  const usage =
    denial.limit !== null && denial.limit >= 0 && !/\d/.test(reason)
      ? ` (${formatLimit(denial.code, denial.current ?? denial.limit)} of ${formatLimit(denial.code, denial.limit)} used)`
      : '';
  if (/\bupgrade\b/i.test(reason)) {
    return `${reason}${usage}: ${denial.upgradeUrl}`;
  }
  return `${reason}${usage}. Upgrade your OrchestrAI plan to lift this limit: ${denial.upgradeUrl}`;
}
