#!/usr/bin/env bash
# Bake an unattended JungleBlock appliance image for the Raspberry Pi 3 (arm64).
#
# It customises the official Raspberry Pi OS Lite (64-bit) image by PURE FILE
# INJECTION into the loop-mounted partitions — no chroot, no qemu, nothing ARM
# is ever executed at bake time. The result is a .img.xz you DD to an SD/USB:
# plug into a Pi 3 with wired ethernet (DHCP), power on, and JungleBlock serves DNS
# on :53 and the web UI on :80 out of the box, with the console dashboard on
# the HDMI output.
#
# Requires root + loop devices (losetup, mount). The sanctioned way to get that
# without touching the host is a privileged container — see `make image`, which
# runs this inside `docker run --privileged`. Per-partition OFFSET loop devices
# are used (not `losetup -P`), so it works in containers with no udev.
#
# Usage:
#   deploy/rpi/build-image.sh [--base <rpi-os-lite.img[.xz]>] [--out <file.img.xz>]
#                             [--self-test]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HERE="$REPO_ROOT/deploy/rpi"

BASE_URL="${BASE_URL:-https://downloads.raspberrypi.com/raspios_lite_arm64/images/raspios_lite_arm64-2024-11-19/2024-11-19-raspios-bookworm-arm64-lite.img.xz}"
BASE_IMG="${BASE_IMG:-}"
OUT="${OUT:-$REPO_ROOT/jungleblock-rpi3-arm64.img.xz}"
SELF_TEST=0
RAW="${RAW:-0}"

while [ $# -gt 0 ]; do
	case "$1" in
	--base) BASE_IMG="$2"; shift 2 ;;
	--out) OUT="$2"; shift 2 ;;
	--self-test) SELF_TEST=1; shift ;;
	# Leave the result uncompressed — for writing straight to a device, where
	# the xz round-trip is pure wasted CPU.
	--raw) RAW=1; shift ;;
	*) echo "unknown arg: $1" >&2; exit 2 ;;
	esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1" >&2; exit 1; }; }
need losetup; need mount; need umount; need xz; need sfdisk

WORK="$(mktemp -d)"
BOOT_MNT="$WORK/boot"; ROOT_MNT="$WORK/root"
BOOTLP=""; ROOTLP=""
mkdir -p "$BOOT_MNT" "$ROOT_MNT"

cleanup() {
	set +e
	mountpoint -q "$BOOT_MNT" && umount "$BOOT_MNT"
	mountpoint -q "$ROOT_MNT" && umount "$ROOT_MNT"
	[ -n "$BOOTLP" ] && losetup -d "$BOOTLP" 2>/dev/null
	[ -n "$ROOTLP" ] && losetup -d "$ROOTLP" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

# --- 1. jungleblock arm64 binary (cross-compiled, no cgo, no ARM executed here) ----
BIN="$WORK/jungleblock"
if [ -n "${BLOCKY_BIN:-}" ] && [ -f "${BLOCKY_BIN}" ]; then
	echo ">> using prebuilt binary: $BLOCKY_BIN"
	cp "$BLOCKY_BIN" "$BIN"
else
	need go
	echo ">> building jungleblock (linux/arm64)…"
	( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags="-s -w" -o "$BIN" . )
fi
echo "   $(du -h "$BIN" | cut -f1) arm64 binary"

# --- 2. obtain / build the base image -----------------------------------------
IMG="$WORK/base.img"
if [ "$SELF_TEST" = 1 ]; then
	echo ">> self-test: fabricating a minimal 2-partition image…"
	need mkfs.vfat; need mkfs.ext4
	truncate -s 320M "$IMG"
	sfdisk "$IMG" >/dev/null <<-SFDISK
		label: dos
		unit: sectors
		start=8192, size=131072, type=c
		start=139264, size=,      type=83
	SFDISK
else
	if [ -z "$BASE_IMG" ]; then
		echo ">> downloading Raspberry Pi OS Lite arm64 base…"; need curl
		curl -fL "$BASE_URL" -o "$WORK/base.img.xz"; BASE_IMG="$WORK/base.img.xz"
	fi
	case "$BASE_IMG" in
	*.xz) echo ">> decompressing base…"; xz -dc "$BASE_IMG" > "$IMG" ;;
	*)    cp "$BASE_IMG" "$IMG" ;;
	esac
	truncate -s +512M "$IMG"   # headroom before the first-boot rootfs expand
fi

