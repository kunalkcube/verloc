import { useMemo, useState } from 'react';
import {
  fmtInt, fmtMoney, fmtMs, fmtTokens, HIST_LABELS,
  type TabProps, type MetricSet, type KeyStat,
} from '../api';
import { Badge } from '../components/ui/badge';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '../components/ui/empty';
import { Input } from '../components/ui/input';
import { Separator } from '../components/ui/separator';
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '../components/ui/sheet';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';
import { rollupSeries, type DimRollup } from '../window';
import { ArrowUpRight, Search, Server } from 'lucide-react';

const PREFIX_TO_BACKEND: Record<string, string> = {
  nv: 'nvidia', cf: 'cf', vl: 'vercel', ol: 'ollama',
  cb: 'cerebras', sf: 'stepfun', ps: 'poolside', ml: 'mistral',
  sl: 'siliconflow', tr: 'tokenrouter', oc: 'opencode', zm: 'zenmux',
  fi: 'freeinference', or: 'openrouter', gq: 'groq', no: 'novita',
  wb: 'wandb', hy: 'hyper', gm: 'gmi',
};

function HealthBar({ pct, tone }: { pct: number; tone: 'ok' | 'warn' | 'bad' }) {
  const fill = tone === 'ok' ? 'bg-primary' : tone === 'warn' ? 'bg-chart-4' : 'bg-destructive';
  return (
    <div className="h-1 w-full overflow-hidden bg-edge-700">
      <div className={`h-full ${fill}`} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
    </div>
  );
}

