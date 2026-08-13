# blocky — unattended Raspberry Pi 3 appliance image

Bake an SD/USB image that turns a **Raspberry Pi 3** into a plug-and-play
blocky box: flash the image, plug in wired ethernet (DHCP) + power, and it
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
3. `blocky.service` seeds its config once from `/boot/firmware/appliance.yml`
   (into `/var/lib/blocky`) and starts serving. It binds the privileged ports
   :53 and :80 via `AmbientCapabilities=CAP_NET_BIND_SERVICE` — **no root**.
4. `blocky-dashboard.service` renders the console dashboard on **tty1 (HDMI)**.

Then point your LAN's DHCP DNS (or each device) at the Pi's IP. The web UI is at
`http://<pi-ip>/`.

## What's on the image

| File | Purpose |
|------|---------|
| `/usr/local/bin/blocky` | the arm64 binary |
| `/etc/systemd/system/blocky.service` | resolver + ad-blocker + noise machine (`--db-dir /var/lib/blocky`) |
| `/etc/systemd/system/blocky-dashboard.service` | htop-style console dashboard on tty1 |
| `/boot/firmware/appliance.yml` | first-boot seed config (port 80, recursive default, noise on) |

Edit `appliance.yml` on the boot partition before first boot to change ports,
upstreams, or privacy defaults. After first boot, use the web UI.

## arm64 note

The Pi 3 is a Cortex-A53 (arm64). The image is 64-bit Raspberry Pi OS Lite,
matching the `linux/arm64` binary the CI already builds. (An older Pi needing
32-bit can rebuild with `GOARCH=arm GOARM=7` and a `raspios_lite_armhf` base.)
