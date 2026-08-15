# Configuration

How JungleBlock stores and applies configuration. Everything lives in a single
SQLite file (`config.db`) that JungleBlock reads once at boot and rebuilds live
on every change — **no restart, zero DNS downtime**. This page covers the
storage model, the settings the web UI exposes, backup/restore, and the
first-run password.

JungleBlock is a privacy-first DNS appliance — a hard fork of
[blocky](https://github.com/0xERR0R/blocky) by 0xERR0R (Apache-2.0). The Go
module path stays `github.com/0xERR0R/blocky`, so config keys and YAML shapes
are blocky's; the appliance behavior on top is JungleBlock's. It is a resolver +
blocker + appliance — **not a DHCP server**.

Related pages: [Architecture](architecture.md) ·
[Deployment](deployment.md) · [API reference](api-reference.md) ·
[README](../README.md).

---

## The configstore model

Configuration is one SQLite database, `config.db`, in the data directory
(`/data` in the container, `/var/lib/jungleblock` on the Pi). It is **not** a
plain YAML file — it is a raw-YAML blob plus typed section tables that overlay
it.

| Layer | What it is | Edited by |
|---|---|---|
| **Raw-YAML blob** | Single-row full config, the source of truth | Settings → raw YAML editor |
| **Upstream tables** | Typed rows for upstream groups + strategies | Upstreams screen |
| **Blocking tables** | Categories, client segments, allow/deny entries | Blocking / Lists screens |
| **Auth table** | Password hash + session secret (never in the blob) | Login / first-run setup |

On load, JungleBlock parses the YAML blob through the full validation pipeline
(defaults → strict unmarshal → migrate → validate), overlays the typed tables
on top, then **re-validates the merged result**. Anything invalid is rejected
before it can go live.

Two things to know about the overlay:

- **Upstreams always overlay.** Upstream groups from the typed tables replace
  what the blob says.
- **Blocking overlays only in `sqlite` query-log mode.** The blocking category
  sources stream out of the query-log database, so the blocking tables (and the
  Lists/Groups screens, the noise engine, the live stream, the disk guardian,
  and the stats reader) all require `queryLog.type: sqlite`. This is the
  default and the required mode for the full appliance feature set — run it.

Every typed mutation (toggle a category, add an allow entry, assign a segment)
is validated by overlaying the candidate onto the blob and running the full
config validation **before** it is persisted. Nothing invalid ever lands in
`config.db`.

---

## Apply: how changes take effect

Editing config never restarts the process. Every write bumps the blob's
`updated_at`; the Settings screen shows a **dirty** flag when the stored config
differs from what is currently serving. You make it live by hitting **Apply**.

```
edit setting  →  validated + persisted to config.db  →  "dirty"
                                                          │
                                              click Apply │  (RequestApply)
                                                          ▼
                        supervisor rebuilds only the resolver chain,
                        swaps it in atomically, listeners keep serving
```

On Apply, JungleBlock builds a **new resolver bundle** (the whole chain, the
upstream tree, the blocking rules, the noise engine, the list updater) off to
the side. On success it swaps the new bundle in atomically; in-flight queries
finish on the old chain, new queries pick up the new one. On any build error the
**old bundle stays live** and the error is returned — you are never left serving
nothing. A bad new config is logged and dropped; the running config keeps
serving.

**Most changes hot-swap with zero downtime.** A few force a brief full restart
because they touch the listeners themselves:

- DNS/HTTP ports, TLS cert/key/min-version, HTTP/3
- Prometheus enable, query-log target
- toggling the **noise engine** or the **list updater** on/off

Everything else — upstreams, strategies, blocking, groups, clients, privacy
knobs, local DNS, conditional forwarding — is a live hot-swap.

---

## Settings the web UI exposes

Each screen writes to the store and takes effect on Apply. See the
[API reference](api-reference.md) for the exact `/api/ui/*` endpoints behind
each.

### Upstreams and strategies

Per-group upstream servers and the strategy used to pick among them.

- **Recursive-from-root** (default group's default strategy): resolves
  iteratively from the root servers with DNSSEC validation on. Needs **no
  upstreams** to work. If you do configure upstreams on a recursive group, they
  become a fallback tier used only when recursion fails.
- **Encrypted upstreams**: `tls://` for DoT, `https://` for DoH.
- **Strategies**: `parallel_best` (races two, fastest wins),
  `strict`, `round_robin`, plus fork strategies (`time_hop`, `domain_shard`).
- Per-group entry edits swap live without a full rebuild.

```yaml
upstreams:
  groups:
    default:
      - 9.9.9.9            # Quad9, as a fallback tier
      - 149.112.112.112
    # tls://1.1.1.1        # DoT example
    # https://dns.google/dns-query   # DoH example
  strategy: recursive
  timeout: 2s
```

### Blocking: allow/deny, adlists, groups

The Blocking and Lists screens drive the blocking tables.

- **Categories**: embedded blocklist categories (from blocklistproject) you
  toggle on/off. Default-on set is small on purpose (~100 MB, not ~540 MB):
  `ads, tracking, phishing, scam, ransomware, fraud`. The giants
  (malware, abuse, porn) and content filters stay opt-in.
- **Adlists**: add your own blocklist URLs on the Lists screen.
- **Allow / deny entries**: manual always-allow / always-block, each with a
  per-entry enable toggle and a comment. The type — exact, wildcard (`*.`), or
  regex (`/…/`) — is **auto-derived from the domain syntax**; you don't pick it.
- **Household groups**: named policy bundles assigned to devices by
  **name / IP / CIDR** (no MAC — the resolver matches on name/IP/CIDR). Enable
  or disable a group live with zero DNS downtime. A device with segment rows
  gets exactly those categories instead of the global enabled set.

### Clients

The Clients screen shows per-client query/block counts and last-seen, a
fingerprint drill-down, and a **device-class** table (auto-detected, with manual
override). Device class feeds the noise engine's per-class shaping.

### Privacy / noise

The Privacy screen configures the cover-traffic **noise engine** — decoy DNS
designed to be indistinguishable from real lookups so an on-path observer can't
profile you — plus EDNS(0) padding and query-case randomization. The noise
engine is off by default in a bare config but **on** in the appliance seed.
Toggling it on/off forces a full restart (it's a listener-level change); tuning
its knobs hot-swaps.

```yaml
privacy:
  decoy:
    enable: true
    queriesPerMinute: 4
  ednsPadding: false            # RFC 7830, default off
  queryCaseRandomization: false # DNS 0x20, default off
```

See [PRIVACY.md](PRIVACY.md) for the full stance and mechanics.

### Conditional forwarding

The conditional-forwarding UI sends reverse-DNS and local names to your router
(so `192.168.x.x` PTR lookups and bare hostnames resolve on the LAN) while
everything else goes through the resolver.

### Local DNS

A structured editor for local DNS records (the `customDNS` zone) — static
name→IP mappings served straight from JungleBlock.

### Theme

Light/dark toggle. Purely a UI preference.

### Raw YAML editor

The Settings screen has a read-only config summary and a **raw YAML editor**
that edits the blob directly. Validate before saving (empty body validates the
stored blob); saving persists through the full pipeline and only lands on
success. The auth table lives in its own table, **not** the blob — a raw save
never clobbers your password.

---

## Backup and restore

One-click backup of the **entire `config.db`** — the JungleBlock equivalent of
Pi-hole's Teleporter. Download it from the UI, restore it with validation.

- **Included**: everything in `config.db` — upstreams, blocking rules, groups,
  clients, privacy, local DNS, the raw blob.
- **Excluded**: querylog history (that lives in the separate `querylog.db`).
- Restore is validated before it replaces the running config.

You can also back up out-of-band by copying the file while the box is idle:

```bash
# container data dir is /data; Pi host path is /var/lib/jungleblock
cp /var/lib/jungleblock/config.db  config.db.bak
```

`config.db` is SQLite in WAL mode with a single writer — copy it when quiet, or
use the UI backup which snapshots it safely.

---

## Auth and the first-run password

The whole web UI and `/api/ui/*` sit behind a session gate.

- **First run**: set an admin password. It is hashed with **argon2id**
  (64 MiB, t=1, p=4) — the plaintext is never stored.
- **Login**: issues a signed session cookie (HMAC-SHA256 over `uid|expiry`)
  backed by a persisted 32-byte session secret. Rotating the secret invalidates
  every cookie.
- **Lockout**: per-IP failed-login lockout over a 15-minute window.
- **Loopback is exempt**: requests from localhost pass ungated, which is how the
  local tty/HDMI dashboard keeps polling without a cookie.
- **Legacy `/api/*` control routes** (OpenAPI/Grafana) stay ungated for
  cookieless callers; only `/api/ui/*` and the SPA are gated.

**Honest limit:** without TLS, the password and cookie ride **cleartext on the
LAN**. Auth closes the "any LAN device can own the box" hole — it does **not**
protect against an on-path sniffer. Put TLS in front if that's your threat
model.

The web UI binds **LAN-only**. The only thing JungleBlock sends to the internet
is real DNS resolution plus indistinguishable decoy DNS — **no telemetry, no
phone-home, nothing else**.

---

## The YAML blob shape

The blob is a standard blocky config. A minimal appliance blob:

```yaml
ports:
  dns: 53
  http: 80

upstreams:
  groups:
    default:
      - 9.9.9.9
      - 149.112.112.112
  strategy: recursive
  timeout: 2s

queryLog:
  type: sqlite          # required for the full appliance feature set
  target: /data/querylog.db

privacy:
  decoy:
    enable: true
    queriesPerMinute: 4

blocking:
  # categories, allow/deny, groups, and client segments are managed
  # through the typed tables and overlaid on load — you rarely edit
  # these by hand in the blob.
```

When JungleBlock imports a seed config, it stores explicit values for the noise
engine's knobs — omitted fields would seed as Go zero-values (i.e. off), so the
seed sets each one deliberately. When editing by hand, prefer the UI screens
over the raw blob for anything the typed tables own (upstreams, blocking); the
typed tables overlay the blob on load and are what the UI writes.
