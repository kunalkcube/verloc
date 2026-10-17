import { useMemo } from 'react';
import { fmtInt, fmtMoney, fmtMs, fmtTokens, type TabProps } from '../api';
import { rollupSeries } from '../window';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { LatencyAreaChart } from '../components/charts/LatencyAreaChart';
import { ModelRadar } from '../components/charts/ModelRadar';
import { KpiCard, PanelCard, shortName } from '../components/charts/shared';

export default function Latency({ state, series, range }: TabProps) {
  const roll = useMemo(() => rollupSeries(series), [series]);
  const booting = !state;

  // Top-5 models by window requests. Request / cost / token shares follow the
  // selected window; error counts and session latency enrich from live process
  // counters and are labeled as such below.
  const footprints = useMemo(() => {
    const top = Object.entries(roll.models)
      .sort((a, b) => b[1].requests - a[1].requests)
      .slice(0, 5);
    return top.map(([name, w]) => {
      const session = state?.models[name];
      return {
        name,
        requests: w.requests,
        errors: session?.err ?? 0,
        cost: w.cost,
        latencyMs: session?.lat_p95_ms ?? session?.lat_avg_ms ?? 0,
        tokens: w.tokIn + w.tokOut,
      };
    });
  }, [roll, state]);

  const sessionP95 = state?.latency_ms.p95 ?? 0;

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 md:grid-cols-4 [&>*]:border-0">
        {booting ? (
          Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-[86px]" />)
        ) : (
          <>
            <KpiCard label="Session p50" value={fmtMs(state?.latency_ms.p50)} sub="live process counters" />
            <KpiCard label="Session p95" value={fmtMs(sessionP95)} sub="live process counters" accent={sessionP95 > 5000 ? 'text-destructive' : 'text-chart-2'} />
            <KpiCard label="Session p99" value={fmtMs(state?.latency_ms.p99)} sub="live process counters" />
            <KpiCard label="Window requests" value={fmtInt(roll.requests)} sub={`${range.label} · ${Object.keys(roll.models).length} models`} />
          </>
        )}
      </div>

      <p className="border-l-2 border-chart-2/40 bg-edge-950/50 px-4 py-3 text-[11px] leading-relaxed text-edge-400">
        Snapshot-reported percentile trends are retained as historical context only. These are not exact
        percentiles of the selected window and are never averaged into a window percentile.
        Durable filtered distributions appear in Analytics above.
      </p>

      <PanelCard
        title="Latency trends"
        description={`Snapshot-reported p50 / p95 / p99 + TTFT average · ${range.label} (legacy coverage)`}
      >
        <LatencyAreaChart series={series} range={range} loading={booting} />
      </PanelCard>

      <PanelCard
        title="Model comparison / top five by requests"
        description="Normalized footprint · window request / cost / token values with session error / latency counters"
      >
        {booting ? (
          <Skeleton className="h-[260px]" />
        ) : (
          <>
            <ModelRadar models={footprints} />
            {footprints.length > 0 && (
              <div className="mt-5 border-t border-edge-700 pt-4">
                <Table className="text-[11px] [&_th]:text-[10px] [&_th]:uppercase [&_th]:tracking-wider [&_td]:py-3">
                  <TableHeader>
                    <TableRow>
                      <TableHead>Model</TableHead>
                      <TableHead className="text-right">Req</TableHead>
                      <TableHead className="text-right">Err (session)</TableHead>
                      <TableHead className="text-right">Known cost</TableHead>
                      <TableHead className="text-right">p95 (session)</TableHead>
                      <TableHead className="text-right">Tokens</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {footprints.map((m) => (
                      <TableRow key={m.name}>
                        <TableCell className="max-w-[220px] truncate font-mono text-xs text-edge-100" title={m.name}>
                          {shortName(m.name)}
                        </TableCell>
                        <TableCell className="text-right font-mono">{fmtInt(m.requests)}</TableCell>
                        <TableCell className="text-right font-mono text-destructive">{m.errors ? fmtInt(m.errors) : '0'}</TableCell>
                        <TableCell className="text-right font-mono text-chart-4">{fmtMoney(m.cost)}</TableCell>
                        <TableCell className="text-right font-mono">{m.latencyMs ? fmtMs(m.latencyMs) : '—'}</TableCell>
                        <TableCell className="text-right font-mono">{fmtTokens(m.tokens)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
                <p className="mt-2 text-[11px] text-muted-foreground">
                  Error and p95 values are session counters since this process started; request, cost and
                  token values follow the selected window. Latency percentiles are histogram-bucket estimates.
                </p>
              </div>
            )}
          </>
        )}
      </PanelCard>
    </div>
  );
}
