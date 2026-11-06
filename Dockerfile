# Default deployable build for pb-offline-sync: stock PocketBase + the sync plugin.
#
# Build context = repo root (Dokploy: point the Application at this repo, build
# type Dockerfile). Synced collections and the admin user are configured at
# runtime by env — see README Part 1 (Option A) and Part 3 (Dokploy).
#
#   docker build -t pb-offline-sync .
#   docker run -p 8090:8090 \
#     -v /etc/dokploy/storage/pb-sync/pb_data:/pb/pb_data \
#     -e PB_ADMIN_EMAIL=you@example.com -e PB_ADMIN_PASSWORD=… \
#     -e SYNC_COLLECTIONS=notes pb-offline-sync
#
# syntax=docker/dockerfile:1

FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
# Optional PocketBase version override for the Go build (e.g. v0.40.6).
# Leave empty to use the version pinned in server/go.mod. NOTE: the plugin's
# change-log/hook behaviour is only verified against the pinned version
# (protocol §17.1.4) — bumping this is untested until CI covers it.
ARG PB_VERSION=
# go.mod pins an exact toolchain; allow Go to fetch it if the base image lags.
ENV GOTOOLCHAIN=auto
WORKDIR /src

# Cache module downloads independently of the source.
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download

COPY server ./server
RUN cd server \
 && if [ -n "$PB_VERSION" ]; then \
      go get "github.com/pocketbase/pocketbase@${PB_VERSION}" && go mod tidy; \
    fi \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/pb ./cmd/pb-offline-sync

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
# ca-certificates for outbound TLS; busybox wget for HEALTHCHECK; su-exec lets
# the entrypoint repair a root-owned bind mount, then drop to the `pb` user.
RUN apk add --no-cache ca-certificates su-exec \
 && adduser -D -h /pb pb \
 && mkdir -p /pb/pb_data \
 && chown -R pb:pb /pb
WORKDIR /pb
COPY --from=build /out/pb /pb/pb
COPY docker-entrypoint.sh /pb/docker-entrypoint.sh
RUN chmod +x /pb/docker-entrypoint.sh
# No USER on purpose: the entrypoint starts as root only to fix ownership of a
# bind-mounted data dir, then execs the server as the unprivileged `pb` user.

# PocketBase data (SQLite DB + uploads) lives here: always mount persistent
# storage at /pb/pb_data (bind mount on Dokploy) or data is lost on redeploy.
VOLUME ["/pb/pb_data"]
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8090/api/health || exit 1

# No CMD: the entrypoint runs `serve` by default, or any arguments you pass.
ENTRYPOINT ["/pb/docker-entrypoint.sh"]
