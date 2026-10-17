import { useMemo } from 'react';
import { ArrowUpRight, Bell, Radio } from 'lucide-react';
import { fmtDateTime, type TabProps } from '../api';
import { rangeSince } from '../window';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Skeleton } from '../components/ui/skeleton';

const METRIC_HELP: Record<string, string> = {
  error_rate: 'share of requests that failed (%), over window',
  failover_rate: 'share of requests that needed a retry (%)',
  cost_per_hour: 'estimated spend rate ($/h)',
  rps: 'requests per second',
  tokens_per_hour: 'token throughput (tok/h)',
  latency_p95: 'p95 latency of the window (ms)',
  ttft_avg: 'average time-to-first-token (ms)',
  ttft_p95: 'p95 time-to-first-token of recorded streams (ms)',
  keys_in_cooldown: 'count of keys currently in cooldown',
  telemetry_errors: 'cumulative write errors and dropped telemetry records this process',
  telemetry_queued: 'telemetry records waiting to be written',
};

const severityColor: Record<string, string> = {
  info: 'border-chart-2/40 bg-chart-2/5 text-chart-2',
  warning: 'border-chart-4/40 bg-chart-4/5 text-chart-4',
  critical: 'border-destructive/40 bg-destructive/5 text-destructive',
};

export default function Alerts({ state, liveAlerts, range }: TabProps) {
  const rules = state?.alert_rules || [];
  const events = useMemo(() => {
    const since = rangeSince(range);
    return [...liveAlerts].filter((a) => a.ts >= since).sort((a, b) => b.ts - a.ts);
  }, [liveAlerts, range]);

  const lastByRule = useMemo(() => {
    const latest: Record<string, { ts: number; value: number }> = {};
    for (const event of events) {
      if (!latest[event.rule_id] || event.ts > latest[event.rule_id].ts) {
        latest[event.rule_id] = { ts: event.ts, value: event.value };
      }
    }
    return latest;
  }, [events]);

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 border border-edge-700 bg-edge-900 lg:grid-cols-4">
        {[
          { label: 'Configured rules', value: state === null ? '—' : rules.length, tone: 'text-primary', detail: 'Persisted gateway configuration' },
          { label: 'Recent triggers', value: events.length, tone: 'text-foreground', detail: range.label },
          { label: 'Critical triggers', value: events.filter((event) => event.severity === 'critical').length, tone: 'text-destructive', detail: 'In selected window' },
          { label: 'Warning triggers', value: events.filter((event) => event.severity === 'warning').length, tone: 'text-chart-4', detail: 'In selected window' },
        ].map((metric) => (
          <div key={metric.label} className="min-w-0 border-b border-r border-edge-700 p-5 last:border-r-0 lg:border-b-0">
            <p className="text-[10px] tracking-[0.14em] text-muted-foreground uppercase">{metric.label}</p>
            <p className={`mt-3 text-3xl tracking-tight tabular-nums ${metric.tone}`}>{metric.value}</p>
            <p className="mt-2 text-[10px] text-muted-foreground">{metric.detail}</p>
          </div>
        ))}
      </div>

      <div className="grid grid-cols-1 items-start gap-5 xl:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)]">
        <Card className="gap-0 overflow-hidden border-edge-700 bg-edge-900">
          <CardHeader className="border-b border-edge-700 py-5">
            <CardTitle className="flex items-center gap-2 text-xs tracking-[0.12em] uppercase"><Bell className="size-4 text-primary" />01 / Rule registry</CardTitle>
            <CardDescription>Thresholds and evaluation windows, as configured.</CardDescription>
            <CardAction>
              <Button variant="outline" size="sm" nativeButton={false} render={<a href="?tab=settings" />}>Configure<ArrowUpRight /></Button>
            </CardAction>
          </CardHeader>
          <CardContent className="px-0 py-0">
            {state === null ? (
              <div className="space-y-3 p-5">
                <Skeleton className="h-28 w-full" />
                <Skeleton className="h-28 w-full" />
                <Skeleton className="h-28 w-full" />
              </div>
            ) : rules.length === 0 ? (
              <Empty className="py-14">
                <EmptyHeader>
                  <EmptyTitle>No rules configured</EmptyTitle>
                  <EmptyDescription>Add alert rules in Settings to monitor gateway thresholds.</EmptyDescription>
                </EmptyHeader>
              </Empty>
            ) : (
              <div className="divide-y divide-edge-700">
                {rules.map((rule) => {
                  const last = lastByRule[rule.id];
                  return (
                    <article key={rule.id} className="space-y-4 p-5">
                      <div className="flex flex-wrap items-start justify-between gap-3">
                        <div className="min-w-0">
                          <p className="text-sm break-all text-foreground">{rule.id}</p>
                          <p className="mt-1 text-[11px] leading-relaxed text-muted-foreground">{METRIC_HELP[rule.metric] || rule.metric}</p>
                        </div>
                        <Badge variant="outline" className={severityColor[rule.severity || 'warning'] || ''}>{rule.severity || 'warning'}</Badge>
                      </div>
                      <dl className="grid grid-cols-3 border border-edge-700 bg-edge-950 text-xs">
                        <div className="min-w-0 border-r border-edge-700 p-3"><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Threshold</dt><dd className="mt-2 text-chart-2">≥ {rule.threshold}</dd></div>
                        <div className="min-w-0 border-r border-edge-700 p-3"><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Window</dt><dd className="mt-2">{rule.window_sec ? `${rule.window_sec}s` : 'Latest tick'}</dd></div>
                        <div className="min-w-0 p-3"><dt className="text-[9px] tracking-widest text-muted-foreground uppercase">Cooldown</dt><dd className="mt-2">{rule.cooldown_sec > 0 ? `${rule.cooldown_sec}s` : 'Default'}</dd></div>
                      </dl>
                      <div className="flex flex-wrap items-center justify-between gap-2 text-[10px]">
                        <Badge variant="outline" className="max-w-full break-all whitespace-normal">{rule.scope || 'global'}</Badge>
                        {last ? (
                          <span className="text-chart-4">Last {fmtDateTime(last.ts)} · {(last.value ?? 0).toFixed(2)}</span>
                        ) : (
                          <span className="text-muted-foreground">No triggers in this window</span>
                        )}
                      </div>
                    </article>
                  );
                })}
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="gap-0 overflow-hidden border-edge-700 bg-edge-900">
          <CardHeader className="border-b border-edge-700 py-5">
            <CardTitle className="flex items-center gap-2 text-xs tracking-[0.12em] uppercase"><Radio className="size-4 text-chart-2" />02 / Trigger stream</CardTitle>
            <CardDescription>Saved history merged with live events · {range.label}</CardDescription>
            <CardAction><Badge variant="outline">{events.length} events</Badge></CardAction>
          </CardHeader>
          <CardContent className="px-0 py-0">
            <div className="max-h-[800px] overflow-y-auto">
              {events.length === 0 ? (
                <Empty className="py-20">
                  <EmptyHeader>
                    <EmptyTitle>No recorded triggers</EmptyTitle>
                    <EmptyDescription>No triggers in the {range.label} window. This is a bounded event history, not a health guarantee.</EmptyDescription>
                  </EmptyHeader>
                </Empty>
              ) : events.map((event, index) => (
                <article key={`${event.ts}-${event.rule_id}-${index}`} className="border-b border-edge-700 p-5 last:border-0">
                  <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
                    <Badge variant="outline" className={severityColor[event.severity] || ''}>{event.severity}</Badge>
                    <time className="text-[10px] text-muted-foreground">{fmtDateTime(event.ts)}</time>
                  </div>
                  <p className="text-xs leading-relaxed break-words text-foreground">{event.message}</p>
                  <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-[10px]">
                    <span className="break-all text-muted-foreground">{event.rule_id}</span>
                    <span className="border border-edge-700 bg-edge-950 px-2 py-1 text-chart-2">{(event.value ?? 0).toFixed(2)} ≥ {event.threshold ?? 0}</span>
                  </div>
                </article>
              ))}
            </div>
            <p className="border-t border-edge-700 bg-edge-950/60 p-4 text-[10px] leading-relaxed text-muted-foreground">History is bounded to 200 events; it is not an exhaustive audit export. Spend thresholds notify but do not enforce a billing budget.</p>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
