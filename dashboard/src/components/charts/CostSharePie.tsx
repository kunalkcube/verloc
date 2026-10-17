// Known-cost share by provider. The ledger includes every active provider;
// only positive known costs contribute slices to the donut.
import { useMemo } from 'react';
import { Cell, Pie, PieChart } from 'recharts';
import { fmtInt, fmtMoney } from '../../api';
import { ChartContainer, ChartTooltip, ChartTooltipContent } from '../ui/chart';
import { ChartEmptyState, ChartSkeleton, PALETTE } from './shared';

export interface ProviderCostSlice {
  name: string;
  cost: number;
  requests: number;
}

const MAX_SLICES = 6;

export function CostSharePie({ rows, loading }: { rows: ProviderCostSlice[]; loading?: boolean }) {
  const { providers, slices, total, groupedCount } = useMemo(() => {
    const providers = [...rows].filter((row) => row.requests > 0 || row.cost > 0).sort((a, b) => b.cost - a.cost);
    const priced = providers.filter((row) => row.cost > 0);
    const total = priced.reduce((sum, row) => sum + row.cost, 0);
    if (priced.length <= MAX_SLICES) return { providers, slices: priced, total, groupedCount: 0 };
    const tail = priced.slice(MAX_SLICES - 1);
    return {
      providers,
      slices: [
        ...priced.slice(0, MAX_SLICES - 1),
        { name: `${tail.length} other providers`, cost: tail.reduce((sum, row) => sum + row.cost, 0), requests: tail.reduce((sum, row) => sum + row.requests, 0) },
      ],
      total,
      groupedCount: tail.length,
    };
  }, [rows]);

  if (loading) return <ChartSkeleton height={240} />;
  if (!providers.length) {
    return <ChartEmptyState title="No provider activity in this window" description="Provider spend and request details appear here after traffic is recorded." />;
  }

  return (
    <div className="grid min-w-0 grid-cols-1 items-start gap-6 lg:grid-cols-[240px_minmax(0,1fr)]">
      <div className="min-w-0">
        {slices.length > 0 ? (
          <div className="relative">
            <ChartContainer config={{ cost: { label: 'Known spend', color: PALETTE.cost } }} className="mx-auto h-[240px] w-full max-w-[280px]">
              <PieChart>
                <ChartTooltip
                  content={
                    <ChartTooltipContent
                      hideLabel
                      className="w-56 max-w-[calc(100vw-4rem)]"
                      formatter={(value, name, item) => (
                        <div className="grid w-full min-w-0 gap-3">
                          <div className="break-all font-medium text-foreground">{name}</div>
                          <dl className="grid gap-2 text-[11px]">
                            <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Known spend</dt><dd className="tabular-nums">{fmtMoney(Number(value))}</dd></div>
                            <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Share</dt><dd className="tabular-nums">{((Number(value) / total) * 100).toFixed(1)}%</dd></div>
                            <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Requests</dt><dd className="tabular-nums">{fmtInt(item.payload.requests)}</dd></div>
                          </dl>
                        </div>
                      )}
                    />
                  }
                />
                <Pie data={slices} dataKey="cost" nameKey="name" innerRadius={66} outerRadius={94} paddingAngle={slices.length > 1 ? 3 : 0} stroke="var(--color-edge-900)" strokeWidth={2}>
                  {slices.map((slice, index) => <Cell key={slice.name} fill={PALETTE.providers[index]} />)}
                </Pie>
              </PieChart>
            </ChartContainer>
            <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-1">
              <span className="text-lg font-medium tracking-tight text-edge-100 tabular-nums">{fmtMoney(total)}</span>
              <span className="text-[9px] uppercase tracking-[.12em] text-edge-400">Known spend</span>
            </div>
          </div>
        ) : (
          <ChartEmptyState title="No known spend" description="Requests are listed alongside. Missing prices or usage do not mean free traffic." />
        )}
        {groupedCount > 0 && <p className="mt-2 text-[10px] leading-relaxed text-edge-400">The donut combines {groupedCount} providers into one slice. Every provider is listed individually.</p>}
      </div>
      <ul aria-label="Provider allocation details" className="min-w-0 divide-y divide-edge-700 border-y border-edge-700">
        {providers.map((provider, index) => (
          <li key={provider.name} className="min-w-0 py-4">
            <div className="mb-3 flex min-w-0 items-start gap-2.5">
              <span className="mt-1 size-2 shrink-0" style={{ background: provider.cost > 0 ? PALETTE.providers[Math.min(index, MAX_SLICES - 1)] : 'var(--color-edge-600)' }} aria-hidden="true" />
              <span className="min-w-0 break-all text-[12px] leading-relaxed text-edge-100">{provider.name}</span>
            </div>
            <dl className="grid grid-cols-3 gap-3 pl-[18px]">
              <div className="min-w-0"><dt className="text-[9px] uppercase tracking-[.08em] text-edge-400">Known spend</dt><dd className="mt-1 break-all text-[12px] text-edge-200 tabular-nums">{fmtMoney(provider.cost)}</dd></div>
              <div className="min-w-0"><dt className="text-[9px] uppercase tracking-[.08em] text-edge-400">Share</dt><dd className="mt-1 text-[12px] text-edge-200 tabular-nums">{total > 0 ? `${((provider.cost / total) * 100).toFixed(1)}%` : '—'}</dd></div>
              <div className="min-w-0"><dt className="text-[9px] uppercase tracking-[.08em] text-edge-400">Requests</dt><dd className="mt-1 text-[12px] text-edge-200 tabular-nums">{fmtInt(provider.requests)}</dd></div>
            </dl>
          </li>
        ))}
      </ul>
      <p className="border-t border-edge-700 pt-3 text-[10px] leading-relaxed text-muted-foreground lg:col-span-2">
        Share is based on the window's known spend. Request counts include all recorded outcomes; unknown costs are excluded from spend, not treated as free traffic.
      </p>
    </div>
  );
}
