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
   stages the preprocessed tree, points `VGI_ELASTICSEARCH_WORKER` at the built
   worker binary, warms the extension cache once (`INSTALL vgi FROM community`),
   then runs the suite in a single `haybarn-unittest` invocation. Any failed
   assertion exits non-zero and fails the job.

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
  ci/run-integration.sh
make os-down
```

Or just `make test-sql`, which does all of the above (build, boot OpenSearch,
seed, run, tear down) with `haybarn-unittest` on `PATH`.
