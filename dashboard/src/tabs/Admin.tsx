import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  listAdminProviders, listAdminModels, upsertAdminProvider, upsertAdminModel,
  deleteAdminProvider, deleteAdminModel, deleteAdminProviderKey,
  discoverUpstreamModels, testUpstreamModel, lookupModelHints,
  onboard, bulkModels,
  type AdminProvider, type AdminModel, type TabProps, type DiscoveredModel, type ModelHint,
  type ProbeConn, type TestResult, type RouteTarget,
} from '../api';
import { toast } from 'sonner';
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '../components/ui/alert-dialog';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Checkbox } from '../components/ui/checkbox';
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '../components/ui/dialog';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Field, FieldDescription, FieldLabel } from '../components/ui/field';
import { Input } from '../components/ui/input';
import { Label } from '../components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/ui/select';
import { Separator } from '../components/ui/separator';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { Textarea } from '../components/ui/textarea';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';
import { ArrowRight, Check, Database, FlaskConical, KeyRound, Layers3, Pencil, Plus, RefreshCw, Search, Server, ShieldCheck, Trash2 } from 'lucide-react';

type Section = 'providers' | 'models';

type WizMeta = {
  dn: string; description: string; owned_by: string; tags: string[];
  pi: string; po: string; pc: string; ctx: string; mx: string; src: string;
};

const emptyMeta = (upstream: string): WizMeta => ({
  dn: upstream, description: '', owned_by: '', tags: [],
  pi: '', po: '', pc: '', ctx: '', mx: '', src: '',
});

const PRESETS: Record<string, { name: string; url: string; apiFormat?: 'openai' | 'systemone'; noAuth?: boolean; synth?: boolean; hint: string }> = {
  openai: { name: 'openai', url: 'https://api.openai.com/v1', hint: 'Any OpenAI-compatible endpoint works here.' },
  openrouter: { name: 'openrouter', url: 'https://openrouter.ai/api/v1', hint: 'Unified gateway; upstream ids look like org/model.' },
  vercel: { name: 'vercel', url: 'https://ai-gateway.vercel.sh/v1', hint: 'Vercel AI Gateway OpenAI-compatible base.' },
  cloudflare: { name: 'cloudflare', url: 'https://api.cloudflare.com/client/v4/accounts/%ACCOUNT%/ai/v1', hint: 'Replace %ACCOUNT% with your account id; keys use account_id:api_key lines.' },
  typesafe: { name: 'typesafe', url: 'https://api.typesafe.ai/v1', apiFormat: 'systemone', hint: 'Jev typed questions use /v1/systemone, not chat completions.' },
  opencode: { name: 'opencode', url: 'https://opencode.ai/zen/v1', noAuth: true, hint: 'Keyless endpoint — no_auth is ticked automatically.' },
  custom: { name: '', url: '', hint: 'Any OpenAI-compatible base URL starting with http(s)://.' },
};

function existingPrefixes(providers: { name: string }[], models: { id: string }[]): Set<string> {
  const s = new Set<string>();
  for (const p of providers) if (p.name) s.add(p.name);
  for (const m of models) {
    const i = m.id.indexOf('/');
    if (i > 0) s.add(m.id.slice(0, i));
  }
  return s;
}

function uniquePrefix(providerName: string, providers: { name: string }[], models: { id: string }[]): string {
  const letters = (providerName || '').toLowerCase().replace(/[^a-z0-9]/g, '') || 'xx';
  const taken = existingPrefixes(providers, models);
  const cands = [letters.slice(0, 2) || 'xx', letters.slice(0, 3), letters.slice(0, 4)];
  for (const c of cands) if (c && !taken.has(c)) return c;
  const base = letters.slice(0, 2) || 'xx';
  let suffix = 1;
  while (taken.has(`${base}${suffix}`)) suffix++;
  return `${base}${suffix}`;
}

function suggestVerlocId(providerName: string, upstream: string, providers: { name: string }[] = [], models: { id: string }[] = []): string {
  const up = (upstream || '').replace(/^\/+/, '');
  return `${uniquePrefix(providerName, providers, models)}/${up}`;
}

function findCatalogMatch(upstream: string, models: AdminModel[], backend: string): AdminModel | null {
  const normalized = upstream.trim().toLowerCase().replace(/:free$/, '-free');
  const matches = models.filter((model) =>
    model.backend === backend && model.upstream.trim().toLowerCase().replace(/:free$/, '-free') === normalized);
  if (matches.length === 0) return null;
  const first = matches[0];
  // Aliases with conflicting prices cannot establish reliable metadata.
  return matches.every((model) =>
    model.pricing_known === first.pricing_known &&
    model.pricing_input === first.pricing_input && model.pricing_output === first.pricing_output &&
    model.pricing_cache === first.pricing_cache &&
    model.context_window === first.context_window && model.max_tokens === first.max_tokens) ? first : null;
}

function metaFromMatch(upstream: string, match: AdminModel | null): WizMeta {
  if (!match) return emptyMeta(upstream);
  return {
    dn: match.name || upstream,
    description: match.description || '',
    owned_by: match.owned_by || '',
    tags: match.tags || [],
    pi: match.pricing_known ? String(match.pricing_input) : '',
    po: match.pricing_known ? String(match.pricing_output) : '',
    pc: match.pricing_known ? String(match.pricing_cache) : '',
    ctx: match.context_window > 0 ? String(match.context_window) : '',
    mx: match.max_tokens > 0 ? String(match.max_tokens) : '',
    src: 'catalog',
  };
}

function metaFromHint(upstream: string, hint: ModelHint): WizMeta {
  return {
    dn: hint.name || upstream,
    description: hint.description || '',
    owned_by: hint.owned_by || '',
    tags: hint.tags || [],
    pi: hint.pricing_input !== undefined ? String(hint.pricing_input) : '',
    po: hint.pricing_output !== undefined ? String(hint.pricing_output) : '',
    pc: hint.pricing_cache !== undefined ? String(hint.pricing_cache) : '',
    ctx: (hint.context_window || 0) > 0 ? String(hint.context_window) : '',
    mx: (hint.max_tokens || 0) > 0 ? String(hint.max_tokens) : '',
    src: hint.source || 'catalog',
  };
}

function fillMetaGaps(upstream: string, current: WizMeta, hint: WizMeta): WizMeta {
  return {
    dn: current.dn !== upstream ? current.dn : hint.dn,
    description: current.description || hint.description,
    owned_by: current.owned_by || hint.owned_by,
    tags: current.tags.length ? current.tags : hint.tags,
    pi: current.pi || hint.pi, po: current.po || hint.po, pc: current.pc || hint.pc,
    ctx: current.ctx || hint.ctx, mx: current.mx || hint.mx,
    src: current.src === hint.src ? current.src : [current.src, hint.src].filter(Boolean).join(' + '),
  };
}

function firstProbeKey(keysText: string): { api_key: string; account_id?: string } {
  const line = (keysText || '').split('\n').map((s) => s.trim()).find(Boolean) || '';
  if (!line) return { api_key: '' };
  return parseKeyLine(line);
}

function parseKeyLine(line: string): { api_key: string; account_id?: string } {
  const i = line.indexOf(':');
  if (i > 0 && !line.startsWith('sk-') && !line.startsWith('Bearer')) {
    const left = line.slice(0, i);
    const right = line.slice(i + 1);
    if (left.length <= 64 && !left.includes(' ')) {
      return { account_id: left, api_key: right };
    }
  }
  return { api_key: line };
}

// Scroll the main pane only, and only when the form is actually off screen.
function revealForm(el: HTMLElement | null) {
  if (!el) return;
  const scroller = el.closest('main');
  const top = scroller ? scroller.getBoundingClientRect().top : 0;
  const bottom = scroller ? scroller.getBoundingClientRect().bottom : window.innerHeight;
  const r = el.getBoundingClientRect();
  if (r.top >= top && r.top <= bottom - 48) return;
  el.scrollIntoView({ behavior: 'auto', block: 'start' });
}

type PendingConfirm = { title: string; body: string; confirmLabel: string; run: () => void };

