#!/usr/bin/env bash
# Copyright 2026 Query Farm LLC - https://query.farm
#
# Run this repo's sqllogictest suite (test/sql/*.test) against the
# vgi-elasticsearch VGI worker, using a prebuilt standalone `haybarn-unittest`
# and the signed community `vgi` extension — no C++ build from source. See
# ci/README.md.
#
# The es worker queries a real Elasticsearch/OpenSearch cluster (PIT +
# search_after), so the suite needs a running cluster. In CI that cluster is a
# single-node OpenSearch *service container*; locally `make test-sql` boots one
# in Docker. Either way the cluster URL + index name are provided via
# VGI_ES_TEST_URL + VGI_ES_TEST_INDEX, and this script builds + runs the repo's
# `seed` binary to bulk-load the fixed test index before running the suite
# (mirroring `make test-sql`).
#
# Required environment:
#   HAYBARN_UNITTEST          path to the haybarn-unittest binary
#   VGI_ELASTICSEARCH_WORKER  worker LOCATION the .test files ATTACH (the built
#                             Go worker binary the vgi extension spawns over stdio)
#   VGI_ES_TEST_URL           base URL of the OpenSearch/Elasticsearch cluster
# Optional:
#   VGI_ES_TEST_INDEX         index to seed + query (default: vgi_es_e2e)
#   SEED_COUNT                number of documents to seed (default: 25)
#   STAGE                     scratch dir for the preprocessed tree (default: mktemp)
set -euo pipefail

: "${HAYBARN_UNITTEST:?path to the haybarn-unittest binary}"
: "${VGI_ELASTICSEARCH_WORKER:?worker LOCATION (the built Go worker binary)}"
: "${VGI_ES_TEST_URL:?base URL of the OpenSearch/Elasticsearch cluster}"

export VGI_ES_TEST_INDEX="${VGI_ES_TEST_INDEX:-vgi_es_e2e}"
SEED_COUNT="${SEED_COUNT:-25}"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
STAGE="${STAGE:-$(mktemp -d)}"

# --- Wait for the cluster to answer, then seed the fixed test index ----------
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

# --- Stage the preprocessed tests -------------------------------------------
echo "Staging preprocessed tests into $STAGE ..."
mkdir -p "$STAGE/test/sql"
for f in "$REPO"/test/sql/*.test; do
  awk -f "$HERE/preprocess-require.awk" "$f" > "$STAGE/test/sql/$(basename "$f")"
done

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

# Run the whole suite in one invocation, streaming the runner's native
# sqllogictest report. Any failed assertion exits non-zero and fails the job.
echo "Running suite (worker: $VGI_ELASTICSEARCH_WORKER) ..."
"$HAYBARN_UNITTEST" "test/sql/*"
