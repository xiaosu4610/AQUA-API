<div align="center">

<img src="favicon.ico" width="76" alt="AQUA-API">

# AQUA-API

**One endpoint for every AI upstream you own.**

Official API keys · Cloud vendors · Resellers · Subscription accounts · Self-hosted models
Unified protocols, smart routing, precise billing, and an admin console out of the box.

[![License](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8.svg)](https://go.dev)
[![CGO](https://img.shields.io/badge/CGO-free-success.svg)](#technical-choices-and-trade-offs)
[![Deploy](https://img.shields.io/badge/Deploy-single%20binary-informational.svg)](#-up-and-running-in-60-seconds)

[简体中文](README.md) ｜ [**English**](README.en.md)

</div>

---

## What is this

AQUA-API is a **self-hosted LLM API gateway** and an **AI asset (usage) management system**.

Your upstreams are a mess of incompatible things: OpenAI keys, Azure, Claude, Gemini,
cloud vendors, resellers, subscription accounts (Claude / Codex / Gemini), and local
models running on Ollama or vLLM. Your downstream is your apps: Claude Code, Codex CLI,
Cursor, your own services, scripts, plugins.

AQUA-API sits in the middle and turns all of it into **one endpoint, one protocol, one clear bill**.

---

## Why not just another proxy

| | The usual approach | AQUA-API |
|---|---|---|
| **Upstream keys** | Stored in plaintext, readable in the UI | **AES-256-GCM encrypted at rest**; keys injected only via env vars — a compromised admin UI still leaks nothing |
| **Master key** | Written into the config file | The config field is **ignored outright** — it can never leak through the repo |
| **Upstream failures** | Permanently disabled after N failures; the pool shrinks over time | **Cooldown + half-open**: rate limits are temporary avoidance with automatic recovery; only an explicit "credential revoked" retires a key |
| **Rate limits** | One global threshold | **Per-credential** limits (weight / priority / RPM / in-flight); over-limit rotates to the next key instead of punishing it |
| **Quota** | Checked before and after; concurrent requests overspend | **Reserve → settle → refund**: available = quota − used − reserved. No negative balances, even under concurrency |
| **Protocols** | OpenAI-compatible only | OpenAI / Anthropic / Gemini on the downstream side; upstream picks an adapter by channel type |
| **Paid models** | A key is a permission; no audience segmentation | **Groups** decide channels and prices, and **a key can select its group**: free and paid audiences share upstreams but never share bills |

---

## Feature overview

### Gateway & forwarding

| Capability | Notes |
|---|---|
| Downstream protocols | OpenAI-compatible (`/v1/chat/completions`, `/v1/models`, `/v1/embeddings`), Anthropic, Gemini |
| Upstream protocols | OpenAI-compatible, **Azure OpenAI** (deployment + api-version), **Anthropic**, **Gemini** |
| Channel type catalog | **79** upstream types registered (text / image / video / audio / embedding / aggregator / self-hosted / subscription); the UI expands each type's own fields |
| Streaming | SSE forwarded chunk by chunk; **bidirectional conversion** when upstream and downstream protocols differ (Anthropic / Gemini events ↔ OpenAI chunks) |
| Canonical intermediate form | Everything converges on the OpenAI protocol (N×1): adding an upstream means writing one "in", adding a downstream one "out" |
| Timeouts | Upstream wait limit is **300 seconds** — long answers are not cut off |
| Error passthrough | Real upstream errors are returned as-is (RFC7807 `detail` and OpenAI `error.message`), never swallowed |

### Credential pool & scheduling

| Capability | Notes |
|---|---|
| Strategies | **Sequential / round-robin / weighted random / least recently used / least in-flight** (default), configurable per channel |
| Per-credential settings | Weight, priority, per-minute limit, in-flight count, cooldown deadline — all editable in the admin UI |
| Failure handling | 429 / 5xx → short cooldown (exponential backoff); 401 / 403 / 402 → long cooldown; **retired only when provably invalid**; transport errors don't count |
| Session affinity | A session pins to one credential (better upstream cache hits); if it becomes unavailable, affinity is dropped and the binding cleared |
| Key entry modes | Single / bulk paste / merged |

### Billing & accounting

| Capability | Notes |
|---|---|
| Formula | `quota = (prompt_tokens × prompt_price + completion_tokens × completion_price) / 1,000,000`, plus **per-call pricing** |
| Price rules | Match by model name or **wildcard pattern**, attachable to groups; in-memory cache, price changes apply immediately |
| Quota safety | Reserve + settle + refund; **unpriced models skip reservation** (free models are never blocked by a quota wall) |
| Quota semantics | `-1` means unlimited; the check is "remaining ≤ 0", not "== 0" — closing an overdraft hole |
| Payment channels | Manual / Epay / **Stripe** / **Alipay** (RSA2 signing) / **WeChat Pay** (APIv3 + platform cert verification + AES-GCM decryption) |
| Redeem codes | Bulk generation; redemption is a single atomic transaction (10 concurrent attempts on one code → exactly one succeeds) |
| Order accounting | Callback verification, idempotent crediting, manual fulfilment / close / refund |

### Operations & admin

| Capability | Notes |
|---|---|
| Entry points | **User site** (landing / model plaza / console) and **admin console** (`/admin`) |
| Model plaza | Faceted filters (group / vendor / availability) with live facet counts, search, sorting, card & list views, detail modal with pricing table and a runnable cURL |
| Channels | CRUD, connectivity probes, credential-pool drawer, **one-click model list fetch from upstream** |
| Groups | First-class entity with billing multiplier and reference counts (deletion tells you what it affects) |
| Tokens | Quota / expiry / model allowlist / **owning group**; plaintext shown exactly once |
| Async tasks | Submission, polling, cancellation for generation tasks; per-call billing; automatic refunds on failure |
| Redeem codes / orders / logs / users / subscriptions | Full admin pages |
| Mobile | Bottom navigation, tables degrade to cards, safe-area handling, bottom-sheet modals |

---

## 🚀 Up and running in 60 seconds

```bash
git clone https://gitee.com/xiaosu4610/AQUA-API.git
cd AQUA-API

go build -o aqua ./cmd/aqua     # pure Go, no CGO, no gcc needed
./aqua -gen-key                 # generate the encryption master key

export AQUA_APP_KEY="<the key from the step above>"
./aqua                          # defaults to 127.0.0.1:8787; SQLite is created and migrated automatically
```

Open `http://127.0.0.1:8787` — landing page, model plaza, console and admin console all live there.

> The frontend is embedded into the binary via `go:embed`, so **deployment is a single file**.
> Data lives in `./data/aqua.db` by default.
> Back up the master key separately: **change it and every encrypted upstream key becomes unrecoverable.**

---

## Supported protocols & upstreams

**Downstream** (how your apps connect): OpenAI-compatible · Anthropic · Gemini

**Upstream** (how we connect out):

| Status | Types |
|---|---|
| ✅ Implemented | OpenAI-compatible (covering DeepSeek / Kimi / Zhipu / Qwen / SiliconFlow / OpenRouter / Groq / Together / Mistral / xAI / Ollama / vLLM / resellers), **Azure OpenAI**, **Anthropic**, **Gemini** |
| 🧭 Registered, adapter pending | AWS Bedrock (SigV4), Google Vertex (service-account JWT), and image / video / audio / embedding upstreams |

> Types that are registered but lack a finished adapter are shown as **"coming soon" and cannot be selected** —
> you never configure half a channel only to find it cannot work.

---

## Configuration

Priority: **defaults < config file < environment variables**.

| Variable | Required | Notes |
|---|---|---|
| `AQUA_APP_KEY` | ✅ | Encryption master key, **env only** (the config field is ignored). Generate with `aqua -gen-key` |
| `AQUA_SERVER_LISTEN` | | Listen address, default `127.0.0.1:8787` |
| `AQUA_DATABASE_DRIVER` / `AQUA_DATABASE_DSN` | | Defaults to `sqlite` / `./data/aqua.db` |
| `AQUA_RELAY_GROUP` | | Gateway **default group** (which group group-less tokens resolve to), default `default` |
| `AQUA_SMTP_*` | | Outbound mail (signup codes, notifications) |
| `AQUA_EPAY_KEY`, `AQUA_STRIPE_SECRET_KEY`, `AQUA_ALIPAY_PRIVATE_KEY`, `AQUA_WECHATPAY_APIV3_KEY`, … | | Payment credentials — **env only, every one of them** |
| `AQUA_LOG_LEVEL` / `AQUA_LOG_FORMAT` | | `debug`/`info`/`warn`/`error`, `text`/`json` |

**Rule: secrets never go into the database.** Payment, SMTP and the master key are env-only;
operational parameters (gateway URL, merchant ID, FX rate, limits, toggles) live in the database and are editable in the UI.

---

## API surface

| Group | Paths |
|---|---|
| Public | `/healthz`, `/api/models` (model plaza), `/api/auth/*`, payment callbacks |
| Authenticated | `/api/user/*`, `/api/user/tokens`, `/api/user/orders`, `/api/user/redeem` |
| Admin | `/api/admin/*` (channels / credentials / groups / models / pricing / tokens / users / orders / redeem codes / tasks / logs / settings) |
| Forwarding | `/v1/chat/completions`, `/v1/models`, `/v1/embeddings` |

Errors follow the OpenAI format so existing SDKs can parse them directly:

```json
{"error":{"message":"no available upstream channel for this model","type":"server_error","code":"no_available_channel"}}
```

---

## Technical choices and trade-offs

| Choice | Reason |
|---|---|
| **Pure Go, zero CGO** | One `go build` produces a single binary; cross-compilation just works; no gcc |
| **SQLite (`modernc.org/sqlite`)** | Pure-Go driver; zero ops for self-hosting, and backups are a file copy |
| **Hand-written SQL + versioned migrations** | Migrations are auditable and reviewable; no ORM, so no "nobody can read the generated SQL" |
| **Frontend embedded in the binary** | One-file deployment; no extra static file service to expose |
| **No mandatory Redis** | Single-node works; Redis is only needed if you want shared rate limiting across instances |
| **Rate limiting keyed on IP *and* user** | IP-only limits are trivially bypassed by rotating proxies |
| **Cooldown is distinct from retirement** | "Temporarily rate limited" and "permanently invalid" are different states; conflating them shrinks the credential pool during incidents |

---

## Roadmap

- [x] Protocol conversion (OpenAI ↔ Anthropic ↔ Gemini) with bidirectional streaming
- [x] Credential scheduling strategies, cooldown/half-open, session affinity, in-flight counting
- [x] Reserve / settle / refund quota system
- [x] Groups & multipliers, model plaza, redeem codes, five payment channels, async tasks
- [ ] AWS Bedrock / Google Vertex signature auth
- [ ] Image / video / audio upstream adapters
- [ ] Model ID mapping wired into the forwarding path (already configurable in the UI)
- [ ] Standalone admin entrypoint + install wizard (browser-based setup)
- [ ] Subscription quota windows (auto-reset every 5h / day / week)
- [ ] Admin action audit log, announcements & FAQ, proxy pool

---

## Development

```bash
go build ./...     # build
go test ./...      # test
gofmt -w .         # format
```

Mandatory engineering conventions:

1. **Small commits** — commit each independently describable step immediately; never batch a day of work into one commit.
2. **AI-friendly comments** — every source file starts with an *Intent / Flow / Extension* header so that anyone (human or AI) understands in 30 seconds what the file does, how data flows, and where to extend it.

See [`AGENTS.md`](AGENTS.md) (Chinese).

### Layout

```
cmd/aqua/             entrypoint (wiring only, no business logic)
internal/config/      config loading & validation
internal/model/       domain models & repository interfaces
internal/store/       persistence (SQL + versioned migrations)
internal/server/      HTTP layer (routing / middleware / handlers)
internal/relay/       protocol adaptation & forwarding (core domain)
internal/payment/     payment channel adapters
internal/channeltype/ upstream/downstream type registry
web/                  frontend (build output embedded into the binary)
```

---

## License

Source code is licensed under [**GNU AGPL-3.0**](LICENSE).

> **If you offer it as a network service**, AGPL section 13 requires you to make the
> corresponding source available to your users. Private/internal use is unaffected,
> but keep the copyright notices intact.

Brand assets (`favicon.ico`, the "AQUA-API" name and marks) are not covered by the
source license — see [NOTICE](NOTICE).

---

<div align="center">

**If this saved you an afternoon of reconciling invoices, a star is appreciated ⭐**

</div>
