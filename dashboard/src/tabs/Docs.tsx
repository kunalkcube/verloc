import { useState, type ReactNode } from 'react';
import { Check, Copy } from 'lucide-react';
import { Button } from '../components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';

const SECTIONS: Array<[string, string, string]> = [
  ['01', 'Overview', 'overview'],
  ['02', 'Authentication', 'authentication'],
  ['03', 'Endpoints', 'endpoints'],
  ['04', 'Chat Completions', 'chat-completions'],
  ['05', 'Responses', 'responses'],
  ['06', 'System One', 'system-one'],
  ['07', 'Anthropic Messages', 'anthropic-messages'],
  ['08', 'Utility endpoints', 'utility-endpoints'],
  ['09', 'Errors', 'errors'],
  ['10', 'Failover', 'failover'],
  ['11', 'Model routing', 'model-routing'],
  ['12', 'Client keys', 'client-keys'],
  ['13', 'Configuration', 'configuration'],
  ['14', 'Platform features', 'features'],
  ['15', 'Compatibility', 'compatibility'],
];

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const [failed, setFailed] = useState(false);
  const copy = () => {
    // navigator.clipboard is unavailable outside secure contexts (plain HTTP).
    if (!navigator.clipboard) { setFailed(true); window.setTimeout(() => setFailed(false), 1500); return; }
    navigator.clipboard.writeText(text).then(
      () => { setCopied(true); window.setTimeout(() => setCopied(false), 1500); },
      () => { setFailed(true); window.setTimeout(() => setFailed(false), 1500); },
    );
  };
  const label = copied ? 'Copied' : failed ? 'Copy unavailable' : 'Copy to clipboard';
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-sm"
      aria-label={label}
      title={label}
      className="absolute top-2 right-2 text-edge-500 hover:text-primary"
      onClick={copy}
    >
      {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
    </Button>
  );
}

function CodeBlock({ code, label }: { code: string; label?: string }) {
  return (
    <div className="border border-edge-700 bg-edge-950">
      {label && <div className="border-b border-edge-700 px-4 py-2 text-[9px] tracking-[.18em] text-edge-500 uppercase">{label}</div>}
      <div className="relative">
        <CopyButton text={code} />
        <pre className="overflow-x-auto p-4 pr-12 text-[11px] leading-[1.7] text-edge-200"><code>{code}</code></pre>
      </div>
    </div>
  );
}

function Section({ id, index, kicker, title, description, children }: {
  id: string; index: string; kicker: string; title: string; description?: string; children: ReactNode;
}) {
  return (
    <Card id={id} className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
      <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
        <span className="text-[10px] tracking-[0.16em] text-primary uppercase">{index} / {kicker}</span>
        <CardTitle>{title}</CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      <CardContent className="min-w-0 space-y-5 p-6">{children}</CardContent>
    </Card>
  );
}

function Method({ method }: { method: 'GET' | 'POST' }) {
  const tone = method === 'GET' ? 'border-chart-2/40 bg-chart-2/10 text-chart-2' : 'border-primary/40 bg-primary/10 text-primary';
  return <span className={`inline-block border px-2 py-0.5 text-[10px] font-medium tracking-[.12em] ${tone}`}>{method}</span>;
}

function Endpoint({ method, path, alias }: { method: 'GET' | 'POST'; path: string; alias?: string }) {
  return (
    <div className="flex flex-wrap items-center gap-3 border border-edge-700 bg-edge-950 px-4 py-3">
      <Method method={method} />
      <code className="text-xs text-edge-100">{path}</code>
      {alias && <span className="text-[10px] text-edge-500">alias <code className="text-edge-400">{alias}</code></span>}
    </div>
  );
}

