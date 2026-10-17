import { useEffect, useState, type FormEvent } from 'react';
import { toast } from 'sonner';
import { Ban, Check, Copy, KeyRound, Pencil, Plus, RefreshCw, RotateCw } from 'lucide-react';
import {
  createClientKey, editClientKey, fmtDateTime, listAdminModels, listClientKeys,
  rotateClientKey, revokeClientKey,
  type AdminModel, type ClientKey, type ClientKeyInput,
} from '../api';
import {
  AlertDialog, AlertDialogContent, AlertDialogDescription, AlertDialogFooter,
  AlertDialogHeader, AlertDialogTitle,
} from '../components/ui/alert-dialog';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Checkbox } from '../components/ui/checkbox';
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '../components/ui/dialog';
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Field, FieldDescription, FieldLabel, FieldLegend, FieldSet } from '../components/ui/field';
import { Input } from '../components/ui/input';
import { Label } from '../components/ui/label';
import { Skeleton } from '../components/ui/skeleton';
import {
  Sheet, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle,
} from '../components/ui/sheet';
import { Switch } from '../components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';

const blank: ClientKeyInput = { name: '', allowed_models: ['*'], expires_at: 0, rpm: 0, tpm: 0, max_concurrent: 0 };

const toLocalInput = (ts: number) =>
  new Date(ts - new Date(ts).getTimezoneOffset() * 60000).toISOString().slice(0, 16);

