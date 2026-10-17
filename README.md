# verloc

A self-hosted, OpenAI-compatible LLM gateway with an embedded operations dashboard. Point applications at one endpoint; manage upstream providers, credentials, model routes, client access, and usage from the browser.

verloc runs as a single Go binary. SQLite stores management state and analytics; the React dashboard is embedded in the binary. Node.js is a build dependency, not a runtime dependency. No external database, Redis, or separate dashboard server is required.

## Capabilities

- **Routing:** public model aliases, ordered provider fallbacks, credential rotation, and per-provider cooldowns.
- **Inference:** Chat Completions and Responses with JSON/SSE, plus native System One typed questions for TypeSafe Jev and Cloudflare Clef. Failover happens before response bytes reach the client; interrupted streams are not silently resumed.
- **Access control:** one owner account for administration; separate application API keys with model scopes, expiry, rotation, revocation, and request/token/concurrency limits.
- **Live management:** add providers, discover/test upstream models, edit pricing and routes, and change gateway limits without restarting.
- **Analytics:** durable request metadata, provider/model/client breakdowns, input/output/cache tokens, known cost, errors, latency and time-to-first-token, request attempts, and operational alerts.
- **Persistence:** encrypted upstream credentials, hashed application keys, management audit events, and database backup/restore commands.

## Deployment scope

Designed for a **single gateway instance on a single host**, with local persistent storage and one administrative owner. For a production deployment, use HTTPS, protect the data directory and encryption key, verify backups, monitor disk/telemetry health, and validate capacity against your own workload.

This is not a clustered gateway or a billing system. There is no built-in multi-owner RBAC, SSO, distributed rate limiting, or cross-instance coordination. Read the [operating limits](#operating-limits) before exposing it to other users.

## Build and start

### Requirements

- Go **1.25 or newer**.
- Node.js **22.12 or newer** and npm to build the dashboard.
- A writable local filesystem for SQLite and private key files.
- An upstream provider implementing the endpoint you intend to use.

From the repository root:

```sh
npm --prefix dashboard ci
make build
./verloc -version
```

The build produces `./verloc`, including the dashboard. CGO is disabled; a C compiler is not required.

Run from a dedicated directory so runtime state stays outside the source checkout:

```sh
install -d -m 700 "$HOME/.local/share/verloc"
install -m 755 ./verloc "$HOME/.local/share/verloc/verloc"
cd "$HOME/.local/share/verloc"

umask 077
printf '{"listen":"127.0.0.1:8080"}\n' > bootstrap.json
./verloc -config bootstrap.json
```

Open **<http://127.0.0.1:8080/dash/>**.

A bootstrap file is optional. Running `./verloc` in an empty directory creates the default databases and key files without `config.json`. **The default listener is `:8080` on all interfaces**; the example above binds to loopback explicitly. Do not expose plain HTTP credentials on an untrusted network.

### First-run setup

1. Read `verloc-data/setup.token` privately on the gateway host. The server logs its path, not its value. Do not share or commit it.
2. Enter that token in the dashboard and create the owner password: at least 12 characters, at most 1,024 UTF-8 bytes. Setup can be completed only once.
3. Open **Providers** and add an upstream. Supply its API base URL and credential, or explicitly select no authentication for a keyless provider. Use discovery and a test request to check the connection; tests can incur upstream charges.
4. Add a model with a public ID such as `coding`, choose its provider, and set the upstream model ID. Review pricing instead of assuming that missing prices mean free usage.
5. Open **API Keys**, create an application key, set its model scope and limits, and copy the secret. It is shown once.
6. Use that application key with the gateway URL. The owner password and upstream provider credentials are not gateway API keys.

The gateway is not ready for inference until owner setup is complete and at least one enabled model has an enabled route. Configuration readiness does not prove that the upstream is reachable or has available quota; run a model test as well.

## Use with applications

Set an application's OpenAI-compatible **base URL** to `http://127.0.0.1:8080/v1`, its API key to the application key issued by verloc, and its model to a configured **public model ID**.

For the examples below, set `VERLOC_API_KEY` privately in your shell and use a model ID you created. Do not commit application keys to client configuration files.

```sh
export VERLOC_BASE_URL="http://127.0.0.1:8080/v1"
export VERLOC_MODEL="coding"

curl --fail-with-body "$VERLOC_BASE_URL/models" \
  -H "Authorization: Bearer $VERLOC_API_KEY"

curl --fail-with-body "$VERLOC_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $VERLOC_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$VERLOC_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with hello.\"}]}"
```

Streaming:

```sh
curl --fail-with-body --no-buffer "$VERLOC_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $VERLOC_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$VERLOC_MODEL\",\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"Reply with hello.\"}]}"
```

With the Python OpenAI SDK (`pip install openai`):

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url=os.environ["VERLOC_BASE_URL"],
    api_key=os.environ["VERLOC_API_KEY"],
)
response = client.chat.completions.create(
    model=os.environ["VERLOC_MODEL"],
    messages=[{"role": "user", "content": "Reply with hello."}],
)
print(response.choices[0].message.content)
```

### Typed decisions: TypeSafe Jev and Cloudflare Clef

These models accept `state` and typed `questions`, not chat messages:

- **TypeSafe:** choose the **TypeSafe Jev (System One)** provider preset, or set
  `base_url` to `https://api.typesafe.ai/v1` and `api_format` to `systemone`.
  Map a public ID such as `ts/jev-latest` to `jev-latest`. Discovery accepts
  TypeSafe's `models[].name` IDs.
