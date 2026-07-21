#!/bin/sh
# Copyright 2026 Query Farm LLC - https://query.farm
#
# Dispatch the single vgi-elasticsearch image into one of its transports:
#   http   (default) the HTTP server on $PORT (8000), bound 0.0.0.0 so a
#                    published host port reaches it. Serves /health.
#   stdio            a worker DuckDB spawns over stdio (on-host execution).
#   unix             the AF_UNIX launcher transport on $UNIX_SOCK.
# Any other first argument is exec'd verbatim (escape hatch for debugging).
#
# The worker is stateless (it queries a remote Elasticsearch/OpenSearch cluster
# per request; the endpoint + index are es_search arguments), so there is no
# /data to create and no state env to wire — each mode just exec's the binary.
set -e

case "${1:-http}" in
  http)
    shift 2>/dev/null || true
    # The Go SDK's RunHttp binds EXACTLY the address main() passes it. main()
    # defaults --http-addr to 127.0.0.1:0 (an ephemeral loopback port) for
    # dev/CI; in a container we must bind 0.0.0.0 on a FIXED port so
    # `-p $PORT:$PORT` and the /health probe reach it.
    exec vgi-elasticsearch-worker --http --http-addr "0.0.0.0:${PORT:-8000}" "$@"
    ;;
  stdio)
    shift 2>/dev/null || true
    exec vgi-elasticsearch-worker "$@"
    ;;
  unix)
    shift 2>/dev/null || true
    exec vgi-elasticsearch-worker --unix "${UNIX_SOCK:-/tmp/vgi-elasticsearch.sock}" "$@"
    ;;
  *)
    exec "$@"
    ;;
esac
