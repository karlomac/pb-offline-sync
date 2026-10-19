#!/bin/sh
# Entrypoint for the default pb-offline-sync PocketBase image.
#
# Works with the standard PocketBase-on-Dokploy env convention:
#   PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD  create (or refresh) the superuser on boot
#   PB_DATA_DIR                         data directory (default /pb/pb_data)
#   PB_HTTP_ADDR                        listen address (default 0.0.0.0:8090)
#
# With no arguments it runs `serve`. Any arguments are passed straight to the
# PocketBase binary (e.g. `docker run <image> superuser upsert a@b.c pw`).
set -eu

DATA_DIR="${PB_DATA_DIR:-/pb/pb_data}"
HTTP_ADDR="${PB_HTTP_ADDR:-0.0.0.0:8090}"

# Idempotent: creates the superuser on a fresh volume, updates it on later boots.
if [ -n "${PB_ADMIN_EMAIL:-}" ] && [ -n "${PB_ADMIN_PASSWORD:-}" ]; then
  echo "pb-offline-sync: ensuring superuser ${PB_ADMIN_EMAIL}"
  /pb/pb superuser upsert "${PB_ADMIN_EMAIL}" "${PB_ADMIN_PASSWORD}" --dir="${DATA_DIR}"
else
  echo "pb-offline-sync: PB_ADMIN_EMAIL/PB_ADMIN_PASSWORD not set; skipping superuser bootstrap"
fi

if [ "$#" -eq 0 ]; then
  set -- serve --http="${HTTP_ADDR}" --dir="${DATA_DIR}"
fi

exec /pb/pb "$@"
