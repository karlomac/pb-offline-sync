#!/bin/sh
# Entrypoint for the default pb-offline-sync PocketBase image.
#
# Works with the standard PocketBase-on-Dokploy env convention:
#   PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD  create (or refresh) the superuser on boot
#   PB_DATA_DIR                         data directory (default /pb/pb_data)
#   PB_HTTP_ADDR                        listen address (default 0.0.0.0:8090)
#   PB_ENCRYPTION_KEY                   encrypts PocketBase settings at rest
#
# With no arguments it runs `serve`. Any arguments are passed straight to the
# PocketBase binary (e.g. `docker run <image> superuser upsert a@b.c pw`).
#
# The container starts as root so it can repair ownership of a bind-mounted data
# directory (Dokploy creates these root-owned), then drops to the unprivileged
# `pb` user for both the admin bootstrap and the server. Without this, SQLite
# fails with "unable to open database file (14)" on a root-owned mount.
#
# Note: PocketBase's superuser CLI takes the password as a command-line argument,
# so it is briefly visible in /proc. If /proc visibility matters, run first-boot
# superuser init as a one-shot job instead of on every boot.
set -eu

DATA_DIR="${PB_DATA_DIR:-/pb/pb_data}"
HTTP_ADDR="${PB_HTTP_ADDR:-0.0.0.0:8090}"

case "$DATA_DIR" in
  /pb/*) ;;
  *)
    echo "pb-offline-sync: PB_DATA_DIR must be under /pb (got '${DATA_DIR}')" >&2
    exit 1
    ;;
esac

if [ "$#" -eq 0 ]; then
  set -- serve --http="${HTTP_ADDR}" --dir="${DATA_DIR}"
  if [ -n "${PB_ENCRYPTION_KEY:-}" ]; then
    set -- "$@" --encryptionEnv=PB_ENCRYPTION_KEY
  fi
fi

is_root=0
if [ "$(id -u)" = "0" ]; then
  is_root=1
  mkdir -p "$DATA_DIR"
  # Fast path: only recurse when the directory itself isn't already pb-owned.
  if [ "$(stat -c %u "$DATA_DIR" 2>/dev/null || echo 0)" != "$(id -u pb)" ]; then
    echo "pb-offline-sync: repairing ownership of ${DATA_DIR} for the pb user"
    chown -R pb:pb "$DATA_DIR"
  fi
fi

# Idempotent: creates the superuser on a fresh volume, updates it on later boots.
if [ -n "${PB_ADMIN_EMAIL:-}" ] && [ -n "${PB_ADMIN_PASSWORD:-}" ]; then
  echo "pb-offline-sync: ensuring superuser from PB_ADMIN_*"
  if [ "$is_root" = "1" ]; then
    su-exec pb:pb /pb/pb superuser upsert "${PB_ADMIN_EMAIL}" "${PB_ADMIN_PASSWORD}" --dir="${DATA_DIR}"
  else
    /pb/pb superuser upsert "${PB_ADMIN_EMAIL}" "${PB_ADMIN_PASSWORD}" --dir="${DATA_DIR}"
  fi
else
  echo "pb-offline-sync: PB_ADMIN_EMAIL/PB_ADMIN_PASSWORD not set; skipping superuser bootstrap"
fi

# exec so the server is PID 1 and receives signals directly.
if [ "$is_root" = "1" ]; then
  exec su-exec pb:pb /pb/pb "$@"
fi
exec /pb/pb "$@"
