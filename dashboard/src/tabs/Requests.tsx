import { useEffect, useState } from 'react';
import { Inbox, TriangleAlert } from 'lucide-react';
import { getRequests, fmtInt, fmtMoney, fmtTokens, fmtMs, fmtDateTime, type RequestPage, type TabProps, type ReqRecord } from '../api';
import { HistoryFilters, useHistoryFilters } from '../components/HistoryFilters';
import { InlineError, KpiCard } from '../components/charts/shared';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty';
import { Pagination, PaginationContent, PaginationItem, PaginationNext, PaginationPrevious } from '@/components/ui/pagination';
import { Separator } from '@/components/ui/separator';
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet';
import { Skeleton } from '@/components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';

function outcomeVariant(status: number): 'destructive' | 'secondary' | 'default' | 'outline' {
  if (status <= 0) return 'outline';
  if (status >= 500) return 'destructive';
  if (status >= 400) return 'secondary';
  return 'default';
}

function outcomeLabel(r: ReqRecord): string {
  return r.outcome || (r.ok ? 'completed' : 'failed');
}


function SkeletonRows({ rows = 8 }: { rows?: number }) {
  return (
    <>
      {Array.from({ length: rows }, (_, i) => (
        <TableRow key={i}>
          <TableCell><div className="space-y-1.5"><Skeleton className="h-3.5 w-32" /><Skeleton className="h-3 w-24" /></div></TableCell>
          <TableCell><Skeleton className="h-3.5 w-28" /></TableCell>
          <TableCell><Skeleton className="h-3.5 w-20" /></TableCell>
          <TableCell><Skeleton className="h-3.5 w-24" /></TableCell>
          <TableCell><Skeleton className="h-5 w-14" /></TableCell>
          <TableCell><Skeleton className="h-3.5 w-24" /></TableCell>
          <TableCell><Skeleton className="h-3.5 w-24" /></TableCell>
          <TableCell><Skeleton className="h-3.5 w-16" /></TableCell>
        </TableRow>
      ))}
    </>
  );
}

