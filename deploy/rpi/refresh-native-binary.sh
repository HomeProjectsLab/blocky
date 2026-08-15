#!/usr/bin/env bash
# Refresh the NATIVE /usr/local/bin/blocky binary (drives the tty1 HDMI
# dashboard, a host unit) from the just-pulled container image. The container
# updates via `compose pull && up -d`, but the native binary is baked once at
# image build — without this it stays stale until a reflash.
set -euo pipefail

IMAGE="ghcr.io/homeprojectslab/blocky:latest"

cid="$(docker create "$IMAGE")"
docker cp "$cid:/app/blocky" /tmp/blocky.new
docker rm "$cid" >/dev/null

# GUARD: the update timer fires every 5 min. Only replace + restart the
# dashboard when the binary actually changed, else it restarts every tick.
if ! cmp -s /tmp/blocky.new /usr/local/bin/blocky; then
	install -m755 /tmp/blocky.new /usr/local/bin/blocky
	systemctl restart blocky-dashboard.service
fi
