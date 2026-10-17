// API client for the verloc embedded dashboard.

export interface MetricSet {
  requests: number;
  ok: number;
  err: number;
  stream: number;
  tok_in: number;
  tok_out: number;
  tok_cache: number;
  lat_avg_ms: number;
  lat_p50_ms?: number;
  lat_p95_ms?: number;
  lat_p99_ms?: number;
  ttft_avg_ms?: number;
  failover: number;
  status: Record<string, number>;
  lat_hist?: number[];
  cost?: number;
  lifetime_cost?: number;
}

export interface KeyStat {
  key: string;
  healthy: boolean;
  fail_count: number;
  cooldown_sec: number;
  requests: number;
  ok: number;
  err: number;
  failover: number;
  tok_in: number;
  tok_out: number;
  tok_cache: number;
  cost: number;
}

export interface ReqRecord {
  ts: number;
  id: string;
  model: string;
  upstream: string;
  backend: string;
  key?: string;
  ip?: string;
  ua?: string;
  stream: boolean;
  ok: boolean;
  status: number;
  failover: number;
  lat_ms: number;
  ttft_ms?: number;
  tok_in: number;
  tok_out: number;
  tok_cache: number;
  cost: number;
  err_msg?: string;
  client_key_id?: string;
  client_name?: string;
  outcome?: string;
  usage_known?: boolean;
  cost_known?: boolean;
  attempts?: { backend: string; upstream: string; status: number; outcome: string; lat_ms: number }[];
}

export interface AlertEvent {
  ts: number;
  rule_id: string;
  name: string;
  severity: string;
  scope: string;
  metric: string;
  value: number;
  threshold: number;
  message: string;
}

export interface AlertRule {
  id: string;
  metric: string;
  scope: string;
  threshold: number;
  window_sec: number;
  cooldown_sec: number;
  severity: string;
}

export interface KeyEvent {
  ts: number;
  backend: string;
  key: string;
  event: string;
  status: number;
  msg: string;
}

export interface Lifetime {
  cost: number;
  requests: number;
  tok_in: number;
  tok_out: number;
  tok_cache: number;
  err: number;
}

export interface State {
  telemetry?: { queued: number; dropped_requests: number; dropped_key_events: number; write_errors: number; last_write_ts: number };
  ready: boolean;
  ts: number;
  uptime_sec: number;
  started_at: number;
  global: MetricSet;
  models: Record<string, MetricSet>;
  backends: Record<string, MetricSet>;
  keys: Record<string, KeyStat[]>;
  latency_ms: { p50: number; p95: number; p99: number };
  lifetime: Lifetime;
  delta: {
    d_ms: number;
    requests: number;
    err: number;
    failover: number;
    tok_in: number;
    tok_out: number;
    tok_cache: number;
    cost: number;
    models: Record<string, { requests: number; tok_in: number; tok_out: number; tok_cache: number; cost: number }>;
  };
  alerts: AlertEvent[];
  alert_rules: AlertRule[];
  key_events: KeyEvent[];
  pricing: Record<string, { input: number; output: number; cache_read: number }>;
  subscribers: number;
  version: string;
}

export interface SeriesPoint {
  ts: number;
  d_requests: number;
  d_tok_in: number;
  d_tok_out: number;
  d_tok_cache: number;
  d_cost: number;
  d_ms: number;
  d_err: number;
  lat_p50_ms?: number;
  lat_p95_ms?: number;
  lat_p99_ms?: number;
  lat_avg_ms?: number;
  ttft_avg_ms?: number;
  d_models?: Record<string, { requests?: number; tok_in?: number; tok_out?: number; tok_cache?: number; cost?: number }>;
  d_backends?: Record<string, { requests?: number; tok_in?: number; tok_out?: number; tok_cache?: number; err?: number; cost?: number }>;
}

export interface DataRange {
  oldest_ts: number | null;
  newest_ts: number | null;
  count: number;
  now: number;
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  return adminJSON<T>(url, { signal });
}

