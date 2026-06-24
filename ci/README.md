# CI: the vgi-elasticsearch worker integration suite

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs the Go unit
tests and this repo's sqllogictest suite (`test/sql/*.test`) against the
vgi-elasticsearch VGI worker through the **real DuckDB `vgi` extension** on
every push / PR.

## How it works (no C++ build)

Rather than building the vgi DuckDB extension from source, CI drives a
**prebuilt** standalone `haybarn-unittest` (the DuckDB/Haybarn sqllogictest
runner, published in Haybarn's releases) and installs the **signed** `vgi`
extension from the Haybarn community channel:

1. **Build the worker** — `go build -o vgi-elasticsearch-worker
   ./cmd/vgi-elasticsearch-worker`. The resulting binary is a self-contained
   stdio worker the extension can spawn; `VGI_ELASTICSEARCH_WORKER` (an absolute
   path) is the ATTACH `LOCATION`.
2. **Download the runner** — the `haybarn_unittest-linux-amd64.zip` asset from
   the latest Haybarn release.
3. **Preprocess** — [`preprocess-require.awk`](preprocess-require.awk) rewrites
   any `require <ext>` gate into an explicit signed `INSTALL <ext> FROM
   {community,core}; LOAD <ext>;`. This repo's tests already use an explicit
   `LOAD vgi;` (haybarn silently *skips* `require vgi`), so the awk is mostly a
   pass-through here; `require-env` and everything else pass through untouched.
4. **Run** — [`run-integration.sh`](run-integration.sh) waits for the cluster,
   builds + runs the repo's `seed` binary to bulk-load the fixed test index,
   resolves the worker LOCATION for the selected transport (see below), stages
   the preprocessed tree, warms the extension cache once (`INSTALL vgi FROM
   community`), then runs the suite in a single `haybarn-unittest` invocation.
   Any failed assertion exits non-zero and fails the job.

## Transport matrix (subprocess / http / unix)

The SQL E2E runs the **full** suite over each transport the `vgi` extension
supports, selected by `run-integration.sh`'s `TRANSPORT` env var (the CI
`integration` job is a `transport: [subprocess, http, unix]` matrix). The
extension picks the transport from the ATTACH `LOCATION`, so the *same* suite
exercises a different transport just by changing what `VGI_ELASTICSEARCH_WORKER`
resolves to:

- **subprocess** (default) — `VGI_ELASTICSEARCH_WORKER` = the built worker
  binary; the extension spawns it over stdin/stdout.
- **http** — the script starts `<worker> --http`, which prints `PORT:<n>` on
  stdout once listening; the script parses that and sets
  `VGI_ELASTICSEARCH_WORKER = http://127.0.0.1:<port>` (the **bare**
  `scheme://host:port`, no path — the extension POSTs each RPC method at
  `<LOCATION>/<method>`). The HTTP worker-RPC rides DuckDB's `httpfs` HTTP
  client, so for the http leg **only** the script injects `INSTALL httpfs FROM
  core; LOAD httpfs;` after each `LOAD vgi;` in the staged tests.
- **unix** — the script starts `<worker> --unix <sock>`, which prints
  `UNIX:<path>` once listening, and sets `VGI_ELASTICSEARCH_WORKER =
  unix://<sock>`.

For http/unix the worker is launched **by the script** (not DuckDB) and
trap-killed on exit. The OpenSearch service container + seed step run for **all**
transports — the worker queries the live cluster on every leg.

**No tests are gated.** The `es_search` streaming table function works over the
stateless HTTP transport because its scan state already externalizes the scan
position: the PIT id plus the last hit's `search_after` sort values are plain
gob-encodable fields the framework snapshots into the HTTP continuation token
each tick, and the worker resumes from them (one page per `Process` tick). See
the "EXTERNALIZED scan state" comments in `internal/esworker/functions.go`.

**Silent-skip guard.** The DuckDB/Haybarn sqllogictest runner *skips* (exit 0,
not fail) any test whose error message contains an `"HTTP"`-flavoured substring,
so a broken http leg would otherwise report "All tests were skipped" and go
GREEN having run nothing. `run-integration.sh` fails the leg if the runner
reports every test skipped — never trust a green http leg without it.

## The cluster

The es worker queries a real Elasticsearch/OpenSearch cluster (PIT +
`search_after` deep pagination), so the E2E needs a running cluster. In CI a
**single-node OpenSearch** (Apache-2.0, security plugin disabled for the test
only) runs as a GitHub Actions **service container** on host port 9209;
`VGI_ES_TEST_URL` / `VGI_ES_TEST_INDEX` point the suite at it. Locally `make
test-sql` boots the same image in Docker, seeds the index, runs the suite, and
tears the container down.

## Run it locally

```bash
make os-up
go build -o vgi-elasticsearch-worker ./cmd/vgi-elasticsearch-worker
HAYBARN_UNITTEST=/path/to/haybarn-unittest \
VGI_ELASTICSEARCH_WORKER="$PWD/vgi-elasticsearch-worker" \
VGI_ES_TEST_URL="http://127.0.0.1:9209" \
VGI_ES_TEST_INDEX="vgi_es_e2e" \
TRANSPORT=subprocess \
  ci/run-integration.sh   # or TRANSPORT=http / TRANSPORT=unix
make os-down
```

Or just `make test-sql`, which does all of the above (build, boot OpenSearch,
seed, run, tear down) with `haybarn-unittest` on `PATH`.
