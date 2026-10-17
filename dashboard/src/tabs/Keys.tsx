import { useMemo, useState } from 'react';
import { ArrowUpRight, KeyRound, Search } from 'lucide-react';
import { fmtInt, fmtMoney, fmtTokens, fmtDateTime, type TabProps, type KeyStat } from '../api';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Checkbox } from '../components/ui/checkbox';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Input } from '../components/ui/input';
import { Label } from '../components/ui/label';
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '../components/ui/sheet';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';

function SuccessBar({ pct }: { pct: number }) {
  const fill = pct < 80 ? 'bg-destructive' : pct < 98 ? 'bg-chart-4' : 'bg-primary';
  return <div className="h-1 w-full overflow-hidden bg-edge-700"><div className={`h-full ${fill}`} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} /></div>;
}

function Health({ credential }: { credential: KeyStat }) {
  return (
    <Tooltip>
      <TooltipTrigger className="text-left">
        <span className={`inline-flex items-center gap-2 text-[10px] tracking-widest uppercase ${credential.healthy ? 'text-primary' : 'text-chart-4'}`}>
          <span className={`size-1.5 ${credential.healthy ? 'bg-primary' : 'bg-chart-4'}`} />
          {credential.healthy ? 'Available' : 'Cooldown'}
        </span>
      </TooltipTrigger>
      <TooltipContent>{credential.healthy ? 'Available to the upstream key pool' : `Cooling down · ${credential.cooldown_sec}s · ${credential.fail_count} recent failures`}</TooltipContent>
    </Tooltip>
  );
}