export const getState = () => getJSON<State>('/dash/api/state');
export const getSeriesSince = (sinceMs: number, limit = 500, signal?: AbortSignal) =>
  getJSON<SeriesPoint[]>(`/dash/api/series?since=${sinceMs}&limit=${limit}`, signal);
export const getRange = () => getJSON<DataRange>('/dash/api/range');
export const getRecent = (limit = 200) => getJSON<ReqRecord[]>(`/dash/api/recent?limit=${limit}`);
export const getAlerts = (limit = 200) => getJSON<AlertEvent[]>(`/dash/api/alerts?limit=${limit}`);
export const getKeyEvents = (limit = 200) => getJSON<KeyEvent[]>(`/dash/api/key-events?limit=${limit}`);

// ---- Admin CRUD (providers / models) ----

export interface AdminKey {
  id: string;
  masked: string;
  account_id?: string;
}

export interface AdminProvider {
  name: string;
  disabled?: boolean;
  base_url: string;
  api_format?: 'openai' | 'systemone';
  auth_header: string;
  auth_header_format: string;
  cooldown_seconds: number;
  request_timeout_seconds: number;
  no_auth: boolean;
  synthesize_cli_headers: boolean;
  strip_reasoning_content: boolean | null;
  max_tokens: number;
  keys: AdminKey[];
  model_count: number;
}

export interface AdminModel {
  id: string;
  disabled?: boolean;
  routes?: RouteTarget[];
  pricing_known: boolean;
  backend: string;
  upstream: string;
  name: string;
  description?: string;
  owned_by?: string;
  context_window: number;
  max_tokens: number;
  tags?: string[];
  pricing_input: number;
  pricing_output: number;
  pricing_cache: number;
  has_meta: boolean;
  embedded: boolean;
}

let csrfToken = '';
export class APIError extends Error {
  status: number;
  constructor(status: number, message: string) { super(message); this.status = status; }
}

async function adminJSON<T>(url: string, init?: RequestInit, timeoutMs = 20000): Promise<T> {
  try {
    const res = await fetch(url, {
      ...init,
      credentials: 'same-origin',
      signal: init?.signal || AbortSignal.timeout(timeoutMs),
      headers: { 'Content-Type': 'application/json', ...(csrfToken ? { 'X-CSRF-Token': csrfToken } : {}), ...(init?.headers || {}) },
    });
    const body: unknown = await res.json().catch(() => ({}));
    if (!res.ok) {
      const error = body && typeof body === 'object' && 'error' in body ? body.error : null;
      const msg = typeof error === 'string' ? error : error && typeof error === 'object' && 'message' in error && typeof error.message === 'string' ? error.message : `HTTP ${res.status}`;
      if (res.status === 401 && !url.includes('/auth/')) window.dispatchEvent(new Event('verloc:unauthorized'));
      throw new APIError(res.status, msg);
    }
    return body as T;
  } catch (e) {
    const name = e instanceof Error ? e.name : '';
    if (name === 'TimeoutError' || name === 'AbortError') {
      throw new Error('request timed out — refresh to confirm it saved');
    }
    throw e;
  }
}

export const listAdminProviders = () =>
  adminJSON<{ providers: AdminProvider[] }>('/dash/api/admin/providers');
export const upsertAdminProvider = (body: Record<string, unknown>) =>
  adminJSON<{ ok: boolean; provider: AdminProvider }>('/dash/api/admin/providers', {
    method: 'PUT', body: JSON.stringify(body),
  });
export const deleteAdminProvider = (name: string) =>
  adminJSON<{ ok: boolean }>(`/dash/api/admin/providers/${encodeURIComponent(name)}`, { method: 'DELETE' });
export const deleteAdminProviderKey = (name: string, id: string) =>
  adminJSON<{ ok: boolean; provider: AdminProvider }>(
    `/dash/api/admin/providers/${encodeURIComponent(name)}/keys/${encodeURIComponent(id)}`,
    { method: 'DELETE' },
  );

