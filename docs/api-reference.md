# JungleBlock HTTP API Reference

This page documents the **web UI API** — the `/api/ui/*` REST surface and the `/api/ui/stream` SSE feed that the JungleBlock web app and the HDMI/tty dashboard consume. It covers every endpoint (grouped by area), the auth gate that protects them, and the `needsApply` → apply contract shared by all config-mutating routes.

JungleBlock is a privacy-first DNS appliance, a hard fork of [blocky](https://github.com/0xERR0R/blocky) by 0xERR0R (Apache-2.0). The Go module path stays `github.com/0xERR0R/blocky`, so import paths in examples legitimately show it — only the product is JungleBlock.

For the resolver internals behind these endpoints see [architecture.md](./architecture.md); for how the appliance is deployed see [deployment.md](./deployment.md); for config-file fields see [configuration.md](./configuration.md).

> **Two API surfaces.** `/api/ui/*` (this page) is the modern, auth-gated web-app API. The **legacy OpenAPI `/api/*`** control routes (blocking control, list refresh, cache control, stats) are a separate, ungated surface kept for Grafana and cross-origin tooling — see the upstream OpenAPI spec at `GET /docs/openapi.yaml`.

---

## Requirements

Most `/api/ui/*` endpoints are backed by the configstore and the SQLite query log.

- **`queryLog.type: sqlite` is the default and required mode** for the full feature set (stats, noise, clients, blocking categories, live stream). It is the appliance default.
- When the configstore is nil (unit tests / raw YAML-import mode), config-backed endpoints answer **`503`** (`storeUnavailable`).
- The SSE stream and stats endpoints answer **`503`** when not in sqlite mode.
- `GET /api/ui/system` always answers **`200`**; missing subsystems report zeroes rather than erroring.

---

## The CRUD → apply contract

Config mutations do **not** take effect immediately. A successful `PUT`/`POST`/`DELETE` on a config-backed resource persists to `config.db` and returns:

```json
{ "needsApply": true }
```

The change is staged, not live. To activate all staged changes with a single zero-downtime hot-swap, call:

```bash
curl -X POST http://<pi-ip>/api/ui/config/apply
```

`apply` triggers the supervisor's `RequestApply` — the resolver chain is rebuilt off to the side and swapped atomically; DNS listeners keep serving throughout. Poll `GET /api/ui/config/status` for the `dirty` flag (staged-but-unapplied) and `lastApplied` / `updatedAt` timestamps. A few changes (ports, TLS, HTTP3, Prometheus, query-log target, toggling the noise engine or updater) force a brief full restart instead of a hot-swap — see [architecture.md](./architecture.md#config-store-hot-swap-zero-downtime).

```bash
# Typical flow
curl -X PUT  http://<pi-ip>/api/ui/blocking/categories/ads -d '{"enabled":true}'   # -> {"needsApply":true}
curl -X POST http://<pi-ip>/api/ui/config/apply                                     # activate
curl -s     http://<pi-ip>/api/ui/config/status                                     # confirm dirty:false
```

---

## Endpoints by area

### Auth

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/ui/auth/setup` | First-run: set the admin password |
| POST | `/api/ui/auth/login` | Sign in; issues the signed session cookie |
| POST | `/api/ui/auth/logout` | Clear the session cookie |
| GET | `/api/ui/auth/status` | Whether a password is configured and the logged-in state |

### System

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ui/system` | Process + storage health tiles (always `200`; missing parts report zero) |
| GET | `/api/ui/stats/overview` | QPS / counters / block-ratio overview tiles |
| GET | `/api/ui/stats/buckets` | Time-bucketed QPS-by-outcome series (`step`, `from`, `to`) |
| GET | `/api/ui/stats/top` | Top lists — domains / clients (`col`, `n`) |
| GET | `/api/ui/stats/latency` | Latency distribution tiles |
| GET | `/api/ui/queries` | Paginated query log (`client`, `domain`, `qtype`, `rtype`, `decoys`, `limit`, `offset`) |

### Blocking, lists & groups

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ui/blocking/` | Blocking state, category grid, per-client segments |
| PUT | `/api/ui/blocking/categories/{name}` | Toggle / configure a blocklist category |
| PUT | `/api/ui/blocking/segments/{client}` | Set a client's block profile / household group segment |
| POST | `/api/ui/blocking/allow` | Add an allowlist entry |
| POST | `/api/ui/blocking/deny` | Add a denylist entry |
| DELETE | `/api/ui/blocking/allow/{id}` | Remove an allowlist entry |
| DELETE | `/api/ui/blocking/deny/{id}` | Remove a denylist entry |
| GET | `/api/ui/localdns/` | Read local DNS records (`customDNS.zone`) |
| PUT | `/api/ui/localdns/` | Replace local DNS records |

**Lists screen.** Adlist (blocklist URL) management and per-entry enable toggle + comment ride the same blocking tables. Allow/deny entry **types are auto-derived** from the domain syntax — plain host = exact, `*.` prefix = wildcard, `/…/` = regex — you do not choose a type.

**Household groups.** Named policy bundles are assigned to devices by **name / IP / CIDR (no MAC** — the resolver matches on name/IP/CIDR). A device's segment rows give it exactly those categories in place of the global set. Enable / disable is live with zero DNS downtime via the hot-swap path.

### Upstreams & conditional forwarding

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ui/upstreams/` | List upstream groups + strategies |
| PUT | `/api/ui/upstreams/groups/{name}` | Create / update an upstream group |
| DELETE | `/api/ui/upstreams/groups/{name}` | Delete an upstream group |
| PUT | `/api/ui/upstreams/groups/{name}/entries` | Replace a group's upstream entries (live per-group swap) |

Conditional-forwarding (send reverse-DNS + local names to your router) is configured through the upstream/config surface; the resolver applies it via the `ConditionalUpstreamResolver` in the chain.

### Clients

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ui/clients` | Client list (queries, blocked, lastSeen) |
| GET | `/api/ui/clients/classes` | Device-class table (auto-detected + manual override) |
| PUT | `/api/ui/clients/classes/{client}` | Set a client's manual device class |
| GET | `/api/ui/clients/{name}` | Client drill-down (history, transports, fingerprint) |

### Privacy & noise

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ui/privacy` | Read the privacy block (decoy / TTL-jitter / EDNS-padding) |
| PUT | `/api/ui/privacy` | Apply a new privacy config block (`204`) |
| GET | `/api/ui/noise/overview` | Decoy / noise-engine overview (decoy-scoped) |
| GET | `/api/ui/noise/buckets` | Decoy-QPS-by-source time series |
| GET | `/api/ui/noise/top` | Top fake / decoy domains |
| GET | `/api/ui/noise/sourcemix` | Decoy source-mix breakdown |

The noise endpoints surface the cover-traffic engine — the decoy DNS that makes real lookups unprofilable to an on-path observer. See [architecture.md](./architecture.md#the-noise-cover-traffic-engine) for how decoys are generated.

### Config, backup & restore

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ui/config/raw` | Fetch the stored raw-YAML config blob |
| PUT | `/api/ui/config/raw` | Replace the raw-YAML config blob |
| POST | `/api/ui/config/validate` | Validate posted YAML (empty body = validate stored) |
| POST | `/api/ui/config/apply` | Trigger the zero-downtime hot-swap (`RequestApply`) |
| GET | `/api/ui/config/status` | `dirty` flag + `lastApplied` + `updatedAt` |

**Backup / restore** (the Pi-hole "Teleporter" equivalent) is one-click download of the entire `config.db` and a validated restore. **Query-log history is excluded** from the backup — only configuration travels.

---

## SSE live stream

```
GET /api/ui/stream
```

A long-lived `text/event-stream` response:

- One `event: query` per resolved query, carrying the **same JSON item shape as `GET /api/ui/queries`** (the shape is shared so field names can't drift).
- A `: ping` comment every **15 s** to keep the connection alive.
- The server's global write timeout is lifted for this response.
- Returns **`503`** when not in sqlite mode.

```bash
curl -N http://<pi-ip>/api/ui/stream
```

The noise wire on the `/noise` page consumes the same stream scoped to decoy events (`decoy=1`).

---

## Auth gate

A session gate (`newSessionGate`) wraps **every route**, registered before all handlers. It protects the SPA page routes **and** all `/api/ui/*`. Bypass rules apply in this order:

1. **Loopback exemption.** Any request from a loopback `RemoteAddr` passes ungated. This is how the local **tty / HDMI dashboard** keeps polling the API cookieless.
2. **Public allowlist** — never gated:
   - `/login`
   - `/static/*` (embedded assets)
   - `/api/ui/auth/*`
   - `/metrics` (Prometheus)
   - the DoH path (`cfg.Ports.DOHPath`, including `/{clientID}`)
3. **Legacy control API.** `/api/*` that is **not** `/api/ui/*` (the OpenAPI / Grafana routes) is left ungated for cookieless cross-origin callers.
4. **Everything else** needs a valid signed session cookie. Unauthenticated `/api/*` → **`401`** JSON; a browser request → **`302`** to `/login`.

### How auth works

- **First-run setup** then signed-cookie login. Passwords are hashed with **argon2id** (PHC string, 64 MiB / t=1 / p=4), verified in constant time.
- The session cookie is an **HMAC-SHA256 over `uid|expiry`** signed with a persisted 32-byte session secret (auto-generated on first read; rotating it invalidates every cookie).
- Auth settings live in their **own `config.db` table**, never the YAML blob — a raw-config save cannot clobber the password.
- Per-IP failed-login **lockout** over a 15-minute window.

> **Honest limit.** Without TLS, the password and cookie ride **cleartext on the LAN**. The gate closes the "any LAN device owns the box" hole; it does not defend against on-path sniffing. This matches JungleBlock's stance: the web UI binds LAN-only, and the only thing the box sends to the internet is indistinguishable DNS queries — no telemetry, no phone-home. See [PRIVACY.md](./PRIVACY.md).

### Other ungated routes (allowlisted, not `/api/ui/*`)

`GET/POST {DOHPath}` and `{DOHPath}/{clientID}` (DoH resolver) · `GET /metrics` (Prometheus) · `GET /static/*` · `GET /docs/openapi.yaml`, `GET /docs/config.schema.json` · `GET /robots.txt` · `/debug/*` (pprof) · the legacy OpenAPI `/api/*` control routes.
