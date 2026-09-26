<div align="center">

<img src="favicon.ico" width="88" alt="AQUA-API">

# AQUA-API

### One endpoint for every AI upstream you own

**Self-hosted LLM API Gateway · AI Asset (Usage) Management**

Official API keys · Cloud vendors · Resellers · Subscription accounts · Self-hosted models
Unified protocols · Smart routing · Precise billing · A ready-to-use admin console

## 🌐 Official website `https://aqua.ltzy.top`

> If the domain ever changes, **this repository is the source of truth** (updated here first).

[![License](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8.svg?logo=go&logoColor=white)](https://go.dev)
[![CGO](https://img.shields.io/badge/CGO-free-success.svg)](#why-aqua-api)
[![Deploy](https://img.shields.io/badge/Deploy-single%20binary%20%2F%20Docker-informational.svg)](#-quick-start)
[![Platform](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey.svg)](#-quick-start)

[简体中文](README.md) ｜ [**English**](README.en.md) ｜ [🏠 Live demo](https://aqua.ltzy.top)

</div>

---

## 🔗 Official links

| | Address |
|---|---|
| **Official website (live demo)** | **`https://aqua.ltzy.top`** |
| **Primary repository** | `https://gitee.com/xiaosu4610/AQUA-API` |
| **Mirror** | GitHub, synced automatically from Gitee |

**This repository is the authoritative source for the official address.** If the domain changes,
it is updated **here first** and only then mirrored anywhere else. Bookmarking this repository
is more reliable than bookmarking a domain.

### 🛡 Watch out for imposters

- This project offers and authorises **no** "top-up agent", "managed hosting" or "official shared
  account" services. The server code is fully open source (AGPL-3.0), so anyone can self-host it —
  **being able to run it does not make a site official.**
- Only the addresses above are official. Any other domain is unrelated to this project, even if the
  UI looks identical.
- We will never DM you asking for passwords, payment credentials or verification codes.

### Link blocked or unreachable?

These domains are frequently **flagged by social platforms** (QQ / WeChat and similar) — we have
experienced mass reporting ourselves. If a link will not open:

1. Try another browser, or switch networks (mobile data ↔ home broadband);
2. **Share this repository link instead of the bare domain** — code-hosting links are far less
   likely to be blocked, and anyone can confirm the current official address from the repo itself;
3. If you are sure it is a false positive, file an appeal through the platform's own process.

---

## 📖 Table of contents

- [🔗 Official links](#-official-links)
- [⚠️ Disclaimer](#-disclaimer)
- [What is this](#what-is-this)
- [Why AQUA-API](#why-aqua-api)
- [Core features](#core-features)
- [Supported protocols & upstreams](#supported-protocols--upstreams)
- [🚀 Quick start](#-quick-start)
- [Configuration](#configuration)
- [Client integration](#client-integration)
- [FAQ](#faq)
- [Roadmap](#roadmap)
- [Development](#development)
- [License](#license)

---

## ⚠️ Disclaimer

This project is **vendor-neutral gateway software**. It only provides protocol conversion,
request routing and usage accounting.

1. **Compliance is entirely the operator's responsibility** — including each upstream
   provider's Terms of Service, account policies, local laws and export-control rules.
2. **Do not use it to circumvent an upstream's billing, quota or regional restrictions.**
   Whether pooling, reselling or account sharing is permitted depends on each provider's
   terms. That is your call to make, not a feature promise of this software.
3. This project **ships no upstream accounts, keys or credits**, and makes no warranty
   about the availability, stability or legality of any upstream.
4. The software is provided "as is", without liability for any direct or indirect damages.

**By using it you accept the above.**

---

## What is this

AQUA-API is a **self-hosted LLM API gateway** and an **AI asset (usage) management system**.

Your upstreams are usually a mess of incompatible things: OpenAI keys, Azure, Claude,
Gemini, cloud vendors, resellers, subscription accounts (Claude / Codex / Gemini), and
local models on Ollama or vLLM. Your downstream is your apps: Claude Code, Codex CLI,
Cursor, your own services, scripts, plugins.

AQUA-API sits in between and turns all of it into **one endpoint, one protocol, one clear bill**:

```
                      ┌──────────────────────────────┐
   Claude Code ─┐     │                              │     ┌─ Official OpenAI keys
   Codex CLI  ──┤     │          AQUA-API            │     ├─ Azure OpenAI
   Cursor     ──┼────▶│                              │────▶├─ Anthropic / Gemini
   Your app   ──┤     │  protocols · routing · billing│     ├─ Cloud vendors / resellers
   Scripts    ──┘     │  credential pool · console    │     ├─ Subscription accounts
                      └──────────────────────────────┘     └─ Local Ollama / vLLM
                        OpenAI / Anthropic / Gemini            adapted by channel type
```

---

## Why AQUA-API

There is no shortage of proxies. What is scarce is one you can **trust with your books**.
Every row below is a decision made after being burned by the alternative:

| Concern | The usual approach | AQUA-API |
|---|---|---|
| **Upstream keys** | Stored in plaintext, readable in the UI | **AES-256-GCM encrypted at rest**, injected only via env vars — a compromised console yields no usable credentials |
| **Master key** | Written into the config file | The config field is **ignored outright** — it can never leak through the repo |
| **Upstream failures** | Permanently disabled after N failures; the pool shrinks | **Cooldown + half-open**: rate limiting is temporary avoidance with automatic recovery; retired only on an explicit "credential revoked" |
| **Rate limits** | One global threshold | **Per-credential** (weight / priority / RPM / in-flight); over-limit rotates keys instead of punishing them |
| **Quota** | Checked before and after; concurrent requests overspend | **Reserve → settle → refund**: available = quota − used − reserved. No negative balances under concurrency |
| **Streaming billing** | Reads only the first N bytes; long answers bill **0** | Incremental SSE parsing — a `usage` frame at the very end of the stream is still captured |
| **Protocols** | OpenAI-compatible only | **OpenAI / Anthropic / Gemini** downstream, upstream adapted by channel type |
| **Audience segmentation** | A key is a permission; free and paid cannot be separated | **Groups** decide channels and prices, and **a key picks its group**: same upstream, separate books |
| **Deployment** | Needs a database, Redis, a compiler | **Single binary + SQLite**, frontend embedded, zero CGO — no gcc required |

---

## Core features

### 🌐 Gateway & forwarding

- **Three downstream protocols**: OpenAI-compatible (`/v1/chat/completions`, `/v1/models`, `/v1/embeddings`), Anthropic, Gemini
- **Upstream adapters**: OpenAI-compatible, **Azure OpenAI** (deployment + api-version), **Anthropic**, **Gemini**
- **79 registered channel types**: text / image / video / audio / embedding / aggregator / self-hosted / subscription. The UI **expands only the fields a type needs** (pick Azure and you see *deployment* + *api-version*)
- **Bidirectional streaming conversion**: Anthropic / Gemini SSE events ↔ OpenAI `chat.completion.chunk`, including tool calls
- **Canonical intermediate form**: everything converges on the OpenAI protocol (N×1) — adding an upstream means writing one "in", a downstream one "out"
- **300-second upstream timeout** so long answers are not cut off
- **Faithful error passthrough** (RFC7807 `detail`, OpenAI `error.message`) — errors are never swallowed

### 🔑 Credential pool & scheduling

- **Five strategies**: sequential / round-robin / weighted random / least recently used / **least in-flight** (default), switchable per channel
- **Per-credential tuning**: weight, priority, per-minute limit, in-flight count, cooldown deadline
- **Graded failure handling**: 429 / 5xx → short cooldown with exponential backoff; 401 / 403 / 402 → long cooldown; **retired only when provably invalid**; transport errors never count
- **Session affinity**: a session pins to one credential for better cache hits; affinity is dropped cleanly if it goes unavailable
- **Multiple entry modes**: single, bulk paste, merged

### 💰 Billing & accounting

- **Formula**: `quota = (prompt_tokens × prompt_price + completion_tokens × completion_price) / 1,000,000`, plus **per-call pricing**
- **Price rules**: match by model name or **wildcard**, attachable to groups; in-memory cache with immediate effect
- **Quota safety**: reserve + settle + refund; **unpriced models skip reservation** so free models are never blocked by a quota wall
- **Semantics**: `-1` means unlimited; the check is "remaining ≤ 0" rather than "== 0", closing an overdraft hole
- **Payment channels**: manual / Epay / **Stripe** / **Alipay** (RSA2) / **WeChat Pay** (APIv3 + platform cert verification + AES-GCM)
- **Redeem codes**: bulk generation; redemption is a single atomic transaction (10 concurrent attempts on one code → exactly one wins)
- **Order accounting**: callback verification, idempotent crediting, manual fulfilment / close / refund

### 🛠 Operations & console

- **Model plaza**: faceted filters (group / vendor / availability) with **live facet counts**, search, sorting, card & list views, detail modal with pricing and a runnable cURL
- **Channels**: CRUD, connectivity probes, credential-pool drawer, **one-click model list fetch from upstream**
- **Groups**: first-class entity with billing multiplier and **reference counts** (deletion tells you what it affects)
- **Tokens**: quota / expiry / model allowlist / **owning group**; plaintext shown exactly once
- **Async tasks**: submission, polling, cancellation; per-call billing; automatic refunds on failure
- **Also included**: users, redeem codes, orders, request logs, subscription accounts (OAuth)
- **Mobile**: bottom navigation, tables degrade to cards, safe-area handling, bottom-sheet modals

---

## Supported protocols & upstreams

**Downstream** (how your apps connect): `OpenAI-compatible` · `Anthropic` · `Gemini`

**Upstream** (how we connect out):

| Status | Types |
|---|---|
| ✅ **Implemented** | OpenAI-compatible (DeepSeek / Kimi / Zhipu / Qwen / SiliconFlow / OpenRouter / Groq / Together / Mistral / xAI / Ollama / vLLM / resellers), **Azure OpenAI**, **Anthropic**, **Gemini** |
| 🧭 **Registered, adapter pending** | AWS Bedrock (SigV4), Google Vertex (service-account JWT), plus image / video / audio / embedding upstreams |

> **Straight answer**: types whose adapter is not finished are shown as *"coming soon"* and
> **cannot be selected** — you will never configure half a channel only to find it cannot work.

---

## 🚀 Quick start

### Option 1 — Docker Compose (recommended)

```bash
git clone https://gitee.com/xiaosu4610/AQUA-API.git && cd AQUA-API
cp .env.example .env

docker build -t aqua-api:local .          # first build (frontend + backend + runtime image)
docker run --rm aqua-api:local -gen-key   # prints a master key → put it in .env as AQUA_APP_KEY

docker compose up -d
```

Open `http://127.0.0.1:8787`. Data lives in `./data` on the host — moving servers is a directory copy.

### Option 2 — docker run

```bash
docker build -t aqua-api:local .

docker run -d --name aqua-api \
  -p 8787:8787 \
  -e AQUA_APP_KEY="<your master key>" \
  -e AQUA_SERVER_LISTEN=0.0.0.0:8787 \
  -v "$PWD/data:/data" \
  --restart unless-stopped \
  aqua-api:local
```

### Option 3 — Single binary (Linux / systemd)

```bash
go build -o aqua ./cmd/aqua           # pure Go, zero CGO, no gcc needed
./aqua -gen-key                        # generate the encryption master key

sudo useradd -r -s /usr/sbin/nologin aqua
sudo mkdir -p /opt/aqua /etc/aqua /var/lib/aqua
sudo cp aqua /opt/aqua/aqua && sudo chown aqua:aqua /opt/aqua/aqua

sudo cp .env /etc/aqua/aqua.env        # fill in real secrets
sudo chmod 600 /etc/aqua/aqua.env && sudo chown root:root /etc/aqua/aqua.env

sudo cp aqua-api.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now aqua-api
```

**Upgrading**: replace `/opt/aqua/aqua` and `sudo systemctl restart aqua-api`
(migrations run automatically on startup).

### Option 4 — From source (development)

```bash
cd web && npm ci && npm run build && cd ..   # optional: repo ships a placeholder web/dist

go build -o bin/aqua ./cmd/aqua
export AQUA_APP_KEY="<your master key>"
./bin/aqua -config ./aqua.json
curl http://127.0.0.1:8787/healthz
```

> **The frontend is embedded** via `go:embed`, so **deployment is a single file**.
> Building without running the frontend build leaves a placeholder page — the API still works.

### Reverse proxy

Put Nginx or Caddy in front for HTTPS. Two things that bite people:

```nginx
location / {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;

    # 1) Streaming must not be buffered, or the UI waits for the whole answer
    proxy_buffering off;

    # 2) Must exceed the gateway's upstream timeout (300s), or long answers get cut
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

> Behind Cloudflare's orange-cloud proxy, origin fetches are hard-capped at **100 seconds**
> (you'll see 524s). To use the full 300-second timeout, add a **DNS-only (grey cloud)** record.

---

## Configuration

Priority: **defaults < config file < environment variables**.

| Variable | Required | Notes |
|---|---|---|
| `AQUA_APP_KEY` | ✅ | Encryption master key, **env only** (config field ignored). Generate with `aqua -gen-key` |
| `AQUA_SERVER_LISTEN` | | Default `127.0.0.1:8787`; must be `0.0.0.0:8787` inside containers |
| `AQUA_SERVER_MODE` | | `debug` / `release` / `test` |
| `AQUA_DATABASE_DRIVER` / `AQUA_DATABASE_DSN` | | `sqlite`, default `./data/aqua.db` |
| `AQUA_RELAY_GROUP` | | Gateway **default group** (where group-less tokens resolve), default `default` |
| `AQUA_SMTP_*` | | Outbound mail (signup codes, notifications) |
| `AQUA_EPAY_KEY`, `AQUA_STRIPE_SECRET_KEY`, `AQUA_STRIPE_WEBHOOK_SECRET`, `AQUA_ALIPAY_PRIVATE_KEY`, `AQUA_ALIPAY_PUBLIC_KEY`, `AQUA_WECHATPAY_APIV3_KEY`, `AQUA_WECHATPAY_PRIVATE_KEY`, `AQUA_WECHATPAY_PLATFORM_PUBLIC_KEY` | | Payment credentials |
| `AQUA_LOG_LEVEL` / `AQUA_LOG_FORMAT` | | `debug`/`info`/`warn`/`error`, `text`/`json` |

Full sample: [`.env.example`](.env.example).

### Two security rules

1. **Secrets never enter the database.** Payment, SMTP and the master key are env-only;
   only operational parameters (gateway URL, merchant ID, FX rate, limits, toggles) live in
   the database and are editable in the console. Even a full database dump yields no usable credential.
2. **Back up the master key separately.** Change it and every stored upstream key becomes
   undecryptable — you would have to re-enter them all.

---

## Client integration

Any OpenAI-compatible client works: point the Base URL at AQUA-API and use an AQUA-API token as the key.

### curl

```bash
curl https://your-domain/v1/chat/completions \
  -H "Authorization: Bearer sk-your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "your-model",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

### OpenAI SDK (Python)

```python
from openai import OpenAI

client = OpenAI(base_url="https://your-domain/v1", api_key="sk-your-token")
resp = client.chat.completions.create(
    model="your-model",
    messages=[{"role": "user", "content": "hello"}],
)
print(resp.choices[0].message.content)
```

### Claude Code / Anthropic clients

AQUA-API speaks Anthropic natively, so it can take over Claude Code traffic directly:

```bash
export ANTHROPIC_BASE_URL=https://your-domain
export ANTHROPIC_AUTH_TOKEN=sk-your-token
claude
```

### Other clients

Cursor, Codex CLI, Cherry Studio, NextChat, LobeChat and similar: choose
"OpenAI-compatible / custom OpenAI endpoint" and paste the Base URL and token.

---

## FAQ

**`/healthz` returns 503?**
That means the database is unreachable. Check the log for the database error; with SQLite,
verify permissions on the data directory first.

**Why can't I see upstream keys in plaintext in the console?**
By design. Keys are AES-256-GCM encrypted at rest and shown masked — a compromised console
cannot export usable credentials. To rotate one, just overwrite it.

**After moving a channel to a new group, every token says "no available channel".**
The most common trap. The gateway has a **default group** (`AQUA_RELAY_GROUP`) that decides
where group-less tokens look for channels. After moving channels you must update it and
restart, or existing tokens lose their route instantly.

**Why is a free model still blocked by quota?**
Models with no matching price rule **skip reservation** and should not be blocked. If one is,
check whether the group has a **wildcard price rule** (e.g. `*`) — that makes the model "priced".

**Frequent upstream 429s / timeouts?**
429 is a **credential-level** failure: the gateway rotates to another key and puts that key
into a short cooldown (exponential backoff, auto-recovery). If it happens a lot, you likely
have too few keys or a low per-key limit — add keys or lower the per-minute cap.

**How do I back up?**
Stop the service (or use `VACUUM INTO` for a hot copy) → copy `aqua.db` → **and back up
`AQUA_APP_KEY`**. Without the master key the upstream keys in that backup are undecryptable bytes.

**MySQL / PostgreSQL support?**
SQLite only today, which covers self-hosting and small-to-medium scale. The storage layer
already has a dialect seam (per-dialect migration directories, a driver registry with field
metadata) so adding one later does not require rewriting the business layer.

**How do I add a new upstream type?**
Register its metadata in `internal/channeltype/catalog.go` (default base URL, auth mode, extra
required parameters, path template, capability flags). If it belongs to an existing protocol
family (OpenAI-compatible), that is all. A different protocol needs an adapter in `internal/relay/`.

**Why don't I see my upstream key in the logs?**
Also by design: logs print whether credentials were injected plus the upstream host and path —
**not even the query string**, so query-param keys cannot leak.

---

## Roadmap

- [x] Protocol conversion (OpenAI ↔ Anthropic ↔ Gemini) with bidirectional streaming and tool calls
- [x] Five credential scheduling strategies, cooldown/half-open, session affinity, in-flight counting
- [x] Reserve / settle / refund quota system; incremental streaming usage parsing
- [x] Groups & multipliers, model plaza, redeem codes, five payment channels, async tasks
- [x] Single-binary + Docker deployment with an embedded frontend
- [ ] AWS Bedrock / Google Vertex signature auth
- [ ] Image / video / audio upstream adapters
- [ ] Model ID mapping wired into the forwarding path (already configurable in the console)
- [ ] Standalone admin entrypoint + browser-based install wizard
- [ ] Subscription quota windows (auto-reset every 5h / day / week)
- [ ] Admin action audit log, announcements & FAQ, proxy pool

---

## Development

```bash
go build ./...       # build
go test ./...        # test
gofmt -w .           # format

cd web && npm ci && npm run type-check && npm run build   # frontend
```

### Mandatory conventions

1. **Small commits** — commit each independently describable step immediately; every commit
   should be buildable and revertible.
2. **AI-friendly comments** — every source file starts with an **Intent / Flow / Extension**
   header so anyone (human or AI) understands in 30 seconds what it does, how data flows, and
   where to extend it.
3. **Secrets never in the database, on disk, or in logs.**

See [`AGENTS.md`](AGENTS.md) (Chinese).

### Layout

```
cmd/aqua/              entrypoint (wiring only)
internal/config/       config loading & validation
internal/model/        domain models & repository interfaces
internal/store/        persistence (SQL + versioned migrations, per dialect)
internal/server/       HTTP layer (routing / middleware / handlers)
internal/relay/        protocol adaptation & forwarding (core domain)
internal/payment/      payment channel adapters
internal/channeltype/  channel type registry
web/                   frontend (build output embedded into the binary)
Dockerfile             multi-stage: frontend → backend → minimal runtime
aqua-api.service       systemd unit for bare-metal deployment
```

---

## License

Source code is licensed under [**GNU AGPL-3.0**](LICENSE).

> **If you offer it as a network service**, AGPL section 13 requires you to make the
> corresponding source available to your users. Private/internal use is unaffected, but keep
> the copyright notices intact.

Brand assets (`favicon.ico`, the "AQUA-API" name and marks) are not covered by the source
license — see [NOTICE](NOTICE).

---

<div align="center">

**If this saved you an afternoon of reconciling invoices, a star is appreciated ⭐**

[🏠 Live demo](https://aqua.ltzy.top) ｜ [🐛 Issues](https://gitee.com/xiaosu4610/AQUA-API/issues) ｜ [📖 简体中文](README.md)

</div>
