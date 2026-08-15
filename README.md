[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

# JungleBlock

**A privacy-first DNS appliance for your home network — resolve, block, and disappear into the noise.**

JungleBlock is a self-hosted DNS box that resolves recursively from the root, blocks ads and
trackers per-client, and emits indistinguishable decoy DNS so an on-path observer can't profile what
you actually look up. Flash one image to a Raspberry Pi, plug in ethernet, point your router's DNS at
it — done. It is a **hard fork of [blocky](https://github.com/0xERR0R/blocky)** by 0xERR0R
(Apache-2.0); the Go module path stays `github.com/0xERR0R/blocky` (so code and import examples
legitimately show it) — only the product and branding are JungleBlock.

> JungleBlock is **not a DHCP server.** It is a resolver + blocker + appliance, on purpose. Hand out
> leases with your router; hand out *answers* with JungleBlock.

## Features

**Core (shipped, stable)**

- **Recursive-from-root resolution** with DNSSEC validation — no dependence on a third-party
  resolver; bogus answers become SERVFAIL.
- **Encrypted upstreams** — DoH (`https://`) and DoT (`tls://`) when you do forward, with optional
  EDNS(0) padding and 0x20 case randomization.
- **Per-client blocking** — 25 embedded blocklist categories, allow/deny rules, per-device policy.
- **Cover-traffic noise engine** — emits indistinguishable decoy DNS shaped to a household diurnal
  curve, drawn from your own replayed queries + a Tranco-1M corpus, replayed in real page-load
  cohorts with matched fingerprints. On-path observers can't tell real lookups from chaff.
- **Zero-downtime config hot-swap** — the resolver rebuilds on apply while listeners keep serving;
  no dropped queries.
- **SQLite query log** with hourly aggregate rollups and a disk guardian that auto-trims oldest raw
  rows to keep the appliance from filling its card.
- **Live SSE query stream** and a reactive **TUI dashboard** on HDMI/tty1.
- **Raspberry Pi image build** plus a host systemd **auto-update timer**.
- **Live QPS counters** (10s / 1m / 5m / 10m / 1h) and a per-core CPU/mem/disk header.

**New — supersede-your-Pi-hole parity**

- **Login & auth** — first-run password setup, signed-cookie session gating the whole web UI and
  `/api/ui/*`, argon2id hashing, per-IP login lockout. Loopback stays exempt so the local console
  dashboard keeps working.
- **Backup / restore** — one-click download of the entire `config.db` and validated restore
  (Pi-hole "Teleporter" equivalent); query-log history is excluded.
- **Lists screen** — add your own blocklist URLs (adlists), per-entry enable toggle and comment on
  allow/deny; exact / wildcard / regex types auto-derived from the domain syntax.
- **Household groups** — named policy bundles assigned to devices by name / IP / CIDR, enabled or
  disabled live with zero DNS downtime.
- **UX** — light/dark theme toggle, conditional-forwarding UI (send reverse-DNS + local names to
  your router), dashboard badges (recursive-from-root indicator, noise-engine impact ratio,
  device-class chips).
- **Appliance** — native-binary auto-refresh so the tty dashboard never goes stale after a container
  update.

## JungleBlock vs Pi-hole

Pi-hole is a fine LAN ad-blocker. JungleBlock aims a rung higher on privacy while matching the
household features people actually use.

| | Pi-hole | JungleBlock |
|---|---|---|
| Block ads/trackers per-client | Yes | Yes |
| Login, backup, adlists, groups | Yes | Yes |
| **Recursive-from-root** resolver | via add-on | **built in** (DNSSEC-validating) |
| **Encrypted upstreams** (DoH/DoT) | via add-on | **built in** |
| **Cover-traffic noise** (decoy DNS) | No | **Yes** |
| **Zero-downtime config hot-swap** | No | **Yes** |
| **DHCP server** | Yes | **No — out of scope** |

What JungleBlock adds beyond parity: it can resolve **without trusting any upstream at all**, it can
**encrypt** the upstreams it does use, it **buries your real lookups in indistinguishable noise**, and
it **applies config changes without dropping a single query**. What it deliberately leaves to your
router: **DHCP**.

## Quick start

### Docker

```bash
docker run -d \
  --name jungleblock \
  -p 53:53/tcp -p 53:53/udp \
  -p 4000:4000/tcp \
  -v /path/to/data:/data \
  ghcr.io/homeprojectslab/jungleblock:latest serve --db-dir /data
```

Point your router (or a single client) at the host on port 53, then open the web UI at
`http://<host>:4000`. On first launch JungleBlock self-creates `config.db` (seeded recursive, so it
resolves immediately) and `querylog.db`. Set the admin password on first visit.

### Raspberry Pi image

Flash one image, boot, done — DNS on `:53`, web UI on `:80`, live dashboard on HDMI. A Pi 3 sustains
~1000–1100 QPS.

```bash
make image        # builds ./jungleblock-rpi3-arm64.img.xz
```

The full build, flash, first-boot, and auto-update flow is in **[docs/deployment.md](docs/deployment.md)**.

## Privacy

The point of the box: **only indistinguishable DNS leaves it.**

- The **web UI and `/api/ui/*` bind LAN-only** and are gated behind login.
- The only thing JungleBlock sends to the internet is DNS — real queries plus decoy queries an
  on-path observer can't tell apart. **No telemetry, no phone-home, nothing else.**

Honest limits: without TLS on the LAN, the login cookie rides in cleartext — auth closes the "any LAN
device owns the box" hole, not on-path sniffing. And cover traffic makes de-anonymization *expensive
and probabilistic*, not impossible. The full threat model is in **[docs/PRIVACY.md](docs/PRIVACY.md)**.

## Screenshots / UI tour

_Coming soon._ The web UI ships Dashboard, Live, Queries, Clients (with fingerprint drill-down),
Upstreams, Blocking, Lists, Privacy/Noise, Local DNS, Settings, and System pages — plus the
htop-style HDMI console dashboard. See **[docs/api-reference.md](docs/api-reference.md)** for the
page/route map.

## Documentation

| Doc | What's in it |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Resolver chain, blocking model, config-store hot-swap, noise engine, query log |
| [docs/deployment.md](docs/deployment.md) | Pi image build, flashing, first boot, auto-update, container runtime |
| [docs/configuration.md](docs/configuration.md) | Every config knob and how the SQLite config store works |
| [docs/api-reference.md](docs/api-reference.md) | HTTP API (`/api/ui/*`), SSE stream, web pages, TUI |
| [docs/PRIVACY.md](docs/PRIVACY.md) | The honest threat model for the noise engine |

## Credits & License

JungleBlock is a hard fork of **[0xERR0R/blocky](https://github.com/0xERR0R/blocky)** by Dimitri
Herzog and contributors, licensed under the **Apache License 2.0**. The upstream resolver chain,
blocking engine, caching, and DNS-protocol support are their work; this fork adds recursive-from-root
resolution, the SQLite config store, query recording with client fingerprinting, the web UI, the
appliance tooling, and the noise engine. The Go module path remains `github.com/0xERR0R/blocky` and
the original license is retained in [LICENSE](LICENSE). If you want the lean, file-configured upstream
project, go support it directly.
