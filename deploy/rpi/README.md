# JungleBlock — unattended Raspberry Pi 3 appliance image

Bake an SD/USB image that turns a **Raspberry Pi 3** into a plug-and-play
JungleBlock box: flash the image, plug in wired ethernet (DHCP) + power, and it
resolves DNS on **:53**, serves the web UI/API on **:80**, and shows a live
**htop-style dashboard on the HDMI console** — no setup, no login, no X.

## Build

```bash
make image           # produces ./blocky-rpi3-arm64.img.xz
# or, to just verify the tooling without the ~450MB base download:
make image-selftest
```

`make image` cross-compiles the `linux/arm64` binary on the host (no cgo), then
runs `deploy/rpi/build-image.sh` inside a **privileged Docker container** (the
only place loop-mounting is allowed). The script customises the official
Raspberry Pi OS Lite (64-bit) image by **pure file injection** into the
loop-mounted partitions — no chroot, no qemu, nothing ARM runs at bake time.
Per-partition *offset* loop devices are used, so it works in containers with no
udev.

Override the base: `BASE_IMG=/path/to/raspios-lite-arm64.img.xz make image`.

## Flash

Write `blocky-rpi3-arm64.img.xz` to the card with Raspberry Pi Imager or
balenaEtcher (both read `.img.xz` directly), or on the command line with
`xzcat` piped into a raw-disk writer targeting your SD/USB device.

## First boot (unattended)

1. Raspberry Pi OS expands the root filesystem (stock `init_resize`, untouched).
2. Wired ethernet comes up via DHCP (RPi OS default — no wifi/country setup).
3. `jungleblock-stack.service` runs `/opt/blocky/bootstrap.sh`, which installs Docker
   (one time, a few minutes on a Pi 3), seeds the config database once from
   `/boot/firmware/appliance.yml`, and brings the compose stack up.
4. The dashboard container renders the console UI on **tty1 (HDMI)**.

Then point your LAN's DHCP DNS (or each device) at the Pi's IP. The web UI is at
`http://<pi-ip>/`.

## Updates are pull-based — no SSH needed

The appliance runs from containers and pull a newer image via a host systemd timer (jungleblock-update.timer) — polling
`ghcr.io/homeprojectslab/jungleblock:latest` **every 30 s**. Push a new image and the
Pi restarts itself onto it within the poll interval — which is why the box needs
no inbound access at all.

Two consequences worth knowing:

- **The ghcr package must be public**, otherwise the Pi cannot pull it
  anonymously and the stack never starts. The first CI push creates a *private*
  package — flip it to public once in the repo's package settings.
- A 30 s interval is ~2,900 registry checks/day. Only the manifest digest is
  fetched so it is cheap, but if you ever hit rate limits, raise `--interval`
  in `compose.yml`.

## What's on the image

| Path | Purpose |
|------|---------|
| `/opt/blocky/compose.yml` | resolver container (dashboard + auto-update are host units) |
| `/opt/blocky/bootstrap.sh` | first-boot Docker install, config seed, `compose up` |
| `/etc/systemd/system/jungleblock-stack.service` | brings the stack up at boot |
| `/boot/firmware/appliance.yml` | first-boot seed config (port 80, recursive default, noise on) |
| `/usr/local/bin/jungleblock` | native binary, kept only as an offline rescue tool — nothing starts it |

Edit `appliance.yml` on the boot partition before first boot to change ports,
upstreams, or privacy defaults. After first boot, use the web UI.

### Why host networking

`compose.yml` runs the resolver with `network_mode: host`, and that is
load-bearing rather than a shortcut: under bridge networking every LAN client
would arrive as the Docker gateway address, which silently breaks per-client
blocking segmentation, client fingerprinting, device-class personas and the
per-client stats. It also avoids a userland proxy hop on every DNS packet.

## arm64 note

The Pi 3 is a Cortex-A53 (arm64). The image is 64-bit Raspberry Pi OS Lite,
matching the `linux/arm64` binary the CI already builds. (An older Pi needing
32-bit can rebuild with `GOARCH=arm GOARM=7` and a `raspios_lite_armhf` base.)
