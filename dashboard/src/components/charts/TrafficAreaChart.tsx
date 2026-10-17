// Traffic + spend over the selected series window (Overview).
// Real snapshot data: requests per tick and known cost per tick.

import { useMemo } from 'react';
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from 'recharts';
import { fmtInt, fmtMoney, type RangePreset, type SeriesPoint } from '../../api';
import { fmtAxis } from '../../window';
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent } from '../ui/chart';
import { ChartEmptyState, ChartSkeleton, GRID_STROKE, PALETTE, TICK } from './shared';

export function TrafficAreaChart({ series, range, loading }: {
  series: SeriesPoint[];
  range: RangePreset;
  loading?: boolean;
}) {
  const data = useMemo(() => series.map((p) => ({
    t: fmtAxis(p.ts, range),
    requests: p.d_requests || 0,
    cost: p.d_cost || 0,
  })), [series, range]);

  if (loading) return <ChartSkeleton height={320} />;
  if (!data.length) {
    return <ChartEmptyState title="No traffic in this window" description="No gateway snapshots cover the selected chart range." />;
  }

  return (
    <ChartContainer
      config={{
        requests: { label: 'Requests', color: PALETTE.requests },
        cost: { label: 'Known cost', color: PALETTE.cost },
      }}
      className="h-[280px] w-full sm:h-[340px]"
    >
      <AreaChart data={data} margin={{ top: 16, right: 0, left: 0, bottom: 4 }}>
        <defs>
          <linearGradient id="traffic-requests" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor={PALETTE.requests} stopOpacity={0.24} /><stop offset="100%" stopColor={PALETTE.requests} stopOpacity={0.01} /></linearGradient>
          <linearGradient id="traffic-cost" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor={PALETTE.cost} stopOpacity={0.12} /><stop offset="100%" stopColor={PALETTE.cost} stopOpacity={0} /></linearGradient>
        </defs>
        <CartesianGrid stroke={GRID_STROKE} strokeDasharray="3 6" vertical={false} />
        <XAxis dataKey="t" tick={TICK} tickLine={false} axisLine={false} tickMargin={8} minTickGap={48} />
        <YAxis yAxisId="req" tick={TICK} tickLine={false} axisLine={false} width={44} allowDecimals={false} />
        <YAxis
          yAxisId="cost"
          orientation="right"
          tick={TICK}
          tickLine={false}
          axisLine={false}
          width={56}
          tickFormatter={(v: number) => fmtMoney(Number(v))}
        />
        <ChartTooltip
          content={
            <ChartTooltipContent
              labelFormatter={(_, payload) => payload?.[0]?.payload?.t ?? ''}
              formatter={(value, name) => (
                <span className="font-mono">
                  {name === 'cost' ? fmtMoney(Number(value)) : `${fmtInt(Number(value))} req`}
                </span>
              )}
            />
          }
        />
        <Area yAxisId="req" type="monotone" dataKey="requests" name="requests" stroke="var(--color-requests)" fill="url(#traffic-requests)" dot={false} strokeWidth={2} />
        <Area yAxisId="cost" type="monotone" dataKey="cost" name="cost" stroke="var(--color-cost)" fill="url(#traffic-cost)" dot={false} strokeWidth={1.5} strokeDasharray="5 3" />
        <ChartLegend content={<ChartLegendContent />} />
      </AreaChart>
    </ChartContainer>
  );
}
