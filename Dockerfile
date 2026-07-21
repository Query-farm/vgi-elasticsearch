# Copyright 2026 Query Farm LLC - https://query.farm
#
# Single image that serves the network transports of the `vgi-elasticsearch`
# VGI worker:
#   docker run ... IMG            -> HTTP server on $PORT      (default; Fly.io / local)
#   docker run -i ... IMG stdio   -> stdio worker DuckDB spawns on-host
#   docker run ... IMG unix       -> AF_UNIX launcher transport on $UNIX_SOCK
# See docker-entrypoint.sh.
#
# The worker is STATELESS: every es_search call takes the cluster endpoint +
# index as arguments and queries a remote Elasticsearch/OpenSearch over HTTP, so
# there is no /data volume, no model registry, and no `farm.query.vgi.volumes`
# mount-discovery label. The image is just the binary + a tiny entrypoint.
# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# CGO is REQUIRED: the vgi-go SDK links DuckDB (via duckdb/duckdb-go-bindings),
# so CGO_ENABLED=0 fails to build. bookworm pins the same glibc the slim runtime
# ships. Native per-arch runners build each arch (no cross-compile), so the
# prebuilt duckdb-go-bindings static lib for the build arch is linked directly.
FROM golang:1.26-bookworm AS build
WORKDIR /src

ENV CGO_ENABLED=1

RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc g++ libc6-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Resolve modules first (their own layer, cached across code-only changes). The
# module cache is a BuildKit cache mount, so it persists across rebuilds without
# bloating the image.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal

# BuildKit cache mounts persist the Go build + module caches across image
# rebuilds, so incremental code changes only recompile the changed packages and
# the CGO link, not the full DuckDB-linked tree every time. The binary is copied
# OUT to a non-cache path before the layer ends (cache mounts don't persist).
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -ldflags="-s -w" \
        -o /vgi-elasticsearch-worker ./cmd/vgi-elasticsearch-worker

# ---- runtime stage ---------------------------------------------------------
# debian-slim (not distroless) so the HEALTHCHECK below has a real `curl`.
FROM debian:bookworm-slim

# Build metadata, wired from docker/metadata-action outputs in CI.
ARG VERSION=0.0.0
ARG GIT_COMMIT=unknown
ARG SOURCE_URL=https://github.com/Query-farm/vgi-elasticsearch

# Standard OCI labels + the VGI transport-advertisement label. `transports`
# lists the NETWORK transports this image serves (http); stdio is a spawn mode,
# not a network transport, so it is not listed.
LABEL org.opencontainers.image.title="vgi-elasticsearch" \
      org.opencontainers.image.description="Query Elasticsearch/OpenSearch indices as SQL tables (PIT + search_after deep pagination) — a VGI worker for DuckDB/SQL (stdio + HTTP)" \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.licenses="MIT" \
      farm.query.vgi.transports='["http"]'

ENV PORT=8000 \
    VGI_ELASTICSEARCH_GIT_COMMIT=${GIT_COMMIT}

WORKDIR /app

# ca-certificates: the worker queries Elasticsearch/OpenSearch clusters over
# HTTPS. curl backs the HEALTHCHECK below.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# `--chmod` sets the mode in the COPY layer itself, avoiding a second full-size
# layer that a separate `RUN chmod` would create (overlayfs copies the whole
# file on a metadata change).
COPY --from=build --chmod=0755 /vgi-elasticsearch-worker /usr/local/bin/vgi-elasticsearch-worker
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# Run unprivileged. No state, no volume — there is nothing to own or persist.
RUN useradd --create-home --uid 10001 app
USER app

EXPOSE 8000

# Readiness probe for HTTP mode. Inert for a short-lived stdio container, which
# has no HTTP server (the probe just fails harmlessly there).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS "http://localhost:${PORT:-8000}/health" || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["http"]
