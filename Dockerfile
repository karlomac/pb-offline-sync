# Default deployable build for pb-offline-sync: stock PocketBase + the sync plugin.
#
# Build context = repo root (Dokploy: point the Application at this repo, build
# type Dockerfile). The synced collections are configured at runtime by env —
# see README Part 1 (Option A) and Part 3 (Dokploy).
#
#   docker build -t pb-offline-sync .
#   docker run -p 8090:8090 -v pb-data:/pb/pb_data -e SYNC_COLLECTIONS=notes pb-offline-sync
#
# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
# go.mod pins an exact toolchain; allow Go to fetch it if the base image lags.
ENV GOTOOLCHAIN=auto
WORKDIR /src

# Cache module downloads independently of the source.
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download

COPY server ./server
RUN cd server && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/pb ./cmd/pocketbase-sync

FROM alpine:3.20
# wget (busybox) is used by HEALTHCHECK; ca-certificates for outbound TLS.
RUN apk add --no-cache ca-certificates \
 && adduser -D -h /pb pb \
 && mkdir -p /pb/pb_data \
 && chown -R pb:pb /pb
WORKDIR /pb
COPY --from=build /out/pb /pb/pb
USER pb

# PocketBase data (SQLite DB + uploads) lives here: always mount a persistent
# volume at /pb/pb_data or data is lost on redeploy.
VOLUME ["/pb/pb_data"]
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8090/api/health || exit 1

ENTRYPOINT ["/pb/pb"]
CMD ["serve", "--http=0.0.0.0:8090", "--dir=/pb/pb_data"]
