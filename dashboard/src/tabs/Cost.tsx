import { useMemo, useState } from 'react';
import { fmtInt, fmtMoney, fmtTokens, type TabProps } from '../api';
import { rollupSeries, fmtSpan } from '../window';
import { Badge } from '../components/ui/badge';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/ui/select';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';
import { ModelCostBar, type CostBarMetric } from '../components/charts/ModelCostBar';
import { KpiCard, PanelCard } from '../components/charts/shared';

export default function Cost({ state, series, range }: TabProps) {
  const [metric, setMetric] = useState<CostBarMetric>('cost');
  const roll = useMemo(() => rollupSeries(series), [series]);
  const booting = !state;

  const modelRows = useMemo(() => Object.entries(roll.models).map(([name, m]) => ({
    name, cost: m.cost, requests: m.requests, tokens: m.tokIn + m.tokOut,
  })), [roll]);

  const providerRows = useMemo(() => Object.entries(roll.backends)
    .map(([name, m]) => ({ name, cost: m.cost, req: m.requests, tokIn: m.tokIn, tokOut: m.tokOut, err: m.err }))
    .sort((a, b) => b.cost - a.cost || b.req - a.req), [roll]);

  const sessionCost = useMemo(() => {
    if (!state) return 0;
    return Object.values(state.models).reduce((s, m) => s + (m.cost || 0), 0);
  }, [state]);

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 md:grid-cols-4 [&>*]:border-0">
        {booting ? (
          Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-[86px]" />)
        ) : (
          <>
            <KpiCard label="Window known cost" value={fmtMoney(roll.cost)} sub={`${range.label} · ${fmtSpan(roll.from, roll.to)}`} accent="text-chart-4" />
            <KpiCard label="Window requests" value={fmtInt(roll.requests)} sub={`${fmtTokens(roll.tokIn + roll.tokOut)} tokens`} />
            <KpiCard label="Lifetime cost" value={fmtMoney(state?.lifetime?.cost ?? 0)} sub="persisted in SQLite" />
            <KpiCard label="Session cost" value={fmtMoney(sessionCost)} sub="since this process started" />
          </>
        )}
      </div>

      <div className="grid items-start gap-5 2xl:grid-cols-2">
      <PanelCard
        title="Known cost by model"
        description="Priced usage only — requests with unknown cost are excluded, never counted as zero"
        className="h-full"
        action={
          <div className="flex items-center gap-2">
            <Tooltip>
              <TooltipTrigger>
                <span className="cursor-help"><Badge variant="outline" className="border-chart-4/40 text-chart-4">Known</Badge></span>
              </TooltipTrigger>
              <TooltipContent>
                Totals exclude missing usage and missing pricing. Unknown cost is not free usage.
              </TooltipContent>
            </Tooltip>
            <Select value={metric} onValueChange={(v) => setMetric(v as CostBarMetric)}>
              <SelectTrigger size="sm" aria-label="Bar metric">
                <SelectValue>{metric === 'cost' ? 'Known cost' : 'Requests'}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="cost">Known cost</SelectItem>
                <SelectItem value="requests">Requests</SelectItem>
              </SelectContent>
            </Select>
          </div>
        }
      >
        <ModelCostBar rows={modelRows} metric={metric} loading={booting} />
      </PanelCard>

      <PanelCard
        title="All providers by window cost"
        description={`${providerRows.length} providers · ${range.label} · rows sum to the window totals below`}
      >
        {booting ? (
          <Skeleton className="h-[280px]" />
        ) : (
          <Table className="text-[11px] [&_th]:text-[10px] [&_th]:uppercase [&_th]:tracking-wider [&_td]:py-3">
            <TableHeader>
              <TableRow>
                <TableHead>Provider</TableHead>
                <TableHead className="text-right">Known cost</TableHead>
                <TableHead className="text-right">Req</TableHead>
                <TableHead className="text-right">In</TableHead>
                <TableHead className="text-right">Out</TableHead>
                <TableHead className="text-right">Err</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {providerRows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="py-8 text-center text-xs uppercase tracking-widest text-muted-foreground">
                    No traffic in the {range.label} window
                  </TableCell>
                </TableRow>
              )}
              {providerRows.map((r) => (
                <TableRow key={r.name}>
                  <TableCell className="max-w-[220px] whitespace-normal break-all font-mono text-xs text-edge-100">
                    {r.name || 'Unattributed provider'}
                  </TableCell>
                  <TableCell className="text-right font-mono text-chart-4">{fmtMoney(r.cost)}</TableCell>
                  <TableCell className="text-right font-mono">{fmtInt(r.req)}</TableCell>
                  <TableCell className="text-right font-mono">{fmtTokens(r.tokIn)}</TableCell>
                  <TableCell className="text-right font-mono">{fmtTokens(r.tokOut)}</TableCell>
                  <TableCell className="text-right font-mono text-destructive">{r.err ? fmtInt(r.err) : ''}</TableCell>
                </TableRow>
              ))}
              {providerRows.length > 0 && (
                <TableRow className="border-t border-edge-600 bg-edge-950 font-medium">
                  <TableCell className="text-xs uppercase tracking-widest text-muted-foreground">Window total</TableCell>
                  <TableCell className="text-right font-mono text-chart-4">{fmtMoney(roll.cost)}</TableCell>
                  <TableCell className="text-right font-mono">{fmtInt(roll.requests)}</TableCell>
                  <TableCell className="text-right font-mono">{fmtTokens(roll.tokIn)}</TableCell>
                  <TableCell className="text-right font-mono">{fmtTokens(roll.tokOut)}</TableCell>
                  <TableCell className="text-right font-mono text-destructive">{roll.err ? fmtInt(roll.err) : ''}</TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        )}
      </PanelCard>
      </div>
    </div>
  );
}
