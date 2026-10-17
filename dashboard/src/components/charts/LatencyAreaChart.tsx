// Snapshot-reported latency trends (Latency tab).
// Real series percentiles + TTFT. These are legacy snapshot coverage: historical
// context only, never averaged into a window percentile.

import { useMemo } from 'react';
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from 'recharts';
import { fmtMs, type RangePreset, type SeriesPoint } from '../../api';
import { fmtAxis } from '../../window';
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent } from '../ui/chart';
import { ChartEmptyState, ChartSkeleton, GRID_STROKE, PALETTE, TICK } from './shared';

export function LatencyAreaChart({ series, range, loading }: {
  series: SeriesPoint[];
  range: RangePreset;
  loading?: boolean;
}) {
  const data = useMemo(() => {
    const out: { t: string; p50: number; p95: number; p99: number; ttft: number }[] = [];
    for (const p of series) {
      if (!p.lat_p95_ms && !p.lat_p50_ms) continue;
      out.push({
        t: fmtAxis(p.ts, range),
        p50: p.lat_p50_ms ?? 0,
        p95: p.lat_p95_ms ?? 0,
        p99: p.lat_p99_ms ?? 0,
        ttft: p.ttft_avg_ms ?? 0,
      });
    }
    return out;
  }, [series, range]);

  if (loading) return <ChartSkeleton height={260} />;
  if (!data.length) {
    return <ChartEmptyState title="No latency data in this window" description="Snapshot-reported percentiles will appear here once samples cover the range." />;
  }

  return (
    <ChartContainer
      config={{
        p50: { label: 'p50', color: PALETTE.p50 },
        p95: { label: 'p95', color: PALETTE.p95 },
        p99: { label: 'p99', color: PALETTE.p99 },
        ttft: { label: 'TTFT avg', color: PALETTE.ttft },
      }}
      className="h-[280px] w-full sm:h-[320px]"
    >
      <AreaChart data={data} margin={{ top: 16, right: 8, left: 0, bottom: 4 }}>
        <CartesianGrid stroke={GRID_STROKE} strokeDasharray="3 6" vertical={false} />
        <XAxis dataKey="t" tick={TICK} tickLine={false} axisLine={false} tickMargin={8} minTickGap={48} />
        <YAxis tick={TICK} tickLine={false} axisLine={false} width={52} tickFormatter={(v: number) => fmtMs(Number(v))} />
        <ChartTooltip
          content={
            <ChartTooltipContent
              formatter={(value, name) => <span className="font-mono">{fmtMs(Number(value))} · {name}</span>}
            />
          }
        />
        <Area type="monotone" dataKey="p50" name="p50" stroke="var(--color-p50)" fill="var(--color-p50)" fillOpacity={0.08} dot={false} strokeWidth={1.5} />
        <Area type="monotone" dataKey="p95" name="p95" stroke="var(--color-p95)" fill="var(--color-p95)" fillOpacity={0.05} dot={false} strokeWidth={2} />
        <Area type="monotone" dataKey="p99" name="p99" stroke="var(--color-p99)" fill="var(--color-p99)" fillOpacity={0.03} dot={false} strokeWidth={1.5} />
        <Area type="monotone" dataKey="ttft" name="ttft" stroke="var(--color-ttft)" fill="transparent" dot={false} strokeWidth={1.5} strokeDasharray="5 3" />
        <ChartLegend content={<ChartLegendContent />} />
      </AreaChart>
    </ChartContainer>
  );
}
