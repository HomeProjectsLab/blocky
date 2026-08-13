#!/usr/bin/env bash
# Bake an unattended blocky appliance image for the Raspberry Pi 3 (arm64).
#
# It customises the official Raspberry Pi OS Lite (64-bit) image by PURE FILE
# INJECTION into the loop-mounted partitions — no chroot, no qemu, nothing ARM
# is ever executed at bake time. The result is a .img.xz you DD to an SD/USB:
# plug into a Pi 3 with wired ethernet (DHCP), power on, and blocky serves DNS
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
OUT="${OUT:-$REPO_ROOT/blocky-rpi3-arm64.img.xz}"
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

# --- 1. blocky arm64 binary (cross-compiled, no cgo, no ARM executed here) ----
BIN="$WORK/blocky"
if [ -n "${BLOCKY_BIN:-}" ] && [ -f "${BLOCKY_BIN}" ]; then
	echo ">> using prebuilt binary: $BLOCKY_BIN"
	cp "$BLOCKY_BIN" "$BIN"
else
	need go
	echo ">> building blocky (linux/arm64)…"
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
echo ">> injecting blocky + units + config…"
install -Dm755 "$BIN" "$ROOT_MNT/usr/local/bin/blocky"
install -Dm644 "$HERE/systemd/blocky.service"           "$ROOT_MNT/etc/systemd/system/blocky.service"
install -Dm644 "$HERE/systemd/blocky-dashboard.service" "$ROOT_MNT/etc/systemd/system/blocky-dashboard.service"
install -d "$ROOT_MNT/etc/systemd/system/multi-user.target.wants"
ln -sf ../blocky.service           "$ROOT_MNT/etc/systemd/system/multi-user.target.wants/blocky.service"
ln -sf ../blocky-dashboard.service "$ROOT_MNT/etc/systemd/system/multi-user.target.wants/blocky-dashboard.service"
ln -sf /dev/null "$ROOT_MNT/etc/systemd/system/getty@tty1.service"   # dashboard owns tty1
install -Dm644 "$HERE/appliance.yml" "$BOOT_MNT/appliance.yml"

# verify what we placed
echo ">> injected tree:"; ls -l "$ROOT_MNT/usr/local/bin/blocky" \
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
  - blocky resolves DNS on :53 and serves the web UI on http://<pi-ip>:80
  - the console dashboard shows on the HDMI output
  - point your LAN's DHCP DNS (or each device) at <pi-ip>
EOF