- **Cloudflare:** keep the Workers AI provider's OpenAI-compatible base
  `https://api.cloudflare.com/client/v4/accounts/%ACCOUNT%/ai/v1` and its
  per-key account IDs. Map `cf/clef` to `@cf/cloudflare/clef`, or
  `cf/clef-flash` to `@cf/cloudflare/clef-flash`. These models alone use the
  native `/ai/run/...` path; other Cloudflare models retain chat behavior.

```sh
curl --fail-with-body "$VERLOC_BASE_URL/systemone" \
  -H "Authorization: Bearer $VERLOC_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "ts/jev-latest",
    "state": {"incident": "Checkout has failed for the last hour."},
    "questions": {
      "urgent": {"type": "noul", "instructions": "Is this incident urgent?"},
      "team": {
        "type": "choice",
        "instructions": "Which team should handle it?",
        "criteria": {"billing": "Payments and invoices", "technical": "Outages and bugs"}
      },
      "severity": {
        "type": "score",
        "instructions": "Rate customer impact.",
        "criteria": ["None", "Minor", "Major", "Critical"]
      }
    }
  }'
```

The response retains native `answers`, probabilities, confidence, actual model
version, and token usage. Cloudflare's REST envelope is unwrapped. Clef image
inputs are forwarded unchanged; upstream media limits still apply. Native models
are labeled `type: "systemone"` in the scoped `/v1/models` response.

System One is non-streaming and accepts **uncompressed JSON** through verloc.
Disable SDK body compression for this route. Gateway body limits still apply
(8 MiB by default); adjust Settings when your provider permits larger requests.
TPM-limited application keys require a configured model context window and reserve
that bound plus request bytes before reconciling actual usage. Native models
cannot be called through Chat Completions or Responses.

