import { useEffect, useMemo, useState } from 'react';
import {
  ratePerSec, fmtInt, fmtMoney, fmtTokens, fmtMs,
  getAnalytics, type TabProps, type Analytics,
} from '../api';
import { rollupSeries, fmtSpan } from '../window';
import { Badge } from '../components/ui/badge';
import { Card, CardContent } from '../components/ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/ui/select';
import { Skeleton } from '../components/ui/skeleton';
import { TrafficAreaChart } from '../components/charts/TrafficAreaChart';
import { CostSharePie } from '../components/charts/CostSharePie';
import {
  CHART_RANGES, filterSeriesByDays, InlineError, KpiCard, PanelCard, type ChartRangeKey,
} from '../components/charts/shared';

export default function Overview({ state, series, range }: TabProps) {
  const [chartRange, setChartRange] = useState<ChartRangeKey>('30d');
  const [detail, setDetail] = useState<Analytics | null>(null);
  const [detailError, setDetailError] = useState('');
  const [detailRound, setDetailRound] = useState(0);

  useEffect(() => {
    let stopped = false;
    let inFlight = false;
    const controller = new AbortController();
    setDetail(null);
    setDetailError('');
    const load = async () => {
      if (inFlight || stopped) return;
      inFlight = true;
      const query = new URLSearchParams();
      if (range.ms !== null) query.set('since', String(Date.now() - range.ms));
      try {
        const response = await getAnalytics(query, controller.signal);
        if (!stopped) { setDetail(response); setDetailError(''); }
      } catch (e) {
        if (!stopped) setDetailError(e instanceof Error ? e.message : 'Detailed latency unavailable');
      } finally { inFlight = false; }
    };
    void load();
    const timer = setInterval(load, 15000);
    return () => { stopped = true; controller.abort(); clearInterval(timer); };
  }, [range, detailRound]);

  const days = CHART_RANGES.find((r) => r.value === chartRange)?.days ?? 30;
  const windowed = useMemo(() => filterSeriesByDays(series, days), [series, days]);
  const roll = useMemo(() => rollupSeries(windowed), [windowed]);

  const rps = ratePerSec(state?.delta?.requests ?? 0, state?.delta?.d_ms || 3000);
  const errRate = roll.requests > 0 ? (roll.err / roll.requests) * 100 : 0;
  const booting = !state;

  const pieRows = useMemo(() => Object.entries(roll.backends).map(([name, m]) => ({
    name: name || 'Unattributed provider', cost: m.cost, requests: m.requests,
  })), [roll]);

  const summary = booting ? 'Loading gateway state…' : `${fmtInt(roll.requests)} requests · ${fmtMoney(roll.cost)} known spend · ${fmtTokens(roll.tokIn + roll.tokOut)} tokens`;

  return (
    <div className="space-y-5">
      <Card className="rounded-none border-edge-700 bg-edge-900/95 py-0 shadow-none">
        <CardContent className="flex flex-wrap items-center justify-between gap-4 px-5 py-4">
          <div className="min-w-0">
            <div className="text-[10px] uppercase tracking-[.2em] text-primary">01 / Traffic summary</div>
            <p className="mt-2 text-xs leading-relaxed text-edge-200">{summary}</p>
            <p className="mt-1 text-[10px] text-edge-400">{booting ? 'Waiting for snapshots' : fmtSpan(roll.from, roll.to)} · within global {range.label} window</p>
          </div>
          <div className="flex items-center gap-3">
            <span className="text-[10px] uppercase tracking-widest text-edge-400">Chart range</span>
            <Select value={chartRange} onValueChange={(v) => setChartRange(v as ChartRangeKey)}>
              <SelectTrigger size="sm" aria-label="Chart range"><SelectValue>{CHART_RANGES.find((option) => option.value === chartRange)?.label}</SelectValue></SelectTrigger>
              <SelectContent>{CHART_RANGES.map((r) => <SelectItem key={r.value} value={r.value}>{r.label}</SelectItem>)}</SelectContent>
            </Select>
          </div>
        </CardContent>
      </Card>

      <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 md:grid-cols-3 xl:grid-cols-6 [&>*]:border-0">
        {booting ? (
          Array.from({ length: 6 }).map((_, i) => <Skeleton key={i} className="h-[86px]" />)
        ) : (
          <>
            <KpiCard label="Requests" value={fmtInt(roll.requests)} sub={`${rps.toFixed(2)}/s now`} />
            <KpiCard label="Known spend" value={fmtMoney(roll.cost)} sub={`lifetime ${fmtMoney(state?.lifetime?.cost ?? 0)}`} accent="text-chart-4" />
            <KpiCard label="Tokens" value={fmtTokens(roll.tokIn + roll.tokOut)} sub={`${fmtTokens(roll.tokIn)} in · ${fmtTokens(roll.tokOut)} out`} />
            <KpiCard label="Error rate" value={`${errRate.toFixed(1)}%`} sub={`${fmtInt(roll.err)} errors`} accent={errRate > 0 ? 'text-destructive' : 'text-primary'} />
            <KpiCard
              label="Latency p95"
              value={detail ? fmtMs(detail.latency.p95) : <Skeleton className="h-6 w-20" />}
              sub={detail ? `p50 ${fmtMs(detail.latency.p50)} · p99 ${fmtMs(detail.latency.p99)} · bucket estimates` : 'Loading durable distributions…'}
            />
            <KpiCard
              label="Average TTFT"
              value={detail ? fmtMs(detail.summary.avg_ttft_ms) : <Skeleton className="h-6 w-20" />}
              sub="Durable recorded streams in window"
            />
          </>
        )}
      </div>

      <div className="space-y-2 border-l-2 border-primary/40 bg-edge-900/80 px-4 py-3 text-[11px] leading-relaxed text-edge-400">
        <p>
          Traffic and spend charts use durable gateway snapshots for the chart range above.
          Latency and TTFT use durable request distributions, not averaged snapshot percentiles.
          Unknown usage and missing prices are not zero; inspect coverage in Analytics.
        </p>
        {detail && (detail.summary.unknown_usage > 0 || detail.summary.unknown_cost > 0) && (
          <p>
            <Badge variant="outline" className="whitespace-normal border-chart-4/40 text-chart-4">
              {fmtInt(detail.summary.unknown_usage)} requests with unknown usage · {fmtInt(detail.summary.unknown_cost)} with unknown cost
            </Badge>
          </p>
        )}
        {(detail?.coverage.legacy_aggregates_only || !!detail?.coverage.legacy_summary?.requests) && (
          <p className="text-chart-4">
            Legacy snapshot history extends beyond attributed request detail. Historical client identities
            and latency distributions are unavailable for that legacy period.
          </p>
        )}
        {detailError && <InlineError message={`${detailError}. Retrying automatically.`} onRetry={() => setDetailRound((v) => v + 1)} />}
      </div>

      <PanelCard title="Traffic / known spend" description={`Requests and known cost per snapshot tick · ${chartRange} chart range`} action={<Badge variant="outline">Snapshot series</Badge>}>
        <TrafficAreaChart series={windowed} range={range} loading={booting && !windowed.length} />
      </PanelCard>

      <div className="grid grid-cols-1 items-start gap-5 xl:grid-cols-12">
        <PanelCard title="Provider allocation" description="Known spend, share, and recorded request counts by provider" className="xl:col-span-8">
          <CostSharePie rows={pieRows} loading={booting} />
        </PanelCard>
        <PanelCard title="Data sources" description="Snapshot and request-level coverage" className="xl:col-span-4">
          <dl className="space-y-4 text-[11px] leading-relaxed">
            <div><dt className="mb-1 text-primary">Snapshot window</dt><dd className="text-edge-400">Traffic, tokens, known spend and error rate follow the chart range above, within the global time window.</dd></div>
            <div className="border-t border-edge-700 pt-4"><dt className="mb-1 text-chart-2">Durable request window</dt><dd className="text-edge-400">Latency and TTFT use the global {range.label} window. Percentiles are histogram-bucket estimates, not averages of snapshots.</dd></div>
            <div className="border-t border-edge-700 pt-4"><dt className="mb-1 text-chart-4">Coverage matters</dt><dd className="text-edge-400">Unknown usage and missing prices never mean zero. Analytics provides attributed breakdowns and full coverage notes.</dd></div>
          </dl>
        </PanelCard>
      </div>

      {series.length === 0 && !booting && (
        <Empty className="mt-4">
          <EmptyHeader>
            <EmptyTitle>No snapshots in the {range.label} window</EmptyTitle>
            <EmptyDescription>Widen the time window above to see gateway traffic.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}
    </div>
  );
}