export default function ClientKeys() {
  const [keys, setKeys] = useState<ClientKey[]>([]);
  const [models, setModels] = useState<AdminModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState<ClientKeyInput | null>(null);
  const [editing, setEditing] = useState('');
  const [formError, setFormError] = useState('');
  const [expiry, setExpiry] = useState('');
  const [hasExpiry, setHasExpiry] = useState(false);
  const [confirm, setConfirm] = useState<{ action: 'rotate' | 'revoke'; key: ClientKey } | null>(null);
  const [confirmBusy, setConfirmBusy] = useState(false);
  const [confirmError, setConfirmError] = useState('');
  // One-time secret: memory only, never written to browser storage.
  const [secret, setSecret] = useState('');
  const [copied, setCopied] = useState(false);
  const [secretError, setSecretError] = useState('');

  const refresh = async () => {
    setLoadError('');
    try {
      setKeys((await listClientKeys()).keys || []);
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : 'Could not load API keys');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
    listAdminModels()
      .then((m) => setModels(m.models || []))
      .catch((e) => setLoadError(e instanceof Error ? e.message : 'Could not load model scopes'));
  }, []);

  const openCreate = () => {
    setDraft({ ...blank, allowed_models: ['*'] });
    setEditing('');
    setExpiry('');
    setHasExpiry(false);
    setFormError('');
  };

  const openEdit = (key: ClientKey) => {
    setEditing(key.id);
    setDraft({
      name: key.name,
      allowed_models: key.allowed_models || [],
      expires_at: key.expires_at,
      rpm: key.rpm,
      tpm: key.tpm,
      max_concurrent: key.max_concurrent,
    });
    setHasExpiry(key.expires_at > 0);
    setExpiry(key.expires_at > 0 ? toLocalInput(key.expires_at) : '');
    setFormError('');
  };

  const submitDraft = async (event: FormEvent) => {
    event.preventDefault();
    if (!draft) return;
    const expires = hasExpiry ? new Date(expiry).getTime() : 0;
    if (
      !draft.name.trim() ||
      !Number.isFinite(expires) ||
      (expires > 0 && expires <= Date.now()) ||
      [draft.rpm, draft.tpm, draft.max_concurrent].some((v) => v < 0 || !Number.isInteger(v))
    ) {
      setFormError('Enter a name, a future expiry (or switch expiry off), and non-negative integer limits.');
      return;
    }
    setBusy(true);
    setFormError('');
    try {
      const body = { ...draft, name: draft.name.trim(), expires_at: expires };
      if (editing) {
        await editClientKey(editing, body);
        toast.success('API key policy saved.');
      } else {
        const response = await createClientKey(body);
        setSecret(response.key);
        setCopied(false);
        setSecretError('');
        toast.success('API key created. Save the secret now — it is shown only once.');
      }
      setDraft(null);
      setEditing('');
      await refresh();
    } catch (e) {
      setFormError(e instanceof Error ? e.message : 'API key operation failed');
    } finally {
      setBusy(false);
    }
  };

  const doConfirm = async () => {
    if (!confirm) return;
    setConfirmBusy(true);
    setConfirmError('');
    try {
      if (confirm.action === 'rotate') {
        const result = await rotateClientKey(confirm.key.id);
        setSecret(result.key);
        setCopied(false);
        setSecretError('');
        toast.success('API key rotated. Update your application now.');
      } else {
        await revokeClientKey(confirm.key.id);
        toast.success('API key revoked. Attribution history is retained.');
      }
      setConfirm(null);
      await refresh();
    } catch (e) {
      setConfirmError(e instanceof Error ? e.message : 'API key operation failed');
    } finally {
      setConfirmBusy(false);
    }
  };

  const copySecret = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
    } catch {
      setSecretError('Clipboard unavailable. Select and copy the secret manually.');
    }
  };

  const allModels = !draft || draft.allowed_models.includes('*');
  const scopeIds = draft
    ? [...models.map((m) => m.id), ...draft.allowed_models].filter((id, i, arr) => arr.indexOf(id) === i)
    : [];

  const now = Date.now();
  const activeKeys = keys.filter((key) => !key.revoked_at && (!key.expires_at || key.expires_at > now));
  const expiredKeys = keys.filter((key) => !key.revoked_at && key.expires_at > 0 && key.expires_at <= now);
  const summary = [
    { label: 'Credentials', value: keys.length, detail: 'Including retained history', accent: '' },
    { label: 'Active', value: activeKeys.length, detail: `${expiredKeys.length} expired`, accent: 'text-primary' },
    { label: 'Model restricted', value: activeKeys.filter((key) => key.allowed_models?.length && !key.allowed_models.includes('*')).length, detail: 'Among active credentials', accent: 'text-chart-2' },
    { label: 'Revoked', value: keys.filter((key) => key.revoked_at).length, detail: 'Attribution retained', accent: 'text-muted-foreground' },
  ];

  return (
    <div className="min-w-0 space-y-5">
      <div className="grid grid-cols-2 border border-edge-700 bg-edge-900 lg:grid-cols-4">
        {summary.map((item) => (
          <div key={item.label} className="min-w-0 border-b border-r border-edge-700 px-5 py-5 last:border-r-0 lg:border-b-0">
            <p className="text-[10px] tracking-[0.16em] text-muted-foreground uppercase">{item.label}</p>
            <p className={`mt-3 text-3xl tracking-tight tabular-nums ${item.accent}`}>{loading || loadError ? '—' : item.value}</p>
            <p className="mt-2 text-[10px] text-muted-foreground">{item.detail}</p>
          </div>
        ))}
      </div>
      {loadError && (
        <div className="flex flex-wrap items-center gap-3 border border-destructive/40 bg-destructive/10 px-4 py-3 text-sm">
          <p role="alert" className="flex-1 text-destructive">{loadError}</p>
          <Button variant="outline" size="sm" onClick={() => { setLoading(true); void refresh(); }}>
            Retry
          </Button>
        </div>
      )}

      <Card className="gap-0 overflow-hidden border-edge-700 bg-edge-900">
        <CardHeader className="gap-4 border-b border-edge-700 py-5">
          <CardTitle className="text-xs tracking-[0.12em] uppercase">01 / Credential registry</CardTitle>
          <CardDescription>Application access only. Owner sessions and upstream provider secrets are managed separately.</CardDescription>
          <CardAction className="self-center">
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" size="sm" disabled={busy || loading} onClick={() => void refresh()}>
                <RefreshCw />Refresh
              </Button>
              <Button size="sm" onClick={openCreate}>
                <Plus />Create API key
              </Button>
            </div>
          </CardAction>
        </CardHeader>
        <CardContent className="px-0 py-0">
          {loading && (
            <div className="space-y-3 p-5">
              <Skeleton className="h-16 w-full" />
              <Skeleton className="h-16 w-full" />
              <Skeleton className="h-16 w-full" />
            </div>
          )}
          {!loading && keys.length === 0 && !loadError && (
            <Empty>
              <EmptyHeader>
                <EmptyTitle>No API keys yet</EmptyTitle>
                <EmptyDescription>Create a scoped key for your first application.</EmptyDescription>
              </EmptyHeader>
              <EmptyContent>
                <Button size="sm" onClick={openCreate}><Plus />Create API key</Button>
              </EmptyContent>
            </Empty>
          )}
          {!loading && keys.length > 0 && (
            <Table className="min-w-[1060px]">
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-5">Application / prefix</TableHead>
                  <TableHead>Model access</TableHead>
                  <TableHead>Admission policy</TableHead>
                  <TableHead>Lifecycle</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((key) => {
                  const status = key.revoked_at ? 'Revoked' : key.expires_at > 0 && key.expires_at < Date.now() ? 'Expired' : 'Active';
                  return (
                    <TableRow key={key.id} className={key.revoked_at ? 'opacity-60' : 'hover:bg-edge-800/60'}>
                      <TableCell className="py-5 pl-5 whitespace-normal">
                        <div className="flex min-w-0 items-center gap-3">
                          <span className="flex size-9 shrink-0 items-center justify-center border border-edge-700 bg-edge-950 text-primary"><KeyRound className="size-4" /></span>
                          <div className="min-w-0 break-all">
                            <div className="font-medium text-foreground">{key.name}</div>
                            <div className="mt-1 text-[11px] text-muted-foreground">{key.prefix}…</div>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell className="max-w-56 whitespace-normal">
                        <div className="text-xs leading-relaxed break-all text-muted-foreground">
                          {key.allowed_models?.includes('*') || !key.allowed_models?.length ? <Badge variant="outline">All models (*)</Badge> : key.allowed_models.join(', ')}
                        </div>
                      </TableCell>
                      <TableCell className="text-[11px] whitespace-nowrap">
                        <div><span className="text-muted-foreground">RPM</span> {key.rpm || 'unlimited'}</div>
                        <div className="mt-1"><span className="text-muted-foreground">TPM</span> {key.tpm || 'unlimited'}</div>
                        <div className="mt-1"><span className="text-muted-foreground">Concurrent</span> {key.max_concurrent || 'unlimited'}</div>
                      </TableCell>
                      <TableCell className="text-xs whitespace-nowrap">
                        <div>{fmtDateTime(key.created_at)}</div>
                        <div className="text-muted-foreground">
                          {key.expires_at > 0 ? `Expires ${fmtDateTime(key.expires_at)}` : 'Never expires'}
                        </div>
                      </TableCell>
                      <TableCell>
                        <Badge variant={status === 'Revoked' ? 'destructive' : 'outline'} className={status === 'Expired' ? 'border-chart-4/40 text-chart-4' : status === 'Active' ? 'border-primary/40 text-primary' : ''}>
                          {status}
                        </Badge>
                      </TableCell>
                      <TableCell className="pr-5 text-right">
                        {!key.revoked_at && (
                          <div className="flex justify-end gap-2">
                            <Button variant="outline" size="sm" disabled={busy} onClick={() => openEdit(key)}>
                              <Pencil />Edit
                            </Button>
                            <Button
                              variant="outline"
                              size="sm"
                              disabled={busy}
                              onClick={() => { setConfirm({ action: 'rotate', key }); setConfirmError(''); }}
                            >
                              <RotateCw />Rotate
                            </Button>
                            <Button
                              variant="destructive"
                              size="sm"
                              disabled={busy}
                              onClick={() => { setConfirm({ action: 'revoke', key }); setConfirmError(''); }}
                            >
                              <Ban />Revoke
                            </Button>
                          </div>
                        )}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
        <div className="border-t border-edge-700 bg-edge-950/60 px-5 py-3 text-[10px] leading-relaxed text-muted-foreground">
          Zero limits mean unlimited within gateway-wide bounds. Secrets cannot be retrieved after creation or rotation.
        </div>
      </Card>

      <Sheet open={draft !== null} onOpenChange={(open) => { if (!open && !busy) setDraft(null); }}>
        <SheetContent className="w-full overflow-y-auto border-edge-700 bg-edge-900 sm:max-w-xl">
          <SheetHeader className="border-b border-edge-700 p-6">
            <p className="text-[10px] tracking-[0.16em] text-primary uppercase">Access policy / {editing ? 'edit' : 'new credential'}</p>
            <SheetTitle>{editing ? 'Edit API key policy' : 'Create client API key'}</SheetTitle>
            <SheetDescription>
              {editing
                ? 'Update scopes, limits and expiry. The secret itself never changes here.'
                : 'The secret is revealed once after creation and never stored in the browser.'}
            </SheetDescription>
          </SheetHeader>
          {draft && (
            <form className="space-y-6 px-6 pb-6" onSubmit={(e) => void submitDraft(e)}>
              {formError && <p role="alert" className="text-sm text-destructive">{formError}</p>}
              <p className="text-[10px] tracking-[0.14em] text-muted-foreground uppercase">01 / Application identity</p>
              <Field>
                <FieldLabel htmlFor="key-name">Application name</FieldLabel>
                <Input
                  id="key-name"
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                  placeholder="my-app"
                />
              </Field>
              <div className="flex items-center justify-between gap-3 border border-edge-700 bg-edge-950 p-4">
                <div>
                  <Label htmlFor="key-expiry-switch">Set expiry</Label>
                  <p className="text-xs text-muted-foreground">Off means the key never expires.</p>
                </div>
                <Switch
                  id="key-expiry-switch"
                  checked={hasExpiry}
                  onCheckedChange={(v) => {
                    const on = v === true;
                    setHasExpiry(on);
                    if (on && !expiry) setExpiry(toLocalInput(Date.now() + 30 * 86400000));
                  }}
                />
              </div>
              {hasExpiry && (
                <Field>
                  <FieldLabel htmlFor="key-expiry">Expires at (local time)</FieldLabel>
                  <Input
                    id="key-expiry"
                    type="datetime-local"
                    value={expiry}
                    onChange={(e) => setExpiry(e.target.value)}
                  />
                </Field>
              )}
              <FieldSet className="border-t border-edge-700 pt-5">
                <FieldLegend className="text-xs tracking-[0.12em] uppercase">02 / Model access</FieldLegend>
                <div className="flex items-center justify-between gap-3">
                  <div>
                    <Label htmlFor="key-all-models">All models</Label>
                    <FieldDescription>Includes future models added later.</FieldDescription>
                  </div>
                  <Switch
                    id="key-all-models"
                    checked={allModels}
                    onCheckedChange={(v) => {
                      if (v === true) setDraft({ ...draft, allowed_models: ['*'] });
                      else setDraft({ ...draft, allowed_models: models.map((m) => m.id) });
                    }}
                  />
                </div>
                {!allModels && (
                  <div className="max-h-72 space-y-3 overflow-y-auto border border-edge-700 bg-edge-950 p-4">
                    <FieldDescription>Select specific models to restrict access. Disabled models remain unavailable.</FieldDescription>
                    {scopeIds.map((id) => (
                      <Label key={id} className="flex min-w-0 cursor-pointer items-start gap-2 font-normal">
                        <Checkbox
                          className="shrink-0"
                          checked={draft.allowed_models.includes(id)}
                          onCheckedChange={(v) => setDraft({
                            ...draft,
                            allowed_models: v === true
                              ? [...draft.allowed_models, id]
                              : draft.allowed_models.filter((m) => m !== id),
                          })}
                        />
                        <span className="min-w-0 break-all font-mono text-xs">{id}</span>
                      </Label>
                    ))}
                    {scopeIds.length === 0 && (
                      <p className="text-xs text-muted-foreground">No models known yet — the key will match no models until scopes are added.</p>
                    )}
                  </div>
                )}
              </FieldSet>
              <p className="border-t border-edge-700 pt-5 text-[10px] tracking-[0.14em] text-muted-foreground uppercase">03 / Admission limits</p>
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
                {(['rpm', 'tpm', 'max_concurrent'] as const).map((field) => (
                  <Field key={field}>
                    <FieldLabel htmlFor={`key-${field}`}>
                      {field === 'rpm' ? 'Requests / minute' : field === 'tpm' ? 'Tokens / minute' : 'Concurrent requests'}
                    </FieldLabel>
                    <Input
                      id={`key-${field}`}
                      type="number"
                      min={0}
                      step={1}
                      value={String(draft[field])}
                      onChange={(e) => setDraft({ ...draft, [field]: Number(e.target.value) })}
                    />
                  </Field>
                ))}
              </div>
              <FieldDescription>
                TPM is conservatively reserved before an upstream request and reconciled when token
                usage is known. Zero means unlimited. Limits are not billing budgets.
              </FieldDescription>
              <SheetFooter className="border-t border-edge-700 px-0 pt-5">
                <Button type="button" variant="outline" disabled={busy} onClick={() => setDraft(null)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={busy}>
                  {busy ? 'Saving…' : editing ? 'Save policy' : 'Create key'}
                </Button>
              </SheetFooter>
            </form>
          )}
        </SheetContent>
      </Sheet>

      <AlertDialog open={confirm !== null} onOpenChange={(open) => { if (!open && !confirmBusy) setConfirm(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {confirm?.action === 'rotate' ? `Rotate ${confirm.key.name}?` : `Revoke ${confirm?.key.name}?`}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.action === 'rotate'
                ? 'Its current secret stops working immediately. The new secret is shown once.'
                : 'This immediately blocks new requests and cannot be undone. History is retained.'}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {confirmError && <p role="alert" className="text-sm text-destructive">{confirmError}</p>}
          <AlertDialogFooter>
            <Button variant="outline" disabled={confirmBusy} onClick={() => setConfirm(null)}>
              Cancel
            </Button>
            <Button
              variant={confirm?.action === 'revoke' ? 'destructive' : 'default'}
              disabled={confirmBusy}
              onClick={() => void doConfirm()}
            >
              {confirmBusy ? 'Working…' : confirm?.action === 'rotate' ? 'Rotate key' : 'Revoke key'}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={secret !== ''}>
        <DialogContent showCloseButton={false} className="gap-6 border-primary/50 bg-edge-900 sm:max-w-xl">
          <DialogHeader>
            <div className="mb-2 flex items-center gap-2 text-[10px] tracking-[0.16em] text-primary uppercase"><KeyRound className="size-4" />One-time secret / action required</div>
            <DialogTitle className="text-xl">Save your API key now</DialogTitle>
            <DialogDescription>
              This secret is shown only once. Save it to your application secret manager.
              It is never written to browser storage.
            </DialogDescription>
          </DialogHeader>
          <div className="border border-primary/30 bg-primary/5 p-5">
            <p className="mb-3 text-[10px] tracking-[0.12em] text-primary uppercase">Copy to your secret manager</p>
            <code className="block text-sm leading-relaxed break-all text-foreground select-all">{secret}</code>
          </div>
          {secretError && <p role="alert" className="text-sm text-destructive">{secretError}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => void copySecret()}>
              {copied ? <Check /> : <Copy />}{copied ? 'Copied' : 'Copy secret'}
            </Button>
            <Button onClick={() => setSecret('')}>I saved it · close</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