export default function Admin({ state, section }: TabProps & { section: Section }) {
  const [providers, setProviders] = useState<AdminProvider[]>([]);
  const [models, setModels] = useState<AdminModel[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [selectedModels, setSelectedModels] = useState<string[]>([]);
  const [pq, setPq] = useState('');
  const [mq, setMq] = useState('');
  const [confirm, setConfirm] = useState<PendingConfirm | null>(null);

  const [editProvider, setEditProvider] = useState<Partial<Omit<AdminProvider, 'cooldown_seconds' | 'request_timeout_seconds' | 'max_tokens'>> & {
    name: string; keys_text?: string; append_key?: string;
    cooldown_seconds?: string; request_timeout_seconds?: string; max_tokens?: string;
  } | null>(null);
  const [originalProviderName, setOriginalProviderName] = useState('');
  const [editModel, setEditModel] = useState<Partial<Omit<AdminModel, 'context_window' | 'max_tokens' | 'pricing_input' | 'pricing_output' | 'pricing_cache'>> & {
    id: string; context_window?: string; max_tokens?: string;
    pricing_input?: string; pricing_output?: string; pricing_cache?: string;
  } | null>(null);
  const [originalModelID, setOriginalModelID] = useState('');
  const providerFormRef = useRef<HTMLDivElement | null>(null);
  const modelFormRef = useRef<HTMLDivElement | null>(null);

  // ---- Guided-add wizard state (staged until the final transactional save) ----
  const [wizOpen, setWizOpen] = useState(false);
  const [wizStep, setWizStep] = useState<1 | 2 | 3>(1);
  const [wUseExisting, setWUseExisting] = useState('');
  const [wName, setWName] = useState('');
  const [wBase, setWBase] = useState('');
  const [wAPIFormat, setWAPIFormat] = useState<'openai' | 'systemone'>('openai');
  const [wKeysText, setWKeysText] = useState('');
  const [wNoAuth, setWNoAuth] = useState(false);
  const [wSynth, setWSynth] = useState(false);
  const [wStrip, setWStrip] = useState(true);
  const [wCooldown, setWCooldown] = useState('30');
  const [wCap, setWCap] = useState('');
  const [wTimeout, setWTimeout] = useState('');
  const [wAuthH, setWAuthH] = useState('');
  const [wAuthF, setWAuthF] = useState('');
  const [wDiscovered, setWDiscovered] = useState<DiscoveredModel[]>([]);
  const [wFetchBusy, setWFetchBusy] = useState(false);
  const [wFetchErr, setWFetchErr] = useState<string | null>(null);
  const [wFilter, setWFilter] = useState('');
  const [wManual, setWManual] = useState('');
  const [wSel, setWSel] = useState<Record<string, boolean>>({});
  const [wIds, setWIds] = useState<Record<string, string>>({});
  const [wMeta, setWMeta] = useState<Record<string, WizMeta>>({});
  const [wTests, setWTests] = useState<Record<string, TestResult>>({});
  const [wTesting, setWTesting] = useState<Record<string, boolean>>({});
  const [wSaving, setWSaving] = useState(false);

  const refresh = useCallback(async (opts?: { keepErr?: boolean }) => {
    if (!opts?.keepErr) setErr(null);
    try {
      const [p, m] = await Promise.all([listAdminProviders(), listAdminModels()]);
      setProviders(p.providers || []);
      setModels(m.models || []);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  // Edit forms open above the list on <xl screens; bring them into view when needed.
  const providerFormKey = !editProvider
    ? ''
    : (providers.some((p) => p.name === editProvider.name) ? editProvider.name : 'new');
  const modelFormKey = !editModel
    ? ''
    : (models.some((m) => m.id === editModel.id) ? editModel.id : 'new');
  useEffect(() => {
    if (!providerFormKey) return;
    revealForm(providerFormRef.current);
  }, [providerFormKey, section]);
  useEffect(() => {
    if (!modelFormKey) return;
    revealForm(modelFormRef.current);
  }, [modelFormKey, section]);

  const run = async (fn: () => Promise<void>, success: string) => {
    setBusy(true);
    setErr(null);
    let saved = false;
    try {
      await fn();
      saved = true;
      toast.success(success);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
    // Reload after the button is free. A slow list must not leave the save
    // button stuck once the write itself has finished.
    void refresh(saved ? undefined : { keepErr: true });
  };

  const filteredProviders = useMemo(() => {
    const q = pq.trim().toLowerCase();
    if (!q) return providers;
    return providers.filter((p) =>
      `${p.name} ${p.base_url}`.toLowerCase().includes(q));
  }, [providers, pq]);

  const filteredModels = useMemo(() => {
    const q = mq.trim().toLowerCase();
    if (!q) return models;
    return models.filter((m) =>
      `${m.id} ${m.name} ${m.backend} ${m.upstream}`.toLowerCase().includes(q));
  }, [models, mq]);

  const doSaveProvider = (body: Record<string, unknown>) => {
    void run(async () => {
      await upsertAdminProvider(body);
      setEditProvider(null);
    }, 'Provider saved · live apply');
  };

  const saveProvider = () => {
    if (!editProvider?.name || !editProvider.base_url) {
      setErr('name and base_url required');
      return;
    }
    const existing = providers.find((p) => p.name === editProvider.name.trim());
    const connChanged = existing && (
      existing.base_url !== editProvider.base_url.trim() ||
      existing.auth_header !== (editProvider.auth_header || '') ||
      existing.auth_header_format !== (editProvider.auth_header_format || '') ||
      existing.no_auth !== !!editProvider.no_auth ||
      (existing.api_format || 'openai') !== (editProvider.api_format || 'openai')
    );
    const body: Record<string, unknown> = {
      name: editProvider.name.trim(),
      base_url: editProvider.base_url.trim(),
      api_format: editProvider.api_format || 'openai',
      auth_header: editProvider.auth_header || '',
      auth_header_format: editProvider.auth_header_format || '',
      disabled: !!editProvider.disabled,
      cooldown_seconds: Number(editProvider.cooldown_seconds) || 0,
      request_timeout_seconds: Number(editProvider.request_timeout_seconds) || 0,
      no_auth: !!editProvider.no_auth,
      synthesize_cli_headers: !!editProvider.synthesize_cli_headers,
      strip_reasoning_content: editProvider.strip_reasoning_content ?? null,
      max_tokens: Number(editProvider.max_tokens) || 0,
    };
    if ((!existing || !originalProviderName) && editProvider.keys_text !== undefined) {
      body.keys = editProvider.keys_text
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean)
        .map((line) => {
          const parsed = parseKeyLine(line);
          return parsed.account_id
            ? { account_id: parsed.account_id, api_key: parsed.api_key }
            : parsed.api_key;
        });
    }
    if (editProvider.append_key?.trim()) {
      const parsed = parseKeyLine(editProvider.append_key.trim());
      body.append_keys = [parsed.account_id
        ? { account_id: parsed.account_id, api_key: parsed.api_key }
        : parsed.api_key];
    }
    if ((connChanged || (existing && !originalProviderName))) {
      setConfirm({
        title: `Overwrite ${existing?.name}?`,
        body: `Existing model routes will use these connection settings${!originalProviderName ? ' and new credentials replace the stored ones' : ''}.`,
        confirmLabel: 'Overwrite provider',
        run: () => doSaveProvider(body),
      });
      return;
    }
    doSaveProvider(body);
  };

  const doSaveModel = (body: Record<string, unknown>, existingID?: string) => {
    void run(async () => {
      await upsertAdminModel(body, existingID);
      setEditModel(null);
    }, 'Model saved · live apply');
  };

  const saveModel = () => {
    if (!editModel?.id || !editModel.backend || !editModel.upstream) {
      setErr('id, backend, and upstream required');
      return;
    }
    const existing = models.find((model) => model.id === editModel.id.trim());
    const routingChanged = existing && (existing.backend !== editModel.backend || existing.upstream !== editModel.upstream || JSON.stringify(existing.routes || []) !== JSON.stringify(editModel.routes || []));
    const body = {
      id: editModel.id.trim(),
      backend: editModel.backend.trim(),
      disabled: !!editModel.disabled,
      routes: editModel.routes || [],
      overwrite: !!existing,
      upstream: editModel.upstream.trim(),
      name: editModel.name || editModel.id,
      description: editModel.description || '',
      owned_by: editModel.owned_by || '',
      context_window: Number(editModel.context_window) || 0,
      max_tokens: Number(editModel.max_tokens) || 0,
      tags: editModel.tags || [],
      ...(editModel.pricing_input?.trim() ? { pricing_input: Number(editModel.pricing_input) } : {}),
      ...(editModel.pricing_output?.trim() ? { pricing_output: Number(editModel.pricing_output) } : {}),
      ...(editModel.pricing_cache?.trim() ? { pricing_cache: Number(editModel.pricing_cache) } : {}),
    };
    if (routingChanged || (existing && !originalModelID)) {
      setConfirm({
        title: `Overwrite model ${editModel.id.trim()}?`,
        body: 'Existing applications using this ID will use the reviewed routing and metadata.',
        confirmLabel: 'Overwrite model',
        run: () => doSaveModel(body, originalModelID || undefined),
      });
      return;
    }
    doSaveModel(body, originalModelID || undefined);
  };

  const editingExistingProvider = !!originalProviderName;
  const editingExistingModel = !!originalModelID;

  // ---- Wizard helpers (staged inputs; nothing is saved until review) ----
  const wizProviderName = wUseExisting || wName.trim();
  const wizProbeConn = (): ProbeConn => {
    if (wUseExisting) return { provider: wUseExisting };
    const fk = firstProbeKey(wKeysText);
    return {
      base_url: wBase.trim(),
      api_format: wAPIFormat,
      api_key: fk.api_key,
      account_id: fk.account_id,
      auth_header: wAuthH || undefined,
      auth_header_format: wAuthF || undefined,
      no_auth: wNoAuth,
      synthesize_cli_headers: wSynth,
    };
  };

  const wizEnsureRow = (upstream: string, providerName: string, hint?: ModelHint) => {
    const id = suggestVerlocId(providerName, upstream, providers, models);
    const local = findCatalogMatch(upstream, models, providerName);
    const base = metaFromMatch(upstream, local);
    const external = hint ? metaFromHint(upstream, hint) : null;
    const meta = external ? fillMetaGaps(upstream, external, base) : base;
    setWIds((prev) => (prev[upstream] ? prev : { ...prev, [upstream]: id }));
    setWMeta((prev) => ({
      ...prev, [upstream]: prev[upstream] ? fillMetaGaps(upstream, prev[upstream], meta) : meta,
    }));
  };

  const wizLookupRow = async (upstream: string) => {
    setWFetchBusy(true);
    try {
      const result = await lookupModelHints([upstream]);
      if (result.hints[upstream]) wizEnsureRow(upstream, wizProviderName, result.hints[upstream]);
    } catch (error) {
      setWFetchErr(error instanceof Error ? error.message : 'Metadata lookup failed');
    } finally {
      setWFetchBusy(false);
    }
  };

  const wizFetch = async () => {
    setWFetchErr(null);
    const name = wizProviderName;
    if (!wUseExisting && (!wName.trim() || !wBase.trim())) {
      setWFetchErr('provider name and base_url required');
      return;
    }
    if (!wUseExisting && !wNoAuth && !firstProbeKey(wKeysText).api_key) {
      setWFetchErr('paste at least one API key (or tick no_auth)');
      return;
    }
    setWFetchBusy(true);
    try {
      const res = await discoverUpstreamModels(wizProbeConn());
      setWDiscovered(res.models || []);
      const sel: Record<string, boolean> = {};
      (res.models || []).forEach((m) => {
        sel[m.id] = wSel[m.id] ?? true;
        wizEnsureRow(m.id, name || 'xx', m.hint);
      });
      setWSel((prev) => ({ ...sel, ...prev }));
      setWizStep(2);
    } catch (e) {
      setWFetchErr((e as Error).message);
    } finally {
      setWFetchBusy(false);
    }
  };

  const wizSelectedUpstreams = useMemo(
    () => Object.keys(wSel).filter((u) => wSel[u]),
    [wSel],
  );

  const wizFiltered = useMemo(() => {
    const q = wFilter.trim().toLowerCase();
    if (!q) return wDiscovered;
    return wDiscovered.filter((m) => `${m.id} ${m.owned_by || ''}`.toLowerCase().includes(q));
  }, [wDiscovered, wFilter]);

  const wizAllFilteredSelected = wizFiltered.length > 0 && wizFiltered.every((m) => !!wSel[m.id]);

  const wizTestRow = (upstream: string) => {
    const conn = wizProbeConn();
    setWTesting((p) => ({ ...p, [upstream]: true }));
    testUpstreamModel({ ...conn, upstream })
      .then((res) => setWTests((p) => ({ ...p, [upstream]: res })))
      .catch((e) => setWTests((p) => ({
        ...p, [upstream]: { ok: false, status: 0, latency_ms: 0, error: (e as Error).message },
      })))
      .finally(() => setWTesting((p) => ({ ...p, [upstream]: false })));
  };

  const doWizBulkSave = async () => {
    const backend = wizProviderName;
    const rows = wizSelectedUpstreams;
    setWSaving(true);
    setWFetchErr(null);
    try {
      const stagedProvider = wUseExisting ? undefined : {
        name: wName.trim(), base_url: wBase.trim(), auth_header: wAuthH,
        api_format: wAPIFormat,
        auth_header_format: wAuthF, no_auth: wNoAuth, synthesize_cli_headers: wSynth,
        strip_reasoning_content: wStrip, cooldown_seconds: Number(wCooldown) || 0,
        request_timeout_seconds: Number(wTimeout) || 0, max_tokens: Number(wCap) || 0,
        keys: wKeysText.split('\n').map((s) => s.trim()).filter(Boolean).map((line) => {
          const parsed = parseKeyLine(line);
          return parsed.account_id ? { account_id: parsed.account_id, api_key: parsed.api_key } : parsed.api_key;
        }),
      };
      const collisions = rows.filter((u) => models.some((m) => m.id === wIds[u]?.trim()));
      const providerCollision = !wUseExisting && providers.some((p) => p.name === wName.trim());
      await onboard({
        provider: stagedProvider,
        overwrite: collisions.length > 0 || providerCollision,
        models: rows.map((upstream) => {
          const meta = wMeta[upstream] || emptyMeta(upstream);
          const id = wIds[upstream].trim();
          return {
            id, backend, upstream, name: meta.dn.trim() || id,
            description: meta.description, owned_by: meta.owned_by, tags: meta.tags,
            context_window: Number(meta.ctx) || 0,
            max_tokens: Number(meta.mx) || 0, ...(meta.pi.trim() ? { pricing_input: Number(meta.pi) } : {}),
            ...(meta.po.trim() ? { pricing_output: Number(meta.po) } : {}), ...(meta.pc.trim() ? { pricing_cache: Number(meta.pc) } : {}),
          };
        }),
      });
      setWKeysText('');
      setWName(''); setWBase(''); setWUseExisting(''); setWDiscovered([]);
      setWAPIFormat('openai');
      setWSel({}); setWIds({}); setWMeta({}); setWTests({});
      toast.success(`${rows.length} model${rows.length > 1 ? 's' : ''} saved · live apply`);
      setWizOpen(false);
      setWizStep(1);
    } catch (e) {
      setWFetchErr((e as Error).message);
    } finally {
      setWSaving(false);
    }
    void refresh();
  };

  const wizBulkSave = () => {
    const backend = wizProviderName;
    if (!backend) {
      setWFetchErr('Choose or enter a provider first');
      setWizStep(1);
      return;
    }
    if (!wUseExisting && (!wBase.trim() || (!wNoAuth && !firstProbeKey(wKeysText).api_key))) {
      setWFetchErr('Enter a provider endpoint and at least one credential, or explicitly enable keyless access.');
      setWizStep(1);
      return;
    }
    const rows = wizSelectedUpstreams;
    if (rows.length === 0) {
      setWFetchErr('select at least one model');
      setWizStep(2);
      return;
    }
    for (const u of rows) {
      const id = (wIds[u] || '').trim();
      if (!id) {
        setWFetchErr(`verloc id missing for ${u}`);
        return;
      }
    }
    const collisions = rows.filter((u) => models.some((m) => m.id === wIds[u]?.trim()));
    const providerCollision = !wUseExisting && providers.some((p) => p.name === wName.trim());
    if (collisions.length > 0 || providerCollision) {
      setConfirm({
        title: `Overwrite ${collisions.length} model route${collisions.length === 1 ? '' : 's'}${providerCollision ? ' and the existing provider' : ''}?`,
        body: 'This changes live application routing. Provider and models apply together in one transaction.',
        confirmLabel: 'Save · live apply',
        run: () => { void doWizBulkSave(); },
      });
      return;
    }
    void doWizBulkSave();
  };

  const deleteProvider = (p: AdminProvider) => {
    setConfirm({
      title: `Delete provider ${p.name}?`,
      body: `Its ${p.keys.length} stored credential${p.keys.length === 1 ? '' : 's'} and connection settings are removed. Model routes pointing at ${p.name} stop working.`,
      confirmLabel: 'Delete provider',
      run: () => { void run(async () => { await deleteAdminProvider(p.name); }, 'Provider deleted'); },
    });
  };

  const deleteModel = (id: string) => {
    setConfirm({
      title: `Delete model ${id}?`,
      body: 'Applications using this ID will fail until it is re-added. This cannot be undone.',
      confirmLabel: 'Delete model',
      run: () => {
        void run(async () => {
          await deleteAdminModel(id);
          if (editModel?.id === id) setEditModel(null);
        }, 'Model deleted');
      },
    });
  };

  const bulkAction = (action: 'enable' | 'disable' | 'delete') => {
    if (selectedModels.length === 0) return;
    if (action === 'delete') {
      setConfirm({
        title: `Delete ${selectedModels.length} model route${selectedModels.length === 1 ? '' : 's'}?`,
        body: 'Permanently removes the selected routes. This cannot be undone.',
        confirmLabel: 'Delete selected',
        run: () => {
          void run(async () => { await bulkModels(selectedModels, action); setSelectedModels([]); }, `${selectedModels.length} model(s) deleted`);
        },
      });
      return;
    }
    void run(async () => { await bulkModels(selectedModels, action); setSelectedModels([]); }, `${selectedModels.length} model(s): ${action}`);
  };

  const allFilteredSelected = filteredModels.length > 0 && filteredModels.every((m) => selectedModels.includes(m.id));

  return (
    <div className="min-w-0 space-y-5">
      <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 lg:grid-cols-4">
        {(section === 'providers' ? [
          { label: 'Connections', value: providers.length, detail: 'Configured upstream providers', icon: Server, color: 'text-primary' },
          { label: 'Enabled', value: providers.filter((p) => !p.disabled).length, detail: 'Connections allowed to serve', icon: ShieldCheck, color: 'text-chart-2' },
          { label: 'Stored credentials', value: providers.reduce((sum, p) => sum + p.keys.length, 0), detail: 'Encrypted upstream keys', icon: KeyRound, color: 'text-chart-3' },
          { label: 'Keyless endpoints', value: providers.filter((p) => p.no_auth).length, detail: 'No credential header required', icon: Layers3, color: 'text-chart-2' },
        ] : [
          { label: 'Catalog routes', value: models.length, detail: 'Application-facing model IDs', icon: Database, color: 'text-primary' },
          { label: 'Enabled', value: models.filter((m) => !m.disabled).length, detail: 'Model routes allowed to serve', icon: ShieldCheck, color: 'text-chart-2' },
          { label: 'With fallbacks', value: models.filter((m) => (m.routes || []).length > 0).length, detail: 'Routes with additional targets', icon: Layers3, color: 'text-chart-3' },
          { label: 'Unknown pricing', value: models.filter((m) => !m.pricing_known).length, detail: 'Not counted as free usage', icon: Database, color: 'text-chart-4' },
        ]).map(({ label, value, detail, icon: Icon, color }) => (
          <div key={label} className="min-w-0 bg-edge-900 p-4 sm:p-5">
            <div className="flex items-center justify-between gap-2 text-[10px] uppercase tracking-[0.14em] text-muted-foreground">
              <span>{label}</span><Icon className={`size-3.5 shrink-0 ${color}`} />
            </div>
            <div className={`mt-4 text-3xl tabular-nums tracking-tight ${color}`}>{loading ? '—' : value}</div>
            <p className="mt-2 text-[10px] leading-relaxed text-muted-foreground">{detail}</p>
          </div>
        ))}
      </div>

      <Card className="gap-0 border-edge-700 bg-edge-900 py-0">
        <div className="flex flex-col gap-4 p-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
          <div className="flex min-w-0 items-start gap-3">
            <ShieldCheck className="mt-0.5 size-4 shrink-0 text-primary" />
            <div>
              <p className="text-[10px] uppercase tracking-[0.14em] text-primary">Management workspace</p>
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                Encrypted credentials. Changes apply live. Guided onboarding stages discovery, tests, and catalog hints before one transactional save.
              </p>
            </div>
          </div>
          <div className="flex shrink-0 flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" disabled={busy || loading} onClick={() => { void refresh(); }}>
              <RefreshCw className={`size-3.5 ${loading ? 'animate-spin' : ''}`} />Refresh
            </Button>
            <Button size="sm" onClick={() => { setWFetchErr(null); setWizOpen(true); }}>
              <Plus className="size-3.5" />Guided add
            </Button>
          </div>
        </div>
        {err && (
          <CardContent className="border-t border-edge-700 p-4">
            <div role="alert" className="flex flex-wrap items-center gap-2 border border-destructive/40 bg-destructive/10 px-3 py-3 text-xs">
              <span className="min-w-0 flex-1 break-words text-destructive">{err}</span>
              <Button variant="outline" size="xs" onClick={() => { void refresh(); }}>retry</Button>
            </div>
          </CardContent>
        )}
      </Card>

      {/* ---- Guided-add wizard (multi-step dialog; staged until review save) ---- */}
      <Dialog open={wizOpen} onOpenChange={setWizOpen}>
        <DialogContent className="max-h-[90dvh] overflow-y-auto border-edge-600 bg-edge-900 p-4 sm:max-w-5xl sm:p-6">
          <DialogHeader className="border-b border-edge-700 pb-5">
            <p className="text-[10px] uppercase tracking-[0.18em] text-primary">Transactional onboarding</p>
            <DialogTitle className="text-lg">Connect an upstream. Publish your models.</DialogTitle>
            <DialogDescription className="max-w-2xl text-xs leading-relaxed">
              Discovery and testing use staged inputs without saving. Provider and model changes apply
              together in one transaction only after the final review.
            </DialogDescription>
          </DialogHeader>

          <div className="grid grid-cols-3 gap-px border border-edge-700 bg-edge-700">
            {([
              { title: 'Connection', description: 'Provider + credentials' },
              { title: 'Select models', description: `${wizSelectedUpstreams.length} selected` },
              { title: 'Review + apply', description: 'Metadata + live tests' },
            ] as const).map(({ title, description }, i) => {
              const step = (i + 1) as 1 | 2 | 3;
              const active = wizStep === step;
              return (
                <button
                  key={title}
                  type="button"
                  aria-current={active ? 'step' : undefined}
                  onClick={() => setWizStep(step)}
                  className={`min-w-0 border-t-2 p-3 text-left transition-colors focus-visible:outline-2 focus-visible:outline-primary sm:p-4 ${active ? 'border-primary bg-primary/10 text-primary' : 'border-transparent bg-edge-950 text-muted-foreground hover:bg-edge-800 hover:text-foreground'}`}
                >
                  <span className="mb-2 block text-[10px] tabular-nums">{String(step).padStart(2, '0')}</span>
                  <span className="block text-xs">{title}</span>
                  <span className="mt-1 hidden text-[10px] text-muted-foreground sm:block">{description}</span>
                </button>
              );
            })}
          </div>

          {wFetchErr && (
            <div role="alert" className="border border-destructive/40 bg-destructive/10 px-3 py-3 text-xs break-words text-destructive">
              {wFetchErr}
            </div>
          )}

          {wizStep === 1 && (
            <div className="space-y-5">
              <Field>
                <FieldLabel>Provider</FieldLabel>
                <Select value={wUseExisting} onValueChange={(v) => { setWUseExisting(v === '__new' ? '' : (v ?? '')); setWFetchErr(null); }}>
                  <SelectTrigger aria-label="Existing provider or new provider">
                    <SelectValue placeholder="+ new provider…" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="__new">+ new provider…</SelectItem>
                    {providers.map((p) => <SelectItem key={p.name} value={p.name}>{p.name} · {p.base_url}</SelectItem>)}
                  </SelectContent>
                </Select>
                {wUseExisting
                  ? <FieldDescription>Using saved provider keys server-side — no need to paste keys again. Fetch lists its live upstream models.</FieldDescription>
                  : <FieldDescription>Connect an OpenAI-compatible or System One endpoint, then fetch its live model list.</FieldDescription>}
              </Field>

              {!wUseExisting && (
                <>
                  <div className="flex items-center gap-3 border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em]">
                    <span className="text-primary">01 / Endpoint</span><span className="text-muted-foreground">Provider connection</span>
                  </div>
                  <Field>
                    <FieldLabel>Provider preset</FieldLabel>
                    <Select
                      value=""
                      onValueChange={(key) => {
                        const preset = PRESETS[key ?? ''];
                        if (!preset) return;
                        setWName(preset.name); setWBase(preset.url);
                        setWNoAuth(!!preset.noAuth); setWSynth(!!preset.synth);
                        setWAPIFormat(preset.apiFormat || 'openai');
                        setWAuthH(''); setWAuthF('');
                      }}
                    >
                      <SelectTrigger aria-label="Provider preset">
                        <SelectValue placeholder="Choose a preset…" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="openai">OpenAI-compatible</SelectItem>
                        <SelectItem value="openrouter">OpenRouter</SelectItem>
                        <SelectItem value="vercel">Vercel AI Gateway</SelectItem>
                        <SelectItem value="cloudflare">Cloudflare Workers AI</SelectItem>
                        <SelectItem value="typesafe">TypeSafe Jev (System One)</SelectItem>
                        <SelectItem value="opencode">OpenCode (keyless)</SelectItem>
                        <SelectItem value="custom">Custom endpoint</SelectItem>
                      </SelectContent>
                    </Select>
                    <FieldDescription>Presets fill name, base URL, and keyless flags. Cloudflare uses %ACCOUNT% in the URL plus account_id:api_key lines.</FieldDescription>
                  </Field>

                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                    <Field>
                      <FieldLabel htmlFor="wiz-name">Name</FieldLabel>
                      <Input id="wiz-name" value={wName} onChange={(e) => setWName(e.target.value)} placeholder="acme" className="font-mono" />
                      <FieldDescription>Lowercase, no / : space.</FieldDescription>
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="wiz-base">Base URL</FieldLabel>
                      <Input id="wiz-base" value={wBase} onChange={(e) => setWBase(e.target.value)} placeholder="https://api.example.com/v1" className="font-mono" />
                      <FieldDescription>Usually ends in /v1.</FieldDescription>
                    </Field>
                  </div>
                  <Field>
                    <FieldLabel>API format</FieldLabel>
                    <Select value={wAPIFormat} onValueChange={(value) => setWAPIFormat(value === 'systemone' ? 'systemone' : 'openai')}>
                      <SelectTrigger aria-label="New provider API format">
                        <SelectValue>{wAPIFormat === 'systemone' ? 'System One (typed questions)' : 'OpenAI-compatible'}</SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="openai">OpenAI-compatible</SelectItem>
                        <SelectItem value="systemone">System One (typed questions)</SelectItem>
                      </SelectContent>
                    </Select>
                    <FieldDescription>Jev uses System One. Cloudflare stays OpenAI-compatible; Clef is routed natively per model.</FieldDescription>
                  </Field>
                  <p className="border-l-2 border-chart-4 bg-chart-4/5 px-3 py-2 text-[11px] leading-relaxed text-chart-4">Use an http(s):// base URL, usually ending in /v1. Review your endpoint before saving.</p>

                  <div className="flex items-center gap-3 border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em]">
                    <span className="text-chart-2">02 / Credentials</span><span className="text-muted-foreground">Staged until final save</span>
                  </div>
                  <Field>
                    <FieldLabel htmlFor="wiz-keys">API keys</FieldLabel>
                    <Textarea
                      id="wiz-keys"
                      value={wKeysText}
                      onChange={(e) => setWKeysText(e.target.value)}
                      placeholder="sk-…"
                      rows={3}
                      className="font-mono"
                    />
                    <FieldDescription>One per line; or account_id:api_key for Cloudflare-style credentials. Staged only — stored encrypted on save.</FieldDescription>
                  </Field>

                  <details className="border border-edge-700 bg-edge-950 px-4 py-3">
                    <summary className="cursor-pointer text-[11px] font-medium text-chart-3">Advanced / authentication, limits, and protocol options</summary>
                    <div className="space-y-4 border-t border-edge-700 pt-4 mt-3">
                      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                        <Field>
                          <FieldLabel>Auth header</FieldLabel>
                          <Input value={wAuthH} onChange={(e) => setWAuthH(e.target.value)} placeholder="Authorization (default)" />
                        </Field>
                        <Field>
                          <FieldLabel>Auth format (%s = key)</FieldLabel>
                          <Input value={wAuthF} onChange={(e) => setWAuthF(e.target.value)} placeholder="Bearer %s (default)" />
                        </Field>
                      </div>
                      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                        <Field>
                          <FieldLabel>Cooldown sec</FieldLabel>
                          <Input value={wCooldown} onChange={(e) => setWCooldown(e.target.value)} inputMode="numeric" className="font-mono" />
                        </Field>
                        <Field>
                          <FieldLabel>Max tokens cap (0 = none)</FieldLabel>
                          <Input value={wCap} onChange={(e) => setWCap(e.target.value)} inputMode="numeric" className="font-mono" />
                        </Field>
                        <Field>
                          <FieldLabel>Timeout sec (0 = none)</FieldLabel>
                          <Input value={wTimeout} onChange={(e) => setWTimeout(e.target.value)} inputMode="numeric" className="font-mono" />
                        </Field>
                      </div>
                      <div className="flex flex-wrap gap-4 text-xs">
                        <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                          <Checkbox checked={wNoAuth} onCheckedChange={(v) => setWNoAuth(v === true)} />
                          <Tooltip>
                            <TooltipTrigger><span>no_auth (keyless)</span></TooltipTrigger>
                            <TooltipContent>Skip credential headers entirely — for public or keyless endpoints like OpenCode.</TooltipContent>
                          </Tooltip>
                        </Label>
                        <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                          <Checkbox checked={wSynth} onCheckedChange={(v) => setWSynth(v === true)} />
                          synthesize_cli_headers
                        </Label>
                        <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                          <Checkbox checked={wStrip} onCheckedChange={(v) => setWStrip(v === true)} />
                          strip_reasoning_content
                        </Label>
                      </div>
                    </div>
                  </details>
                </>
              )}

              <DialogFooter className="border-t border-edge-700 pt-4">
                <Button
                  variant="outline"
                  disabled={wFetchBusy}
                  onClick={() => {
                    if (!wizProviderName || (!wUseExisting && !wBase.trim())) {
                      setWFetchErr('Enter a provider name and endpoint first.');
                      return;
                    }
                    setWizStep(2);
                  }}
                >
                  Enter models manually
                </Button>
                <Button disabled={wFetchBusy || busy} onClick={() => { void wizFetch(); }}>
                  {wFetchBusy ? 'fetching…' : 'fetch models →'}
                </Button>
              </DialogFooter>
            </div>
          )}

          {wizStep === 2 && (
            <div className="space-y-5">
              <div className="flex flex-wrap items-center justify-between gap-3 border-b border-edge-700 pb-4">
                <div className="relative min-w-0 flex-1 sm:max-w-sm">
                  <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                  <Input value={wFilter} onChange={(e) => setWFilter(e.target.value)} placeholder="Filter upstream models…" className="w-full pl-8" />
                </div>
                <span className="text-[10px] uppercase tracking-wide text-muted-foreground"><span className="text-primary">{wizSelectedUpstreams.length} selected</span> / {wDiscovered.length} discovered</span>
              </div>

              {wDiscovered.length === 0 && (
                <p className="border border-dashed border-edge-600 bg-edge-950 p-5 text-xs leading-relaxed text-muted-foreground">No discovery results yet. Fetch models from the connection step, or enter upstream IDs below when discovery is unavailable.</p>
              )}

              <div className="overflow-x-auto border border-edge-700 bg-edge-950">
                <Table className="min-w-[680px]">
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-10">
                        <Checkbox
                          aria-label="Select or unselect all filtered models"
                          checked={wizAllFilteredSelected}
                          onCheckedChange={(v) => {
                            const on = v === true;
                            if (on) wizFiltered.forEach((m) => wizEnsureRow(m.id, wizProviderName || 'xx', m.hint));
                            setWSel((prev) => {
                              const next = { ...prev };
                              wizFiltered.forEach((m) => { next[m.id] = on; });
                              return next;
                            });
                          }}
                        />
                      </TableHead>
                      <TableHead>Upstream model</TableHead>
                      <TableHead>Verloc id</TableHead>
                      <TableHead>Display name</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {wizFiltered.map((m) => {
                      const checked = !!wSel[m.id];
                      const idVal = wIds[m.id] || '';
                      const dup = models.some((x) => x.id === idVal.trim() && idVal.trim() !== '');
                      return (
                        <TableRow key={m.id}>
                          <TableCell>
                            <Checkbox
                              aria-label={`Select upstream model ${m.id}`}
                              checked={checked}
                              onCheckedChange={(v) => {
                                const on = v === true;
                                setWSel((p) => ({ ...p, [m.id]: on }));
                                if (on) wizEnsureRow(m.id, wizProviderName || 'xx', m.hint);
                              }}
                            />
                          </TableCell>
                          <TableCell>
                            <div className="max-w-64 break-all text-[11px] text-chart-2">{m.id}</div>
                            {m.owned_by && <div className="text-[10px] text-muted-foreground">{m.owned_by}</div>}
                          </TableCell>
                          <TableCell>
                            {checked ? (
                              <div className="flex min-w-44 items-center gap-2">
                                <Input
                                  value={idVal}
                                  onChange={(e) => setWIds((p) => ({ ...p, [m.id]: e.target.value }))}
                                  placeholder={`${uniquePrefix(wizProviderName || 'xx', providers, models)}/${m.id}`}
                                  aria-label={`verloc id for ${m.id}`}
                                  className="font-mono text-[11px]"
                                />
                                {dup && <Badge variant="outline" className="shrink-0 border-chart-4/40 text-chart-4">exists · overwrites</Badge>}
                              </div>
                            ) : (
                              <span className="text-[11px] text-muted-foreground">—</span>
                            )}
                          </TableCell>
                          <TableCell>
                            {checked ? (
                              <Input
                                value={(wMeta[m.id] || emptyMeta(m.id)).dn}
                                onChange={(e) => setWMeta((p) => ({
                                  ...p,
                                  [m.id]: { ...(p[m.id] || emptyMeta(m.id)), dn: e.target.value },
                                }))}
                                placeholder="display name"
                                aria-label={`display name for ${m.id}`}
                                className="min-w-32 text-[11px]"
                              />
                            ) : (
                              <span className="text-[11px] text-muted-foreground">—</span>
                            )}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>

              <div className="flex flex-wrap items-end gap-3 border border-edge-700 bg-edge-950 p-4">
                <Field className="min-w-0 basis-56 flex-1">
                  <FieldLabel>Manual upstream ID</FieldLabel>
                  <Input value={wManual} onChange={(e) => setWManual(e.target.value)} placeholder="org/model-id" className="font-mono" />
                </Field>
                <Button
                  variant="outline"
                  disabled={wFetchBusy}
                  onClick={() => {
                    const u = wManual.trim();
                    if (!u) return;
                    setWDiscovered((p) => (p.some((x) => x.id === u) ? p : [...p, { id: u }]));
                    setWSel((p) => ({ ...p, [u]: true }));
                    wizEnsureRow(u, wizProviderName || 'xx');
                    void wizLookupRow(u);
                    setWManual('');
                  }}
                >
                  + add row
                </Button>
              </div>
              <p className="text-[11px] leading-relaxed text-muted-foreground">Model IDs receive an editable collision-free prefix. Catalog matches and external hints prefill available fields; blank pricing remains unknown, while explicit zero means free. Existing IDs require confirmation before overwrite.</p>

              <DialogFooter className="border-t border-edge-700 pt-4">
                <Button variant="outline" onClick={() => setWizStep(1)}>← provider</Button>
                <Button disabled={wFetchBusy || wizSelectedUpstreams.length === 0} onClick={() => setWizStep(3)}>
                  review {wizSelectedUpstreams.length} →
                </Button>
              </DialogFooter>
            </div>
          )}

          {wizStep === 3 && (
            <div className="space-y-5">
              {wizSelectedUpstreams.length === 0 && (
                <p className="text-xs text-muted-foreground">Nothing selected — go back and tick at least one model.</p>
              )}
              {wizSelectedUpstreams.map((u) => {
                const meta = wMeta[u] || emptyMeta(u);
                const setMeta = (patch: Partial<WizMeta>) =>
                  setWMeta((p) => ({ ...p, [u]: { ...meta, ...patch } }));
                const t = wTests[u];
                const testing = !!wTesting[u];
                return (
                  <div key={u} className="space-y-4 border border-edge-700 bg-edge-950 p-4">
                    <div className="flex flex-wrap items-start justify-between gap-3 border-b border-edge-700 pb-3">
                      <div className="min-w-0 flex-1 basis-56">
                        <div className="break-all text-xs text-primary">{wIds[u] || u}</div>
                        <div className="mt-1 flex items-start gap-2 text-[11px] text-muted-foreground"><ArrowRight className="mt-0.5 size-3 shrink-0" /><span className="break-all">{wizProviderName}:{u}</span></div>
                        {meta.src !== '' && <div className="mt-2 text-[10px] text-chart-3">Metadata source / {meta.src}</div>}
                      </div>
                      <div className="flex flex-wrap items-center gap-2">
                        {t && (t.ok
                          ? <Badge variant="outline" className="border-primary/40 text-primary"><Check className="size-3" />{t.latency_ms}ms</Badge>
                          : <Badge variant="outline" className="border-destructive/40 text-destructive">fail · {t.status || 'err'}</Badge>)}
                        <Button variant="outline" size="xs" disabled={testing} onClick={() => wizTestRow(u)}>
                          <FlaskConical className="size-3" />{testing ? 'Testing…' : 'Test upstream'}
                        </Button>
                        <Button variant="ghost" size="xs" onClick={() => setWSel((p) => ({ ...p, [u]: false }))}>remove</Button>
                      </div>
                    </div>
                    {t && !t.ok && (
                      <div className="border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs break-words text-destructive">
                        {t.error || 'failed'}{t.hint ? ` — ${t.hint}` : ''}
                      </div>
                    )}
                    {t?.ok && t.reply && (
                      <div className="text-[10px] text-muted-foreground">reply: <span className="font-mono">{t.reply}</span></div>
                    )}
                    <Field>
                      <FieldLabel htmlFor={`wiz-${u}-name`}>Display name</FieldLabel>
                      <Input id={`wiz-${u}-name`} value={meta.dn} onChange={(e) => setMeta({ dn: e.target.value })} placeholder={u} />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor={`wiz-${u}-description`}>Description</FieldLabel>
                      <Textarea id={`wiz-${u}-description`} value={meta.description} onChange={(e) => setMeta({ description: e.target.value })} rows={2} />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor={`wiz-${u}-tags`}>Capabilities / tags</FieldLabel>
                      <Input id={`wiz-${u}-tags`} value={meta.tags.join(', ')} onChange={(e) => setMeta({ tags: e.target.value.split(',').map((tag) => tag.trim()).filter(Boolean) })} />
                    </Field>
                    <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-pi`}>In $/1M</FieldLabel>
                        <Input id={`wiz-${u}-pi`} value={meta.pi} onChange={(e) => setMeta({ pi: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-po`}>Out $/1M</FieldLabel>
                        <Input id={`wiz-${u}-po`} value={meta.po} onChange={(e) => setMeta({ po: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-pc`}>Cache $/1M</FieldLabel>
                        <Input id={`wiz-${u}-pc`} value={meta.pc} onChange={(e) => setMeta({ pc: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-ctx`}>Context</FieldLabel>
                        <Input id={`wiz-${u}-ctx`} value={meta.ctx} onChange={(e) => setMeta({ ctx: e.target.value })} inputMode="numeric" className="font-mono" />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-mx`}>Max out</FieldLabel>
                        <Input id={`wiz-${u}-mx`} value={meta.mx} onChange={(e) => setMeta({ mx: e.target.value })} inputMode="numeric" className="font-mono" />
                      </Field>
                    </div>
                  </div>
                );
              })}
              {wizSelectedUpstreams.filter((u) => models.some((m) => m.id === (wIds[u] || '').trim() && (wIds[u] || '').trim() !== '')).length > 0 && (
                <p className="border-l-2 border-chart-4 bg-chart-4/5 px-3 py-2 text-xs text-chart-4">Existing model IDs will be explicitly confirmed before transactional overwrite.</p>
              )}
              {wizSelectedUpstreams.filter((u) => { const mt = wMeta[u]; return !mt || ((Number(mt.pi) || 0) === 0 && (Number(mt.po) || 0) === 0); }).length > 0 && (
                <p className="text-xs text-muted-foreground">Some prices are zero or unset. Review against provider pricing; unknown cost is not free usage.</p>
              )}
              <DialogFooter className="border-t border-edge-700 pt-4">
                <Button variant="outline" onClick={() => setWizStep(2)}>← pick</Button>
                <Button disabled={wSaving || wFetchBusy || wizSelectedUpstreams.length === 0} onClick={() => wizBulkSave()}>
                  {wSaving ? 'saving…' : `save ${wizSelectedUpstreams.length} model${wizSelectedUpstreams.length === 1 ? '' : 's'} · live apply`}
                </Button>
              </DialogFooter>
            </div>
          )}
        </DialogContent>
      </Dialog>

      {section === 'providers' && (
        <div className={`grid min-w-0 grid-cols-1 items-start gap-5 ${editProvider ? 'xl:grid-cols-[minmax(0,1fr)_minmax(360px,0.8fr)]' : ''}`}>
          <Card className="min-w-0 gap-0 border-edge-700 bg-edge-900 py-0">
            <div className="flex flex-wrap items-center justify-between gap-4 border-b border-edge-700 p-4 sm:p-5">
              <div className="min-w-0">
                <p className="mb-2 text-[10px] uppercase tracking-[0.16em] text-primary">01 / Connection directory</p>
                <CardTitle className="text-sm">Upstream providers <span className="ml-2 text-muted-foreground">{filteredProviders.length}{pq ? ` / ${providers.length}` : ''}</span></CardTitle>
                <CardDescription className="mt-2 text-[11px]">Connection settings, encrypted credentials, and live pool health.</CardDescription>
              </div>
              <div className="flex min-w-0 flex-1 basis-full flex-wrap items-center gap-2 sm:basis-auto sm:justify-end">
                  <div className="relative min-w-0 flex-1 sm:max-w-60">
                    <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input aria-label="Filter providers" value={pq} onChange={(e) => setPq(e.target.value)} placeholder="Search connections…" className="w-full pl-8" />
                  </div>
                  <Button
                    size="sm"
                    className="shrink-0"
                    onClick={() => {
                      setOriginalProviderName('');
                      setEditProvider({
                        name: '', base_url: '', keys_text: '', no_auth: false,
                        synthesize_cli_headers: false, max_tokens: '', cooldown_seconds: '30',
                      });
                    }}
                  >
                    <Plus className="size-3.5" />New provider
                  </Button>
                </div>
            </div>
            <CardContent className="px-0 pb-0">
              <div className="overflow-x-auto">
                <Table className="min-w-[700px]">
                  <TableHeader className="bg-edge-950">
                    <TableRow>
                      <TableHead className="w-[40%] pl-5">Connection / endpoint</TableHead>
                      <TableHead>Credentials</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead className="text-right">Actions</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {loading && (
                      Array.from({ length: 4 }).map((_, i) => (
                        <TableRow key={i}>
                          <TableCell colSpan={4}><Skeleton className="h-16 w-full rounded-none" /></TableCell>
                        </TableRow>
                      ))
                    )}
                    {!loading && filteredProviders.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={4}>
                          <Empty className="py-12">
                            <EmptyHeader>
                              <EmptyTitle>{pq ? 'No providers match' : 'No providers'}</EmptyTitle>
                              <EmptyDescription>{pq ? 'Adjust the filter.' : 'Add a provider manually or via guided add.'}</EmptyDescription>
                            </EmptyHeader>
                          </Empty>
                        </TableCell>
                      </TableRow>
                    )}
                    {filteredProviders.map((p) => {
                      const pool = state?.keys?.[p.name] || [];
                      const up = pool.filter((k) => k.healthy).length;
                      return (
                        <TableRow key={p.name} className={editProvider?.name === p.name ? 'bg-primary/5' : ''}>
                          <TableCell className="py-5 pl-5 align-top">
                            <div className="min-w-0 max-w-md whitespace-normal">
                              <div className="flex items-center gap-2 text-xs font-medium text-foreground"><Server className="size-3.5 shrink-0 text-chart-2" /><span className="break-all">{p.name}</span></div>
                              <div className="mt-2 break-all text-[11px] leading-relaxed text-muted-foreground">{p.base_url}</div>
                              <div className="mt-3 flex flex-wrap gap-1.5">
                                <Badge variant="outline">{p.model_count} models</Badge>
                                {p.api_format === 'systemone' && <Badge variant="outline">System One</Badge>}
                                {p.no_auth && <Badge variant="outline" className="border-chart-2/40 text-chart-2">keyless</Badge>}
                                {p.max_tokens > 0 && <Badge variant="outline">cap {p.max_tokens}</Badge>}
                              </div>
                            </div>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <Tooltip>
                              <TooltipTrigger>
                                <span className="block text-left text-[11px]">
                                  <span className="block">{p.keys.length} stored key{p.keys.length === 1 ? '' : 's'}</span><span className={`mt-1 block ${up < p.keys.length ? 'text-chart-4' : 'text-primary'}`}>{up} healthy in live pool</span>
                                </span>
                              </TooltipTrigger>
                              <TooltipContent>Live pool health from the Keys readout{pool.length !== p.keys.length ? ' (pool view may lag staged edits)' : ''}.</TooltipContent>
                            </Tooltip>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <Badge variant="outline" className={p.disabled ? 'border-chart-4/40 text-chart-4' : 'border-primary/30 text-primary'}>
                              {p.disabled ? 'Disabled' : 'Enabled'}
                            </Badge>
                          </TableCell>
                          <TableCell className="py-5 pr-5 align-top">
                            <div className="flex min-w-44 flex-wrap justify-end gap-1.5">
                              <Button
                                variant="outline"
                                size="xs"
                                disabled={busy}
                                onClick={() => {
                                  void run(async () => {
                                    await upsertAdminProvider({ ...p, keys: undefined, model_count: undefined, disabled: !p.disabled });
                                  }, p.disabled ? 'Provider enabled' : 'Provider disabled');
                                }}
                              >
                                {p.disabled ? 'Enable' : 'Disable'}
                              </Button>
                              <Button
                                variant="outline"
                                size="xs"
                                onClick={() => {
                                  setOriginalProviderName(p.name);
                                  setEditProvider({
                                    ...p, append_key: '',
                                    cooldown_seconds: p.cooldown_seconds ? String(p.cooldown_seconds) : '',
                                    request_timeout_seconds: p.request_timeout_seconds ? String(p.request_timeout_seconds) : '',
                                    max_tokens: p.max_tokens ? String(p.max_tokens) : '',
                                  });
                                }}
                              >
                                <Pencil className="size-3" />edit
                              </Button>
                              <Button variant="destructive" size="xs" disabled={busy} onClick={() => deleteProvider(p)}>
                                <Trash2 className="size-3" />Delete
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
            <div className="flex flex-wrap justify-between gap-2 border-t border-edge-700 bg-edge-950 px-5 py-3 text-[10px] text-muted-foreground">
              <span>{loading ? 'Loading connections…' : `${filteredProviders.length} of ${providers.length} connections`}</span>
              <span>Credentials are separate from application keys</span>
            </div>
          </Card>

          {editProvider && (
            <Card ref={providerFormRef} className="order-first min-w-0 gap-0 border-edge-600 bg-edge-900 py-0 xl:order-last">
              <CardHeader className="gap-2 border-b border-edge-700 bg-edge-950 p-5">
                <p className="text-[10px] uppercase tracking-[0.16em] text-chart-2">Connection editor</p>
                <CardTitle className="break-all text-base">{editingExistingProvider ? editProvider.name : 'New provider'}</CardTitle>
                <CardDescription className="text-[11px] leading-relaxed">{editingExistingProvider ? 'Connection settings apply live on save. Stored credentials are never exposed.' : 'Staged until saved. No connection or credential is stored before you apply.'}</CardDescription>
              </CardHeader>
              <CardContent className="space-y-5 p-5">
                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-primary">01 / Identity + endpoint</p>
                <Field>
                  <FieldLabel htmlFor="ep-name">Name</FieldLabel>
                  <Input id="ep-name" value={editProvider.name || ''} onChange={(e) => setEditProvider({ ...editProvider, name: e.target.value })} disabled={editingExistingProvider} className="font-mono" />
                </Field>
                <Field>
                  <FieldLabel htmlFor="ep-base">Base URL</FieldLabel>
                  <Input id="ep-base" value={editProvider.base_url || ''} onChange={(e) => setEditProvider({ ...editProvider, base_url: e.target.value })} placeholder="https://api.example.com/v1" className="font-mono" />
                </Field>
                <Field>
                  <FieldLabel>API format</FieldLabel>
                  <Select value={editProvider.api_format || 'openai'} onValueChange={(value) => setEditProvider({ ...editProvider, api_format: value === 'systemone' ? 'systemone' : 'openai' })}>
                    <SelectTrigger aria-label="Provider API format">
                      <SelectValue>{editProvider.api_format === 'systemone' ? 'System One (typed questions)' : 'OpenAI-compatible'}</SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="openai">OpenAI-compatible</SelectItem>
                      <SelectItem value="systemone">System One (typed questions)</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel>Auth header</FieldLabel>
                    <Input value={editProvider.auth_header || ''} onChange={(e) => setEditProvider({ ...editProvider, auth_header: e.target.value })} placeholder="Authorization (default)" />
                  </Field>
                  <Field>
                    <FieldLabel>Auth format (%s = key)</FieldLabel>
                    <Input value={editProvider.auth_header_format || ''} onChange={(e) => setEditProvider({ ...editProvider, auth_header_format: e.target.value })} placeholder="Bearer %s (default)" />
                  </Field>
                </div>
                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-chart-3">02 / Limits + protocol</p>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                  <Field>
                    <FieldLabel>Cooldown sec</FieldLabel>
                    <Input value={editProvider.cooldown_seconds ?? ''} onChange={(e) => setEditProvider({ ...editProvider, cooldown_seconds: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel>Timeout sec</FieldLabel>
                    <Input value={editProvider.request_timeout_seconds ?? ''} onChange={(e) => setEditProvider({ ...editProvider, request_timeout_seconds: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel>Max tokens cap</FieldLabel>
                    <Input value={editProvider.max_tokens ?? ''} onChange={(e) => setEditProvider({ ...editProvider, max_tokens: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                </div>
                <div className="grid gap-3 border border-edge-700 bg-edge-950 p-4 text-[11px]">
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.disabled} onCheckedChange={(v) => setEditProvider({ ...editProvider, disabled: v === true })} />
                    Disabled (preserve configuration)
                  </Label>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.no_auth} onCheckedChange={(v) => setEditProvider({ ...editProvider, no_auth: v === true })} />
                    <Tooltip>
                      <TooltipTrigger><span>no_auth</span></TooltipTrigger>
                      <TooltipContent>Keyless endpoint — no credential headers are sent.</TooltipContent>
                    </Tooltip>
                  </Label>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.synthesize_cli_headers} onCheckedChange={(v) => setEditProvider({ ...editProvider, synthesize_cli_headers: v === true })} />
                    synthesize_cli_headers
                  </Label>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox
                      checked={editProvider.strip_reasoning_content !== false}
                      onCheckedChange={(v) => setEditProvider({ ...editProvider, strip_reasoning_content: v === true })}
                    />
                    strip_reasoning_content
                  </Label>
                </div>

                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-chart-2">03 / Credential vault</p>
                {!editingExistingProvider ? (
                  <Field>
                    <FieldLabel htmlFor="ep-keys">API keys</FieldLabel>
                    <Textarea
                      id="ep-keys"
                      value={editProvider.keys_text || ''}
                      onChange={(e) => setEditProvider({ ...editProvider, keys_text: e.target.value })}
                      placeholder="sk-…"
                      rows={4}
                      className="font-mono"
                    />
                    <FieldDescription>One per line; or account_id:api_key. Stored encrypted on save.</FieldDescription>
                  </Field>
                ) : (
                  <div className="space-y-2">
                    <FieldLabel>Existing keys</FieldLabel>
                    {(editProvider.keys || []).map((k) => (
                      <div key={k.id} className="flex min-w-0 flex-wrap items-center justify-between gap-2 border border-edge-700 bg-edge-950 px-3 py-3 text-[11px]">
                        <span className="min-w-0 basis-32 flex-1 break-all text-muted-foreground">{k.masked}{k.account_id ? ` · ${k.account_id}` : ''}</span>
                        <Button
                          variant="destructive"
                          size="xs"
                          className="shrink-0"
                          disabled={busy}
                          onClick={() => {
                            setConfirm({
                              title: `Remove credential ${k.masked}?`,
                              body: 'In-flight requests may still finish. The remaining keys keep serving.',
                              confirmLabel: 'Remove credential',
                              run: () => {
                                void run(async () => {
                                  await deleteAdminProviderKey(editProvider.name, k.id);
                                  const p = (await listAdminProviders()).providers.find((x) => x.name === editProvider.name);
                                  if (p) setEditProvider({ ...editProvider, keys: p.keys, append_key: '' });
                                }, 'Credential removed');
                              },
                            });
                          }}
                        >
                          remove
                        </Button>
                      </div>
                    ))}
                    <Field>
                      <FieldLabel>Append upstream credential (stored encrypted)</FieldLabel>
                      <Input
                        value={editProvider.append_key || ''}
                        onChange={(e) => setEditProvider({ ...editProvider, append_key: e.target.value })}
                        placeholder="sk-… or account_id:api_key"
                        className="font-mono"
                      />
                    </Field>
                  </div>
                )}

                <div className="flex flex-wrap gap-2 border-t border-edge-700 pt-5">
                  <Button disabled={busy} onClick={saveProvider}><Check className="size-3.5" />Save · live apply</Button>
                  <Button variant="outline" onClick={() => setEditProvider(null)}>Cancel</Button>
                </div>
              </CardContent>
            </Card>
          )}
        </div>
      )}

      {section === 'models' && (
        <div className={`grid min-w-0 grid-cols-1 items-start gap-5 ${editModel ? 'xl:grid-cols-[minmax(0,1fr)_minmax(360px,0.8fr)]' : ''}`}>
          <Card className="min-w-0 gap-0 border-edge-700 bg-edge-900 py-0">
            <div className="flex flex-wrap items-center justify-between gap-4 border-b border-edge-700 p-4 sm:p-5">
              <div className="min-w-0">
                <p className="mb-2 text-[10px] uppercase tracking-[0.16em] text-primary">01 / Routing directory</p>
                <CardTitle className="text-sm">Model catalog <span className="ml-2 text-muted-foreground">{filteredModels.length}{mq ? ` / ${models.length}` : ''}</span></CardTitle>
                <CardDescription className="mt-2 text-[11px]">Application IDs, primary targets, and ordered fallback routes.</CardDescription>
              </div>
              <div className="flex min-w-0 flex-1 basis-full flex-wrap items-center gap-2 sm:basis-auto sm:justify-end">
                  <div className="relative min-w-0 flex-1 sm:max-w-60">
                    <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input aria-label="Filter models" value={mq} onChange={(e) => setMq(e.target.value)} placeholder="Search IDs, providers, targets…" className="w-full pl-8" />
                  </div>
                  <Button
                    size="sm"
                    className="shrink-0"
                    onClick={() => {
                      setOriginalModelID('');
                      setEditModel({
                        id: '', backend: providers[0]?.name || '', upstream: '',
                        name: '', context_window: '', max_tokens: '',
                        pricing_input: '', pricing_output: '', pricing_cache: '',
                      });
                    }}
                  >
                    <Plus className="size-3.5" />New model
                  </Button>
                </div>
            </div>
            {(selectedModels.length > 0 || filteredModels.length > 0) && (
              <CardContent className="border-b border-edge-700 bg-edge-950 px-4 py-3 sm:px-5">
                <div className="flex flex-wrap items-center gap-2 text-[11px]">
                  <Button variant="outline" size="xs" disabled={busy || !filteredModels.length} onClick={() => setSelectedModels(filteredModels.map((m) => m.id))}>
                    Select filtered
                  </Button>
                  <span className={selectedModels.length > 0 ? 'text-primary' : 'text-muted-foreground'}>{selectedModels.length} selected</span>
                  <div className="ml-auto flex flex-wrap gap-1.5">
                    <Button variant="outline" size="xs" disabled={busy || !selectedModels.length} onClick={() => bulkAction('enable')}>enable</Button>
                    <Button variant="outline" size="xs" disabled={busy || !selectedModels.length} onClick={() => bulkAction('disable')}>disable</Button>
                    <Button variant="destructive" size="xs" disabled={busy || !selectedModels.length} onClick={() => bulkAction('delete')}>delete</Button>
                  </div>
                </div>
              </CardContent>
            )}
            <CardContent className="px-0 pb-0">
              <div className="overflow-x-auto">
                <Table className="min-w-[800px]">
                  <TableHeader className="bg-edge-950">
                    <TableRow>
                      <TableHead className="w-12 pl-5">
                        <Checkbox
                          aria-label="Select all filtered models"
                          checked={allFilteredSelected}
                          onCheckedChange={(v) => {
                            if (v === true) setSelectedModels(filteredModels.map((m) => m.id));
                            else setSelectedModels([]);
                          }}
                        />
                      </TableHead>
                      <TableHead className="w-[34%]">Application model / pricing</TableHead>
                      <TableHead>Primary target / fallbacks</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead className="text-right">Actions</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {loading && (
                      Array.from({ length: 4 }).map((_, i) => (
                        <TableRow key={i}>
                          <TableCell colSpan={5}><Skeleton className="h-16 w-full rounded-none" /></TableCell>
                        </TableRow>
                      ))
                    )}
                    {!loading && filteredModels.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={5}>
                          <Empty className="py-12">
                            <EmptyHeader>
                              <EmptyTitle>{mq ? 'No models match' : 'No models'}</EmptyTitle>
                              <EmptyDescription>{mq ? 'Adjust the filter.' : 'Add a model manually or via guided add.'}</EmptyDescription>
                            </EmptyHeader>
                          </Empty>
                        </TableCell>
                      </TableRow>
                    )}
                    {filteredModels.map((m) => {
                      const routes: RouteTarget[] = [{ backend: m.backend, upstream: m.upstream }, ...(m.routes || [])];
                      const fallbacks = routes.slice(1);
                      return (
                        <TableRow key={m.id} className={selectedModels.includes(m.id) || editModel?.id === m.id ? 'bg-primary/5' : ''}>
                          <TableCell className="py-5 pl-5 align-top">
                            <Checkbox
                              aria-label={`Select ${m.id}`}
                              checked={selectedModels.includes(m.id)}
                              onCheckedChange={(v) => setSelectedModels(v === true ? [...selectedModels, m.id] : selectedModels.filter((id) => id !== m.id))}
                            />
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <div className="min-w-0 max-w-md whitespace-normal">
                              <div className="break-all text-xs font-medium text-foreground">{m.id}</div>
                              <div className="mt-1 break-words text-[11px] text-muted-foreground">{m.name || m.upstream}</div>
                              <div className="mt-3 flex flex-wrap gap-1.5">
                                <Badge variant="outline" className={m.pricing_known ? 'border-chart-4/30 text-chart-4 font-normal normal-case' : 'border-edge-600 text-muted-foreground font-normal normal-case'}>
                                  {m.pricing_known ? `${m.pricing_input} in / ${m.pricing_output} out · $/1M` : 'Pricing unknown'}
                                </Badge>
                                {m.has_meta && <Badge variant="outline" className="border-chart-2/40 text-chart-2">metadata</Badge>}
                              </div>
                            </div>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <div className="min-w-0 max-w-xs whitespace-normal text-[11px]">
                              <div className="text-chart-2">{m.backend}</div>
                              <div className="mt-1 break-all leading-relaxed text-muted-foreground">{m.upstream}</div>
                              {fallbacks.length > 0 && (
                                <Tooltip>
                                  <TooltipTrigger>
                                    <span className="mt-2 inline-block cursor-help text-chart-3 underline decoration-dotted underline-offset-4">
                                      +{fallbacks.length} fallback{fallbacks.length === 1 ? '' : 's'}
                                    </span>
                                  </TooltipTrigger>
                                  <TooltipContent>
                                    <div className="max-w-sm space-y-2 break-all text-[11px]">
                                      {fallbacks.map((r, i) => <div key={i}>{String(i + 2).padStart(2, '0')} / {r.backend}:{r.upstream}</div>)}
                                    </div>
                                  </TooltipContent>
                                </Tooltip>
                              )}
                            </div>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <Badge variant="outline" className={m.disabled ? 'border-chart-4/40 text-chart-4' : 'border-primary/30 text-primary'}>
                              {m.disabled ? 'Disabled' : 'Enabled'}
                            </Badge>
                          </TableCell>
                          <TableCell className="py-5 pr-5 align-top">
                            <div className="flex min-w-44 flex-wrap justify-end gap-1.5">
                              <Button variant="outline" size="xs" disabled={busy} onClick={() => { void run(async () => { await bulkModels([m.id], m.disabled ? 'enable' : 'disable'); }, m.disabled ? 'Model enabled' : 'Model disabled'); }}>
                                {m.disabled ? 'Enable' : 'Disable'}
                              </Button>
                              <Button
                                variant="outline"
                                size="xs"
                                onClick={() => {
                                  setOriginalModelID(m.id);
                                  setEditModel({
                                    ...m,
                                    context_window: m.context_window ? String(m.context_window) : '',
                                    max_tokens: m.max_tokens ? String(m.max_tokens) : '',
                                    pricing_input: m.pricing_known ? String(m.pricing_input) : '',
                                    pricing_output: m.pricing_known ? String(m.pricing_output) : '',
                                    pricing_cache: m.pricing_known ? String(m.pricing_cache) : '',
                                  });
                                }}
                              >
                                <Pencil className="size-3" />edit
                              </Button>
                              <Button variant="destructive" size="xs" disabled={busy} onClick={() => deleteModel(m.id)}>
                                <Trash2 className="size-3" />Delete
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
            <div className="flex flex-wrap justify-between gap-2 border-t border-edge-700 bg-edge-950 px-5 py-3 text-[10px] text-muted-foreground">
              <span>{loading ? 'Loading catalog…' : `${filteredModels.length} of ${models.length} model routes`}</span>
              <span>Unknown pricing is not free usage</span>
            </div>
          </Card>

          {editModel && (
            <Card ref={modelFormRef} className="order-first min-w-0 gap-0 border-edge-600 bg-edge-900 py-0 xl:order-last">
              <CardHeader className="gap-2 border-b border-edge-700 bg-edge-950 p-5">
                <p className="text-[10px] uppercase tracking-[0.16em] text-chart-3">Model editor</p>
                <CardTitle className="break-all text-base">{editingExistingModel ? editModel.id : 'New model route'}</CardTitle>
                <CardDescription className="text-[11px] leading-relaxed">Stage routing and metadata here, then save to apply live. Guided add can discover and prefill models.</CardDescription>
              </CardHeader>
              <CardContent className="space-y-5 p-5">
                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-primary">01 / Identity + primary route</p>
                <Field>
                  <FieldLabel htmlFor="em-id">Verloc model id</FieldLabel>
                  <Input
                    id="em-id"
                    value={editModel.id}
                    onChange={(e) => setEditModel({ ...editModel, id: e.target.value })}
                    disabled={editingExistingModel}
                    placeholder="xx/org/model"
                    className="font-mono"
                  />
                  <FieldDescription>Short unique prefix, then /upstream-id.</FieldDescription>
                </Field>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel>Backend</FieldLabel>
                    <Select value={editModel.backend || ''} onValueChange={(v) => setEditModel({ ...editModel, backend: v ?? '' })}>
                      <SelectTrigger aria-label="Primary model provider">
                        <SelectValue placeholder="Choose a provider…" />
                      </SelectTrigger>
                      <SelectContent>
                        {providers.map((p) => <SelectItem key={p.name} value={p.name}>{p.name}</SelectItem>)}
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field>
                    <FieldLabel>Upstream model id</FieldLabel>
                    <Input value={editModel.upstream || ''} onChange={(e) => setEditModel({ ...editModel, upstream: e.target.value })} className="font-mono" />
                  </Field>
                </div>
                <div className="flex flex-wrap items-center gap-3 border border-edge-700 bg-edge-950 p-3">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy || !editModel.upstream?.trim()}
                    onClick={async () => {
                      const upstream = editModel.upstream?.trim() || '';
                      setBusy(true);
                      setErr(null);
                      try {
                        const result = await lookupModelHints([upstream]);
                        const local = metaFromMatch(upstream, findCatalogMatch(upstream, models, editModel.backend || ''));
                        const hint = result.hints[upstream];
                        if (!hint && !local.src) { setErr(`No metadata found for ${upstream}`); return; }
                        const meta = hint ? fillMetaGaps(upstream, metaFromHint(upstream, hint), local) : local;
                        setEditModel((current) => !current || current.upstream?.trim() !== upstream ? current : {
                          ...current, name: meta.dn, description: meta.description, owned_by: meta.owned_by,
                          tags: meta.tags, context_window: meta.ctx, max_tokens: meta.mx,
                          pricing_input: meta.pi, pricing_output: meta.po, pricing_cache: meta.pc,
                        });
                        toast.success(`Metadata loaded from ${meta.src}`);
                      } catch (error) {
                        setErr(error instanceof Error ? error.message : 'Metadata lookup failed');
                      } finally {
                        setBusy(false);
                      }
                    }}
                  >
                    <Search className="size-3.5" />Lookup catalog
                  </Button>
                  <span className="min-w-0 basis-40 flex-1 text-[11px] leading-relaxed text-muted-foreground">Match the upstream ID to prefill name, limits, and known pricing.</span>
                </div>
                <Field>
                  <FieldLabel>Display name</FieldLabel>
                  <Input value={editModel.name || ''} onChange={(e) => setEditModel({ ...editModel, name: e.target.value })} />
                </Field>
                <Label className="flex cursor-pointer items-center gap-2 text-xs font-normal">
                  <Checkbox checked={!!editModel.disabled} onCheckedChange={(v) => setEditModel({ ...editModel, disabled: v === true })} />
                  Disabled (preserve route)
                </Label>

                <details className="border border-edge-700 bg-edge-950 p-4" open={(editModel.routes || []).length > 0}>
                  <summary className="cursor-pointer text-[11px] font-medium text-chart-3">02 / Ordered fallback routes · {(editModel.routes || []).length}</summary>
                  <div className="mt-3 space-y-4 border-t border-edge-700 pt-4">
                    <p className="text-[11px] leading-relaxed text-muted-foreground">The primary route is tried first. Additional routes are bounded by gateway retry limits and never used after streaming begins.</p>
                    {(editModel.routes || []).map((route, index) => (
                      <div key={index} className="grid grid-cols-1 gap-3 border-l-2 border-chart-3/40 pl-3 sm:grid-cols-2">
                        <Field>
                          <FieldLabel>{`Route ${index + 2} / provider`}</FieldLabel>
                          <Select
                            value={route.backend}
                            onValueChange={(v) => setEditModel({ ...editModel, routes: (editModel.routes || []).map((r, i) => i === index ? { ...r, backend: v ?? '' } : r) })}
                          >
                            <SelectTrigger aria-label={`Fallback provider ${index + 1}`}>
                              <SelectValue placeholder="provider…" />
                            </SelectTrigger>
                            <SelectContent>
                              {providers.map((p) => <SelectItem key={p.name} value={p.name}>{p.name}</SelectItem>)}
                            </SelectContent>
                          </Select>
                        </Field>
                        <Field>
                          <FieldLabel>Upstream model ID</FieldLabel>
                          <Input value={route.upstream} onChange={(e) => setEditModel({ ...editModel, routes: (editModel.routes || []).map((r, i) => i === index ? { ...r, upstream: e.target.value } : r) })} className="font-mono" />
                        </Field>
                        <div className="flex items-end sm:col-span-2">
                          <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, routes: (editModel.routes || []).filter((_, i) => i !== index) })}>
                            Remove fallback
                          </Button>
                        </div>
                      </div>
                    ))}
                    <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, routes: [...(editModel.routes || []), { backend: providers[0]?.name || '', upstream: '' }] })}>
                      Add fallback route
                    </Button>
                  </div>
                </details>

                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-chart-2">03 / Context + output limits</p>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel>Context window</FieldLabel>
                    <Input value={editModel.context_window ?? ''} onChange={(e) => setEditModel({ ...editModel, context_window: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel>Max output tokens</FieldLabel>
                    <Input value={editModel.max_tokens ?? ''} onChange={(e) => setEditModel({ ...editModel, max_tokens: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                </div>
                <div className="space-y-2 border-b border-edge-700 pb-3">
                  <p className="text-[10px] uppercase tracking-[0.14em] text-chart-4">04 / Pricing per million tokens</p>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">Leave prices blank when unknown. Enter zero only when the provider explicitly offers free usage.</p>
                </div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                  <Field>
                    <FieldLabel>Price in $/1M</FieldLabel>
                    <Input value={editModel.pricing_input ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_input: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel>Out $/1M</FieldLabel>
                    <Input value={editModel.pricing_output ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_output: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel>Cache $/1M</FieldLabel>
                    <Input value={editModel.pricing_cache ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_cache: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                </div>
                <Separator />
                <div className="flex flex-wrap items-center justify-between gap-3 pt-1">
                  <div className="flex flex-wrap gap-2">
                    <Button disabled={busy} onClick={saveModel}><Check className="size-3.5" />Save · live apply</Button>
                    <Button variant="outline" onClick={() => setEditModel(null)}>Cancel</Button>
                  </div>
                  {editingExistingModel && (
                    <Button variant="destructive" disabled={busy} onClick={() => deleteModel(editModel.id)}>
                      delete model
                    </Button>
                  )}
                </div>
              </CardContent>
            </Card>
          )}
        </div>
      )}

      {/* ---- Explicit overwrite / destructive confirm ---- */}
      <AlertDialog open={!!confirm} onOpenChange={(open) => { if (!open) setConfirm(null); }}>
        <AlertDialogContent className="border-edge-600 bg-edge-900">
          <AlertDialogHeader>
            <AlertDialogTitle className="break-words text-base">{confirm?.title}</AlertDialogTitle>
            <AlertDialogDescription className="break-words text-xs leading-relaxed">{confirm?.body}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                confirm?.run();
                setConfirm(null);
              }}
            >
              {confirm?.confirmLabel || 'Confirm'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