export default function Keys({ state, liveKeyEvents }: TabProps) {
  const [q, setQ] = useState('');
  const [onlyDown, setOnlyDown] = useState(false);
  const [selected, setSelected] = useState<{ backend: string; key: string } | null>(null);

  const rows = useMemo(() => {
    if (!state) return [];
    const out: { backend: string; k: KeyStat }[] = [];
    for (const [backend, keys] of Object.entries(state.keys)) {
      for (const k of keys) out.push({ backend, k });
    }
    return out.filter(({ backend, k }) => {
      if (onlyDown && k.healthy) return false;
      return `${backend} ${k.key} ${k.healthy ? 'up available' : 'down cooldown'}`.toLowerCase().includes(q.trim().toLowerCase());
    }).sort((a, b) => Number(a.k.healthy) - Number(b.k.healthy) || b.k.cost - a.k.cost);
  }, [state, q, onlyDown]);

  const totalCost = useMemo(() => rows.reduce((sum, row) => sum + row.k.cost, 0), [rows]);
  const downCount = useMemo(() => rows.filter((row) => !row.k.healthy).length, [rows]);
  const selectedKey = selected ? state?.keys[selected.backend]?.find((key) => key.key === selected.key) : undefined;
  const selectedEvents = selected ? liveKeyEvents.filter((event) => event.backend === selected.backend && event.key === selected.key) : [];

  if (!state) {
    return (
      <div className="space-y-5">
        <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 lg:grid-cols-4">
          {[0, 1, 2, 3].map((index) => <Skeleton key={index} className="h-32 w-full" />)}
        </div>
        <Card className="border-edge-700 bg-edge-900">
          <CardHeader><CardTitle className="text-xs tracking-widest uppercase">01 / Upstream credentials</CardTitle><CardDescription>Loading upstream credential health…</CardDescription></CardHeader>
          <CardContent className="space-y-3">{[0, 1, 2, 3, 4].map((index) => <Skeleton key={index} className="h-14 w-full" />)}</CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 border border-edge-700 bg-edge-900 lg:grid-cols-4">
        {[
          { label: 'Matched credentials', value: fmtInt(rows.length), tone: '' },
          { label: 'Matched cooldown', value: fmtInt(downCount), tone: downCount > 0 ? 'text-chart-4' : 'text-primary' },
          { label: 'Matched session cost', value: fmtMoney(totalCost), tone: 'text-chart-4' },
          { label: 'Recent pool events', value: fmtInt(liveKeyEvents.length), tone: 'text-chart-2' },
        ].map((metric) => (
          <div key={metric.label} className="min-w-0 border-b border-r border-edge-700 p-5 last:border-r-0 lg:border-b-0">
            <p className="text-[10px] tracking-[0.14em] text-muted-foreground uppercase">{metric.label}</p>
            <p className={`mt-3 text-3xl tracking-tight tabular-nums ${metric.tone}`}>{metric.value}</p>
          </div>
        ))}
      </div>

      <Card className="gap-0 overflow-hidden border-edge-700 bg-edge-900">
        <CardHeader className="border-b border-edge-700 py-5">
          <CardTitle className="flex items-center gap-2 text-xs tracking-[0.12em] uppercase"><KeyRound className="size-4 text-primary" />01 / Upstream credential pool</CardTitle>
          <CardDescription>Session counters, not the selected time window. Select a credential to inspect its counters and events.</CardDescription>
          <CardAction className="self-center">
            <div className="flex flex-wrap items-center gap-3">
              <Label className="flex cursor-pointer items-center gap-2 text-[11px] font-normal text-muted-foreground">
                <Checkbox checked={onlyDown} onCheckedChange={(value) => setOnlyDown(value === true)} />Cooldown only
              </Label>
              <div className="relative">
                <Search className="pointer-events-none absolute top-1/2 left-3 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input aria-label="Filter upstream credentials" value={q} onChange={(event) => setQ(event.target.value)} placeholder="Backend or key…" className="w-full pl-9 sm:w-52" />
              </div>
            </div>
          </CardAction>
        </CardHeader>
        <CardContent className="px-0 py-0">
          <Table className="min-w-[1120px]">
            <TableHeader><TableRow>
              <TableHead className="pl-5">Provider / credential</TableHead><TableHead>Pool state</TableHead><TableHead>Requests</TableHead><TableHead>OK</TableHead><TableHead>Errors</TableHead><TableHead>Failover</TableHead><TableHead>Success</TableHead><TableHead>In</TableHead><TableHead>Out</TableHead><TableHead>Cost</TableHead><TableHead>Cooldown</TableHead>
            </TableRow></TableHeader>
            <TableBody>
              {rows.length === 0 && <TableRow><TableCell colSpan={11}><Empty className="py-14"><EmptyHeader><EmptyTitle>No credentials match</EmptyTitle><EmptyDescription>Adjust the search or clear “Cooldown only” to see the full pool.</EmptyDescription></EmptyHeader></Empty></TableCell></TableRow>}
              {rows.map(({ backend, k }) => {
                const success = k.requests > 0 ? (k.ok / k.requests) * 100 : null;
                return (
                  <TableRow key={backend + k.key} className="hover:bg-edge-800/60">
                    <TableCell className="py-4 pl-5">
                      <Button variant="ghost" className="h-auto max-w-64 justify-start px-0 text-left" onClick={() => setSelected({ backend, key: k.key })}>
                        <span className="min-w-0"><span className="block text-[10px] text-chart-2">{backend}</span><span className="mt-1 block truncate text-xs">{k.key}</span></span><ArrowUpRight className="ml-2 size-3 text-muted-foreground" />
                      </Button>
                    </TableCell>
                    <TableCell><Health credential={k} /></TableCell>
                    <TableCell className="tabular-nums">{fmtInt(k.requests)}</TableCell>
                    <TableCell className="text-primary tabular-nums">{fmtInt(k.ok)}</TableCell>
                    <TableCell className={`tabular-nums ${k.err > 0 ? 'text-destructive' : 'text-muted-foreground'}`}>{fmtInt(k.err)}</TableCell>
                    <TableCell className={`tabular-nums ${k.failover > 0 ? 'text-chart-4' : 'text-muted-foreground'}`}>{fmtInt(k.failover)}</TableCell>
                    <TableCell className="min-w-28">
                      {success === null ? <span className="text-[10px] text-muted-foreground">No requests</span> : <div className="space-y-2"><span className="text-[11px] tabular-nums">{success.toFixed(0)}%</span><SuccessBar pct={success} /></div>}
                    </TableCell>
                    <TableCell className="tabular-nums">{fmtTokens(k.tok_in)}</TableCell><TableCell className="tabular-nums">{fmtTokens(k.tok_out)}</TableCell>
                    <TableCell className="text-chart-4 tabular-nums">{fmtMoney(k.cost)}</TableCell>
                    <TableCell className="pr-5 text-chart-4 tabular-nums">{k.healthy ? '—' : `${k.cooldown_sec}s`}</TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
          <p className="border-t border-edge-700 bg-edge-950/60 px-5 py-3 text-[10px] leading-relaxed text-muted-foreground">Cooldown is temporary. Attribution history is retained. Upstream pool credentials are separate from application API keys.</p>
        </CardContent>
      </Card>

      <Card className="gap-0 overflow-hidden border-edge-700 bg-edge-900">
        <CardHeader className="border-b border-edge-700 py-5"><CardTitle className="text-xs tracking-[0.12em] uppercase">02 / Pool event stream</CardTitle><CardDescription>Live cooldown, recovery, and failure attributions this session. Showing up to 60 recent events.</CardDescription></CardHeader>
        <CardContent className="px-0 py-0">
          <div className="max-h-96 overflow-y-auto">
            {liveKeyEvents.length === 0 ? <Empty className="py-12"><EmptyHeader><EmptyTitle>No pool events received</EmptyTitle><EmptyDescription>Credential state changes appear here as they arrive.</EmptyDescription></EmptyHeader></Empty> : liveKeyEvents.slice(0, 60).map((event, index) => (
              <div key={index} className="grid gap-3 border-b border-edge-700 px-5 py-4 text-[11px] last:border-0 md:grid-cols-[160px_90px_minmax(0,1fr)]">
                <time className="text-[10px] text-muted-foreground">{fmtDateTime(event.ts)}</time>
                <div><Badge variant="outline" className={event.event === 'recover' ? 'border-primary/40 text-primary' : event.event === 'cooldown' ? 'border-chart-4/40 text-chart-4' : 'border-destructive/40 text-destructive'}>{event.event}</Badge></div>
                <div className="min-w-0"><span className="text-chart-2">{event.backend}</span><span className="ml-3 break-all text-muted-foreground">{event.key}</span>{event.msg && <p className="mt-2 break-words text-muted-foreground">{event.msg}</p>}</div>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>

      <Sheet open={selected !== null} onOpenChange={(open) => { if (!open) setSelected(null); }}>
        <SheetContent className="w-full overflow-y-auto border-edge-700 bg-edge-900 sm:max-w-xl">
          <SheetHeader className="border-b border-edge-700 p-6"><p className="text-[10px] tracking-[0.16em] text-primary uppercase">Upstream credential / {selected?.backend}</p><SheetTitle className="break-all">{selected?.key}</SheetTitle><SheetDescription>Current pool health and process-session counters. No secret material is exposed.</SheetDescription></SheetHeader>
          <div className="space-y-6 px-6 pb-6">
            {selectedKey ? <>
              <div className="flex flex-wrap items-center justify-between gap-3 border border-edge-700 bg-edge-950 p-4"><Health credential={selectedKey} /><span className="text-[11px] text-muted-foreground">{selectedKey.fail_count} recent failures</span></div>
              <dl className="grid grid-cols-2 border-t border-l border-edge-700">
                {[
                  ['Requests', fmtInt(selectedKey.requests)], ['Successful', fmtInt(selectedKey.ok)], ['Errors', fmtInt(selectedKey.err)], ['Failovers', fmtInt(selectedKey.failover)],
                  ['Input tokens', fmtTokens(selectedKey.tok_in)], ['Output tokens', fmtTokens(selectedKey.tok_out)], ['Cached tokens', fmtTokens(selectedKey.tok_cache)], ['Session cost', fmtMoney(selectedKey.cost)],
                  ['Cooldown', selectedKey.healthy ? 'None' : `${selectedKey.cooldown_sec}s`], ['Success', selectedKey.requests > 0 ? `${((selectedKey.ok / selectedKey.requests) * 100).toFixed(1)}%` : 'No requests'],
                ].map(([label, value]) => <div key={label} className="min-w-0 border-r border-b border-edge-700 bg-edge-950/40 p-4"><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">{label}</dt><dd className="mt-2 text-lg text-foreground tabular-nums">{value}</dd></div>)}
              </dl>
              <section><h3 className="mb-3 text-[10px] tracking-[0.14em] text-muted-foreground uppercase">Recent attribution / {selectedEvents.length} events</h3>
                {selectedEvents.length === 0 ? <p className="border border-edge-700 p-4 text-xs text-muted-foreground">No recent events received for this credential.</p> : selectedEvents.slice(0, 30).map((event, index) => <div key={index} className="space-y-2 border-b border-edge-700 py-4 text-xs"><div className="flex items-center justify-between gap-3"><Badge variant="outline">{event.event}</Badge><time className="text-[10px] text-muted-foreground">{fmtDateTime(event.ts)}</time></div>{event.msg && <p className="break-words text-muted-foreground">{event.msg}</p>}</div>)}
              </section>
            </> : <Empty><EmptyHeader><EmptyTitle>Credential no longer in pool</EmptyTitle><EmptyDescription>The current gateway snapshot no longer contains this credential.</EmptyDescription></EmptyHeader></Empty>}
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
