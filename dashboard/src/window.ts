// Roll up SQLite series ticks into the selected dashboard window.
// Session counters in /dash/api/state reset on process start; these totals do not.

import type { RangePreset, SeriesPoint } from './api';

export interface DimRollup {
  requests: number;
  tokIn: number;
  tokOut: number;
  tokCache: number;
  cost: number;
  err: number;
}

export interface WindowRollup {
  requests: number;
  tokIn: number;
  tokOut: number;
  tokCache: number;
  cost: number;
  err: number;
  ms: number;
  from: number;
  to: number;
  models: Record<string, DimRollup>;
  backends: Record<string, DimRollup>;
}

function blank(): DimRollup {
  return { requests: 0, tokIn: 0, tokOut: 0, tokCache: 0, cost: 0, err: 0 };
}

export function emptyRollup(): WindowRollup {
  return {
    requests: 0, tokIn: 0, tokOut: 0, tokCache: 0, cost: 0, err: 0, ms: 0,
    from: 0, to: 0, models: {}, backends: {},
  };
}

export function rollupSeries(series: SeriesPoint[]): WindowRollup {
  const out = emptyRollup();
  for (const p of series) {
    const req = p.d_requests || 0;
    out.requests += req;
    out.tokIn += p.d_tok_in || 0;
    out.tokOut += p.d_tok_out || 0;
    out.tokCache += p.d_tok_cache || 0;
    out.cost += p.d_cost || 0;
    out.err += p.d_err || 0;
    out.ms += p.d_ms || 0;
    if (!out.from || p.ts < out.from) out.from = p.ts;
    if (p.ts > out.to) out.to = p.ts;
    for (const [k, v] of Object.entries(p.d_models || {})) {
      const a = (out.models[k] ||= blank());
      a.requests += v.requests || 0;
      a.tokIn += v.tok_in || 0;
      a.tokOut += v.tok_out || 0;
      a.tokCache += v.tok_cache || 0;
      a.cost += v.cost || 0;
    }
    for (const [k, v] of Object.entries(p.d_backends || {})) {
      const a = (out.backends[k] ||= blank());
      a.requests += v.requests || 0;
      a.tokIn += v.tok_in || 0;
      a.tokOut += v.tok_out || 0;
      a.tokCache += v.tok_cache || 0;
      a.cost += v.cost || 0;
      a.err += v.err || 0;
    }
  }
  return out;
}

export function rangeSince(range: RangePreset, now = Date.now()): number {
  return range.ms == null ? 0 : now - range.ms;
}

/** How much of the selected window actually has samples. */
export function fmtSpan(from: number, to: number): string {
  if (!from || !to) return 'no samples in window';
  const ms = Math.max(0, to - from);
  if (ms < 60_000) return 'covers <1m';
  const mins = Math.round(ms / 60_000);
  if (mins < 120) return `covers ${mins}m`;
  const hours = ms / 3_600_000;
  if (hours < 48) return `covers ${hours < 10 ? hours.toFixed(1) : Math.round(hours)}h`;
  const days = ms / 86_400_000;
  return `covers ${days < 10 ? days.toFixed(1) : Math.round(days)}d`;
}

export function fmtAxis(ts: number, range: RangePreset): string {
  const d = new Date(ts);
  const ms = range.ms ?? Number.POSITIVE_INFINITY;
  if (ms > 60 * 60 * 1000) {
    return d.toLocaleString(undefined, { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  }
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}
