import { useEffect, useState, type ReactNode } from 'react';
import { ArrowRight, KeyRound, Loader2, LockKeyhole, Network, ShieldCheck, TriangleAlert } from 'lucide-react';
import { getSession, login, setup, type Session } from '../api';
import { MicroSlats } from './MicroSlats';
import { Button } from './ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './ui/card';
import { Field, FieldLabel } from './ui/field';
import { Input } from './ui/input';

export default function Auth({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [token, setToken] = useState('');
  const load = async () => {
    setError('');
    try { setSession(await getSession()); } catch (e) { setError(e instanceof Error ? e.message : 'Unable to load session'); }
  };
  useEffect(() => {
    void load();
    const expired = () => { setSession({ authenticated: false, setup_required: false }); setError('Your session expired. Sign in again.'); };
    const signedOut = () => { setSession({ authenticated: false, setup_required: false }); };
    window.addEventListener('verloc:unauthorized', expired);
    window.addEventListener('verloc:logout', signedOut);
    return () => { window.removeEventListener('verloc:unauthorized', expired); window.removeEventListener('verloc:logout', signedOut); };
  }, []);
  if (session?.authenticated) return children;
  const title = !session ? 'Connecting to gateway' : session.setup_required ? 'Set up owner access' : 'Sign in to verloc';
  return (
    <MicroSlats className="h-dvh overflow-y-auto">
      <div className="flex min-h-full flex-col">
        <header className="flex shrink-0 items-center justify-between gap-4 border-b border-edge-700 px-6 py-5 md:px-10">
          <div className="flex items-center gap-3">
            <div className="brand-mark" aria-hidden="true"><img src="/dash/logo.png" alt="" /></div>
            <span className="text-2xl font-medium tracking-[-.07em]">verloc<span className="text-primary">_</span></span>
          </div>
          <span className="console-kicker hidden text-edge-500 sm:inline">Self-hosted / OpenAI-compatible</span>
          <LockKeyhole className="size-4 text-edge-500 sm:hidden" aria-hidden="true" />
        </header>
        <main className="mx-auto grid w-full max-w-[1360px] flex-1 items-center gap-12 px-6 py-10 lg:grid-cols-2 lg:gap-20 lg:px-12 lg:py-16">
          <section className="hidden lg:block" aria-label="Gateway architecture">
            <div className="console-section-marker console-kicker text-primary">The control plane</div>
            <h1 className="mt-6 text-[44px] leading-[1.2] font-medium tracking-[-.06em]">LLM gateway<br /><span className="text-edge-400">Operations console</span></h1>
            <p className="mt-5 max-w-[420px] text-[12px] leading-[1.9] text-edge-400">Manage provider connections, model routing, application access, and request analytics.</p>
            <div className="my-10 max-w-[460px] border border-edge-700 bg-edge-950/70 p-6">
              <div className="console-kicker mb-5 text-[9px] text-edge-500">Request path / 01</div>
              <div className="flex items-center gap-3">
                <div className="border border-edge-600 bg-edge-900 px-3 py-4 text-[10px] text-edge-300">Applications</div>
                <div className="h-px min-w-4 flex-1 bg-edge-600" />
                <div className="border border-primary/40 bg-primary/5 px-5 py-4 text-[12px] text-primary">verloc</div>
                <div className="h-px min-w-4 flex-1 bg-edge-600" />
                <div className="border border-cyber-purple/30 bg-cyber-purple/5 px-3 py-4 text-[10px] text-cyber-purple">Providers</div>
              </div>
              <div className="mt-4 flex items-center justify-between text-[9px] text-edge-500"><span>Scoped API keys</span><span>Managed upstreams</span></div>
            </div>
            <div className="flex max-w-[460px] items-start gap-3 border-t border-edge-700 pt-5">
              <ShieldCheck className="mt-0.5 size-4 shrink-0 text-primary" strokeWidth={1.5} aria-hidden="true" />
              <div><div className="text-[11px] text-edge-200">Metadata-only analytics</div><p className="mt-1 text-[11px] leading-relaxed text-edge-500">Prompts and completions are forwarded to your providers, not stored in analytics.</p></div>
            </div>
          </section>
          <section className="mx-auto w-full max-w-[430px]" aria-label="Owner authentication">
            <div className="mb-4 flex items-center justify-between text-[9px] uppercase tracking-[.15em] text-edge-500"><span>Owner access</span><span>Gateway administration</span></div>
            <Card className="border-edge-600 [--card-spacing:--spacing(7)]">
              <CardHeader>
                <div className="mb-3 flex size-10 items-center justify-center border border-primary/25 bg-primary/5 text-primary"><KeyRound className="size-4" strokeWidth={1.5} aria-hidden="true" /></div>
                <CardTitle className="text-[22px] tracking-[-.05em]">{title}</CardTitle>
                <CardDescription className="mt-2">{session?.setup_required ? 'Create the single-owner account to start managing your gateway.' : 'Sign in to your gateway control plane.'}</CardDescription>
              </CardHeader>
              <CardContent>
                <form className="space-y-5" onSubmit={async (event) => {
                  event.preventDefault();
                  if (!session) return;
                  if (session.setup_required && (password.length < 12 || password !== confirmation)) { setError('Use at least 12 characters and make sure both passwords match.'); return; }
                  setBusy(true); setError('');
                  try {
                    const next = session.setup_required ? await setup(token, password) : await login(password);
                    setPassword(''); setConfirmation(''); setToken(''); setSession(next);
                  } catch (e) { setError(e instanceof Error ? e.message : 'Sign in failed'); }
                  finally { setBusy(false); }
                }}>
                  {error && <div role="alert" className="flex items-start gap-2 border border-destructive/30 bg-destructive/5 p-3 text-[11px] leading-relaxed text-destructive"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" /><span>{error}</span></div>}
                  {!session ? <>
                    <p className="text-[11px] leading-relaxed text-edge-400">Checking the owner session with the gateway.</p>
                    <Button variant="outline" className="w-full" onClick={() => void load()}><Network aria-hidden="true" />Retry connection</Button>
                  </> : <>
                    {session.setup_required && <>
                      <p className="border-l-2 border-cyber-blue/40 pl-3 text-[11px] leading-relaxed text-edge-400">Read the setup token from the gateway host's private setup-token file (default: verloc-data/setup.token). Setup is available only once.</p>
                      <Field>
                        <FieldLabel htmlFor="verloc-setup-token">Setup token</FieldLabel>
                        <Input id="verloc-setup-token" type="password" autoComplete="one-time-code" value={token} onChange={(event) => setToken(event.target.value)} />
                      </Field>
                    </>}
                    <Field>
                      <FieldLabel htmlFor="verloc-password">{session.setup_required ? 'Choose a password (12+ characters)' : 'Password'}</FieldLabel>
                      <Input id="verloc-password" type="password" autoComplete={session.setup_required ? 'new-password' : 'current-password'} placeholder="Enter your password" value={password} onChange={(event) => setPassword(event.target.value)} />
                    </Field>
                    {session.setup_required && <Field>
                      <FieldLabel htmlFor="verloc-password-confirm">Confirm password</FieldLabel>
                      <Input id="verloc-password-confirm" type="password" autoComplete="new-password" value={confirmation} onChange={(event) => setConfirmation(event.target.value)} />
                    </Field>}
                    <Button type="submit" disabled={busy || !password || (session.setup_required && !token)} className="h-10 w-full justify-between px-4">
                      {busy ? 'Authenticating…' : session.setup_required ? 'Create owner account' : 'Sign in'}
                      {busy ? <Loader2 className="animate-spin" aria-hidden="true" /> : <ArrowRight aria-hidden="true" />}
                    </Button>
                    <div className="flex items-start gap-2 border-t border-edge-700 pt-4 text-[10px] leading-[1.8] text-edge-500"><LockKeyhole className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" /><p>Owner access is separate from application API keys. Sessions use HttpOnly cookies; application-key secrets are not saved to browser storage.</p></div>
                  </>}
                </form>
              </CardContent>
            </Card>
            <p className="mt-5 text-center text-[9px] uppercase tracking-[.12em] text-edge-500">Single-owner administration</p>
          </section>
        </main>
        <footer className="flex flex-wrap justify-between gap-2 border-t border-edge-700 px-6 py-4 text-[9px] uppercase tracking-[.13em] text-edge-500 md:px-10"><span>verloc / gateway control plane</span><span>Self-hosted / SQLite storage</span></footer>
      </div>
    </MicroSlats>
  );
}