function Spec({ rows }: { rows: Array<[string, string, string]> }) {
  return (
    <Table className="min-w-[560px]">
      <TableHeader>
        <TableRow>
          <TableHead>Field</TableHead>
          <TableHead>Type</TableHead>
          <TableHead>Description</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map(([field, type, description]) => (
          <TableRow key={field}>
            <TableCell className="py-3 text-[11px] whitespace-nowrap text-primary"><code>{field}</code></TableCell>
            <TableCell className="py-3 text-[11px] whitespace-nowrap text-edge-400">{type}</TableCell>
            <TableCell className="py-3 text-[11px] whitespace-normal text-edge-300">{description}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function Prose({ children }: { children: ReactNode }) {
  return <p className="text-[11px] leading-[1.9] text-edge-400">{children}</p>;
}

function Note({ children }: { children: ReactNode }) {
  return <div className="border-l-2 border-primary/50 bg-primary/5 px-4 py-3 text-[11px] leading-relaxed text-edge-300">{children}</div>;
}

const QUICKSTART_CURL = `curl "$BASE_URL/chat/completions" \\
  -H "Authorization: Bearer vlk_..." \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "<model-id>",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'`;

const QUICKSTART_JS = `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "https://your-gateway.example/v1",
  apiKey: process.env.VERLOC_API_KEY, // vlk_...
});

const stream = await client.chat.completions.create({
  model: "<model-id>",
  messages: [{ role: "user", content: "Summarise this log line." }],
  stream: true,
});
for await (const chunk of stream) process.stdout.write(chunk.choices[0]?.delta?.content ?? "");`;

const CHAT_REQUEST = `{
  "model": "nv/deepseek-ai/deepseek-v4.1-flash",
  "messages": [
    { "role": "system", "content": "You are terse." },
    { "role": "user", "content": "Name three load balancers." }
  ],
  "temperature": 0.2,
  "max_tokens": 256,
  "stream": false
}`;

const CHAT_STREAM = `data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Nginx"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":", HAProxy"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":9,"total_tokens":30}}

data: [DONE]`;

const RESPONSES_REQUEST = `{
  "model": "<responses-capable-model-id>",
  "input": "Explain consistent hashing in one paragraph.",
  "max_output_tokens": 300
}`;

const SYSTEMONE_REQUEST = `{
  "model": "@cf/cloudflare/clef",
  "state": { "queue_depth": 142, "error_rate": 0.031 },
  "questions": ["Which team should handle this incident?"]
}`;

const SYSTEMONE_RESPONSE = `{  "model": "clef",
  "answers": ["{\\"team\\":\\"technical\\",\\"confidence\\":0.94}"],
  "probabilities": [0.94],
  "confidence": 0.94,
  "legend": { "team": ["technical", "billing", "abuse"] },
  "usage": { "input_tokens": 338, "output_tokens": 0 }
}`;

const ANTHROPIC_REQUEST = `{
  "model": "nv/deepseek-ai/deepseek-v4.1-flash",
  "max_tokens": 1024,
  "system": "You are a coding agent.",
  "messages": [
    { "role": "user", "content": "Explain this error" }
  ]
}`;

const ANTHROPIC_RESPONSE = `{
  "id": "msg_chatcmpl-...",
  "type": "message",
  "role": "assistant",
  "model": "nv/deepseek-ai/deepseek-v4.1-flash",
  "content": [
    { "type": "text", "text": "The error means..." }
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": { "input_tokens": 25, "output_tokens": 148, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0 }
}`;

const ANTHROPIC_STREAM_EVENTS = `event: message_start
data: {"type":"message_start","message":{"id":"msg_...","type":"message","role":"assistant","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The error means..."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":148}}

event: message_stop
data: {"type":"message_stop"}`;

const ANTHROPIC_CLAUDE_CODE = `export ANTHROPIC_BASE_URL="$BASE_URL"
export ANTHROPIC_API_KEY="vlk_..."
export ANTHROPIC_MODEL="nv/deepseek-ai/deepseek-v4.1-flash"    # main model
export ANTHROPIC_SMALL_FAST_MODEL="nv/..."                     # background tasks
# Optional: populate the /model picker from this gateway
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`;

const ERROR_BODY = `{
  "error": {
    "message": "unknown or disabled model",
    "type": "gateway_error"
  }
}`;

const AUTH_ERROR = `{
  "error": {
    "message": "valid client API key required",
    "type": "invalid_request"
  }
}`;

const MODELS_RESPONSE = `{
  "object": "list",
  "data": [
    {
      "id": "nv/deepseek-ai/deepseek-v4.1-flash",
      "object": "model",
      "created": 1755815280,
      "owned_by": "nvidia",
      "upstream": "deepseek-ai/deepseek-v4.1-flash",
      "context_window": 1048576,
      "max_tokens": 32768,
      "type": "language",
      "pricing": { "prompt": "0.0000003", "completion": "0.0000012", "input_cache_read": "0.000000007" },
      "pricing_per_million": { "input": 0.3, "output": 1.2, "cache_read": 0.007, "currency": "USD", "per": "1M tokens" }
    }
  ]
}`;

const PRICING_RESPONSE = `{
  "object": "list",
  "per": "1M tokens",
  "currency": "USD",
  "data": {
    "nv/deepseek-ai/deepseek-v4.1-flash": { "input": 0.3, "output": 1.2, "cache_read": 0.007, "currency": "USD", "per": "1M tokens" }
  }
}`;

export default function Docs() {
  const base = `${location.origin}/v1`;

  return (
    <div className="space-y-5">
      <nav aria-label="Documentation sections" className="flex flex-wrap border border-edge-700 bg-edge-900">
        {SECTIONS.map(([index, label, id]) => (
          <a key={id} href={`#${id}`} className="flex items-center gap-2 border-r border-edge-700 px-4 py-3 text-[10px] transition-colors hover:bg-edge-800 hover:text-primary">
            <span className="text-primary">{index}</span><span className="text-muted-foreground">{label}</span>
          </a>
        ))}
      </nav>

      <Section id="overview" index="01" kicker="Introduction" title="Overview"
        description="verloc is an OpenAI-compatible multi-backend LLM gateway. Applications point at one base URL; verloc load-balances credentials, fails over across providers, remaps public model IDs, and records cost and latency.">
        <Prose>
          One endpoint speaks the OpenAI wire format, so existing SDKs work by changing only the
          base URL and API key. Requests are routed to a configured upstream provider, with ordered
          fallbacks, before any response bytes reach the caller.
        </Prose>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <div className="border border-edge-700 bg-edge-950 p-4">
            <div className="text-[9px] tracking-widest text-edge-500 uppercase">Base URL</div>
            <code className="mt-2 block text-xs break-all text-primary">{base}</code>
          </div>
          <div className="border border-edge-700 bg-edge-950 p-4">
            <div className="text-[9px] tracking-widest text-edge-500 uppercase">Auth header</div>
            <code className="mt-2 block text-xs break-all text-edge-200">Authorization: Bearer &lt;key&gt;</code>
          </div>
          <div className="border border-edge-700 bg-edge-950 p-4">
            <div className="text-[9px] tracking-widest text-edge-500 uppercase">Content type</div>
            <code className="mt-2 block text-xs break-all text-edge-200">application/json</code>
          </div>
        </div>
        <CodeBlock label="Quickstart — curl" code={QUICKSTART_CURL.replace('$BASE_URL', base)} />
        <CodeBlock label="Quickstart — OpenAI Node SDK" code={QUICKSTART_JS} />
        <Note>
          Only Chat Completions, Responses, and the native System One decision API are implemented.
          Embeddings, images, audio, and other OpenAI endpoints are not proxied.
        </Note>
      </Section>

      <Section id="authentication" index="02" kicker="Access control" title="Authentication"
        description="Inference endpoints use an application API key. The dashboard and operational endpoints use the single-owner session.">
        <Prose>
          Every inference request must send exactly one credential as a Bearer token. Model discovery
          (<code className="font-mono text-edge-200">/v1/models</code>) is public and needs no key.
          Application keys are generated in <strong className="text-edge-200">API Keys</strong> and are shown in full only once, at creation.
        </Prose>
        <Spec rows={[
          ['Authorization', 'header', 'Exactly one header, of the form "Bearer vlk_…". The key is 47 characters: the vlk_ prefix plus 43 URL-safe base64 characters.'],
          ['allowed_models', 'policy', 'A key may be scoped to specific model IDs. An empty scope allows every configured model.'],
          ['rpm / tpm', 'policy', 'Optional per-key request- and token-per-minute ceilings, evaluated over a rolling 60-second window.'],
          ['max_concurrent', 'policy', 'Optional ceiling on simultaneous in-flight requests for the key.'],
          ['expires_at', 'policy', 'Optional epoch-millisecond expiry. Zero means the key never expires.'],
        ]} />
        <CodeBlock label="Rejected key — 401" code={AUTH_ERROR} />
        <Prose>
          Missing, malformed, unknown, revoked, and expired keys all return the same 401 with a
          <code className="mx-1 font-mono text-edge-200">WWW-Authenticate: Bearer</code> header, so key
          probing reveals nothing. A valid key requesting a model outside its scope receives
          <code className="mx-1 font-mono text-edge-200">403 model is not allowed for this client key</code>.
        </Prose>
        <Note>
          Provider credentials never leave the gateway. The client's Bearer token is stripped before
          the request is forwarded and replaced with the selected upstream credential.
        </Note>
      </Section>

      <Section id="endpoints" index="03" kicker="Reference" title="Endpoints"
        description="All gateway routes. Every /v1 path also has an unprefixed alias (for example /chat/completions).">
        <Table className="min-w-[640px]">
          <TableHeader>
            <TableRow>
              <TableHead>Method</TableHead>
              <TableHead>Path</TableHead>
              <TableHead>Access</TableHead>
              <TableHead>Purpose</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {([
              ['POST', '/v1/chat/completions', 'Client key', 'OpenAI-compatible chat; supports SSE streaming'],
              ['POST', '/v1/messages', 'Client key', 'Anthropic Messages protocol (Claude Code / Anthropic SDKs); SSE streaming'],
              ['POST', '/v1/messages/count_tokens', 'Client key', 'Anthropic token counting; character-based estimate'],
              ['POST', '/v1/responses', 'Client key', 'OpenAI Responses passthrough; upstream must implement /responses'],
              ['POST', '/v1/systemone', 'Client key', 'Native Jev/Clef decision API; non-streaming, uncompressed JSON'],
              ['GET', '/v1/models', 'Public', 'Enabled models; a client key narrows the list to its scope'],
              ['GET', '/v1/pricing', 'Client key', 'Scoped catalog in USD per million tokens'],
              ['GET', '/healthz', 'Public', 'Liveness probe; returns ok or draining'],
              ['GET', '/readyz', 'Owner session', 'Readiness, draining flag, and available model count'],
              ['GET', '/metrics', 'Owner session', 'Runtime metrics snapshot (JSON, not Prometheus)'],
              ['GET', '/dash/', 'Public shell', 'Embedded operator dashboard (data endpoints require the owner session)'],
            ] as Array<[string, string, string, string]>).map(([method, path, access, purpose]) => (
              <TableRow key={path}>
                <TableCell className="py-3"><Method method={method as 'GET' | 'POST'} /></TableCell>
                <TableCell className="py-3 text-[11px] whitespace-nowrap text-edge-100"><code>{path}</code></TableCell>
                <TableCell className="py-3 text-[11px] whitespace-nowrap text-edge-400">{access}</TableCell>
                <TableCell className="py-3 text-[11px] whitespace-normal text-edge-300">{purpose}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <Note>
          Inference request bodies default to an 8 MiB cap; every other route is capped at 2 MiB.
          Every response carries strict security headers, and non-dashboard responses are marked
          <code className="mx-1 font-mono text-edge-200">Cache-Control: no-store</code>.
        </Note>
      </Section>

      <Section id="chat-completions" index="04" kicker="Inference" title="Chat Completions"
        description="The primary endpoint. Accepts an OpenAI Chat Completions body, resolves the model, and forwards it to the selected provider.">
        <Endpoint method="POST" path="/v1/chat/completions" alias="/chat/completions" />
        <Prose>
          The gateway reads only a small set of fields and passes the rest through unchanged. This
          preserves provider-specific options such as tools, response_format, and reasoning settings.
        </Prose>
        <Spec rows={[
          ['model', 'string (required)', 'Public verloc model ID. Resolved through the configured model map; unknown or disabled IDs return 404.'],
          ['stream', 'boolean', 'When true, the response is forwarded as Server-Sent Events. Must be a JSON boolean.'],
          ['max_tokens', 'integer', 'Output cap. Read for token-rate reservation on TPM-limited keys and clamped by a provider max_tokens limit.'],
          ['max_completion_tokens', 'integer', 'Accepted form of the output cap; handled like max_tokens.'],
          ['messages, tools, temperature, top_p, response_format, reasoning, …', 'any', 'Passed through unchanged, subject to backend-scoped normalisation described below.'],
        ]} />
        <CodeBlock label="Request" code={CHAT_REQUEST} />
        <Prose>
          Non-streaming responses are returned verbatim from the upstream, with the upstream
          <code className="mx-1 font-mono text-edge-200">Content-Length</code> removed and a fresh
          <code className="mx-1 font-mono text-edge-200">X-Request-ID</code> set. When streaming, each
          SSE line is flushed immediately and the response is marked
          <code className="mx-1 font-mono text-edge-200">text/event-stream</code> with
          <code className="mx-1 font-mono text-edge-200">X-Accel-Buffering: no</code>.
        </Prose>
        <CodeBlock label="Streaming response (SSE)" code={CHAT_STREAM} />
      </Section>

      <Section id="responses" index="05" kicker="Inference" title="Responses API"
        description="A passthrough for the OpenAI Responses API. Responses requests are never translated into Chat Completions.">
        <Endpoint method="POST" path="/v1/responses" alias="/responses" />
        <Prose>
          The selected upstream provider must implement the Responses API. The same model routing,
          failover, and usage accounting apply. Streaming follows the SSE conventions above, with
          terminal detection for response.completed, response.failed, and response.incomplete events.
        </Prose>
        <CodeBlock label="Request" code={RESPONSES_REQUEST} />
        <Prose>
          Usage is read from a top-level <code className="font-mono text-edge-200">usage</code> object or from
          <code className="mx-1 font-mono text-edge-200">response.usage</code>, using
          <code className="mx-1 font-mono text-edge-200">input_tokens</code> and
          <code className="mx-1 font-mono text-edge-200">output_tokens</code>. Omitted usage is recorded as unknown, never as zero.
        </Prose>
      </Section>

      <Section id="system-one" index="06" kicker="Native" title="System One"
        description="The typed decision API for Jev and Cloudflare Clef models. Native models cannot be called through Chat Completions or Responses, and vice versa.">
        <Endpoint method="POST" path="/v1/systemone" alias="/systemone" />
        <Spec rows={[
          ['model', 'string (required)', 'Public model ID mapped to a native provider (api_format "systemone") or a Cloudflare Clef route.'],
          ['state', 'object', 'Opaque decision state, forwarded byte-for-byte with exact numeric values preserved.'],
          ['questions', 'array', 'The typed questions to answer; other native fields (for example images) pass through unchanged.'],
        ]} />
        <Prose>
          Unlike chat, System One requests are not normalised: only the
          <code className="mx-1 font-mono text-edge-200">model</code> field is rewritten, and every other key is preserved.
          Requests must be uncompressed JSON (a non-identity Content-Encoding returns 415), and
          streaming is not supported (stream: true returns 400).
        </Prose>
        <CodeBlock label="Request" code={SYSTEMONE_REQUEST} />
        <Prose>
          Responses are returned in the native shape. Cloudflare's {"{ success, result }"} REST envelope
          is unwrapped, and an unsuccessful envelope triggers failover rather than being reported as
          success. Native models are labelled <code className="font-mono text-edge-200">type: "systemone"</code> in /v1/models.
        </Prose>
        <CodeBlock label="Response" code={SYSTEMONE_RESPONSE} />
      </Section>

      <Section id="anthropic-messages" index="07" kicker="Inference" title="Anthropic Messages API"
        description="The Anthropic Messages wire protocol, so Anthropic-native clients — Claude Code, Cline, the Anthropic SDKs — can point at verloc by changing only the base URL and key.">
        <Endpoint method="POST" path="/v1/messages" alias="/messages" />
        <Prose>
          Requests are translated to the OpenAI chat shape, routed through the same gateway path
          (key rotation, failover, limits, analytics), and responses are translated back — including
          streaming, tool use, and thinking blocks. Authenticate with
          <code className="mx-1 font-mono text-edge-200">x-api-key: {"<vlk_ key>"}</code> (Anthropic
          clients) or the usual Bearer header; both are the same client keys.
        </Prose>
        <Spec rows={[
          ['model', 'string (required)', 'Public verloc model ID, exactly as in /v1/models. Claude-style IDs work only if you add them as models with routes.'],
          ['max_tokens', 'integer (required)', 'Maximum output tokens for this response.'],
          ['system', 'string | blocks', 'System prompt, string or an array of text blocks.'],
          ['messages', 'array (required)', 'user / assistant turns with string or block-array content: text, tool_use, tool_result, image (base64).'],
          ['tools', 'array', 'Tool definitions with name, description, input_schema. tool_result content is folded to text.'],
          ['stop_sequences', 'array', 'Maps to OpenAI stop.'],
          ['temperature / top_p', 'number', 'Passed through. top_k is dropped (no chat equivalent).'],
        ]} />
        <CodeBlock label="Request" code={ANTHROPIC_REQUEST} />
        <CodeBlock label="Response" code={ANTHROPIC_RESPONSE} />
        <Prose>
          Streaming produces the full Anthropic event sequence: message_start, content_block_start,
          content_block_delta (text_delta / thinking_delta / input_json_delta), content_block_stop,
          message_delta (stop_reason + usage), message_stop. Pings are emitted during long thinking
          pauses so client idle watchdogs are not tripped.
        </Prose>
        <CodeBlock label="Stream events" code={ANTHROPIC_STREAM_EVENTS} />
        <Spec rows={[
          ['end_turn', 'finish_reason "stop"', 'Default terminal reason.'],
          ['max_tokens', 'finish_reason "length"', 'Output limit reached.'],
          ['tool_use', 'finish_reason "tool_calls" / "function_call"', 'The model requested tools.'],
          ['refusal', 'finish_reason "content_filter"', 'Upstream refused the content.'],
        ]} />
        <Prose>
          Upstream reasoning (reasoning_content) is surfaced as Anthropic thinking blocks. Inbound
          thinking blocks with signatures and cache_control markers are consumed, not forwarded —
          backends cannot verify Anthropic signatures. count_tokens is served as a character-based
          estimate. Claude Code background calls use haiku by default; set ANTHROPIC_MODEL and
          ANTHROPIC_SMALL_FAST_MODEL, or add claude-named model aliases.
        </Prose>
        <CodeBlock label="Claude Code setup" code={ANTHROPIC_CLAUDE_CODE} />
      </Section>

      <Section id="utility-endpoints" index="08" kicker="Reference" title="Utility endpoints"
        description="Discovery, pricing, and operational probes. Model discovery is public; pricing is filtered by the calling key's scope.">
        <Endpoint method="GET" path="/v1/models" alias="/models" />
        <Prose>
          Lists enabled models, sorted by ID. No credential is required; when a client key is
          presented, the list is narrowed to that key's allowed models. Pricing is exposed both as
          per-token strings (OpenAI/OpenRouter style) and as per-million numeric fields.
        </Prose>
        <CodeBlock label="GET /v1/models" code={MODELS_RESPONSE} />

        <Endpoint method="GET" path="/v1/pricing" alias="/pricing" />
        <Prose>Returns only in-scope models, priced in USD per one million tokens.</Prose>
        <CodeBlock label="GET /v1/pricing" code={PRICING_RESPONSE} />

        <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
          <div className="border border-edge-700 bg-edge-950 p-4">
            <div className="flex items-center gap-2"><Method method="GET" /><code className="text-xs text-edge-100">/healthz</code></div>
            <p className="mt-3 text-[11px] leading-relaxed text-edge-400">Public liveness. Returns 200 while serving and 503 with status "draining" during shutdown.</p>
          </div>
          <div className="border border-edge-700 bg-edge-950 p-4">
            <div className="flex items-center gap-2"><Method method="GET" /><code className="text-xs text-edge-100">/readyz</code></div>
            <p className="mt-3 text-[11px] leading-relaxed text-edge-400">Owner session. Returns ready, available_models, and draining. 503 until an owner exists and a model is available.</p>
          </div>
          <div className="border border-edge-700 bg-edge-950 p-4">
            <div className="flex items-center gap-2"><Method method="GET" /><code className="text-xs text-edge-100">/metrics</code></div>
            <p className="mt-3 text-[11px] leading-relaxed text-edge-400">Owner session. JSON counter snapshot per model and provider, including key health. Not a Prometheus endpoint.</p>
          </div>
        </div>
      </Section>

      <Section id="errors" index="09" kicker="Reliability" title="Errors and status codes"
        description="Two error envelopes are used. Inference errors carry type gateway_error; middleware and routing errors carry type invalid_request.">
        <CodeBlock label="Error body shape" code={ERROR_BODY} />
        <Table className="min-w-[720px]">
          <TableHeader>
            <TableRow>
              <TableHead>Status</TableHead>
              <TableHead>Message</TableHead>
              <TableHead>Cause</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {([
              ['400', 'request body required', 'No request body was sent.'],
              ['400', 'invalid request JSON', 'Body is not JSON, or model/stream/limit fields are the wrong type.'],
              ['400', 'System One does not support streaming', 'stream: true was sent to /v1/systemone.'],
              ['400', 'model does not support this inference endpoint', 'A native model was called on chat/responses, or vice versa.'],
              ['400', 'TPM-limited input requires known model token bounds', 'A token-limited key hit a model with no configured context window.'],
              ['403', 'model is not allowed for this client key', 'The key scope excludes the requested model.'],
              ['404', 'unknown or disabled model', 'The model ID is not configured or is disabled.'],
              ['409', 'client token policy changed; retry the request', 'The key TPM changed mid-request.'],
              ['413', 'request body too large', 'Body exceeded the configured maximum.'],
              ['415', 'System One requests require uncompressed JSON', 'A compressed body was sent to /v1/systemone.'],
              ['429', 'client request rate limit reached', 'The key exceeded its RPM; Retry-After is set.'],
              ['429', 'client token rate limit reached', 'The key exceeded its TPM; Retry-After is set.'],
              ['429', 'client concurrency limit reached', 'The key exceeded max_concurrent; Retry-After: 1.'],
              ['502', 'invalid upstream configuration', 'The selected provider could not build the upstream request.'],
              ['502 / 503 / 504', 'upstream unavailable', 'Every route and key was exhausted within the retry budget.'],
              ['504', 'upstream retry budget exhausted', 'The failover budget expired before a successful response.'],
              ['upstream', 'upstream rejected request', 'A non-retryable upstream error was returned unchanged with its status.'],
              ['503', 'gateway shutting down', 'The server is draining.'],
              ['503', 'gateway capacity unavailable', 'The admission queue is full; Retry-After: 1.'],
            ] as Array<[string, string, string]>).map(([status, message, cause]) => (
              <TableRow key={message + status}>
                <TableCell className="py-3 text-[11px] whitespace-nowrap text-chart-3">{status}</TableCell>
                <TableCell className="py-3 text-[11px] whitespace-normal text-edge-100"><code>{message}</code></TableCell>
                <TableCell className="py-3 text-[11px] whitespace-normal text-edge-400">{cause}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <Note>
          A 400 upstream error that mentions balance, quota, credit, rate limit, or payment is
          retried on the next key. Context-length and invalid-request 400s are returned immediately
          and never retried.
        </Note>
      </Section>

      <Section id="failover" index="10" kicker="Reliability" title="Failover and retries"
        description="verloc tries the primary route first, then each configured fallback in order, rotating through credentials until a response is committed.">
        <Prose>
          Retries are bounded by both an attempt count and a wall-clock budget. Keys that fail enter
          a cooldown and are skipped by later requests. Failover happens before any response byte
          reaches the client; once the first body byte is flushed, the route is committed and no
          further fallback occurs — an interrupted stream is reported as interrupted, never merged
          with another provider's output.
        </Prose>
        <Spec rows={[
          ['Retryable statuses', 'list', '401, 402, 429, 500, 502, 503, 504 — plus 400 errors that indicate balance, quota, credit, rate limit, or payment.'],
          ['Retry order', 'sequence', 'Primary target, then model_routes[id] in declared order. Credentials within a provider rotate round-robin over healthy, non-cooling keys.'],
          ['Cooldown', 'timing', 'A failed key cools down for base × failures (capped at 5 minutes); a Retry-After header overrides, capped at 1 hour. Success resets the key.'],
          ['Client flush', 'safety', 'No status line or headers are sent to the client until an upstream success is confirmed or the retry budget is exhausted.'],
        ]} />
        <Table className="min-w-[560px]">
          <TableHeader>
            <TableRow>
              <TableHead>Limit</TableHead>
              <TableHead>Default</TableHead>
              <TableHead>Maximum</TableHead>
              <TableHead>Effect</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {([
              ['max_attempts', '4', '32', 'Upstream attempts across all routes and keys'],
              ['retry_budget_seconds', '30', '300', 'Wall-clock failover window'],
              ['max_body_bytes', '8 MiB', '64 MiB', 'Inference request body cap'],
              ['max_in_flight', '64', '4096', 'Global concurrent requests'],
              ['max_queued', '128', '16384', 'Requests queued when at capacity'],
              ['queue_timeout_seconds', '10', '120', 'Maximum admission wait'],
              ['stream_idle_timeout_seconds', '60', '600', 'Idle SSE read/write deadline'],
            ] as Array<[string, string, string, string]>).map(([limit, def, max, effect]) => (
              <TableRow key={limit}>
                <TableCell className="py-3 text-[11px] whitespace-nowrap text-primary"><code>{limit}</code></TableCell>
                <TableCell className="py-3 text-[11px] whitespace-nowrap text-edge-200">{def}</TableCell>
                <TableCell className="py-3 text-[11px] whitespace-nowrap text-edge-400">{max}</TableCell>
                <TableCell className="py-3 text-[11px] whitespace-normal text-edge-300">{effect}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Section>

      <Section id="model-routing" index="11" kicker="Routing" title="Model routing"
        description="A public model ID is an alias. It resolves to a backend and an upstream model name, with optional ordered fallbacks.">
        <Prose>
          Each entry in the model map either names a backend explicitly (<code className="font-mono text-edge-200">backend:upstream</code>)
          or a bare upstream model that uses the default provider. The request's model field is
          rewritten to the upstream name before forwarding; the public ID is never sent upstream.
        </Prose>
        <CodeBlock label="Routing config (excerpt)" code={`{
  "default_provider": "nvidia",
  "model_map": {
    "<model-id>": "nvidia:deepseek-ai/deepseek-v4.1-flash",
    "<other-id>": "openai"
  },
  "model_routes": {
    "<model-id>": [
      { "backend": "openrouter", "upstream": "deepseek/deepseek-chat" },
      { "backend": "groq", "upstream": "llama-3.3-70b" }
    ]
  },
  "disabled_models": { "<paused-id>": true }
}`} />
        <Note>
          Backend-specific normalisation (role rewrites, tool-call cleanup, and max-token clamping)
          is scoped to the individual provider and never applied globally. Disabling a model returns
          404 without deleting its configuration.
        </Note>
      </Section>

      <Section id="client-keys" index="12" kicker="Access control" title="Client key policy"
        description="Application keys carry an optional model scope and independent rate ceilings. Manage them under API Keys.">
        <Spec rows={[
          ['name', 'string (1–120)', 'Human label shown in the dashboard and audit history.'],
          ['allowed_models', 'string[] (≤256)', 'Exact model IDs this key may use. Empty means all configured models.'],
          ['expires_at', 'integer (ms)', 'Epoch-millisecond expiry; zero means never. Must be in the future when set.'],
          ['rpm', 'integer', 'Requests per minute, rolling 60-second window. Zero disables the limit.'],
          ['tpm', 'integer', 'Tokens per minute, rolling 60-second window. Zero disables the limit. Requires model token bounds.'],
          ['max_concurrent', 'integer', 'Maximum simultaneous in-flight requests. Zero disables the limit.'],
        ]} />
        <Prose>
          Rate and concurrency breaches return 429 with a Retry-After header. Keys can be rotated
          (issuing a new secret without changing the key ID) or revoked; a revoked key is rejected
          immediately, including if it is already mid-request.
        </Prose>
        <CodeBlock label="Create a key (owner session, CSRF required)" code={`curl "$BASE/dash/api/admin/client-keys" \\
  -H "Content-Type: application/json" \\
  -H "X-CSRF-Token: $CSRF" \\
  -b "verloc_session=$SESSION" \\
  -d '{
    "name": "ci-agent",
    "allowed_models": ["<model-id>"],
    "rpm": 600,
    "tpm": 200000,
    "max_concurrent": 8
  }'`} />
      </Section>

      <Section id="configuration" index="13" kicker="Operations" title="Configuration reference"
        description="configuration.json bootstraps the gateway once; SQLite is the source of truth afterwards. Most settings are applied live from the dashboard.">
        <Prose>
          The management database holds providers, credentials, models, routes, limits, and alert
          rules after the first import. Listener address, paths, and cookie options are read at
          startup and require a restart; everything else applies without one.
        </Prose>
        <Spec rows={[
          ['listen', 'string', 'Bind address. Requires restart. Default ":8080".'],
          ['secure_cookies', 'boolean', 'Force the Secure flag on the owner session cookie when TLS terminates upstream.'],
          ['default_provider', 'string', 'Backend used when a model map value has no backend prefix.'],
          ['providers.<name>.base_url', 'string', 'Upstream base URL; may contain %ACCOUNT% for multi-account providers.'],
          ['providers.<name>.api_format', 'string', '"" or "openai" for OpenAI-compatible, "systemone" for native decision APIs.'],
          ['providers.<name>.auth_header_format', 'string', 'Header template with one %s, for example "Bearer %s".'],
          ['providers.<name>.keys', 'array', 'String keys, or objects with id, account_id, and api_key.'],
          ['providers.<name>.cooldown_seconds', 'integer', 'Per-provider key cooldown, capped at 3600.'],
          ['providers.<name>.request_timeout_seconds', 'integer', 'Upstream response-header timeout, capped at 300.'],
          ['providers.<name>.max_tokens', 'integer', 'Clamp output limits for this provider. Zero means no clamp.'],
          ['providers.<name>.strip_reasoning_content', 'boolean', 'Remove reasoning traces from assistant messages. Defaults to true.'],
          ['providers.<name>.synthesize_cli_headers', 'boolean', 'Send standardised CLI identity headers upstream.'],
          ['providers.<name>.no_auth', 'boolean', 'Keyless provider; no Authorization header is sent.'],
          ['model_map', 'object', 'Public model ID to "backend:upstream" or bare upstream.'],
          ['model_routes', 'object', 'Public model ID to an ordered array of { backend, upstream } fallbacks.'],
          ['model_meta', 'object', 'Catalog overlay: name, description, context_window, max_tokens, tags, and pricing_input/output/cache.'],
          ['disabled_models', 'object', 'Public IDs paused without deletion.'],
          ['limits', 'object', 'Admission and retry bounds (see section 09).'],
          ['dashboard.*', 'object', 'Analytics database path, recent-request ring size, and alert rules.'],
        ]} />
        <Note>
          Pricing is USD per million tokens. An explicit zero means a model is free; a missing price
          means it is unknown. Request-time pricing is preserved for analytics even if the catalog
          changes later.
        </Note>
      </Section>

      <Section id="features" index="14" kicker="Platform" title="Platform features"
        description="Beyond proxying: credential pooling, provider onboarding, a pricing catalog, durable analytics, alerts, and private backups.">
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          {([
            ['Credential pooling', 'Per-provider key pools rotate round-robin over healthy credentials, cool down failing keys, and recover automatically.'],
            ['Ordered failover', 'Primary and fallback routes are tried in order with bounded attempts and a wall-clock retry budget.'],
            ['Model discovery', 'Probe an upstream /models endpoint, test a connection, resolve metadata hints, then onboard models transactionally.'],
            ['Pricing catalog', 'An embedded catalog overlaid by live pricing hints; per-request cost uses the immutable request-time price.'],
            ['Metadata-only analytics', 'Request metadata, tokens, latency, and known cost are stored. Prompts, completions, and bodies never are.'],
            ['Alerts', 'Threshold rules over error rate, failover, cost, latency, TTFT, and key cooldown notify in the dashboard — they do not throttle paid traffic.'],
            ['Private backups', 'Consistent server-side snapshots of the management and analytics databases, with a checksum manifest and CLI restore.'],
            ['Reasoning replay', 'Reasoning traces are stripped or replayed per provider so tool-call follow-ups succeed against DeepSeek-style upstreams.'],
          ] as Array<[string, string]>).map(([title, body]) => (
            <div key={title} className="border border-edge-700 bg-edge-950 p-4">
              <div className="text-[11px] text-edge-100">{title}</div>
              <p className="mt-2 text-[11px] leading-relaxed text-edge-400">{body}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section id="compatibility" index="15" kicker="Scope" title="Compatibility and limitations"
        description="verloc is a focused single-owner gateway. Knowing its boundaries avoids surprises.">
        <ul className="space-y-3">
          {([
            'Chat Completions, Responses, and System One are the only inference APIs. Embeddings, images, audio, and moderation are not implemented.',
            'Responses requests are not translated to Chat Completions; the selected upstream must support the Responses API directly.',
            'System One is non-streaming and requires uncompressed JSON. Chat and Responses support streaming.',
            'A single instance serves a single host with one owner. There is no clustering, RBAC, SSO, or distributed rate limiting.',
            'Analytics history is retained indefinitely with no automatic compaction or size cap.',
            'Rate and alert evaluation windows are process-local and reset when the gateway restarts.',
            'Native and OpenAI-format models are not interchangeable across endpoints; an incompatible call returns 400.',
          ] as string[]).map((item) => (
            <li key={item} className="flex gap-3 border border-edge-700 bg-edge-950 p-4 text-[11px] leading-relaxed text-edge-300">
              <span className="mt-1.5 size-1.5 shrink-0 bg-primary" aria-hidden="true" />{item}
            </li>
          ))}
        </ul>
      </Section>
    </div>
  );
}
