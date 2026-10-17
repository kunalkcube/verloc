import { useEffect, useMemo, useState } from 'react';
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from 'recharts';
import {
  getAnalytics, fmtMoney, fmtInt, fmtTokens, fmtMs, fmtDateTime,
  type Analytics as AnalyticsData, type TabProps, type UsageGroup,
} from '../api';
import { HistoryFilters, useHistoryFilters } from '../components/HistoryFilters';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/ui/select';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { ChartContainer, ChartTooltip, ChartTooltipContent } from '../components/ui/chart';
import { GRID_STROKE, InlineError, KpiCard, PALETTE, PanelCard, TICK } from '../components/charts/shared';
import Cost from './Cost';
import Latency from './Latency';

type UsageGroupKey = 'clients' | 'models' | 'providers';
const UNATTRIBUTED_LABELS: Record<UsageGroupKey, string> = {
  clients: 'Unattributed client',
  models: 'Unattributed model',
  providers: 'Unattributed provider',
};

export default function Analytics(props: TabProps) {
  const { filters, setFilters, params, refreshWindow } = useHistoryFilters(props.range);
  const [data, setData] = useState<AnalyticsData | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(true);
  const [refresh, setRefresh] = useState(0);
  const [group, setGroup] = useState<UsageGroupKey>('clients');
  const query = params.toString();

  useEffect(() => {
    const ac = new AbortController();
    setBusy(true);
    setError('');
    setData(null);
    getAnalytics(new URLSearchParams(query), ac.signal)
      .then(setData)
      .catch((e) => { if (!ac.signal.aborted) setError(e instanceof Error ? e.message : 'Analytics unavailable'); })
      .finally(() => { if (!ac.signal.aborted) setBusy(false); });
    return () => ac.abort();
  }, [query, refresh]);

  const retry = () => { refreshWindow(); setRefresh((v) => v + 1); };
  const summary = data?.summary;

  return (
    <div className="space-y-5">
      <PanelCard title="01 / Explore durable usage" description="Dimensional filters apply to attributed totals, distributions and breakdowns below. Snapshot history is separately labeled.">
        <HistoryFilters filters={filters} onChange={setFilters} busy={busy} onRefresh={retry} />
      </PanelCard>

      {error && <InlineError message={error} onRetry={retry} />}

      {busy && !data && (
        <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-[86px]" />)}
        </div>
      )}

      {!busy && !data && !error && (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>Analytics unavailable</EmptyTitle>
            <EmptyDescription>Retry above to reload durable analytics.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {data && summary && (
        <>
          <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 xl:grid-cols-4 [&>*]:border-0">
            <KpiCard label="Requests" value={fmtInt(summary.requests)} sub={`${fmtInt(summary.errors)} errors`} />
            <KpiCard label="Known cost" value={fmtMoney(summary.cost)} sub={`${fmtInt(summary.unknown_cost)} requests with unknown cost`} accent="text-chart-4" />
            <KpiCard label="Known input / output" value={`${fmtTokens(summary.tok_in)} / ${fmtTokens(summary.tok_out)}`} sub={`${fmtInt(summary.unknown_usage)} requests with unknown usage`} />
            <KpiCard label="Cache tokens" value={fmtTokens(summary.tok_cache)} sub="Reported cache-read tokens" />
          </div>

          {(summary.unknown_usage > 0 || summary.unknown_cost > 0) && (
            <p>
              <Badge variant="outline" className="whitespace-normal border-chart-4/40 px-3 py-2 text-chart-4">
                Incomplete usage / cost coverage — unknown is not zero or free
              </Badge>
            </p>
          )}
          <PanelCard
            title="02 / Dimensional comparison"
            description="Compare request volume and errors, then select a row to narrow the query. All groups remain in the ledger."
            action={
              <Select value={group} onValueChange={(v) => setGroup(v as UsageGroupKey)}>
                <SelectTrigger size="sm" aria-label="Breakdown dimension">
                  <SelectValue>{group === 'clients' ? 'By client' : group === 'models' ? 'By model' : 'By provider'}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="clients">By client</SelectItem>
                  <SelectItem value="models">By model</SelectItem>
                  <SelectItem value="providers">By provider</SelectItem>
                </SelectContent>
              </Select>
            }
          >
            <UsageComparison rows={data[group] || []} dimension={group} />
            <div className="mb-3 mt-5 border-t border-edge-700 pt-4 text-[10px] uppercase tracking-[.16em] text-edge-400">Full {group} ledger / select an identity to filter</div>
            <UsageTable
              rows={data[group] || []}
              dimension={group}
              onFilter={(id) => setFilters({
                ...filters,
                [group === 'clients' ? 'client_key_id' : group === 'models' ? 'model' : 'backend']: id,
              })}
            />
          </PanelCard>


          <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 xl:grid-cols-4 [&>*]:border-0">
            <KpiCard label="Average latency" value={fmtMs(summary.avg_latency_ms)} />
            <KpiCard
              label="Latency p50 / p95"
              value={`${fmtMs(data.latency.p50)} / ${fmtMs(data.latency.p95)}`}
              sub={`p99 ${fmtMs(data.latency.p99)} · bucket estimates`}
            />
            <KpiCard label="Average TTFT" value={fmtMs(summary.avg_ttft_ms)} />
            <KpiCard
              label="TTFT p50 / p95"
              value={`${fmtMs(data.ttft.p50)} / ${fmtMs(data.ttft.p95)}`}
              sub={`p99 ${fmtMs(data.ttft.p99)} · recorded streams`}
            />
          </div>

          <div className="grid items-start gap-5 xl:grid-cols-[minmax(0,3fr)_minmax(260px,1fr)]">
          <PanelCard
            title="03 / Response-time distribution"
            description="Durable histogram · selected filters · legacy snapshots without distributions excluded"
          >
            {data.latency.buckets?.some((count) => count > 0) ? (
              <div>
                <ChartContainer config={{ count: { label: 'Requests', color: PALETTE.latency } }} className="h-[240px] w-full">
                  <BarChart
                    data={data.latency.buckets.map((count, index) => ({
                      count,
                      label: index < data.latency.bounds.length
                        ? `≤${fmtMs(data.latency.bounds[index])}`
                        : `>${fmtMs(data.latency.bounds[data.latency.bounds.length - 1])}`,
                    }))}
                    margin={{ top: 5, right: 8, left: 0, bottom: 0 }}
                  >
                    <CartesianGrid stroke={GRID_STROKE} strokeDasharray="2 4" vertical={false} />
                    <XAxis dataKey="label" tick={TICK} tickLine={false} axisLine={false} tickMargin={8} />
                    <YAxis tick={TICK} tickLine={false} axisLine={false} width={44} allowDecimals={false} />
                    <ChartTooltip
                      content={
                        <ChartTooltipContent
                          formatter={(value, name) => <span className="font-mono">{fmtInt(Number(value))} {name}</span>}
                        />
                      }
                    />
                    <Bar dataKey="count" name="Requests" fill="var(--color-count)" radius={0} />
                  </BarChart>
                </ChartContainer>
                <p className="mt-2 text-[11px] text-muted-foreground">
                  Each bucket is the number of recorded requests in that latency range. Legacy snapshots
                  without distributions are excluded.
                </p>
              </div>
            ) : (
              <Empty>
                <EmptyHeader>
                  <EmptyTitle>No recorded latency distribution</EmptyTitle>
                  <EmptyDescription>No histogram buckets cover these filters.</EmptyDescription>
                </EmptyHeader>
              </Empty>
            )}
          </PanelCard>
          <PanelCard title="Response status" description="Select a status to filter requests">
            <div className="space-y-2">
              {data.status?.length ? (
                data.status.map((s) => (
                  <Button
                    key={s.status}
                    size="sm"
                    variant="outline"
                    className="h-auto w-full justify-between gap-4 px-3 py-3"
                    onClick={() => setFilters({ ...filters, status: String(s.status) })}
                  >
                    <span className={s.status >= 400 ? 'text-destructive' : 'text-primary'}>{s.status <= 0 ? 'No response' : s.status}</span><span className="text-edge-200">{fmtInt(s.requests)} <span className="text-[10px] text-edge-400">req</span></span>
                  </Button>
                ))
              ) : (
                <p className="text-xs text-muted-foreground">No response status data.</p>
              )}
            </div>
          </PanelCard>
          </div>


          <PanelCard title="Coverage and accuracy" description="What the totals above do and do not include">
            <div className="grid gap-4 text-[11px] leading-relaxed text-edge-400 md:grid-cols-2 [&>p]:border-l [&>p]:border-edge-700 [&>p]:pl-3">
              <p className="text-edge-200 md:col-span-2">
                {data.coverage.oldest_ts
                  ? `${fmtDateTime(data.coverage.oldest_ts)} — ${fmtDateTime(data.coverage.newest_ts)}`
                  : 'No durable request coverage for these filters.'}
              </p>
              <p>
                Analytics history is retained indefinitely, with no automatic expiry. History from older
                versions may already be aggregate-only; missing request detail cannot be recovered.
              </p>
              <p>
                Known totals exclude missing usage and missing pricing. Unknown cost is not free usage.
                Client attribution is available only for requests recorded with client identity.
              </p>
              <p>
                Cached input is a subset of input tokens. Total tokens are input + output; cache tokens
                are displayed separately and never added twice.
              </p>
              {(data.coverage.legacy_aggregates_only || !!data.coverage.legacy_summary?.requests) && (
                <p className="text-chart-4">
                  Legacy aggregate-only history exists. It cannot recover historical client identity, exact
                  request drilldowns or percentile distributions. Its totals are reported separately, never
                  mixed into the attributed totals and breakdowns above.
                </p>
              )}
              {!!data.coverage.legacy_summary?.requests && (
                <p className="font-mono">
                  Separate legacy snapshots: {fmtInt(data.coverage.legacy_summary.requests)} requests ·{' '}
                  {fmtInt(data.coverage.legacy_summary.errors)} errors ·{' '}
                  {fmtTokens(data.coverage.legacy_summary.tok_in + data.coverage.legacy_summary.tok_out)} reported tokens ·{' '}
                  {fmtMoney(data.coverage.legacy_summary.cost)} recorded spend. Legacy usage and pricing
                  coverage cannot be reconstructed.
                </p>
              )}
              {!!data.coverage.aggregate_only_requests && (
                <p className="text-chart-4">
                  {fmtInt(data.coverage.aggregate_only_requests)} requests from older versions are available only as hourly aggregates.
                  Their original detail is no longer available; no request-level drilldown is possible.{' '}
                  {data.coverage.partial_buckets_excluded ? 'Partial historical hour buckets are excluded for precise filter boundaries.' : ''}
                </p>
              )}
              {data.coverage.detail_oldest_ts && (
                <p>Retained request detail starts {fmtDateTime(data.coverage.detail_oldest_ts)}.</p>
              )}
              <p>
                Latency percentiles are histogram-bucket estimates over the selected data, never averages of
                time-series percentiles. Missing historical distributions are unavailable, not reconstructed.
              </p>
            </div>
          </PanelCard>
        </>
      )}

      <PanelCard
        title={`04 / Historical spend · ${props.range.label}`}
        description="Saved gateway snapshots for the global time window. Dimensional filters above do not apply. Legacy history is preserved; historical percentiles cannot be reconstructed from snapshots."
      >
        <Cost {...props} />
      </PanelCard>

      <PanelCard title="05 / Latency trends and process diagnostics" description="Snapshot-reported trends and session counters · dimensional filters do not apply">
        <Latency {...props} />
      </PanelCard>
    </div>
  );
}

function UsageComparison({ rows, dimension }: { rows: UsageGroup[]; dimension: UsageGroupKey }) {
  const ranked = useMemo(() => [...rows].sort((a, b) => b.requests - a.requests).slice(0, 8).map((row) => ({
    ...row,
    label: row.name || row.id || UNATTRIBUTED_LABELS[dimension],
  })), [rows, dimension]);
  if (!ranked.length) return null;
  return <div className="grid items-start gap-4 lg:grid-cols-[minmax(0,1fr)_180px]">
    <ChartContainer config={{ requests: { label: 'Requests', color: PALETTE.requests }, errors: { label: 'Errors', color: PALETTE.errors } }} className="w-full" style={{ height: Math.max(180, ranked.length * 46 + 30) }}>
      <BarChart layout="vertical" data={ranked} margin={{ top: 8, right: 8, left: 0, bottom: 0 }} barSize={9} barGap={3}>
        <CartesianGrid stroke={GRID_STROKE} strokeDasharray="3 6" horizontal={false} />
        <YAxis type="category" dataKey="label" width={130} tick={TICK} tickFormatter={(value: string) => value.length > 20 ? `${value.slice(0, 18)}…` : value} tickLine={false} axisLine={false} />
        <XAxis type="number" tick={TICK} tickLine={false} axisLine={false} allowDecimals={false} tickFormatter={(value: number) => fmtInt(value)} />
        <ChartTooltip content={<ChartTooltipContent labelFormatter={(_, payload) => payload?.[0]?.payload?.label || ''} formatter={(value, name) => <span>{fmtInt(Number(value))} {name}</span>} />} />
        <Bar dataKey="requests" name="Requests" fill={PALETTE.requests} radius={0} />
        <Bar dataKey="errors" name="Errors" fill={PALETTE.errors} radius={0} />
      </BarChart>
    </ChartContainer>
    <div className="space-y-4 border-l border-edge-700 pl-4 text-[11px]">
      <p className="text-[10px] uppercase tracking-[.16em] text-edge-400">Request volume</p>
      <div className="flex items-center gap-2 text-edge-200"><span className="size-2 bg-primary" />All requests</div>
      <div className="flex items-center gap-2 text-edge-200"><span className="size-2 bg-destructive" />Errors (subset)</div>
      <p className="text-[10px] leading-relaxed text-edge-400">Top {ranked.length} of {rows.length} groups, ranked by request count. Errors are included in requests, not additional traffic.</p>
    </div>
  </div>;
}

function UsageTable({ rows, dimension, onFilter }: {
  rows: UsageGroup[];
  dimension: UsageGroupKey;
  onFilter: (id: string) => void;
}) {
  const dimLabel = dimension === 'clients' ? 'Client API key' : dimension === 'models' ? 'Model' : 'Provider';
  return (
    <Table className="text-[11px] [&_th]:text-[10px] [&_th]:uppercase [&_th]:tracking-wider [&_td]:py-3">
      <TableHeader>
        <TableRow>
          <TableHead>{dimLabel}</TableHead>
          <TableHead className="text-right">Requests / errors</TableHead>
          <TableHead className="text-right">Avg latency / TTFT</TableHead>
          <TableHead className="text-right">Input / output / cache</TableHead>
          <TableHead className="text-right">Known cost</TableHead>
          <TableHead className="text-right">Unknown usage / cost</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.length === 0 && (
          <TableRow>
            <TableCell colSpan={6} className="py-8 text-center text-xs uppercase tracking-widest text-muted-foreground">
              No usage groups match these filters
            </TableCell>
          </TableRow>
        )}
        {rows.map((row) => (
          <TableRow key={row.id || 'unattributed'}>
            <TableCell className="max-w-[220px]">
              {row.id ? (
                <Button type="button" variant="link" onClick={() => onFilter(row.id)} className="h-auto max-w-full flex-col items-start gap-1 p-0 text-left">
                  <span className="max-w-full whitespace-normal break-all text-[11px] text-primary">{row.name || row.id}</span>
                  {row.name && row.name !== row.id && <span className="max-w-full whitespace-normal break-all text-[10px] font-normal text-edge-400">{row.id}</span>}
                </Button>
              ) : (
                <span className="text-xs text-muted-foreground">{UNATTRIBUTED_LABELS[dimension]}</span>
              )}
            </TableCell>
            <TableCell className="text-right font-mono">
              {fmtInt(row.requests)} / <span className={row.errors > 0 ? 'text-destructive' : ''}>{fmtInt(row.errors)}</span>
            </TableCell>
            <TableCell className="text-right font-mono">{fmtMs(row.avg_latency_ms)} / {fmtMs(row.avg_ttft_ms)}</TableCell>
            <TableCell className="text-right font-mono">
              {row.unknown_usage > 0 && row.tok_in + row.tok_out === 0 ? (
                <span className="text-chart-4">Unknown</span>
              ) : (
                <>{fmtTokens(row.tok_in)} / {fmtTokens(row.tok_out)} / {fmtTokens(row.tok_cache)}</>
              )}
            </TableCell>
            <TableCell className="text-right font-mono text-chart-4">
              {row.unknown_cost > 0 && !row.cost ? <span>Unknown</span> : fmtMoney(row.cost)}
            </TableCell>
            <TableCell className="text-right font-mono">
              {row.unknown_usage > 0 || row.unknown_cost > 0 ? (
                <span className="text-chart-4">{fmtInt(row.unknown_usage)} / {fmtInt(row.unknown_cost)}</span>
              ) : (
                <span className="text-muted-foreground">0 / 0</span>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