export default function Backends({ state, series, range }: TabProps) {
  const [selected, setSelected] = useState<string | null>(null);
  const [q, setQ] = useState('');
  const roll = useMemo(() => rollupSeries(series), [series]);

  const backends = useMemo(() => {
    if (!state) return [];
    const names = new Set([...Object.keys(state.backends), ...Object.keys(roll.backends)]);
    return [...names]
      .filter((name) => name.toLowerCase().includes(q.trim().toLowerCase()))
      .map((name) => {
        const w = roll.backends[name] || { requests: 0, tokIn: 0, tokOut: 0, tokCache: 0, cost: 0, err: 0 };
        const b = state.backends[name];
        const keys = state.keys[name] || [];
        const down = keys.filter((k) => !k.healthy).length;
        const errRate = w.requests > 0 ? (w.err / w.requests) * 100 : 0;
        return { name, w, b, keys, down, errRate };
      })
      .sort((a, b) => b.w.cost - a.w.cost || b.w.requests - a.w.requests);
  }, [state, roll, q]);

  if (!state) {
    return (
      <Card className="border-edge-700 bg-edge-900">
        <CardHeader>
          <CardTitle className="text-xs tracking-[0.12em] uppercase">01 / Provider health</CardTitle>
          <CardDescription>Loading upstream health and selected-window counters…</CardDescription>
        </CardHeader>
        <CardContent className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <div key={i} className="space-y-4 border border-edge-700 bg-edge-950 p-5">
              <Skeleton className="h-4 w-1/3" />
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-4 w-2/3" />
            </div>
          ))}
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 border border-edge-700 bg-edge-900 lg:grid-cols-4">
        {[
          { label: 'Matched backends', value: fmtInt(backends.length), tone: '' },
          { label: 'Window requests', value: fmtInt(backends.reduce((sum, backend) => sum + backend.w.requests, 0)), tone: 'text-primary' },
          { label: 'Window spend', value: fmtMoney(backends.reduce((sum, backend) => sum + backend.w.cost, 0)), tone: 'text-chart-4' },
          { label: 'Keys cooling', value: fmtInt(backends.reduce((sum, backend) => sum + backend.down, 0)), tone: 'text-chart-4' },
        ].map((metric) => (
          <div key={metric.label} className="min-w-0 border-b border-r border-edge-700 p-5 last:border-r-0 lg:border-b-0">
            <p className="text-[10px] tracking-[0.14em] text-muted-foreground uppercase">{metric.label}</p>
            <p className={`mt-3 text-3xl tracking-tight tabular-nums ${metric.tone}`}>{metric.value}</p>
          </div>
        ))}
      </div>
      <Card className="gap-0 overflow-hidden border-edge-700 bg-edge-900">
        <CardHeader className="border-b border-edge-700 py-5">
          <CardTitle className="flex items-center gap-2 text-xs tracking-[0.12em] uppercase"><Server className="size-4 text-primary" />01 / Provider health</CardTitle>
          <CardDescription>{range.label} traffic and spend. Pool health is current; latency counters are process-session scoped. Select a backend to inspect.</CardDescription>
          <CardAction className="self-center">
            <div className="relative">
              <Search className="pointer-events-none absolute top-1/2 left-3 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input aria-label="Filter backends" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Find a backend…" className="w-full pl-9 sm:w-56" />
            </div>
          </CardAction>
        </CardHeader>
        <CardContent className="p-5">
          {backends.length === 0 && (
            <Empty>
              <EmptyHeader>
                <EmptyTitle>{q ? 'No backends match' : 'No backends'}</EmptyTitle>
                <EmptyDescription>Backends appear once providers serve traffic in this window.</EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 2xl:grid-cols-3">
            {backends.map(({ name, w, down, errRate, keys }, index) => (
              <button
                key={name}
                type="button"
                onClick={() => setSelected(name)}
                className="group border border-edge-700 bg-edge-950/70 p-5 text-left transition-colors outline-none hover:border-primary/50 hover:bg-edge-800/60 focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary/30"
              >
                <div className="mb-5 flex items-center justify-between gap-3">
                  <div className="flex min-w-0 items-center gap-3"><span className="text-[10px] text-edge-500">{String(index + 1).padStart(2, '0')}</span><span className="truncate text-sm text-foreground">{name}</span></div>
                  <ArrowUpRight className="size-4 shrink-0 text-edge-500 transition-colors group-hover:text-primary" />
                </div>
                <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
                  <div><div className="text-[9px] tracking-[0.14em] text-muted-foreground uppercase">Window requests</div><div className="mt-2 text-3xl tracking-tight tabular-nums">{fmtInt(w.requests)}</div></div>
                  <div className="text-right"><div className="text-[9px] tracking-[0.14em] text-muted-foreground uppercase">Spend</div><div className="mt-2 text-lg text-chart-4 tabular-nums">{fmtMoney(w.cost)}</div></div>
                </div>
                <dl className="grid grid-cols-2 gap-x-5 gap-y-4 border-y border-edge-700 py-4 text-xs">
                  <div><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Tokens</dt><dd className="mt-1 tabular-nums">{fmtTokens(w.tokIn + w.tokOut)}</dd></div>
                  <div><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Cached</dt><dd className="mt-1 tabular-nums">{fmtTokens(w.tokCache)}</dd></div>
                  <div><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Error rate</dt><dd className={`mt-1 tabular-nums ${errRate > 0 ? 'text-destructive' : ''}`}>{w.requests > 0 ? `${errRate.toFixed(1)}%` : '—'}</dd></div>
                  <div><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Errors</dt><dd className={`mt-1 tabular-nums ${w.err > 0 ? 'text-destructive' : ''}`}>{fmtInt(w.err)}</dd></div>
                </dl>
                <div className="mt-4 space-y-3">
                  <div className="flex items-center justify-between gap-2 text-[10px]">
                    <span className="text-muted-foreground">{keys.length} recorded keys</span>
                    <span className={keys.length === 0 ? 'text-muted-foreground' : down > 0 ? 'text-chart-4' : 'text-primary'}>{keys.length === 0 ? 'No key health data' : down > 0 ? `${down} in cooldown` : 'All available'}</span>
                  </div>
                  <HealthBar pct={keys.length ? ((keys.length - down) / keys.length) * 100 : 0} tone={down > 0 ? 'warn' : 'ok'} />
                </div>
              </button>
            ))}
          </div>
        </CardContent>
      </Card>

      <Sheet open={!!selected} onOpenChange={(open) => { if (!open) setSelected(null); }}>
        <SheetContent className="w-full overflow-y-auto border-edge-700 bg-edge-900 sm:max-w-2xl">
          <SheetHeader className="border-b border-edge-700 p-6">
            <p className="text-[10px] tracking-[0.16em] text-primary uppercase">Provider telemetry / drilldown</p>
            <SheetTitle>{selected}</SheetTitle>
            <SheetDescription>Current key health, session latency, and selected-window model activity.</SheetDescription>
          </SheetHeader>
          <div className="px-6 pb-6">
            {selected && (
              <BackendDetail
                name={selected}
                win={roll.backends[selected]}
                b={state.backends[selected]}
                keys={state.keys[selected] || []}
                models={roll.models}
                rangeLabel={range.label}
              />
            )}
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}

function BackendDetail({ name, win, b, keys, models, rangeLabel }: {
  name: string; win?: DimRollup; b?: MetricSet; keys: KeyStat[]; models: Record<string, DimRollup>; rangeLabel: string;
}) {
  const modelRows = useMemo(() => {
    return Object.entries(models)
      .filter(([id]) => PREFIX_TO_BACKEND[id.split('/')[0]] === name)
      .sort((a, b2) => b2[1].cost - a[1].cost);
  }, [name, models]);

  const hist = b?.lat_hist || [];
  const histTotal = hist.reduce((s, x) => s + x, 0);
  const w = win || { requests: 0, tokIn: 0, tokOut: 0, tokCache: 0, cost: 0, err: 0 };

  return (
    <div>
      <div className="mb-4 text-[10px] tracking-[0.14em] text-primary uppercase">01 / {rangeLabel} window</div>
      <div className="mb-5 grid grid-cols-1 gap-px border border-edge-700 bg-edge-700 sm:grid-cols-2">
        <Mini label="requests" value={fmtInt(w.requests)} />
        <Mini label="cost" value={fmtMoney(w.cost)} />
        <Mini label="tokens" value={`${fmtTokens(w.tokIn)} in · ${fmtTokens(w.tokOut)} out`} />
        <Mini label="errors" value={fmtInt(w.err)} />
      </div>

      <p className="mb-5 border-l-2 border-chart-2/50 bg-chart-2/5 p-3 text-[11px] leading-relaxed text-muted-foreground">Keys and latency are since this process started. Request, cost, and error totals above follow the time window.</p>

      {b && (
        <div className="my-5 grid grid-cols-1 gap-px border border-edge-700 bg-edge-700 sm:grid-cols-2">
          <Mini label="session p95" value={fmtMs(b.lat_p95_ms)} />
          <Mini label="session p99" value={fmtMs(b.lat_p99_ms)} />
          <Mini label="session avg" value={fmtMs(b.lat_avg_ms)} />
          <Mini label="failovers" value={fmtInt(b.failover)} />
        </div>
      )}

      <Separator className="my-4" />

      <div className="mb-3 text-[10px] tracking-[0.14em] text-muted-foreground uppercase">02 / Key pool · session · {keys.length}</div>
      <div className="mb-5 overflow-x-auto border border-edge-700 bg-edge-950/40">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Key</TableHead><TableHead>St</TableHead><TableHead>Req</TableHead><TableHead>Err</TableHead><TableHead>Failover</TableHead><TableHead>Cost</TableHead><TableHead>Cooldown</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.length === 0 && (
              <TableRow>
                <TableCell colSpan={7}>
                  <Empty>
                    <EmptyHeader>
                      <EmptyMedia variant="icon"><Server className="size-4" /></EmptyMedia>
                      <EmptyTitle>No keys</EmptyTitle>
                      <EmptyDescription>No upstream credentials recorded for this backend this session.</EmptyDescription>
                    </EmptyHeader>
                  </Empty>
                </TableCell>
              </TableRow>
            )}
            {keys.map((k) => (
              <TableRow key={k.key}>
                <TableCell className="font-mono text-[10px]">{k.key}</TableCell>
                <TableCell>
                  <Tooltip>
                    <TooltipTrigger>
                      <span className={`inline-flex items-center gap-2 text-[10px] ${k.healthy ? 'text-primary' : 'text-chart-4'}`}><span className={`size-1.5 ${k.healthy ? 'bg-primary' : 'bg-chart-4'}`} />{k.healthy ? 'Available' : 'Cooldown'}</span>
                    </TooltipTrigger>
                    <TooltipContent>{k.healthy ? 'healthy' : `cooling down · ${k.cooldown_sec}s`}</TooltipContent>
                  </Tooltip>
                </TableCell>
                <TableCell className="font-mono">{fmtInt(k.requests)}</TableCell>
                <TableCell className={`tabular-nums ${k.err > 0 ? 'text-destructive' : ''}`}>{fmtInt(k.err)}</TableCell>
                <TableCell className={`tabular-nums ${k.failover > 0 ? 'text-chart-4' : ''}`}>{fmtInt(k.failover)}</TableCell>
                <TableCell className="text-chart-4 tabular-nums">{fmtMoney(k.cost)}</TableCell>
                <TableCell className="text-chart-4 tabular-nums">{k.healthy ? '—' : `${k.cooldown_sec}s`}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>

      <div className="mb-3 text-[10px] tracking-[0.14em] text-muted-foreground uppercase">03 / Latency distribution · session · {fmtInt(histTotal)} samples</div>
      {histTotal > 0 && (
        <div className="mb-1 flex h-32 items-end gap-1 border-b border-edge-700 bg-edge-950/40 px-3 pt-3">
          {hist.map((n, i) => (
            <div key={i} title={`${HIST_LABELS[i]} ms: ${fmtInt(n)}`} className="flex-1 bg-chart-2/70" style={{ height: `${(n / Math.max(...hist)) * 100}%` }} />
          ))}
        </div>
      )}
      {histTotal === 0 && <p className="mb-3 border border-edge-700 bg-edge-950/40 p-5 text-xs text-muted-foreground">No latency samples recorded for this process session.</p>}
      <div className="mb-4 flex gap-0.5 text-[8px] text-muted-foreground">
        {HIST_LABELS.map((l, i) => <div key={i} className="flex-1 text-center">{l}</div>)}
      </div>

      <div className="mb-3 text-[10px] tracking-[0.14em] text-muted-foreground uppercase">04 / Models · {rangeLabel} · {modelRows.length}</div>
      <div className="overflow-x-auto border border-edge-700 bg-edge-950/40">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Model</TableHead><TableHead>Req</TableHead><TableHead>Cost</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {modelRows.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="text-xs text-muted-foreground">No models served in the {rangeLabel} window.</TableCell>
              </TableRow>
            )}
            {modelRows.map(([id, m]) => (
              <TableRow key={id}>
                <TableCell className="max-w-[200px] truncate text-xs">{id}</TableCell>
                <TableCell className="font-mono">{fmtInt(m.requests)}</TableCell>
                <TableCell className="text-chart-4 tabular-nums">{fmtMoney(m.cost)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>

      <div className="mt-4 flex flex-wrap gap-1">
        <Badge variant="outline">session metrics</Badge>
        <Badge variant="outline">{rangeLabel} rollups</Badge>
      </div>
    </div>
  );
}

function Mini({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 bg-edge-950/70 p-4">
      <div className="text-[9px] tracking-[0.14em] text-muted-foreground uppercase">{label}</div>
      <div className="mt-2 break-words text-lg text-foreground tabular-nums">{value}</div>
    </div>
  );
}