export const listAdminModels = () =>
  adminJSON<{ models: AdminModel[] }>('/dash/api/admin/models');
export const upsertAdminModel = (body: Record<string, unknown>, existingID?: string) =>
  adminJSON<{ ok: boolean; model: AdminModel }>(existingID ? `/dash/api/admin/models/${existingID.split('/').map(encodeURIComponent).join('/')}` : '/dash/api/admin/models', {
    method: 'PUT', body: JSON.stringify(body),
  });
export const deleteAdminModel = (id: string) =>
  adminJSON<{ ok: boolean }>(`/dash/api/admin/models/${id.split('/').map(encodeURIComponent).join('/')}`, {
    method: 'DELETE',
  });

// ---- Admin wizard (discover / test) ----

export interface ModelHint {
  name?: string;
  description?: string;
  owned_by?: string;
  tags?: string[];
  pricing_known: boolean;
  pricing_input?: number;
  pricing_output?: number;
  pricing_cache?: number;
  context_window?: number;
  max_tokens?: number;
  source?: string;
}

export interface DiscoveredModel {
  id: string;
  owned_by?: string;
  hint?: ModelHint;
}

export interface ProbeConn {
  provider?: string;
  backend?: string;
  base_url?: string;
  api_key?: string;
  api_format?: 'openai' | 'systemone';
  account_id?: string;
  auth_header?: string;
  auth_header_format?: string;
  no_auth?: boolean;
  synthesize_cli_headers?: boolean;
}

export interface TestResult {
  ok: boolean;
  status: number;
  latency_ms: number;
  reply?: string;
  error?: string;
  hint?: string;
}

export const discoverUpstreamModels = (body: ProbeConn) =>
  adminJSON<{ models: DiscoveredModel[]; count: number }>('/dash/api/admin/discover', {
    method: 'POST', body: JSON.stringify(body),
  }, 60000);

export const testUpstreamModel = (body: ProbeConn & { upstream: string; max_tokens?: number }) =>
  adminJSON<TestResult>('/dash/api/admin/test', {
    method: 'POST', body: JSON.stringify(body),
  }, 60000);

export const lookupModelHints = (upstreams: string[]) =>
  adminJSON<{ hints: Record<string, ModelHint> }>('/dash/api/admin/model-hints', {
    method: 'POST', body: JSON.stringify({ upstreams }),
  }, 60000);

export interface Session { authenticated: boolean; setup_required: boolean; csrf_token?: string }
function rememberSession(session: Session) { csrfToken = session.csrf_token || ''; return session; }
export const getSession = () => adminJSON<Session>('/dash/api/auth/session').then(rememberSession);
export const login = (password: string) => adminJSON<Session>('/dash/api/auth/login', { method: 'POST', body: JSON.stringify({ password }) }).then(rememberSession);
export const setup = (setup_token: string, password: string) => adminJSON<Session>('/dash/api/auth/setup', { method: 'POST', body: JSON.stringify({ setup_token, password }) }).then(rememberSession);
export const changePassword = (current_password: string, new_password: string) => adminJSON<Session>('/dash/api/auth/password', { method: 'POST', body: JSON.stringify({ current_password, new_password }) }).then(rememberSession);
export const logout = async () => { await adminJSON('/dash/api/auth/logout', { method: 'POST' }); csrfToken = ''; };

export interface RouteTarget { backend: string; upstream: string }
export const onboard = (body: { provider?: Record<string, unknown>; models: Record<string, unknown>[]; overwrite?: boolean }) =>
  adminJSON<{ ok: boolean; providers: number; models: number }>('/dash/api/admin/onboard', { method: 'POST', body: JSON.stringify(body) }, 60000);
export const bulkModels = (ids: string[], action: 'enable' | 'disable' | 'delete') =>
  adminJSON<{ ok: boolean; count: number }>('/dash/api/admin/models/bulk', { method: 'POST', body: JSON.stringify({ ids, action }) });

