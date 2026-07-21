#!/usr/bin/env bash
# Copyright 2026 Query Farm LLC - https://query.farm
#
# Run this repo's sqllogictest suite (test/sql/*.test) against the
# vgi-elasticsearch VGI worker, using a prebuilt standalone `haybarn-unittest`
# and the signed community `vgi` extension — no C++ build from source. See
# ci/README.md.
#
# Multi-transport: the same suite runs over whichever transport the TRANSPORT
# env var selects, by changing what `VGI_ELASTICSEARCH_WORKER` resolves to (the
# vgi extension picks the transport from the ATTACH LOCATION string):
#
#   subprocess (default)  VGI_ELASTICSEARCH_WORKER = the stdio worker binary
#                         -> extension spawns it over stdin/stdout.
#   http                  start `<worker> --http` (prints "PORT:<n>"), parse the
#                         port, VGI_ELASTICSEARCH_WORKER = http://127.0.0.1:<port>.
#                         (The extension POSTs each RPC method at <LOCATION>/<method>,
#                         e.g. /catalog_attach; the SDK mounts them at the root, so
#                         LOCATION is the BARE scheme://host:port — no path suffix.)
#   unix                  start `<worker> --unix /tmp/es.sock` (prints
#                         "UNIX:<path>"), VGI_ELASTICSEARCH_WORKER = unix:///tmp/es.sock.
#
# The es worker queries a real Elasticsearch/OpenSearch cluster (PIT +
# search_after) on EVERY transport, so the suite always needs a running cluster.
# In CI that cluster is a single-node OpenSearch *service container*; locally
# `make test-sql` boots one in Docker. Either way the cluster URL + index name
# are provided via VGI_ES_TEST_URL + VGI_ES_TEST_INDEX, and this script builds +
# runs the repo's `seed` binary to bulk-load the fixed test index before running
# the suite (mirroring `make test-sql`). For http/unix the worker is started by
# this script (not DuckDB) and all started processes are trap-killed on exit.
#
# The es_search streaming table function works over the stateless HTTP transport
# because its scan state carries an explicit gob-encodable cursor (PIT id + the
# last hit's search_after sort values) that the framework snapshots into the
# continuation token each tick — see the "EXTERNALIZED scan state" comments in
# internal/esworker/functions.go. No tests are gated.
#
# Required environment:
#   HAYBARN_UNITTEST          path to the haybarn-unittest binary
#   VGI_ELASTICSEARCH_WORKER  for TRANSPORT=subprocess: the worker LOCATION the
#                             .test files ATTACH (the built Go worker binary,
#                             spawned over stdio). For http/unix this is
#                             OVERRIDDEN by this script, but the binary it points
#                             at is reused to launch the out-of-band server, so it
#                             must still be the worker path.
#   VGI_ES_TEST_URL           base URL of the OpenSearch/Elasticsearch cluster
# Optional:
#   TRANSPORT                 subprocess (default) | http | unix
#   VGI_ES_TEST_INDEX         index to seed + query (default: vgi_es_e2e)
#   SEED_COUNT                number of documents to seed (default: 25)
#   STAGE                     scratch dir for the preprocessed tree (default: mktemp)
set -euo pipefail

: "${HAYBARN_UNITTEST:?path to the haybarn-unittest binary}"
: "${VGI_ELASTICSEARCH_WORKER:?worker LOCATION (the built Go worker binary)}"

# SKIP_MOCK (default empty): when set to a non-empty value, run ONLY the cluster-
# free surface — skip the VGI_ES_TEST_URL requirement, the cluster-readiness
# wait, and the seed step. Used by the Docker image_test, which exercises the
# image's transports against the offline test (es_type_mapping view + an offline
# `fields =>` schema bind) with no Elasticsearch/OpenSearch cluster available.
# Empty (the default) preserves the full live-cluster behaviour unchanged.
SKIP_MOCK="${SKIP_MOCK:-}"
if [ -z "$SKIP_MOCK" ]; then
  : "${VGI_ES_TEST_URL:?base URL of the OpenSearch/Elasticsearch cluster}"
fi

# TEST_PATTERN (default test/sql/*): the haybarn glob of staged tests to run.
# Override it to run only the offline subset (e.g. test/sql/offline.test) when
# SKIP_MOCK is set. Staging always preprocesses every test/sql/*.test file; this
# only selects which of them the runner executes.
TEST_PATTERN="${TEST_PATTERN:-test/sql/*}"

TRANSPORT="${TRANSPORT:-subprocess}"
case "$TRANSPORT" in
  subprocess|http|unix) ;;
  *) echo "ERROR: unknown TRANSPORT='$TRANSPORT' (expected subprocess|http|unix)" >&2; exit 2 ;;
esac

export VGI_ES_TEST_INDEX="${VGI_ES_TEST_INDEX:-vgi_es_e2e}"
SEED_COUNT="${SEED_COUNT:-25}"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
STAGE="${STAGE:-$(mktemp -d)}"

# The worker binary the subprocess transport ATTACHes to is also the binary we
# launch out-of-band for http/unix. Capture it before we possibly overwrite
# VGI_ELASTICSEARCH_WORKER with a URL.
WORKER_BIN="$VGI_ELASTICSEARCH_WORKER"

