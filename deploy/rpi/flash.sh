#!/usr/bin/env bash
# Write a baked JungleBlock appliance image to a removable device, with guardrails.
#
# Refuses to touch anything that isn't a hot-pluggable USB disk, anything that
# currently backs a mounted filesystem, and (belt and braces) the disk holding
# the running root filesystem. Intended to run inside the same privileged
# container used to bake the image, with the target passed via --device.
#
# Usage: deploy/rpi/flash.sh <image> <device>     e.g. flash.sh out.img /dev/sda
set -euo pipefail

IMG="${1:?usage: flash.sh <image> <device>}"
DEV="${2:?usage: flash.sh <image> <device>}"

[ -f "$IMG" ] || { echo "no such image: $IMG" >&2; exit 1; }
[ -b "$DEV" ] || { echo "not a block device: $DEV" >&2; exit 1; }

NAME="$(basename "$DEV")"
SYS="/sys/block/$NAME"
[ -d "$SYS" ] || { echo "$DEV is not a whole disk (partitions are not valid targets)" >&2; exit 1; }

# --- guardrails ---------------------------------------------------------------
# 1. must be hot-pluggable removable media (USB), never a fixed internal disk
if [ "$(cat "$SYS/removable" 2>/dev/null || echo 0)" != "1" ]; then
	# 'removable' is 0 for many USB SSDs; fall back to the transport check
	TRAN="$(lsblk -dn -o TRAN "$DEV" 2>/dev/null | tr -d ' ')"
	HOT="$(lsblk -dn -o HOTPLUG "$DEV" 2>/dev/null | tr -d ' ')"
	if [ "$TRAN" != "usb" ] || [ "$HOT" != "1" ]; then
		echo "REFUSING: $DEV is not a hot-pluggable USB disk (tran=$TRAN hotplug=$HOT)" >&2
		exit 1
	fi
fi

# 2. nothing on it may be mounted
if lsblk -ln -o MOUNTPOINTS "$DEV" | grep -q '[^[:space:]]'; then
	echo "REFUSING: $DEV has mounted filesystems — unmount them first:" >&2
	lsblk -o NAME,MOUNTPOINTS "$DEV" >&2
	exit 1
fi

# 3. must not be the disk backing the running root filesystem
ROOTSRC="$(findmnt -no SOURCE / 2>/dev/null || true)"
if [ -n "$ROOTSRC" ]; then
	ROOTDISK="$(lsblk -no PKNAME "$ROOTSRC" 2>/dev/null | head -1 || true)"
	if [ -n "$ROOTDISK" ] && [ "$ROOTDISK" = "$NAME" ]; then
		echo "REFUSING: $DEV backs the running root filesystem" >&2
		exit 1
	fi
fi

SIZE_DEV=$(blockdev --getsize64 "$DEV")
SIZE_IMG=$(stat -c%s "$IMG")
if [ "$SIZE_IMG" -gt "$SIZE_DEV" ]; then
	echo "REFUSING: image ($SIZE_IMG bytes) larger than $DEV ($SIZE_DEV bytes)" >&2
	exit 1
fi

echo ">> target : $DEV  ($(lsblk -dn -o MODEL,SIZE "$DEV" | xargs))"
echo ">> image  : $IMG  ($(numfmt --to=iec "$SIZE_IMG"))"
echo ">> writing…"

# --- write --------------------------------------------------------------------
dd if="$IMG" of="$DEV" bs=4M conv=fsync oflag=direct status=progress
sync
blockdev --rereadpt "$DEV" 2>/dev/null || true

echo ">> verifying the first $((SIZE_IMG / 1048576))MiB read back…"
if cmp -n "$SIZE_IMG" "$IMG" "$DEV"; then
	echo ">> OK — device matches the image byte-for-byte"
else
	echo "!! verification MISMATCH" >&2
	exit 1
fi

echo ">> done. Partition table now on $DEV:"
lsblk -o NAME,SIZE,FSTYPE,LABEL "$DEV"
