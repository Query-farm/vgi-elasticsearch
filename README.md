<p align="center">
  <img src="https://raw.githubusercontent.com/Query-farm/vgi/main/docs/vgi-logo.png" alt="Vector Gateway Interface (VGI)" width="320">
</p>

<p align="center"><em>A <a href="https://query.farm">Query.Farm</a> VGI worker for DuckDB.</em></p>

# Query Elasticsearch & OpenSearch as Tables in DuckDB

> **vgi-elasticsearch** · a [Query.Farm](https://query.farm) VGI worker

A [VGI](https://query.farm) worker (Go) that queries an **Elasticsearch /
OpenSearch** index as a SQL table from DuckDB. It uses **Point-In-Time (PIT) +
`search_after`** for consistent, resumable **deep pagination** — the deprecated
stateful scroll API is *not* used. The pagination cursor (the PIT id + the last
hit's sort values) is externalized into the worker's gob-encoded scan state, so a
scan resumes statelessly across VGI batch boundaries with **no duplicates and no
drops**.

```sql
LOAD vgi;
ATTACH 'elasticsearch' AS es (TYPE vgi, LOCATION '/path/to/vgi-elasticsearch-worker');

-- Page through an entire index as rows (columns derived from the mapping):
SELECT _id, name, n, created
FROM es.es_search('http://localhost:9200', 'my_index');

-- Predicates push to the cluster as a query DSL; only requested fields leave it:
SELECT _id, n
FROM es.es_search('http://localhost:9200', 'my_index')
WHERE n >= 100 AND status = 'active';
```

## The `es_search` table function

```
es_search(endpoint, index, ...) -> rows
```

| Argument        | Kind        | Default      | Description |
|-----------------|-------------|--------------|-------------|
| `endpoint`      | positional  | —            | Cluster base URL, e.g. `http://localhost:9200` |
| `index`         | positional  | —            | Index, alias, or wildcard to search |
| `fields`        | named       | *(introspect)* | `name:estype,...` to declare columns explicitly; empty = read the index mapping |
| `query`         | named       | —            | Raw query-DSL JSON escape hatch (merged under `bool/filter` with pushed predicates) |
| `sort`          | named       | `_id`        | `field[:desc],...`; an `_id` tiebreaker is always appended |
| `flavor`        | named       | `opensearch` | PIT dialect: `opensearch` or `elasticsearch` |
| `keep_alive`    | named       | `1m`         | PIT `keep_alive` duration |
| `page_size`     | named       | `1000`       | Hits fetched per scan tick (`size`) |
| `username`/`password` | named | —            | HTTP basic auth |
| `apikey`        | named       | —            | `Authorization: ApiKey <value>` |
| `insecuretls`   | named       | `false`      | Skip TLS verification (self-signed test clusters) |

Every row carries `_id VARCHAR` and `_score DOUBLE` plus the projected source
fields.

### Resumable deep pagination (the differentiator)

Each `Process` tick does exactly one unit of I/O:

1. open a PIT on the first tick (reuse it thereafter);
2. run one `_search` with `search_after` set to the **last hit's sort values**
   from the previous tick (stored in scan state);
3. emit that page's hits and advance the cursor in state;
4. when a page is short, close the PIT and finish.

The scan state holds only plain exported, gob-encodable fields — the PIT id, the
last sort values (raw JSON), and the column/sort/query plan — so the VGI runtime
can serialize it between ticks and rehydrate the cursor on the next tick. The
HTTP client and the Arrow batch are rebuilt from those fields every tick; nothing
non-serializable (no connection, no `arrow.Record`) ever lives in state.

The sort always ends in an `_id` tiebreaker so the ordering is total and
`search_after` never skips or repeats a document at a page boundary. (We
deliberately avoid the `_shard_doc` tiebreaker: Elasticsearch 8 supports it but
OpenSearch 2.x rejects it with *"No mapping found for [_shard_doc]"*.)

### Projection pushdown

The columns DuckDB actually requests are translated into a `_source` include
list, so bytes the query does not need never leave the cluster. Selecting only
`_id` skips `_source` entirely.

### Predicate pushdown

DuckDB `WHERE` predicates are mapped onto the ES query DSL:

| SQL                | ES query DSL |
|--------------------|--------------|
| `col = v`          | `term`       |
| `col IN (a, b)`    | `terms`      |
| `col >/>=/</<= v`  | `range`      |
| `col != v`         | `bool.must_not term` |
| `col IS NULL`      | `bool.must_not exists` |
| `col IS NOT NULL`  | `exists`     |
| `AND` of the above | combined `bool.filter` |

Predicates that cannot be pushed (`OR`, struct/nested, expression, join-key
filters) fall back to DuckDB post-filtering. Pushdown is always a *performance*
choice, never a correctness one: DuckDB re-applies every predicate client-side,
so an imperfect pushdown can only return a superset, which DuckDB then trims. A
raw `query` argument is AND-merged with the pushed predicates.

### Type mapping

| Elasticsearch type | Arrow / DuckDB type |
|--------------------|---------------------|
| `keyword`, `text`, `ip`, `version`, … | `VARCHAR` |
| `long`, `integer`, `short`, `byte`, `unsigned_long` | `BIGINT` |
| `double`, `float`, `half_float`, `scaled_float` | `DOUBLE` |
| `boolean`          | `BOOLEAN` |
| `date`, `date_nanos` | `TIMESTAMPTZ` (UTC; explicit `arrow_type`) |
| `object`, `nested`, `geo_*`, … | `VARCHAR` holding the raw JSON |

`date` values are parsed from RFC3339 strings or epoch-millis. Object/nested
fields are surfaced as JSON text — query them with DuckDB's `json` functions
(`meta->>'$.idx'`). `STRUCT`/`LIST`/`TIMESTAMPTZ` returns require an explicit
`arrow_type`, which the worker always supplies.

## Building and testing

```
make build      # build the worker + seeder binaries
make test-unit  # pure-Go unit tests (no cluster needed)
make test-live  # Go live tests against a dockerized OpenSearch
make test-sql   # haybarn SQL E2E against a dockerized OpenSearch
make test       # all of the above
```

`test-live` and `test-sql` require Docker; the Makefile boots a single-node
`opensearchproject/opensearch:2.17.0` (Apache-2.0) with the security plugin
disabled on host port 9209, seeds a fixed index, runs the suite, and tears the
container down. `test-sql` also needs `haybarn-unittest` on `PATH`
(`uv tool install haybarn-unittest`).

The centerpiece live test indexes 25 documents and pages through them one batch
per tick at `page_size := 4` (7 ticks: 6×4 + 1 short), **gob-round-tripping the
scan state between every tick**, and asserts every document is returned exactly
once — proving the `search_after` cursor survives a batch boundary.

## Licensing

This worker is MIT-licensed (see `LICENSE`). Its dependency surface is Apache-2.0
and permissive only — **no GPL/AGPL**:

- `github.com/apache/arrow-go/v18` — **Apache-2.0**
- `github.com/Query-farm/vgi-go`, `github.com/Query-farm/vgi-rpc-go` — Query Farm SDK
- the cluster client is implemented directly on the Go **standard library**
  (`net/http` + `encoding/json`), so no Elasticsearch/OpenSearch client library is
  vendored at all. The official Apache-2.0 clients
  (`github.com/opensearch-project/opensearch-go`,
  `github.com/elastic/go-elasticsearch`) are drop-in compatible if a future
  version wants their connection pooling/sniffing.

**Server-license note:** the Elasticsearch *server* is dual-licensed under SSPL /
the Elastic License (not OSS). This worker is only a *client* of an HTTP API,
which those licenses do not restrict. For the test container and CI we
deliberately use **OpenSearch (Apache-2.0)** to keep the license surface clean;
the worker speaks to both flavors at runtime (`flavor := 'elasticsearch'`).

## Caveats and gaps (v1)

- **Object/nested fields** are surfaced as JSON-in-`VARCHAR`, not as Arrow
  `STRUCT`s. Full recursive mapping resolution into nested `STRUCT`/`LIST` types
  is future work; JSON keeps every field usable today via DuckDB's `json`
  functions.
- **Predicate pushdown** covers `term`/`terms`/`range`/`exists` and their `AND`
  composition. `OR`, struct/nested-field, expression, and join-key filters are
  not pushed (they post-filter in DuckDB). Text fields are matched with `term`
  (exact); if you index analyzed `text` you may want a `.keyword` sub-field or the
  raw `query` escape hatch for full-text `match`.
- **PIT keep-alive / expiry:** each search refreshes the PIT's `keep_alive`, but a
  scan that stalls longer than `keep_alive` between ticks can see the PIT expire;
  raise `keep_alive` for very slow consumers. The PIT is closed on scan
  completion; on an abandoned scan it expires on its own.
- **Aggregations are not exposed** — `es_search` returns documents, not `aggs`.
  Use DuckDB's `GROUP BY` over the returned rows, or the raw `query` hatch for
  bespoke document queries.
- `_shard_doc` is intentionally unused (OpenSearch 2.x incompatibility); the `_id`
  tiebreaker is universal but slightly less efficient on very large shards than a
  numeric doc-order sort would be.

---

## Authorship & License

Written by [Query.Farm](https://query.farm).

Copyright 2026 Query Farm LLC - https://query.farm