# Collected PIDs and paths to clean up on exit (the optional out-of-band worker).
WORKER_PID=""
UNIX_SOCK=""
cleanup() {
  # Preserve the script's exit status: this runs on EXIT, so its own last
  # command must not clobber the real exit code (a bare `[ -n "$x" ]` that is
  # false returns 1 and would turn a green run red).
  local rc=$?
  if [ -n "$WORKER_PID" ]; then kill "$WORKER_PID" 2>/dev/null || true; wait "$WORKER_PID" 2>/dev/null || true; fi
  if [ -n "$UNIX_SOCK" ]; then rm -f "$UNIX_SOCK"; fi
  return "$rc"
}
trap cleanup EXIT

# --- Wait for the cluster to answer, then seed the fixed test index ----------
# Skipped entirely when SKIP_MOCK is set (offline surface only — no cluster).
if [ -z "$SKIP_MOCK" ]; then
  echo "Waiting for OpenSearch/Elasticsearch at $VGI_ES_TEST_URL ..."
  READY=""
  for _ in $(seq 1 60); do
    if curl -fs "$VGI_ES_TEST_URL/_cluster/health" >/dev/null 2>&1; then
      READY=1; break
    fi
    sleep 2
  done
  if [ -z "$READY" ]; then
    echo "ERROR: cluster did not become ready at $VGI_ES_TEST_URL" >&2
    exit 1
  fi
  echo "Cluster is up."

  SEED_BIN="$STAGE/seed"
  echo "Building seeder ..."
  ( cd "$REPO" && go build -o "$SEED_BIN" ./cmd/seed )
  echo "Seeding index '$VGI_ES_TEST_INDEX' ($SEED_COUNT docs) ..."
  "$SEED_BIN" --url "$VGI_ES_TEST_URL" --index "$VGI_ES_TEST_INDEX" --count "$SEED_COUNT"
else
  echo "SKIP_MOCK set: skipping cluster wait + seed (cluster-free surface only)."
fi

# --- Per-transport: resolve VGI_ELASTICSEARCH_WORKER (the ATTACH LOCATION) ----
# subprocess keeps the binary path (extension spawns stdio). http/unix start the
# worker out-of-band and hand the extension a URL.
case "$TRANSPORT" in
  subprocess)
    echo "Transport: subprocess/stdio — VGI_ELASTICSEARCH_WORKER=$VGI_ELASTICSEARCH_WORKER"
    ;;

  http)
    # Pre-launched HTTP hook: if the provided worker LOCATION is already an
    # http(s):// URL, the worker is served out-of-band (e.g. a warm Docker
    # container the caller started + health-checked). Use it verbatim — do NOT
    # try to launch a local binary (there may be none on the runner).
    case "$WORKER_BIN" in
      http://*|https://*)
        export VGI_ELASTICSEARCH_WORKER="$WORKER_BIN"
        echo "Transport: http — using pre-launched worker at $VGI_ELASTICSEARCH_WORKER"
        ;;
      *)
    # Start the worker in --http mode; it prints "PORT:<n>" once listening.
    WORKER_PORT_FILE="$(mktemp)"
    echo "Transport: http — starting '$WORKER_BIN --http' ..."
    "$WORKER_BIN" --http >"$WORKER_PORT_FILE" 2>/dev/null &
    WORKER_PID=$!
    WPORT=""
    for _ in $(seq 1 50); do
      WPORT="$(sed -n 's/^PORT:\([0-9][0-9]*\)$/\1/p' "$WORKER_PORT_FILE" 2>/dev/null | head -1)"
      [ -n "$WPORT" ] && break
      # Bail early if the worker died.
      kill -0 "$WORKER_PID" 2>/dev/null || { echo "ERROR: http worker exited before reporting a port" >&2; cat "$WORKER_PORT_FILE" >&2 || true; exit 1; }
      sleep 0.2
    done
    rm -f "$WORKER_PORT_FILE"
    if [ -z "$WPORT" ]; then
      echo "ERROR: http worker did not report a port" >&2
      exit 1
    fi
    # The extension treats the LOCATION as a base and POSTs each RPC method at
    # <LOCATION>/<method> (e.g. /catalog_attach). The SDK mounts those methods
    # at the server root (empty prefix), so the LOCATION must be the bare
    # scheme://host:port with NO path. Appending /vgi would make every method
    # 404 — which the runner silently skips as an error "matching 'HTTP'".
    export VGI_ELASTICSEARCH_WORKER="http://127.0.0.1:$WPORT"
    echo "HTTP worker listening on $VGI_ELASTICSEARCH_WORKER (pid $WORKER_PID)"
        ;;
    esac
    ;;

  unix)
    # Start the worker on an AF_UNIX socket; it prints "UNIX:<path>" once
    # listening. idleTimeout is disabled (we own the process lifecycle).
    UNIX_SOCK="${TMPDIR:-/tmp}/es.$$.sock"
    rm -f "$UNIX_SOCK"
    WORKER_OUT_FILE="$(mktemp)"
    echo "Transport: unix — starting '$WORKER_BIN --unix $UNIX_SOCK' ..."
    "$WORKER_BIN" --unix "$UNIX_SOCK" >"$WORKER_OUT_FILE" 2>/dev/null &
    WORKER_PID=$!
    READY=""
    for _ in $(seq 1 50); do
      if grep -q '^UNIX:' "$WORKER_OUT_FILE" 2>/dev/null && [ -S "$UNIX_SOCK" ]; then
        READY=1; break
      fi
      kill -0 "$WORKER_PID" 2>/dev/null || { echo "ERROR: unix worker exited before the socket was ready" >&2; cat "$WORKER_OUT_FILE" >&2 || true; exit 1; }
      sleep 0.2
    done
    rm -f "$WORKER_OUT_FILE"
    if [ -z "$READY" ]; then
      echo "ERROR: unix worker did not report a ready socket at $UNIX_SOCK" >&2
      exit 1
    fi
    export VGI_ELASTICSEARCH_WORKER="unix://$UNIX_SOCK"
    echo "Unix worker listening on $VGI_ELASTICSEARCH_WORKER (pid $WORKER_PID)"
    ;;
