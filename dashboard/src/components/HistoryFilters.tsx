import { useEffect, useMemo, useState } from 'react';
import { RotateCcw, Search } from 'lucide-react';
import { listAdminModels, listAdminProviders, listClientKeys, type AdminModel, type AdminProvider, type ClientKey, type RangePreset } from '../api';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';

export interface HistoryFilter { model: string; backend: string; client_key_id: string; status: string; q: string; since: string; until: string }
function readHistoryFilters(): HistoryFilter {
  const params = new URLSearchParams(location.search);
  const filters = { model: params.get('model') || '', backend: params.get('backend') || '', client_key_id: params.get('client_key_id') || '', status: params.get('status') || '', q: params.get('q') || '', since: params.get('since') || '', until: params.get('until') || '' };
  for (const key of ['since', 'until'] as const) {
    if (!/^\d+$/.test(filters[key])) continue;
    const date = new Date(Number(filters[key]));
    if (Number.isFinite(date.getTime())) filters[key] = new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  }
  return filters;
}
export function useHistoryFilters(range: RangePreset) {
  const [filters, setFilters] = useState<HistoryFilter>(readHistoryFilters);
  const [anchor, setAnchor] = useState(Date.now);
  useEffect(() => {
    const update = () => setFilters(readHistoryFilters());
    window.addEventListener('popstate', update);
    return () => window.removeEventListener('popstate', update);
  }, []);
  useEffect(() => {
    const url = new URL(location.href);
    for (const [key, value] of Object.entries(filters)) { if (value) url.searchParams.set(key, value); else url.searchParams.delete(key); }
    history.replaceState(null, '', url);
  }, [filters]);
  const params = useMemo(() => {
    const query = new URLSearchParams();
    for (const [key, value] of Object.entries(filters)) {
      if (!value) continue;
      if (key === 'since' || key === 'until') { const ts = new Date(value).getTime(); if (Number.isFinite(ts)) query.set(key, String(ts)); }
      else query.set(key, value);
    }
    if (!filters.since && range.ms !== null) query.set('since', String(Date.now() - range.ms));
    return query;
  }, [filters, range, anchor]);
  return { filters, setFilters, params, refreshWindow: () => setAnchor(Date.now()) };
}

function FilterSelectField({ id, label, value, placeholder, options, onChange }: {
  id: string; label: string; value: string; placeholder: string;
  options: { value: string; label: string }[]; onChange: (v: string) => void;
}) {
  const ALL = '__all__';
  const known: Record<string, true> = {};
  for (const o of options) known[o.value] = true;
  const extra = value && !known[value] ? [{ value, label: value }] : [];
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id} className="text-[10px] uppercase tracking-[.12em] text-edge-400">{label}</Label>
      <Select value={value || ALL} onValueChange={(v) => onChange(v === ALL ? '' : (v ?? ''))}>
        <SelectTrigger id={id} className="w-full">
          <SelectValue placeholder={placeholder}>{value ? (options.find((option) => option.value === value)?.label || value) : placeholder}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ALL}>{placeholder}</SelectItem>
          {[...extra, ...options].map((o) => <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>)}
        </SelectContent>
      </Select>
    </div>
  );
}

export function HistoryFilters({ filters, onChange, onRefresh, busy }: { filters: HistoryFilter; onChange: (filter: HistoryFilter) => void; onRefresh: () => void; busy: boolean }) {
  const [models, setModels] = useState<AdminModel[]>([]);
  const [providers, setProviders] = useState<AdminProvider[]>([]);
  const [clients, setClients] = useState<ClientKey[]>([]);
  const [optionsError, setOptionsError] = useState('');
  const [optionsRefresh, setOptionsRefresh] = useState(0);
  useEffect(() => {
    let active = true;
    setOptionsError('');
    Promise.all([listAdminModels(), listAdminProviders(), listClientKeys()]).then(([m, p, c]) => { if (active) { setModels(m.models || []); setProviders(p.providers || []); setClients(c.keys || []); } }).catch((e) => { if (active) setOptionsError(e instanceof Error ? e.message : 'Filter choices unavailable'); });
    return () => { active = false; };
  }, [optionsRefresh]);
  const update = (key: keyof HistoryFilter, value: string) => onChange({ ...filters, [key]: value });
  const activeCount = Object.values(filters).filter(Boolean).length;
  return <div className="space-y-4">
    {optionsError && <div role="alert" className="flex flex-wrap items-center justify-between gap-3 border border-destructive/40 bg-destructive/10 px-4 py-3 text-xs text-destructive"><span>Could not load filter choices: {optionsError}.</span><Button size="sm" variant="outline" onClick={() => setOptionsRefresh((v) => v + 1)}>Retry choices</Button></div>}
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
      <div className="space-y-1.5 md:col-span-2">
        <Label htmlFor="hf-q" className="text-[10px] uppercase tracking-[.12em] text-edge-400">Search request metadata</Label>
        <div className="relative">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-primary" />
          <Input id="hf-q" value={filters.q} onChange={(e) => update('q', e.target.value)} placeholder="Request ID, model, client…" className="pl-9" />
        </div>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="hf-since" className="text-[10px] uppercase tracking-[.12em] text-edge-400">From · local time</Label>
        <Input id="hf-since" type="datetime-local" value={filters.since} onChange={(e) => update('since', e.target.value)} aria-describedby="hf-window-note" />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="hf-until" className="text-[10px] uppercase tracking-[.12em] text-edge-400">Until · local time</Label>
        <Input id="hf-until" type="datetime-local" value={filters.until} onChange={(e) => update('until', e.target.value)} />
      </div>
      <FilterSelectField id="hf-model" label="Model" placeholder="All models" value={filters.model}
        options={models.map((m) => ({ value: m.id, label: m.id }))} onChange={(v) => update('model', v)} />
      <FilterSelectField id="hf-provider" label="Provider" placeholder="All providers" value={filters.backend}
        options={providers.map((p) => ({ value: p.name, label: p.name }))} onChange={(v) => update('backend', v)} />
      <FilterSelectField id="hf-client" label="Client API key" placeholder="All clients" value={filters.client_key_id}
        options={clients.map((c) => ({ value: c.id, label: `${c.name}${c.revoked_at ? ' (revoked)' : ''}` }))} onChange={(v) => update('client_key_id', v)} />
      <FilterSelectField id="hf-status" label="Status" placeholder="All outcomes" value={filters.status}
        options={[{ value: 'ok', label: 'Successful' }, { value: 'error', label: 'Failed' }]} onChange={(v) => update('status', v)} />
    </div>
    <div className="flex flex-wrap items-center justify-between gap-3 border-t border-edge-700 pt-4">
      <div className="space-y-1 text-[10px] text-edge-400"><p className="text-edge-200">{activeCount ? `${activeCount} active ${activeCount === 1 ? 'filter' : 'filters'}` : 'All dimensions included'}</p><p id="hf-window-note">A custom From time overrides the global window. Dates use your local timezone.</p></div>
      <div className="flex gap-2">
        <Button variant="outline" onClick={() => onChange({ model: '', backend: '', client_key_id: '', status: '', q: '', since: '', until: '' })}>Clear filters</Button>
        <Button disabled={busy} onClick={onRefresh}><RotateCcw data-icon="inline-start" className={busy ? 'animate-spin' : ''} />{busy ? 'Loading…' : 'Refresh'}</Button>
      </div>
    </div>
  </div>;
}
