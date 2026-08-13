[![CI/CD](https://img.shields.io/github/actions/workflow/status/HomeProjectsLab/blocky/ci.yml?branch=main "CI/CD")](https://github.com/HomeProjectsLab/blocky/actions/workflows/ci.yml)
[![Container image](https://img.shields.io/badge/ghcr.io-homeprojectslab%2Fblocky-2496ED?logo=docker&logoColor=white)](https://github.com/HomeProjectsLab/blocky/pkgs/container/blocky)
[![Go version](https://img.shields.io/github/go-mod/go-version/HomeProjectsLab/blocky "Go version")](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

# blocky (HomeProjectsLab fork)

A privacy-first, self-hosted DNS server for the home lab. One static Go binary that is a
**recursive/forwarding resolver**, an **ad-blocker**, a **DNS noise machine**, and a **web UI** —
configured entirely through an embedded SQLite database, no YAML files to hand-edit.

This is a **hard fork** of [0xERR0R/blocky](https://github.com/0xERR0R/blocky) (Apache-2.0). It
keeps blocky's fast resolver and blocking engine and adds recursive resolution, a SQLite config
store, query recording with client fingerprinting, a full web UI, and a configurable DNS decoy
("noise") engine. The Go module path is still `github.com/0xERR0R/blocky`; upstream attribution and
license are intact — see [Attribution](#attribution).

> **Privacy is probabilistic, not magic.** The noise engine makes de-anonymizing your DNS traffic
> *expensive*, it does not make it impossible, and it defends the **wire**, not the host. Read
> [docs/PRIVACY.md](docs/PRIVACY.md) — the honest threat model — before you rely on any of this.

## What it is

- **Resolver** — recursive by default (iterative resolution from the root via [zmap/zdns
  v2](https://github.com/zmap/zdns), DNSSEC-validating), or forwarding to upstreams you configure.
  Per-group strategy: `recursive`, `parallel_best`, `strict`, `random`, `round_robin`,
  `weighted_round_robin`, `weighted_random`, `time_hop`, `domain_shard` (salt-rotating). Upstreams
  and strategies swap at runtime, no restart.
- **Ad-blocker** — 25 embedded [blocklistproject](https://github.com/blocklistproject/Lists)
  categories, per-client group segmentation, allow/deny rules, live domain counts. Inherits
  blocky's blocking engine (regex, wildcards, CNAME/IP blocking, scheduled disable).
- **Query recording** — every query to `querylog.db` with rich metadata and **client
  fingerprinting** (transport, TLS, EDNS option order, 0x20, cookie). Write-path aggregate tables
  feed the dashboards; a live SSE stream feeds the UI.
- **The noise machine** — a background decoy engine that emits cover traffic so an on-path or
  upstream observer cannot tell which domains you actually visit. It shapes total egress to a
  household "persona" curve (hides *how much* you browse), draws decoys from your own visited-domain
  corpus + replayed real queries + Tranco-1M + blocklists, emits them in recorded page-load
  *cohorts* with matched fingerprints, and shadow-completes blocked queries so the box looks like a
  normal unprotected device on the wire. See [docs/PRIVACY.md](docs/PRIVACY.md).
- **Web UI** — Dashboard, Live, Queries, Clients (with fingerprint drill-down), Upstreams,
  Blocking, Privacy, Settings, System, and Noise. Go templates + vanilla ES modules + uPlot,
  embedded in the binary. **No frontend toolchain** — no node, no npm, no bundler.

Single static binary. Multi-arch. Configured by SQLite, not files.

## Quick start

### Docker

```bash
docker run -d \
  --name blocky \
  -p 53:53/tcp -p 53:53/udp \
  -p 4000:4000/tcp \
  -v /path/to/data:/data \
  -e BLOCKY_DB_DIR=/data \
  ghcr.io/homeprojectslab/blocky:latest
```

Point your router or a client at the host on port 53, then open the web UI at
`http://<host>:4000`. Images are published for `linux/amd64`, `linux/arm64`, `linux/386`, and
`linux/arm/v7`.

### Bare binary

```bash
# from a release binary or `go build` (see Build)
./blocky serve --db-dir /path/to/data
```

`--db-dir` (or `BLOCKY_DB_DIR`, default the current directory) is the one thing you point at
persistent storage. Everything else is configured after launch.

### First launch

On first start blocky **self-creates** its two databases in `--db-dir`:

- `config.db` — the entire configuration (tiny, transactional). Seeded with a single `default`
  upstream group set to **recursive**, so the box resolves immediately with zero configuration.
- `querylog.db` — query log, aggregates, decoy sources, and the noise corpus (WAL, append-heavy).

The web UI comes up on **`:4000`** with no auth (LAN-only by design — see the privacy doc). Recursive
resolution works out of the box; the noise engine and ad-blocking are configured from there.

## Configuration

There are **no YAML config files** to maintain. Configuration lives in `config.db` and is edited two
ways:

- **Web UI** — the normal path. Upstreams, blocking, privacy/noise, ports, caching, retention, and a
  raw-config editor with validation are all under `http://<host>:4000`.
- **`blocky import`** — one-time migration of an existing upstream-blocky YAML config into the
  database:

  ```bash
  ./blocky import ./config.yml --db-dir /path/to/data
  ```

  Refuses to overwrite a non-empty database without `--force`.

The full configuration schema (every knob, generated) is in
[docs/config.schema.json](docs/config.schema.json). The privacy/noise knobs that matter are
called out in [docs/PRIVACY.md](docs/PRIVACY.md).

## Privacy

The headline feature deserves an honest description of what it does and does **not** protect against.
[docs/PRIVACY.md](docs/PRIVACY.md) is the threat model: what the noise machine defends (an on-path or
upstream observer trying to unmask *which* domains you visit), how the layered design works, and the
residual leaks it does **not** solve — real usage above the persona ceiling, genuine first-ever
visits, the plaintext recursive-default path, and the explicitly out-of-scope local-DB/LAN-UI threat
model. Cover traffic makes de-anonymization expensive and probabilistic; it is not a guarantee.

## Build

Pure Go, no frontend toolchain, no code generation needed to build:

```bash
go build -o blocky .
# or
make build
```

The web UI (templates, ES modules, uPlot) and the blocklists / Tranco-1M decoy list are embedded via
`go:embed`, so the resulting binary is fully self-contained.

## Releases & images

CI (`.github/workflows/ci.yml`) lints and tests on every push and PR, then:

- builds cross-compiled binaries for **amd64 / arm64 / 386 / armv7**, uploaded as workflow artifacts
  (and attached to GitHub Releases on `v*` tags via goreleaser);
- builds and pushes multi-arch container images to **`ghcr.io/homeprojectslab/blocky`** — `:latest`
  and `:<sha>` on `main`, `:<semver>` on tags.

## Attribution

This project is a hard fork of **[0xERR0R/blocky](https://github.com/0xERR0R/blocky)** by Dimitri
Herzog and contributors, licensed under the **Apache License 2.0**. The upstream resolver chain,
blocking engine, caching, and DNS-protocol support are their work; this fork adds the SQLite config
store, recursive resolution, query recording/fingerprinting, the web UI, and the noise engine. The
original license is retained in [LICENSE](LICENSE). If you want the upstream project — a lean,
file-configured, stateless DNS ad-blocker — go support it directly.
