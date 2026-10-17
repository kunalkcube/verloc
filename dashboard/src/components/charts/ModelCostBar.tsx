// Per-model known cost (Cost tab) with a metric toggle.
// Real window rollup: bars are priced usage only; unknown cost never renders as zero.

import { useMemo } from 'react';
import { Bar, BarChart, CartesianGrid, Cell, XAxis, YAxis } from 'recharts';
import { fmtInt, fmtMoney, fmtTokens } from '../../api';
import { ChartContainer, ChartTooltip, ChartTooltipContent } from '../ui/chart';
import { ChartEmptyState, ChartSkeleton, GRID_STROKE, PALETTE, shortName, TICK } from './shared';

export type CostBarMetric = 'cost' | 'requests';

export interface ModelCostRow {
  name: string;
  cost: number;
  requests: number;
  tokens: number;
}

const MAX_BARS = 10;

export function ModelCostBar({ rows, metric, loading }: {
  rows: ModelCostRow[];
  metric: CostBarMetric;
  loading?: boolean;
}) {
  const { bars, shown, total } = useMemo(() => {
    const ranked = [...rows].sort((a, b) => (metric === 'cost' ? b.cost - a.cost : b.requests - a.requests));
    const total = ranked.reduce((s, r) => s + (metric === 'cost' ? r.cost : r.requests), 0);
    const bars = ranked.slice(0, MAX_BARS);
    return { bars, shown: bars.reduce((s, r) => s + (metric === 'cost' ? r.cost : r.requests), 0), total };
  }, [rows, metric]);


  if (loading) return <ChartSkeleton height={260} />;
  if (!bars.length || total <= 0) {
    return (
      <ChartEmptyState
        title={metric === 'cost' ? 'No known cost in this window' : 'No requests in this window'}
        description={metric === 'cost' ? 'Models with priced usage will appear here. Unknown cost is excluded, never zero.' : undefined}
      />
    );
  }

  return (
    <div>
      <ChartContainer
        config={{ value: { label: metric === 'cost' ? 'Known cost' : 'Requests', color: metric === 'cost' ? PALETTE.cost : PALETTE.requests } }}
        className="w-full"
        style={{ height: Math.max(200, bars.length * 38 + 35) }}
      >
        <BarChart layout="vertical" data={bars.map((r) => ({ name: shortName(r.name), full: r.name, value: metric === 'cost' ? r.cost : r.requests, cost: r.cost, requests: r.requests, tokens: r.tokens }))} margin={{ top: 4, right: 12, left: 0, bottom: 4 }} barSize={16}>
          <CartesianGrid stroke={GRID_STROKE} strokeDasharray="3 6" horizontal={false} />
          <YAxis type="category" dataKey="name" tick={TICK} tickFormatter={(value: string) => value.length > 22 ? `${value.slice(0, 20)}…` : value} tickLine={false} axisLine={false} width={130} />
          <XAxis type="number" tick={TICK} tickLine={false} axisLine={false} allowDecimals={metric === 'cost'} tickFormatter={(v: number) => metric === 'cost' ? fmtMoney(Number(v)) : fmtInt(Number(v))} />
          <ChartTooltip
            content={
              <ChartTooltipContent
                labelFormatter={(_, payload) => String(payload?.[0]?.payload?.full ?? '')}
                formatter={(_value, _name, item) => {
                  const p = item?.payload as { cost: number; requests: number; tokens: number } | undefined;
                  return (
                    <span className="font-mono">
                      {fmtMoney(Number(p?.cost ?? 0))} · {fmtInt(Number(p?.requests ?? 0))} req · {fmtTokens(Number(p?.tokens ?? 0))}
                    </span>
                  );
                }}
              />
            }
          />
          <Bar dataKey="value" name={metric === 'cost' ? 'Known cost' : 'Requests'} radius={0}>
            {bars.map((r, i) => <Cell key={r.name} fill={metric === 'cost' ? PALETTE.cost : PALETTE.requests} fillOpacity={Math.max(0.4, 1 - i * 0.06)} />)}
          </Bar>
        </BarChart>
      </ChartContainer>
      <p className="mt-4 border-t border-edge-700 pt-3 text-[10px] leading-relaxed text-muted-foreground">
        Top {bars.length} of {rows.length} models · shown{' '}
        <span className="font-mono text-edge-100">
          {metric === 'cost' ? fmtMoney(shown) : `${fmtInt(shown)} req`}
        </span>{' '}
        of{' '}
        <span className="font-mono text-edge-100">
          {metric === 'cost' ? fmtMoney(total) : `${fmtInt(total)} req`}
        </span>{' '}
        {metric === 'cost' ? 'window known cost' : 'window requests'}.
      </p>
    </div>
  );
}