esac

# --- Stage the preprocessed tests -------------------------------------------
echo "Staging preprocessed tests into $STAGE ..."
mkdir -p "$STAGE/test/sql"
for f in "$REPO"/test/sql/*.test; do
  awk -f "$HERE/preprocess-require.awk" "$f" > "$STAGE/test/sql/$(basename "$f")"
done

# The HTTP transport needs DuckDB's HTTP client, which the vgi extension drives
# through DuckDB's HTTPUtil — that is only registered when the `httpfs`
# extension is loaded. The .test files only `LOAD vgi`, so over HTTP the
# worker-RPC POSTs fail with an "HTTP"-flavoured error (which the runner then
# silently skips). Inject an explicit signed `INSTALL httpfs FROM core; LOAD
# httpfs;` after each `LOAD vgi;` in the staged tests for the http transport
# only (subprocess/unix do not use the HTTP client, so they need nothing extra).
if [ "$TRANSPORT" = "http" ]; then
  echo "Transport http: injecting 'LOAD httpfs' (required for the worker HTTP RPC) ..."
  for f in "$STAGE"/test/sql/*.test; do
    awk '
      { print }
      /^LOAD[ \t]+vgi;[ \t]*$/ {
        print "";
        print "statement ok";
        print "INSTALL httpfs FROM core;";
        print "";
        print "statement ok";
        print "LOAD httpfs;";
      }
    ' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
  done
fi

cd "$STAGE"

# Warm the extension cache once: vgi from the signed community channel. A miss
# here is only a warning — the per-test LOAD vgi; (the .test files load it
# explicitly) is what actually gates each file, and it needs vgi already
# INSTALLed into the runner's extension dir.
echo "Warming the extension cache (vgi from community) ..."
mkdir -p "$STAGE/test"
cat > "$STAGE/test/_warm.test" <<'EOF'
# name: test/_warm.test
# group: [warm]
statement ok
INSTALL vgi FROM community;
EOF
"$HAYBARN_UNITTEST" "test/_warm.test" >/dev/null 2>&1 || echo "::warning::extension warm step did not fully succeed"
rm -f "$STAGE/test/_warm.test"

# Run the whole suite in one invocation, capturing the runner's native
# sqllogictest report so we can both stream it AND guard against a silent skip.
#
# IMPORTANT: the DuckDB/Haybarn sqllogictest runner SKIPS (not fails, exit 0) a
# test whose error message matches a built-in network-error allowlist that
# includes the substring "HTTP". So a broken HTTP transport would otherwise show
# "All tests were skipped" and the job would go GREEN having run nothing — a
# fake pass. We detect that and fail explicitly. A real run prints
# "All tests passed (N assertions ...)".
echo "Running suite (transport: $TRANSPORT, pattern: $TEST_PATTERN, worker: $VGI_ELASTICSEARCH_WORKER) ..."
RUN_LOG="$STAGE/run.log"
set +e
"$HAYBARN_UNITTEST" "$TEST_PATTERN" 2>&1 | tee "$RUN_LOG"
RUN_RC="${PIPESTATUS[0]}"
set -e

if [ "$RUN_RC" -ne 0 ]; then
  echo "ERROR: suite failed (transport: $TRANSPORT, rc=$RUN_RC)" >&2
  exit "$RUN_RC"
fi

# Guard against the silent-skip fake-pass (see comment above). If every test was
# skipped — and none ran — treat it as a failure for this transport, surfacing
# the skip reason the runner reported.
if grep -q 'All tests were skipped' "$RUN_LOG"; then
  echo "ERROR: every test was SKIPPED on transport '$TRANSPORT' (the runner's" >&2
  echo "       built-in network-error skip swallowed the real error). This is" >&2
  echo "       NOT a pass. Skip reason reported by the runner:" >&2
  grep -A3 'Skipped tests for the following reasons' "$RUN_LOG" >&2 || true
  exit 1
fi