See the upstream [TypeSafe API](https://docs.typesafe.ai/api.md) and
[Cloudflare Clef contract](https://developers.cloudflare.com/workers-ai/models/clef/)
for question and media constraints. Add native providers through the dashboard
(provider presets) or a local `config.json`; the committed root
`config.example.json` is a minimal, key-less nvidia example. Review current
provider prices.

### HTTP surface

| Endpoint | Authentication | Purpose |
| --- | --- | --- |
| `POST /v1/chat/completions` | Application API key | Chat Completions, including streaming |
| `POST /v1/responses` | Application API key | Responses passthrough to a compatible upstream |
| `POST /v1/systemone` | Application API key | Native Jev/Clef typed-question evaluation; no streaming |
| `GET /v1/models` | None (key optional) | Enabled models; a presented key scopes the list to its access, anonymous callers see all enabled models |
| `GET /v1/pricing` | Application API key | Scoped pricing catalog in USD per million tokens |
| `GET /healthz` | None | Process liveness; does not test upstreams or storage |
| `GET /readyz` | Owner session | Storage/owner/model-configuration readiness |
| `GET /metrics` | Owner session | Live **JSON**, not Prometheus exposition |
| `/dash/` | Owner login within the app | Embedded operations dashboard |
| `/dash/api/*` | Owner session | Dashboard and management APIs; writes require CSRF protection |

Authentication endpoints under `/dash/api/auth/` are reachable before login for setup/session/login. Application API keys never grant administrative access. Responses requests are not translated to Chat Completions: the selected upstream must support `/responses`. Other OpenAI APIs, including embeddings, images, and audio, are not implemented.

## Dashboard

The same binary serves all nine screens; there is no separate analytics service.

| Screen | What it exposes |
| --- | --- |
| **Overview** | Request/token totals, known spend, error rate, current request rate, latency p95, average TTFT, traffic history, and provider spend allocation with full provider details |
| **Requests** | Time/model/provider/client/status/search filters; cursor-paginated durable history; per-request usage, cost, outcome, client attribution, and upstream-attempt details |
| **Analytics** | Client/model/provider comparisons and full ledgers, status breakdown, latency distribution, p50/p95/p99 latency and TTFT, cache usage, cost history, and data-coverage notes |
| **Alerts** | Configured thresholds/scopes/windows/cooldowns and a bounded recent history of persisted/live triggers |
| **Providers** | Connection management, discovery/testing, credentials, provider health, cooldowns, and expandable process-session diagnostics |
| **Models** | Public model IDs, upstream mappings, pricing, ordered fallback routes, and bulk enable/disable/delete |
| **API Keys** | Application key lifecycle, model access, expiry, RPM/TPM limits, and concurrency limits |
| **Settings** | Owner password, global admission/retry/body/stream limits, alert rules, server-side backups, database paths, connection details, and recent management audit events |
| **Docs** | In-app gateway API reference: endpoints, schemas, errors, routing, and configuration |

### Automatic model metadata

Discovery preserves rich fields returned by the upstream `/models` endpoint,
including explicit zero prices. It fills missing fields from live catalogs:
[Vercel](https://ai-gateway.vercel.sh/v1/models),
[OpenRouter](https://openrouter.ai/api/v1/models), and
[models.dev](https://models.dev/api.json?type=all), with the existing LiteLLM
catalog as the final fallback. Vercel's public catalog works without a configured
Vercel credential. No local endpoint-capture files are imported or embedded.

**Lookup catalog** in the model editor and manually entered onboarding rows use
the same resolver. Available names, descriptions, ownership, capabilities/tags,
context/output limits, and per-million-token prices are carried through save.
Vercel metadata is cached for one hour; other catalogs for 24 hours. A failed
catalog source does not fail otherwise successful upstream discovery.

Qualified model identities do not match unrelated organizations; ambiguous
bare names and conflicting provider fields remain unresolved. Free variants
remain separate from paid variants. Blank prices mean unknown; explicit `0`
means free. Catalog prices are suggestions across providers: review them before
applying. When no cached-input discount is supplied, newly saved models use the
input rate instead of treating cached tokens as free.

### Accounting and privacy

- Normal analytics stores **metadata**, not prompts, completions, or request/response bodies. Metadata includes model/provider/client identity, timestamps, status, timings, tokens, known cost, client IP/user agent, and attempt summaries. Protect the database accordingly. Payloads are still sent to your chosen upstreams; their policies apply.
- Missing usage and missing pricing are **unknown**, not zero. Known-cost totals exclude requests that cannot be priced; the dashboard reports coverage gaps.
- Cached input is a subset of input tokens. Total tokens are input + output; cached tokens are not added twice. Prices are USD per million tokens; request-time pricing is preserved when catalog prices change later.
- Percentiles are histogram-bucket estimates over recorded request distributions, not averages of snapshot percentiles.
- Snapshot traffic/cost charts and filtered request analytics are different sources. The dashboard labels their windows; dimensional request filters do not apply to the separate snapshot panels.
- New history is retained indefinitely. There is **no automatic expiry or compaction**. Legacy aggregate-only history remains readable, but deleted detail and missing historical attribution cannot be recovered.
- The analytics writer uses a bounded queue. Under overload or storage failures, metadata can be dropped. Queue pressure, dropped records, and write errors are surfaced in the dashboard. Analytics is not a lossless financial ledger.

## State and configuration

**SQLite is the source of truth for managed configuration. A database file alone is not the complete installation.** Keep the external encryption key and any bootstrap options as well.

Default paths below are relative to the directory containing the `-config` file. With no flag, `config.json` is looked up in the process working directory; it may be absent.

| File | Role | Recovery requirement |
| --- | --- | --- |
| `verloc-data/control.db` | Providers, encrypted upstream credentials, models, routes, owner auth, application-key hashes, limits, alert rules, and audit events | Back up |
| `verloc-data/master.key` | External encryption key for upstream credentials | Back up **separately and securely**; loss prevents credential recovery |
| `verloc.db` | Request history, snapshots, alerts, and upstream credential events | Back up to retain analytics |
| `verloc-data/setup.token` | Private first-run owner-setup token | Treat as a secret; not a login or password-reset mechanism |
| Optional bootstrap JSON | Listener, database/key/token paths, secure-cookie mode | Preserve deployment options; contains no provider keys in a new installation |

SQLite also creates `-wal`/`-shm` files while running. Do not move an active database in isolation or back it up with a naive file copy. Use the backup commands below.

Changes made in the dashboard are transactional, saved in `control.db`, and live-applied. The server does not write `config.json`. Changing bootstrap listener/paths/cookie options requires a restart; editing legacy provider/model entries in JSON does not override an already-imported management database.

### Existing `config.json` installations

On the first start with a new management database, verloc imports the legacy provider/model configuration once. Do not point an existing install at an empty management path unintentionally.

To produce a secret-free bootstrap from an imported installation, stop the normal process, run as the same OS user, and then update the service to use the new file:

```sh
./verloc -config config.json -export-bootstrap bootstrap.json
./verloc -config bootstrap.json
```

The export requires a new destination file and records absolute storage paths. It does not remove the old secret-bearing file. Archive it securely or remove it after verifying the migration; do not deploy it over the live databases.

## Production deployment

### HTTPS and storage

Terminate TLS at a trusted reverse proxy and bind verloc to loopback. For example, `/etc/verloc/bootstrap.json`:

```json
{
  "listen": "127.0.0.1:8080",
  "management_db": "/var/lib/verloc/control.db",
  "master_key_file": "/var/lib/verloc/master.key",
  "setup_token_file": "/var/lib/verloc/setup.token",
  "secure_cookies": true,
  "dashboard": {
    "db_path": "/var/lib/verloc/analytics.db"
  }
}
```

`secure_cookies: true` is required for owner sessions when TLS terminates at the proxy. Forwarded headers do not enable it automatically. Preserve the external `Host`, serve the dashboard and its API from the same origin, and allow long-lived SSE/streaming responses. Open the HTTPS URL, not the internal HTTP listener, to sign in.

Example Caddy configuration; replace the domain and configure DNS:

```caddyfile
gateway.example.com {
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
    }
}
```

Use persistent **local** storage. Do not share SQLite files across replicas or put them on a network filesystem. Provider/model discovery and connection tests are owner-authorized outbound requests; grant owner access only to trusted operators.

### systemd example

On a Linux host with systemd, install the binary as `/usr/local/bin/verloc`, create a dedicated `verloc` system user/group, and make `/var/lib/verloc` writable only by that user. Make the bootstrap file readable by the service user. Never run the gateway as root.

Save the following as `/etc/systemd/system/verloc.service`:

```ini
[Unit]
Description=verloc LLM gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=verloc
Group=verloc
WorkingDirectory=/var/lib/verloc
ExecStart=/usr/local/bin/verloc -config /etc/verloc/bootstrap.json
Restart=on-failure
RestartSec=3
TimeoutStopSec=40
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/verloc

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now verloc
sudo systemctl status verloc
journalctl -u verloc -f
```

Read the private setup token on that host and complete setup over HTTPS. `/healthz` is suitable for an unauthenticated liveness probe. `/readyz` and `/metrics` require an owner session; an application key is not accepted. Do not disable their authentication to accommodate a monitor.

### Production checklist

- Complete owner setup; use separate, scoped application keys per application.
- Serve HTTPS; enable secure cookies; keep the internal listener behind the proxy/firewall.
- Restrict access to databases, setup token, backups, and `master.key`; keep secrets out of git and logs.
- Configure global and per-key limits for expected concurrency and upstream quotas; run representative JSON, streaming, and failure scenarios.
- Confirm model availability with an actual upstream test, not only `/readyz`.
- Monitor disk space, request errors, latency, queue pressure, dropped telemetry, and write failures. History does not expire automatically.
- Create and restore a backup in an isolated directory before relying on it. Retain the original master key separately.
- Keep `VERLOC_REQ_DUMP` unset: it is an opt-in payload-debugging path, not production telemetry.

## Backups, recovery, and upgrades

### Back up

**Settings → Backups** creates database snapshots on the gateway host under `backups/` beside the management database. It returns a server-side path; it is not a browser download or an off-host backup.

The CLI can also create a backup and exit. Run it as the service user with the same bootstrap and storage paths. For a simple offline backup, stop the running service first:

```sh
./verloc -config bootstrap.json -backup ./backup-20261016
```

The destination must not already exist; its parent directory must exist. A completed backup contains `management.db`, `analytics.db`, and a checksum manifest. **It does not include `master.key`, the setup-token file, or bootstrap JSON.** Secure the backup, copy it off-host, and secure the master key separately. Application-key hashes and owner auth state in the backup are still sensitive even though upstream credentials are encrypted.

### Restore

Stop any process using the target paths. Choose new, unused database paths and point at the **original** master key. For a default local install, a separate `restore.json` could contain:

```json
{
  "listen": "127.0.0.1:8080",
  "management_db": "recovered/control.db",
  "master_key_file": "verloc-data/master.key",
  "setup_token_file": "recovered/setup.token",
  "dashboard": {
    "db_path": "recovered/analytics.db"
  }
}
```

```sh
./verloc -config restore.json -restore ./backup-20261016
./verloc -config restore.json
```

Restore validates checksums and credential decryption before publishing the new databases. It refuses existing destination databases or their WAL/SHM files. Reapply the appropriate HTTPS/bootstrap options for the destination host. A restored management backup includes the owner account and application keys; it does not initiate owner setup again.

### Upgrade

1. Back up the databases and separately secure the master key.
2. Build and test the new binary, including its embedded dashboard.
3. Stop the service, replace **only the binary**, and restart using the existing bootstrap and data paths.
4. Check liveness, sign in, inspect readiness, and exercise an inference request.

Keep the previous binary for recovery, but do not assume older binaries can read a newer database schema. A full rollback can require restoring the pre-upgrade backup to new paths with its original master key. Never replace production state with a development `config.json` or database.

## Operating limits

- **Single owner and single instance.** No clustered availability or distributed admission guarantees.
- **Compatibility is upstream-dependent.** Endpoint support, tools, structured output, streaming usage, and provider restrictions vary. Routing does not make an unsupported upstream API available.
- **Retries are bounded.** Credential/provider failover stops when attempts or the retry budget are exhausted; no failover occurs after response bytes have been sent.
- **Limits are not billing budgets.** Spend alerts notify in the dashboard; they do not stop paid traffic. Rate/admission counters and alert evaluation windows are process-local and reset on restart.
- **History grows with use.** No scheduled pruning, automatic compaction, or hard database-size cap. Plan storage and backup capacity.
- **Recent operational views are bounded.** Alerts shows at most 200 recent loaded/live events; Settings audit and credential diagnostics are not exhaustive historical export tools. Request history is separately cursor-paginated.
- **Analytics is best-effort under pressure.** Missing upstream usage/pricing and dropped telemetry limit accounting accuracy. This is not a payment or compliance audit ledger.

## Development

| Command | Purpose |
| --- | --- |
| `npm --prefix dashboard ci` | Install locked frontend dependencies |
| `make build` | Build the dashboard, then the CGO-free binary |
| `./scripts/build.sh` | Build with version/commit/build-time metadata |
| `go test ./... -count=1` | Run Go tests |
| `go vet ./...` | Run Go static checks |
| `npm --prefix dashboard run dev` | Start Vite for UI development; use embedded same-origin mode for deployment checks |
| `node scripts/sync-model-details.mjs` | Regenerate the embedded catalog from local catalog sources; not needed for dashboard-managed models |

`internal/server/static/` is committed so a Go-only build can use the existing dashboard: `CGO_ENABLED=0 go build -o verloc ./cmd/verloc`. After frontend changes, rebuild assets and the binary; an old binary cannot pick up new assets from disk.

```text
config.example.json      Minimal key-less nvidia bootstrap example
cmd/verloc/              CLI entry point
internal/server/         HTTP gateway, auth, management, analytics, embedded assets
dashboard/               React dashboard source (builds → internal/server/static)
scripts/                 Release build and catalog tooling
```

## License

Released under the [MIT License](LICENSE).

Copyright (c) 2026 Kunal Kongkan Kashyap.
