#!/usr/bin/env bash
# Refresh the NATIVE /usr/local/bin/jungleblock binary (drives the tty1 HDMI
# dashboard, a host unit) from the just-pulled container image. The container
# updates via `compose pull && up -d`, but the native binary is baked once at
# image build — without this it stays stale until a reflash.
set -euo pipefail

IMAGE="ghcr.io/homeprojectslab/jungleblock:latest"

cid="$(docker create "$IMAGE")"
docker cp "$cid:/app/jungleblock" /tmp/jungleblock.new
docker rm "$cid" >/dev/null

# GUARD: the update timer fires every 5 min. Only replace + restart the
# dashboard when the binary actually changed, else it restarts every tick.
if ! cmp -s /tmp/jungleblock.new /usr/local/bin/jungleblock; then
	install -m755 /tmp/jungleblock.new /usr/local/bin/jungleblock
	systemctl restart jungleblock-dashboard.service
fi