# --- 3. per-partition offset loop devices (container-safe, no udev) -----------
# In a container there is no udev, so a loop device freshly allocated by the
# kernel has no /dev node and losetup fails with ENOENT. Pre-create the nodes
# ourselves when we're root (harmless if they already exist).
ensure_loop_nodes() {
	[ "$(id -u)" = 0 ] || return 0
	local i
	for i in $(seq 0 63); do
		[ -e "/dev/loop$i" ] || mknod "/dev/loop$i" b 7 "$i" 2>/dev/null || true
	done
}
ensure_loop_nodes

echo ">> mapping partitions…"
SEC=512
# sfdisk -d dumps "... : start=  N, size=  M, type=.." — take the first two parts.
read -r B_START B_SIZE < <(sfdisk -d "$IMG" | awk -F'[=,]' '/start=/{gsub(/ /,"",$2);gsub(/ /,"",$4);print $2, $4; exit}')
read -r R_START R_SIZE < <(sfdisk -d "$IMG" | awk -F'[=,]' '/start=/{n++; if(n==2){gsub(/ /,"",$2);gsub(/ /,"",$4);print $2, $4; exit}}')
# root size may be blank (grow-to-end) → size it to the rest of the image.
if [ -z "${R_SIZE:-}" ] || [ "$R_SIZE" = "" ]; then
	TOTAL_SEC=$(( $(stat -c%s "$IMG") / SEC ))
	R_SIZE=$(( TOTAL_SEC - R_START ))
fi
BOOTLP="$(losetup --show -f --offset $((B_START*SEC)) --sizelimit $((B_SIZE*SEC)) "$IMG")"
ROOTLP="$(losetup --show -f --offset $((R_START*SEC)) --sizelimit $((R_SIZE*SEC)) "$IMG")"
echo "   boot=$BOOTLP (@$B_START +$B_SIZE)  root=$ROOTLP (@$R_START +$R_SIZE)"

if [ "$SELF_TEST" = 1 ]; then
	mkfs.vfat "$BOOTLP" >/dev/null
	mkfs.ext4 -q -F "$ROOTLP"
fi
mount "$BOOTLP" "$BOOT_MNT"
mount "$ROOTLP" "$ROOT_MNT"

# --- 4. inject ----------------------------------------------------------------
echo ">> injecting JungleBlock stack + units + config…"
# The appliance runs from containers so a new image can be pulled in place
# (no SSH into the box). The native binary is still injected as a rescue tool
# for offline debugging, but nothing starts it.
install -Dm755 "$BIN" "$ROOT_MNT/usr/local/bin/jungleblock"
install -Dm755 "$HERE/bootstrap.sh" "$ROOT_MNT/opt/blocky/bootstrap.sh"
install -Dm644 "$HERE/compose.yml"  "$ROOT_MNT/opt/blocky/compose.yml"
install -Dm644 "$HERE/systemd/jungleblock-stack.service" "$ROOT_MNT/etc/systemd/system/jungleblock-stack.service"
# The console dashboard runs as a HOST unit bound to tty1, NOT a container: a
# container with tty:true draws into a dockerd-held PTY that never reaches HDMI.
install -Dm644 "$HERE/systemd/jungleblock-dashboard.service" "$ROOT_MNT/etc/systemd/system/jungleblock-dashboard.service"
# Read-only telnet broadcast of the same dashboard (headless, no tty) on :2323.
install -Dm644 "$HERE/systemd/jungleblock-telnet.service" "$ROOT_MNT/etc/systemd/system/jungleblock-telnet.service"
install -d "$ROOT_MNT/etc/systemd/system/multi-user.target.wants"
ln -sf ../jungleblock-stack.service     "$ROOT_MNT/etc/systemd/system/multi-user.target.wants/jungleblock-stack.service"
ln -sf ../jungleblock-dashboard.service "$ROOT_MNT/etc/systemd/system/multi-user.target.wants/jungleblock-dashboard.service"
ln -sf ../jungleblock-telnet.service    "$ROOT_MNT/etc/systemd/system/multi-user.target.wants/jungleblock-telnet.service"

