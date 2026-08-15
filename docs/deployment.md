# Deploying JungleBlock

This page covers running **JungleBlock** — a privacy-first DNS appliance, a hard fork of [blocky](https://github.com/0xERR0R/blocky) by 0xERR0R (Apache-2.0). The focus is the **Raspberry Pi appliance**: flash one image, plug in ethernet and power, and the box resolves DNS on `:53`, serves the web UI on `:80`, and paints a live dashboard on the HDMI console. A generic "run under Docker on any host" quickstart is at the end.

> JungleBlock is a **resolver + blocker + appliance**. It is **not a DHCP server** — that is explicitly out of scope. Point your existing DHCP server's DNS option at the box instead.

For what the box actually does with a query see [architecture.md](architecture.md); for config keys see [configuration.md](configuration.md); for the HTTP surface see [api-reference.md](api-reference.md).

---

## Prerequisites

**For the Pi appliance (recommended path):**

| Need | Detail |
|------|--------|
| Board | Raspberry Pi 3 (Cortex-A53, arm64) or newer |
| Storage | SD card or USB disk to flash the image onto |
| Network | Wired ethernet with DHCP. WiFi is optional and baked in at build time. |
| Build host | Linux with Docker (image bake runs inside a privileged container) and Go toolchain |

**For any host (Docker path):** Docker with the Compose v2 plugin. That's it — the image is `scratch`-based and needs nothing else installed.

A Pi 3 sustains **~1000–1100 QPS** (CPU-bound), degrades gracefully under overload, and shows no memory / goroutine / disk leaks.

---

## Two-artifact model: container + native binary

The same Go binary ships two ways, and this split is the key to the appliance design:

- **Container** (`ghcr.io/homeprojectslab/jungleblock:latest`) runs the actual resolver, web UI, and noise engine. Updated pull-based.
- **Native binary** at `/usr/local/bin/jungleblock`, baked into the image once. It drives the tty1 dashboard (a container with `tty:true` draws into a dockerd-held PTY that never reaches HDMI, so the dashboard must run on the host) and doubles as an offline rescue tool.

Because the native binary is baked once, it goes stale after a container update — fixed by the **native-binary auto-refresh** in the update timer (below).

---

## Building the Pi image

The `Makefile` drives it:

```bash
make image            # → ./jungleblock-rpi3-arm64.img.xz  (with printed sha256)
make image-selftest   # verify the mount/inject/unmount tooling, skips the ~450 MB base download
```

What `make image` does:

1. Host cross-compiles the `linux/arm64` binary — CGO-free, `-trimpath -ldflags="-s -w"`.
2. Runs [`deploy/rpi/build-image.sh`](../deploy/rpi/build-image.sh) inside a **privileged `ubuntu:24.04`** container (the only sanctioned place for loop-mounting; it installs util-linux/dosfstools/e2fsprogs/xz/fdisk).

`build-image.sh` customises the official **Raspberry Pi OS Lite (64-bit) Bookworm** image (pinned `2024-11-19` base, override with `BASE_IMG=`) by **pure file injection** into the loop-mounted boot + root partitions — **no chroot, no qemu, nothing ARM executes at bake time**. It uses per-partition offset loop devices and pre-creates `/dev/loopN` nodes, so it works in containers with no udev. It adds `+512M` headroom before the first-boot rootfs expand and emits `.img.xz` (or pass `--raw` to skip compression).

Optional WiFi is baked at build time via env vars:

```bash
WIFI_SSID="myssid" WIFI_PSK="mypassword" WIFI_COUNTRY="US" make image
```

This writes a NetworkManager keyfile + regulatory country and applies a durable rfkill-unblock fix (the real cure for "Pi 3 WiFi never associates").

### What lands on the image

| Path | Purpose |
|------|---------|
| `/usr/local/bin/jungleblock` | native arm64 binary — drives tty1 dashboard + offline rescue |
| `/opt/jungleblock/compose.yml` | resolver container definition |
| `/opt/jungleblock/bootstrap.sh` | first-boot Docker install, config seed, `compose up` |
| `/opt/jungleblock/refresh-native-binary.sh` | native-binary auto-refresh helper |
| `/etc/systemd/system/jungleblock-stack.service` | brings the stack up at boot |
| `/etc/systemd/system/jungleblock-dashboard.service` | tty1 HDMI dashboard host unit |
| `/etc/systemd/system/jungleblock-update.{service,timer}` | host auto-update timer |
| `/boot/firmware/appliance.yml` | first-boot seed config |
| `/etc/sysctl.d/99-jungleblock.conf` | larger UDP socket buffers + backlog for `:53` |

Two services are **masked** (`ln -sf /dev/null`): `getty@tty1.service` (the dashboard owns tty1) and `userconfig.service` (the headless user is pre-seeded, so its first-boot dialog would otherwise spam "Failed to start userconfig" every 10s). The headless account `jungleblock` exists with a locked/throwaway password so Bookworm will boot headless; SSH is off.

Host sysctls in `99-jungleblock.conf` (`rmem_default=4M`, `rmem_max=wmem_max=8M`, `netdev_max_backlog=4096`) apply because the container uses **host networking** — they let the kernel queue DNS bursts instead of dropping them.

### Flashing

Flash the image with the guarded writer, intended to run inside the same privileged bake container:

```bash
deploy/rpi/flash.sh ./jungleblock-rpi3-arm64.img.xz /dev/sdX
```

`flash.sh` refuses anything that isn't a hot-pluggable USB disk, anything with mounted filesystems, the disk backing the running root, or a target smaller than the image. It writes with `dd bs=4M conv=fsync oflag=direct` then reads back and `cmp`-verifies.

---

## First boot (unattended)

Plug in ethernet + power. No login, no keyboard, no X:

1. RPi OS expands the rootfs (stock `init_resize`).
2. Wired ethernet comes up via DHCP.
3. `jungleblock-stack.service` (oneshot, `RemainAfterExit`, `After=network-online.target`, `TimeoutStartSec=900`) runs `bootstrap.sh`, which:
   - Ensures a working resolver for bootstrap (chicken-and-egg: the box *is* the LAN resolver but isn't up yet — falls back to `9.9.9.9` if `download.docker.com` won't resolve).
   - **Waits for clock sync.** The Pi has no RTC and boots at the ~2024 image build date, so every HTTPS cert reads "not yet valid". It starts `systemd-timesyncd` and waits up to 90×2s for the year to reach ≥2025 before any TLS.
   - Installs Docker if absent (`get.docker.com`, one-time, several minutes on a Pi 3) and verifies the Compose v2 plugin.
   - `chown -R 100:100 /var/lib/jungleblock` so UID 100 in the container can create `config.db` / `querylog.db`.
   - **Seeds config once**: if `/var/lib/jungleblock/config.db` is absent, runs `jungleblock import /appliance.yml` into `/data`.
   - `docker compose pull --quiet` (tolerates failure → cached image) then `up -d --remove-orphans`.
4. `jungleblock-dashboard.service` renders the TUI on tty1/HDMI.

Then point your LAN DHCP DNS option (or each device) at the Pi's IP. Open the web UI at `http://<pi-ip>/`.

### First run: set the admin password

The web UI ships **auth-gated**. On first visit the box has no password, so the login page presents **first-run password setup**:

- Pick an admin password → it is hashed with **argon2id** and stored.
- Subsequent visits require sign-in; a signed session cookie (HMAC-SHA256) gates the whole SPA and every `/api/ui/*` route. Per-IP failed-login **lockout** applies over a 15-minute window.
- **Loopback is exempt**, so the local tty/HDMI dashboard keeps working with no cookie. Legacy `/api/*` control routes (Grafana etc.) stay ungated.

**Honest limit:** without TLS, the password and cookie ride in cleartext on the LAN. Auth closes the "any LAN device owns the box" hole — it is **not** protection against an on-path sniffer. See [PRIVACY.md](PRIVACY.md) for the full stance.

---

## Container runtime (compose.yml)

```yaml
services:
  jungleblock:
    image: ghcr.io/homeprojectslab/jungleblock:latest
    container_name: jungleblock
    restart: unless-stopped
    network_mode: host
    command: ["serve", "--db-dir", "/data"]
    cap_add:
      - NET_BIND_SERVICE
    volumes:
      - /var/lib/jungleblock:/data
      - /boot/firmware/appliance.yml:/appliance.yml:ro
    healthcheck:
      test: ["CMD", "jungleblock", "healthcheck"]
      interval: 30s
      start_period: 60s
```

**`network_mode: host` is load-bearing, not a shortcut.** Under bridge networking every LAN client would arrive as the docker gateway address, silently breaking per-client blocking, client fingerprinting, device-class personas, and per-client stats — and it avoids a userland-proxy hop per DNS packet.

`cap_add: NET_BIND_SERVICE` lets the non-root container bind `:53` and `:80`. The `/data` volume holds `config.db` / `querylog.db` / the decoy list and must be writable by UID 100.

---

## Seed config (appliance.yml)

The first-boot seed at `/boot/firmware/appliance.yml`:

- Ports `dns: 53`, `http: 80` (the box runs nothing else, so the web UI takes standard HTTP 80).
- Default upstream group `9.9.9.9` + `149.112.112.112` with `strategy: recursive` — resolve from root, with Quad9 as a fallback tier used only on recursion failure.
- Querylog `type: sqlite`, `target: /data/querylog.db` (must be under the writable `/data` volume).
- **Noise/decoy engine enabled**, with every knob set explicitly. Import stores Go zero-values for omitted fields, so cohorts / sessions / companions / persona-cover would silently seed **off** if left out. Values mirror the `config/privacy.go` defaults.
- Lists updater enabled.

> **sqlite is the load-bearing mode.** The full appliance feature set — blocking categories, the decoy engine, list updater, live query hub, disk guardian, and the stats reader — all depend on `queryLog.type: sqlite`. Keep it. Details in [configuration.md](configuration.md).

---

## Auto-update (host systemd timer)

Pull-based updates, so the box needs **zero inbound access**. (Watchtower was dropped — its old Docker API client crash-looped against modern Docker Engine.)

- **`jungleblock-update.timer`**: `OnBootSec=2min`, `OnUnitActiveSec=5min`, `Persistent=true`. **Real interval: every 5 minutes.**
- **`jungleblock-update.service`** (oneshot, `Requires`/`After=jungleblock-stack.service`) runs, in order:
  1. `docker compose ... pull --quiet jungleblock`
  2. `docker compose ... up -d jungleblock` — recreates the container only if the pulled image digest changed.
  3. `refresh-native-binary.sh` — `docker create` the image, `docker cp /app/jungleblock → /tmp/jungleblock.new`, and **only if it differs** (`cmp -s`) install it to `/usr/local/bin/jungleblock` and `systemctl restart jungleblock-dashboard.service`. The `cmp` guard prevents a dashboard restart every tick. **This is what keeps the tty dashboard from going stale after a container update.**
  4. `docker image prune -f`.

The service uses the **host** docker CLI, so the API version always matches the daemon.

Manual update / status:

```bash
systemctl start  jungleblock-update.service     # force an update now
systemctl status jungleblock-update.timer       # next scheduled run
journalctl -u    jungleblock-update.service     # what the last run did
```

**Caveat:** the ghcr package must be **public** for anonymous pull. The first CI push creates it private — flip it to public once.

---

## HDMI / tty1 dashboard

`jungleblock-dashboard.service` (host unit, `Type=simple`, `Conflicts=getty@tty1.service`) runs:

```
/usr/local/bin/jungleblock dashboard --api-url http://localhost:80
```

It is a pure API/SSE consumer reaching the host-networked resolver at `localhost:80` and drives the physical console directly (`StandardInput/Output=tty`, `TTYPath=/dev/tty1`, `TERM=linux`). `Restart=always` / `RestartSec=3` **is the readiness wait**: if the API isn't up yet the process exits and systemd retries until `:80` answers.

The dashboard is loopback-exempt (see First run) so it polls cookieless. It renders htop-style stat tiles, QPS sparklines, and per-core CPU/mem/disk from `/proc` and `/sys`.

---

## Run under Docker on any host (quickstart)

Not a Pi? Run the same image anywhere Docker runs.

```bash
mkdir -p /srv/jungleblock/data
```

`compose.yml`:

```yaml
services:
  jungleblock:
    image: ghcr.io/homeprojectslab/jungleblock:latest
    container_name: jungleblock
    restart: unless-stopped
    network_mode: host           # keep for correct per-client attribution
    command: ["serve", "--db-dir", "/data"]
    cap_add:
      - NET_BIND_SERVICE
    volumes:
      - /srv/jungleblock/data:/data
```

```bash
docker compose up -d
docker compose logs -f jungleblock
```

Then:

- DNS on `:53`, web UI on the configured HTTP port (`:80` in the appliance seed; the upstream default is `:4000`).
- Open the UI, complete **first-run password setup**, and point a client's DNS at the host.
- To seed a config file: `docker compose exec jungleblock jungleblock import /path/in/container.yml`, then apply via the UI.

**Notes for non-appliance hosts:**

- If you can't use host networking, publish `53/tcp`, `53/udp`, and the HTTP port explicitly — but be aware every client will then appear as the docker gateway IP, breaking per-client blocking and stats.
- Keep `queryLog.type: sqlite` for the full feature set (see [configuration.md](configuration.md)).
- The native-binary tty dashboard is Pi-appliance-specific; on a generic host just use the web UI.

---

## Zero-downtime config changes

Most config edits hot-swap with no DNS downtime: the supervisor rebuilds only the resolver on apply, and listeners persist. A handful of changes force a brief full restart — ports, TLS cert/key, HTTP3, Prometheus, the query-log target, and toggling the decoy engine or the list updater on/off. Mechanics are in [architecture.md](architecture.md).

Trigger an apply from the UI (Settings → Apply) or the API:

```bash
curl -X POST http://<host>/api/ui/config/apply     # requires a session cookie
```

---

## Upstream credit

JungleBlock is a hard fork of [blocky](https://github.com/0xERR0R/blocky) by 0xERR0R, under Apache-2.0. The `LICENSE` is retained and the Go module path stays `github.com/0xERR0R/blocky` — so build and import examples legitimately still show that path. Only the product and branding are JungleBlock.