export interface ClientKey {
  id: string; name: string; prefix: string; allowed_models: string[]; expires_at: number;
  rpm: number; tpm: number; max_concurrent: number; created_at: number; revoked_at: number;
}
export type ClientKeyInput = Pick<ClientKey, 'name' | 'allowed_models' | 'expires_at' | 'rpm' | 'tpm' | 'max_concurrent'>;
export const listClientKeys = () => adminJSON<{ keys: ClientKey[] }>('/dash/api/admin/client-keys');
export const createClientKey = (body: ClientKeyInput) => adminJSON<{ key: string; client: ClientKey }>('/dash/api/admin/client-keys', { method: 'POST', body: JSON.stringify(body) });
export const editClientKey = (id: string, body: ClientKeyInput) => adminJSON<{ client: ClientKey }>(`/dash/api/admin/client-keys/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(body) });
export const rotateClientKey = (id: string) => adminJSON<{ key: string; client: ClientKey }>(`/dash/api/admin/client-keys/${encodeURIComponent(id)}/rotate`, { method: 'POST' });
export const revokeClientKey = (id: string) => adminJSON<{ ok: boolean }>(`/dash/api/admin/client-keys/${encodeURIComponent(id)}`, { method: 'DELETE' });

export interface RequestSummary {
  requests: number; errors: number; tok_in: number; tok_out: number; tok_cache: number; cost: number;
  unknown_usage: number; unknown_cost: number; avg_latency_ms: number; avg_ttft_ms: number;
}
export interface UsageGroup extends RequestSummary { id: string; name: string }
export interface RequestPage { requests: ReqRecord[]; total: number; next_cursor?: string; summary: RequestSummary }
export interface Analytics {
  summary: RequestSummary; models: UsageGroup[]; providers: UsageGroup[]; clients: UsageGroup[];
  status: { status: number; requests: number }[];
  latency: { p50: number; p95: number; p99: number; buckets: number[]; bounds: number[] };
  ttft: { p50: number; p95: number; p99: number };
  coverage: { oldest_ts: number; newest_ts: number; legacy_aggregates_only: boolean; legacy_summary?: RequestSummary; detail_oldest_ts?: number; detail_newest_ts?: number; aggregate_only_requests?: number; aggregate_bucket_ms?: number; partial_buckets_excluded?: boolean; legacy_distributions_unavailable?: boolean };
}
export const getRequests = (query: URLSearchParams, signal?: AbortSignal) => adminJSON<RequestPage>(`/dash/api/requests?${query}`, { signal });
export const getAnalytics = (query: URLSearchParams, signal?: AbortSignal) => adminJSON<Analytics>(`/dash/api/analytics?${query}`, { signal });
export interface GatewayLimits {
  max_body_bytes: number; max_attempts: number; retry_budget_seconds: number; queue_timeout_seconds: number;
  max_in_flight: number; max_queued: number; stream_idle_timeout_seconds: number;
}
export interface Settings {
  limits: GatewayLimits; secure_cookies: boolean; management_db: string;
  analytics_db: string; model_count: number; provider_count: number;
}
export interface AuditEvent { id: number; ts: number; action: string; subject: string }
export const getSettings = () => adminJSON<Settings>('/dash/api/admin/settings');
export const saveSettings = (body: { limits?: GatewayLimits; alerts?: AlertRule[] }) => adminJSON<{ ok: boolean }>('/dash/api/admin/settings', { method: 'PUT', body: JSON.stringify(body) });
export const getAudit = () => adminJSON<{ events: AuditEvent[] }>('/dash/api/admin/audit');
export const createBackup = () => adminJSON<{ ok: boolean; backup: string }>('/dash/api/admin/backup', { method: 'POST' }, 120000);

// ---- SSE live stream ----
export function connectLive(handlers: {
  onReq?: (r: ReqRecord) => void;
  onAlert?: (a: AlertEvent) => void;
  onKeyEvent?: (e: KeyEvent) => void;
  onInit?: (m: Record<string, unknown>) => void;
  onClose?: () => void;
}): () => void {
  const es = new EventSource('/dash/api/live');
  // Server sends `{"type": <name>, "<kind>": <payload>}`; unwrap the inner payload.
  const unwrap = <T,>(d: unknown): T | null => {
    if (!d || typeof d !== 'object') return null;
    const o = d as Record<string, unknown>;
    return (o.req ?? o.alert ?? o.event ?? d) as T;
  };
  es.addEventListener('req', (e) => {
    try {
      const r = unwrap<ReqRecord>(JSON.parse((e as MessageEvent).data));
      if (r) handlers.onReq?.(r);
    } catch { /* ignore */ }
  });
  es.addEventListener('alert', (e) => {
    try {
      const a = unwrap<AlertEvent>(JSON.parse((e as MessageEvent).data));
      if (a) handlers.onAlert?.(a);
    } catch { /* ignore */ }
  });
  es.addEventListener('key_event', (e) => {
    try {
      const k = unwrap<KeyEvent>(JSON.parse((e as MessageEvent).data));
      if (k) handlers.onKeyEvent?.(k);
    } catch { /* ignore */ }
  });
  es.addEventListener('init', (e) => {
    try { handlers.onInit?.(JSON.parse((e as MessageEvent).data)); } catch { /* ignore */ }
  });
  es.onerror = () => { handlers.onClose?.(); };
  return () => es.close();
}

// ---- time-range presets ----
export interface RangePreset { key: string; label: string; ms: number | null }
export const RANGE_PRESETS: RangePreset[] = [
  { key: '1h', label: '1h', ms: 1 * 60 * 60 * 1000 },
  { key: '1d', label: '1d', ms: 24 * 60 * 60 * 1000 },
  { key: '7d', label: '7d', ms: 7 * 24 * 60 * 60 * 1000 },
  { key: '30d', label: '30d', ms: 30 * 24 * 60 * 60 * 1000 },
  { key: '1y', label: '1y', ms: 365 * 24 * 60 * 60 * 1000 },
  { key: 'all', label: 'All', ms: null },
];

// ---- formatting helpers ----
export const fmtInt = (n: number) => Math.round(n || 0).toLocaleString();
export const fmtMoney = (n?: number) => {
  if (!n || n === 0) return '$0.00';
  if (n < 0.01) return '$' + n.toFixed(5);
  if (n < 1) return '$' + n.toFixed(4);
  return '$' + n.toFixed(2);
};
export const fmtTokens = (n: number) => {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(Math.round(n || 0));
};
export const fmtMs = (n?: number) => {
  if (!n || n <= 0) return '—';
  return n >= 1000 ? (n / 1000).toFixed(2) + 's' : n.toFixed(0) + 'ms';
};
export const ratePerSec = (v: number, ms: number) => (ms > 0 ? ((v || 0) / ms) * 1000 : 0);
export const fmtTime = (ts: number) => new Date(ts).toLocaleTimeString();
export const fmtDateTime = (ts: number) => new Date(ts).toLocaleString();
export const pctOf = (part: number, total: number) => (total > 0 ? ((part || 0) / total) * 100 : 0);

// Histogram bucket labels (ms), matching the Go side.
export const HIST_LABELS = ['<50', '<100', '<250', '<500', '<1s', '<2s', '<5s', '<10s', '<30s', '<60s', '>60s'];

export const SEVERITY_COLOR: Record<string, string> = {
  info: 'text-sky-400 border-sky-500/40',
  warning: 'text-amber-400 border-amber-500/40',
  critical: 'text-red-400 border-red-500/40',
};

// Props shared by every dashboard tab.
export interface TabProps {
  state: State | null;
  series: SeriesPoint[];
  range: RangePreset;
  liveReqs: ReqRecord[];
  liveAlerts: AlertEvent[];
  liveKeyEvents: KeyEvent[];
  onRange: (r: RangePreset) => void;
}