# Auto-update (replaces the abandoned Watchtower container): a host systemd
# timer pulls the latest image and recreates jungleblock if it changed.
install -Dm755 "$HERE/refresh-native-binary.sh" "$ROOT_MNT/opt/blocky/refresh-native-binary.sh"
install -Dm644 "$HERE/systemd/jungleblock-update.service" "$ROOT_MNT/etc/systemd/system/jungleblock-update.service"
install -Dm644 "$HERE/systemd/jungleblock-update.timer"   "$ROOT_MNT/etc/systemd/system/jungleblock-update.timer"
install -d "$ROOT_MNT/etc/systemd/system/timers.target.wants"
ln -sf ../jungleblock-update.timer "$ROOT_MNT/etc/systemd/system/timers.target.wants/jungleblock-update.timer"
ln -sf /dev/null "$ROOT_MNT/etc/systemd/system/getty@tty1.service"   # host jungleblock-dashboard.service owns tty1
# The user is seeded via userconf.txt, so RPi's first-boot account dialog has
# nothing to do — but it stays enabled and spams "Failed to start userconfig"
# every 10s on the console. Mask it.
ln -sf /dev/null "$ROOT_MNT/etc/systemd/system/userconfig.service"
# Larger UDP socket buffers + backlog: under sustained DNS load the listener
# should QUEUE bursts in the kernel instead of dropping them — trading a little
# latency for far fewer timeouts. JungleBlock runs with host networking, so these host
# sysctls apply to its :53 socket (rmem_default is the buffer every UDP socket
# gets without asking).
install -d "$ROOT_MNT/etc/sysctl.d"
cat > "$ROOT_MNT/etc/sysctl.d/99-blocky.conf" <<'SYSCTL'
net.core.rmem_default = 4194304
net.core.rmem_max = 8388608
net.core.wmem_max = 8388608
net.core.netdev_max_backlog = 4096
SYSCTL

install -Dm644 "$HERE/appliance.yml" "$BOOT_MNT/appliance.yml"

# Headless user account. Raspberry Pi OS bookworm REQUIRES a user or it drops to
# an interactive "create user" prompt on the console and never boots headless
# (the "asks for username" hang). Seed one non-interactively via userconf.txt.
# The password is a throwaway random hash we immediately discard, so the account
# exists (satisfies first boot) but cannot be logged into — no console recovery,
# matching the appliance's locked-down policy. SSH is off regardless.
APPLIANCE_USER="${APPLIANCE_USER:-blocky}"
USER_HASH="${APPLIANCE_USER_HASH:-}"
if [ -z "$USER_HASH" ]; then
	if command -v openssl >/dev/null 2>&1; then
		USER_HASH="$(openssl passwd -6 "$(head -c 32 /dev/urandom | base64 | tr -d '\n=')")"
	else
		USER_HASH='*'  # locked password; chpasswd -e accepts it
	fi
fi
printf '%s:%s\n' "$APPLIANCE_USER" "$USER_HASH" > "$BOOT_MNT/userconf.txt"
echo ">> baked headless user '$APPLIANCE_USER' (locked password) — suppresses first-boot prompt"

# WiFi provisioning removed 2026-08-15 — this is a wired-only appliance. It now
# lives on a dedicated routed switch port; a dual-homed WiFi radio caused DNS
# reply asymmetry. WIFI_SSID/WIFI_PSK env vars are intentionally ignored.

# verify what we placed
echo ">> injected tree:"; ls -l "$ROOT_MNT/usr/local/bin/jungleblock" \
	"$ROOT_MNT/etc/systemd/system/multi-user.target.wants/" "$BOOT_MNT/appliance.yml"

sync
echo ">> unmounting…"
umount "$BOOT_MNT"; umount "$ROOT_MNT"
losetup -d "$BOOTLP"; BOOTLP=""
losetup -d "$ROOTLP"; ROOTLP=""

# --- 5. package ---------------------------------------------------------------
if [ "$SELF_TEST" = 1 ]; then
	echo ">> self-test OK: real partition table mapped via offset loops, binary +"
	echo "   units + enable-symlinks + appliance.yml injected, cleanly unmounted."
	exit 0
fi
if [ "$RAW" = 1 ]; then
	echo ">> writing raw image to $OUT …"
	cp "$IMG" "$OUT"
else
	echo ">> compressing to $OUT …"
	xz -T0 -6 -c "$IMG" > "$OUT"
fi
echo ">> done: $OUT ($(du -h "$OUT" | cut -f1))"
echo "   sha256: $(sha256sum "$OUT" | cut -d' ' -f1)"
cat <<EOF

Flash it:  xzcat "$OUT" | sudo dd of=/dev/sdX bs=4M conv=fsync status=progress
       (or use Raspberry Pi Imager / balenaEtcher on the .img.xz)
Then: plug the SD/USB into a Pi 3, connect wired ethernet, power on.
  - JungleBlock resolves DNS on :53 and serves the web UI on http://<pi-ip>:80
  - the console dashboard shows on the HDMI output
  - point your LAN's DHCP DNS (or each device) at <pi-ip>
EOF
