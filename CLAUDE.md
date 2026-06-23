# CLAUDE.md — vgi-elasticsearch

VGI worker (Go) exposing an Elasticsearch/OpenSearch index as a SQL table via
PIT + `search_after` deep pagination. Built on the `github.com/Query-farm/vgi-go`
SDK; mirrors `~/Development/vgi-graphql` (per-tick I/O, resumable cursor in
gob-safe scan state, client rebuilt each tick) and adds projection + predicate
pushdown.

## Layout

- `cmd/vgi-elasticsearch-worker/main.go` — entrypoint; `RunStdio()` (or `--http`).
- `cmd/seed/main.go` — bulk-seeds a fixed test index for the haybarn E2E.
- `internal/esworker/`
  - `client.go` — stdlib HTTP client: `OpenPIT` / `ClosePIT` / `Search` /
    `GetMapping`, both OpenSearch and Elasticsearch PIT dialects (`Flavor`).
  - `query.go` — pure logic: `BuildSearchBody` (PIT + search_after + sort + source
    + query), `sortToDSL` (always appends an `_id` tiebreaker), and
    `BuildQueryFromFilters` (DuckDB `PushdownFilters` → ES query DSL).
  - `types.go` — ES-type → Arrow mapping, mapping introspection → columns,
    `_source` include list, `reattachArrowType`.
  - `rows.go` — hits → Arrow record batch (typed columns, date parsing, JSON for
    object/nested).
  - `functions.go` — the `es_search` table function: `OnBind` (schema),
    `NewState` (resolve plan, projection + predicate pushdown, NO network except
    bind-time mapping), `Process`/`NextPage` (one page per tick, advance cursor).
  - `*_test.go` — unit tests (pure logic) + `live_test.go` (gated on
    `VGI_ES_TEST_URL`).

## Non-negotiable invariants (cost hours if broken)

- **Scan state is gob-encoded between ticks.** `searchState` holds ONLY plain
  exported, gob-encodable fields — strings, ints, bools, `json.RawMessage`. No
  `*http.Client`, no `arrow.Record`, no interfaces. `Column.ArrowType` is
  `arrow.DataType` (an interface) and is tagged `json:"-"`; it is reconstructed
  from `ESType` via `reattachArrowType` after every decode. The live test
  gob-round-trips the state between pages to enforce this.
- **Positional vs named args.** `endpoint`/`index` are positional (`pos=0/1`):
  read them with `GetScalarString(0)`/`(1)`. Everything with `default=` is a named
  option: read by name. Mixing them up yields an empty value (e.g. a meta-only
  bind schema). `name := value` works because these are *table*-function args.
- **Projection order.** Emit columns in `ProjectionIDs` order, not mapping order —
  the SDK validates the batch positionally against the projected `OutputSchema`.
  A wrong order surfaces as an arrow "type mismatch" on a misaligned column. See
  `projectColumns`.
- **`_shard_doc` is forbidden.** OpenSearch 2.x rejects it. Use the `_id`
  tiebreaker (works on both flavors).
- **Pushdown is a superset, never a correctness risk.** `AutoApplyFilters` is
  off, so DuckDB re-applies every predicate. Push only what ES evaluates
  identically; decline anything uncertain (return it un-pushed).
- **STRUCT/LIST/TIMESTAMPTZ returns require an explicit `arrow_type`** — `date`
  uses a concrete `&arrow.TimestampType{Unit: Microsecond, TimeZone: "UTC"}`.
- **Logging to stderr only** (`log/slog` default). Never write to stdout — that is
  the VGI protocol channel.

## Testing

`make test` boots dockerized OpenSearch (Apache-2.0), runs unit + Go-live +
haybarn E2E, tears the container down. haybarn: `statement ok` + `LOAD vgi;`
(never `require vgi`), `require-env VGI_ELASTICSEARCH_WORKER`, ATTACH via
`${VGI_ELASTICSEARCH_WORKER}`. If Docker is unavailable, `make test-unit` still
exercises all pure logic (query building, pushdown, cursor, type mapping).

Avoid SQL reserved words as field names in fixtures (`group` → `grp`).
