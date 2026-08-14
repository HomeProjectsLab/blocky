#!/usr/bin/env bash
# First-boot bootstrap for the blocky appliance.
#
# Installs Docker if it isn't present, seeds the config database from the
# boot-partition appliance.yml, then brings up the container stack. Safe to
# re-run: every step is a no-op once it has succeeded, so systemd can simply
# retry this unit until the network is up.
set -euo pipefail

STACK_DIR=/opt/blocky
COMPOSE="$STACK_DIR/compose.yml"
DATA=/var/lib/blocky
IMAGE="ghcr.io/homeprojectslab/blocky:latest"

log() { echo "[blocky-bootstrap] $*"; }

# --- 1. DNS sanity ------------------------------------------------------------
# Chicken-and-egg: this appliance IS the LAN resolver, but it isn't running yet.
# If DHCP handed out our own address as the only resolver, nothing resolves and
# the Docker install would fail. Fall back to a public resolver just for the
# bootstrap; dhcpcd restores the real one afterwards.
if ! getent hosts download.docker.com >/dev/null 2>&1; then
	log "DNS not usable yet — using a temporary public resolver for bootstrap"
	echo "nameserver 9.9.9.9" > /etc/resolv.conf
fi

# --- 1b. wait for a sane clock ------------------------------------------------
# The Pi has no RTC, so at boot the clock is the image build date (~2024). Every
# HTTPS certificate is then "not yet valid" and the Docker install curl dies with
# SSL error 60. Wait for NTP to correct the clock before any TLS. timesyncd needs
# DNS, which the fallback resolver above now provides.
systemctl start systemd-timesyncd 2>/dev/null || true
if [ "$(date +%Y)" -lt 2025 ]; then
	log "clock is $(date -u '+%Y-%m-%d') (no RTC) — waiting for NTP sync…"
	for _ in $(seq 1 90); do
		[ "$(date +%Y)" -ge 2025 ] && break
		sleep 2
	done
	log "clock now $(date -u '+%Y-%m-%d %H:%M:%S')"
fi

# --- 2. Docker ----------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
	log "installing Docker (one time, a few minutes on a Pi 3)…"
	curl -fsSL https://get.docker.com | sh
	systemctl enable --now docker
fi

# Compose v2 ships as a plugin with the convenience script; fail loudly if not.
if ! docker compose version >/dev/null 2>&1; then
	log "docker compose plugin missing" >&2
	exit 1
fi

# --- 3. seed the config database once ----------------------------------------
mkdir -p "$DATA"
# The image runs as UID 100 (Dockerfile USER 100). $DATA is bind-mounted at
# /data in both the seed and serve containers, and a bind mount keeps the host's
# ownership — so without this chown the root:root dir is read-only to UID 100,
# config.db/querylog.db can't be created, and the stack crash-loops on boot.
chown -R 100:100 "$DATA"

if [ ! -f "$DATA/config.db" ]; then
	APPLIANCE=/boot/firmware/appliance.yml
	[ -f "$APPLIANCE" ] || APPLIANCE=/boot/appliance.yml

	if [ -f "$APPLIANCE" ]; then
		log "seeding config from $APPLIANCE"
		docker run --rm \
			-v "$DATA":/data \
			-v "$APPLIANCE":/appliance.yml:ro \
			"$IMAGE" import /appliance.yml --db-dir /data
	fi
fi

# --- 4. bring the stack up ----------------------------------------------------
log "starting stack"
docker compose -f "$COMPOSE" pull --quiet || log "pull failed, using cached images"
docker compose -f "$COMPOSE" up -d --remove-orphans

log "up. web UI on :80, DNS on :53, Watchtower polling for updates."