function RequestDetail({ record }: { record: ReqRecord }) {
  const meta: [string, string][] = [
    ['Time', fmtDateTime(record.ts)],
    ['Model', record.model],
    ['Upstream', record.upstream || 'Not recorded'],
    ['Provider', record.backend || 'Unattributed'],
    ['Client', record.client_name || record.client_key_id || 'Unattributed'],
    ['Client IP', record.ip || '—'],
    ['User agent', record.ua || '—'],
    ['Upstream credential (masked)', record.key || '—'],
    ['Latency', fmtMs(record.lat_ms)],
    ['TTFT', fmtMs(record.ttft_ms)],
    ['Input / output / cached input (subset)', record.usage_known ? `${fmtTokens(record.tok_in)} / ${fmtTokens(record.tok_out)} / ${fmtTokens(record.tok_cache)}` : 'Unknown'],
    ['Cost', record.cost_known ? fmtMoney(record.cost) : 'Unknown'],
  ];
  return (
    <div className="space-y-5">
      <Card className="gap-0 rounded-none border-edge-700 bg-edge-950 py-0 shadow-none">
        <CardContent className="space-y-4 p-4">
          <div className="flex flex-wrap gap-2">
            <Badge variant={outcomeVariant(record.status)}>{record.status <= 0 ? 'No response' : record.status}</Badge>
            <Badge variant="secondary">{outcomeLabel(record)}</Badge>
            <Badge variant="outline">{record.stream ? 'Stream' : 'JSON'}</Badge>
            {record.failover > 0 && <Badge variant="outline">Failover ×{record.failover}</Badge>}
          </div>
          <div className="grid grid-cols-3 gap-3 border-t border-edge-700 pt-4">
            <div><p className="text-[10px] uppercase tracking-widest text-edge-400">Latency</p><p className="mt-2 text-lg text-chart-2">{fmtMs(record.lat_ms)}</p></div>
            <div><p className="text-[10px] uppercase tracking-widest text-edge-400">TTFT</p><p className="mt-2 text-lg text-chart-3">{fmtMs(record.ttft_ms)}</p></div>
            <div><p className="text-[10px] uppercase tracking-widest text-edge-400">Known cost</p><p className="mt-2 text-lg text-chart-4">{record.cost_known ? fmtMoney(record.cost) : 'Unknown'}</p></div>
          </div>
        </CardContent>
      </Card>
      <dl className="grid gap-x-5 sm:grid-cols-2">
        {meta.map(([label, value]) => (
          <div key={label} className="space-y-1.5 border-b border-edge-700 py-3">
            <dt className="text-[10px] uppercase tracking-[.1em] text-edge-400">{label}</dt>
            <dd className="break-words text-xs leading-relaxed text-edge-100">{value}</dd>
          </div>
        ))}
      </dl>
      <Separator />
      <div className="space-y-3">
        <h3 className="text-[10px] font-medium uppercase tracking-[.18em] text-primary">Execution / upstream attempts</h3>
        {record.attempts?.length ? (
          <Table>
            <TableHeader>
              <TableRow><TableHead>Backend</TableHead><TableHead>Upstream</TableHead><TableHead>Status</TableHead><TableHead>Outcome</TableHead><TableHead className="text-right">Latency</TableHead></TableRow>
            </TableHeader>
            <TableBody>
              {record.attempts.map((a, i) => (
                <TableRow key={i}>
                  <TableCell>{a.backend}</TableCell>
                  <TableCell className="font-mono text-xs">{a.upstream}</TableCell>
                  <TableCell><Badge variant={outcomeVariant(a.status)}>{a.status}</Badge></TableCell>
                  <TableCell>{a.outcome}</TableCell>
                  <TableCell className="text-right">{fmtMs(a.lat_ms)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : (
          <p className="text-sm text-muted-foreground">Attempt detail was not recorded for this request.</p>
        )}
      </div>
      {record.err_msg && <div role="alert" className="break-words border border-destructive/40 bg-destructive/10 px-4 py-3 text-xs leading-relaxed text-destructive">{record.err_msg}</div>}
    </div>
  );
}

export default function Requests({ range }: TabProps) {
  const { filters, setFilters, params, refreshWindow } = useHistoryFilters(range);
  const [data, setData] = useState<RequestPage | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(true);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState<string[]>([]);
  const [refresh, setRefresh] = useState(0);
  const [selected, setSelected] = useState<ReqRecord | null>(null);
  const query = params.toString();
  useEffect(() => { setCursor(''); setPrevious([]); }, [query]);
  useEffect(() => {
    const ac = new AbortController();
    setBusy(true); setError(''); setData(null);
    const request = new URLSearchParams(query); request.set('limit', '50'); if (cursor) request.set('cursor', cursor);
    getRequests(request, ac.signal).then(setData).catch((e) => { if (!ac.signal.aborted) setError(e instanceof Error ? e.message : 'Could not load requests'); }).finally(() => { if (!ac.signal.aborted) setBusy(false); });
    return () => ac.abort();
  }, [query, cursor, refresh]);
  const summary = data?.summary;
  const refreshAll = () => { refreshWindow(); setCursor(''); setPrevious([]); setRefresh((v) => v + 1); };
  const canPrev = previous.length > 0;
  return <div className="space-y-5">
    <Card className="gap-0 rounded-none border-edge-700 bg-edge-900/95 py-0 shadow-none">
      <CardHeader className="gap-2 border-b border-edge-700 px-5 py-4">
        <CardTitle className="text-[11px] uppercase tracking-[.16em] text-primary">01 / Query retained history</CardTitle>
        <CardDescription className="max-w-4xl text-[11px] leading-relaxed">Durable metadata only, retained indefinitely with no automatic expiry. Totals cover every matching request with available detail, not just this page. Some history from older versions is aggregate-only in Analytics. Prompts, completions and credentials are never logged.</CardDescription>
      </CardHeader>
      <CardContent className="p-5">
        <HistoryFilters filters={filters} onChange={setFilters} busy={busy} onRefresh={refreshAll} />
      </CardContent>
    </Card>
    {error && <InlineError message={error} onRetry={() => setRefresh((v) => v + 1)} />}
    <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 md:grid-cols-3 xl:grid-cols-6 [&>*]:border-0">
      <KpiCard label="Matching requests" value={summary ? fmtInt(summary.requests) : '—'} sub="All matching retained detail" />
      <KpiCard label="Errors" value={summary ? fmtInt(summary.errors) : '—'} accent="text-destructive" sub="Within query" />
      <KpiCard label="Known cost" value={summary ? fmtMoney(summary.cost) : '—'} accent="text-chart-4" sub="Missing prices excluded" />
      <KpiCard label="Known tokens" value={summary ? fmtTokens(summary.tok_in + summary.tok_out) : '—'} sub="Input + output" />
      <KpiCard label="Avg latency" value={summary ? fmtMs(summary.avg_latency_ms) : '—'} accent="text-chart-2" sub="Recorded requests" />
      <KpiCard label="Avg TTFT" value={summary ? fmtMs(summary.avg_ttft_ms) : '—'} accent="text-chart-3" sub="Recorded streams" />
    </div>
    {!!summary && (summary.unknown_usage > 0 || summary.unknown_cost > 0) && <div role="note" className="flex items-start gap-3 border border-chart-4/30 bg-chart-4/5 px-4 py-3 text-[11px] leading-relaxed text-chart-4"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" />Usage unknown for {fmtInt(summary.unknown_usage)} requests; cost unknown for {fmtInt(summary.unknown_cost)}. Unknown is not zero or free.</div>}
    <Card className="gap-0 rounded-none border-edge-700 bg-edge-900/95 py-0 shadow-none">
      <CardHeader className="gap-2 border-b border-edge-700 px-5 py-4">
        <CardTitle className="text-[11px] uppercase tracking-[.16em]">02 / Request ledger</CardTitle>
        <CardDescription className="text-[11px]">{data ? `${fmtInt(data.total)} matches · page ${previous.length + 1} · up to 50 records` : 'Loading retained metadata…'} · Select a request to inspect its execution.</CardDescription>
      </CardHeader>
      <CardContent className="px-0">
        <Table className="text-[11px] [&_td]:px-4 [&_td]:py-3 [&_th]:px-4 [&_th]:text-[10px] [&_th]:uppercase [&_th]:tracking-wider">
          <TableHeader>
            <TableRow>
              <TableHead>Time / request</TableHead><TableHead>Model</TableHead><TableHead>Provider</TableHead><TableHead>Client</TableHead><TableHead>Outcome</TableHead><TableHead>Latency / TTFT</TableHead><TableHead>Tokens in / out</TableHead><TableHead className="text-right">Cost</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {busy && <SkeletonRows />}
            {!busy && !error && data?.requests?.map((r) => (
              <TableRow key={r.ts + r.id} className="cursor-pointer hover:bg-primary/5" onClick={() => setSelected(r)}>
                <TableCell>
                  <div className="whitespace-nowrap text-[11px] text-edge-200">{fmtDateTime(r.ts)}</div>
                  <Button variant="link" size="sm" onClick={(e) => { e.stopPropagation(); setSelected(r); }} className="mt-1 h-auto max-w-48 justify-start truncate p-0 text-[10px] text-primary" title={`Inspect request ${r.id}`}>{r.id}</Button>
                </TableCell>
                <TableCell className="max-w-52 truncate text-edge-100" title={r.model}>{r.model}</TableCell>
                <TableCell className="text-edge-300">{r.backend || 'Unattributed'}</TableCell>
                <TableCell className="max-w-40 truncate text-edge-300" title={r.client_name || r.client_key_id || 'Unattributed'}>{r.client_name || r.client_key_id || 'Unattributed'}</TableCell>
                <TableCell>
                  <Badge variant={outcomeVariant(r.status)}>{r.status <= 0 ? 'No response' : r.status}</Badge>
                  <div className="mt-1 text-xs text-muted-foreground">{outcomeLabel(r)}</div>
                </TableCell>
                <TableCell className="whitespace-nowrap tabular-nums"><span className="text-chart-2">{fmtMs(r.lat_ms)}</span><span className="text-edge-500"> / </span>{fmtMs(r.ttft_ms)}</TableCell>
                <TableCell className={`whitespace-nowrap tabular-nums ${r.usage_known ? 'text-edge-200' : 'text-chart-4'}`}>{r.usage_known ? `${fmtTokens(r.tok_in)} / ${fmtTokens(r.tok_out)}` : 'Unknown'}</TableCell>
                <TableCell className="text-right tabular-nums text-chart-4">{r.cost_known ? fmtMoney(r.cost) : 'Unknown'}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {!busy && !error && !data?.requests?.length && (
          <Empty className="border-0">
            <EmptyHeader>
              <EmptyMedia variant="icon"><Inbox /></EmptyMedia>
              <EmptyTitle>No requests match</EmptyTitle>
              <EmptyDescription>Try widening the time window or clearing filters.</EmptyDescription>
            </EmptyHeader>
          </Empty>
        )}
        {!busy && !!error && !data && (
          <Empty className="border-0">
            <EmptyHeader>
              <EmptyMedia variant="icon"><Inbox /></EmptyMedia>
              <EmptyTitle>History unavailable</EmptyTitle>
              <EmptyDescription>Retry above to reload request history.</EmptyDescription>
            </EmptyHeader>
          </Empty>
        )}
      </CardContent>
      <CardFooter className="flex-col items-stretch gap-2 border-t border-edge-700 bg-edge-950/50 px-4 py-3">
        <Pagination>
          <PaginationContent className="w-full justify-between">
            <PaginationItem>
              <PaginationPrevious
                href="#"
                aria-disabled={!canPrev || busy}
                className={!canPrev || busy ? 'pointer-events-none opacity-50' : undefined}
                onClick={(e) => { e.preventDefault(); if (canPrev && !busy) { setCursor(previous[previous.length - 1]); setPrevious((v) => v.slice(0, -1)); } }}
              />
            </PaginationItem>
            <PaginationItem>
              <span className="px-2 text-xs text-muted-foreground">Page {previous.length + 1}{data ? ` · ${fmtInt(data.total)} matches` : ''}</span>
            </PaginationItem>
            <PaginationItem>
              <PaginationNext
                href="#"
                aria-disabled={busy || !data?.next_cursor}
                className={busy || !data?.next_cursor ? 'pointer-events-none opacity-50' : undefined}
                onClick={(e) => { e.preventDefault(); if (data?.next_cursor && !busy) { setPrevious((v) => [...v, cursor]); setCursor(data.next_cursor as string); } }}
              />
            </PaginationItem>
          </PaginationContent>
        </Pagination>
      </CardFooter>
    </Card>
    <Sheet open={!!selected} onOpenChange={(open) => { if (!open) setSelected(null); }}>
      <SheetContent side="right" className="w-full overflow-y-auto border-edge-700 bg-edge-900 sm:max-w-2xl">
        <SheetHeader className="border-b border-edge-700 px-5 py-5 pr-12">
          <SheetTitle className="break-all text-sm leading-relaxed"><span className="mb-2 block text-[10px] font-normal uppercase tracking-[.18em] text-primary">Request / execution detail</span>{selected?.id}</SheetTitle>
          <SheetDescription className="text-[11px]">{selected ? fmtDateTime(selected.ts) : ''}</SheetDescription>
        </SheetHeader>
        <div className="px-5 pb-5">{selected && <RequestDetail record={selected} />}</div>
      </SheetContent>
    </Sheet>
  </div>;
}
