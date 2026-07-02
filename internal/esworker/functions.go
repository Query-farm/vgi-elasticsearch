// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
)

// CatalogName is the VGI catalog name advertised by this worker.
const CatalogName = "elasticsearch"

// AgentTestTasks is the catalog's `vgi.agent_test_tasks` suite (VGI152/VGI920):
// a fixed set of analyst tasks the agent-check pass executes against the worker.
//
// es_search is a CONNECTOR — a live Elasticsearch/OpenSearch cluster is required
// to actually fetch rows, but the column shape resolves entirely offline from an
// explicit `fields` spec (parseFieldsSpec, no network). So the portable, cluster-
// free testable surface is schema binding via `DESCRIBE SELECT ... fields => …`,
// exactly the pattern the executable examples use. These tasks exercise that
// deterministic mapping (ES type -> DuckDB/Arrow type, the always-present _id /
// _score meta columns, and projection) so they pass in CI with no cluster.
const AgentTestTasks = `[
  {
    "name": "describe_schema_from_fields",
    "prompt": "Using this worker, and WITHOUT contacting a live cluster, show the full column schema that es_search exposes for an index named 'products' whose source fields are a keyword field 'name' and a double field 'price'. Pass an explicit fields spec so no network call is made, and include the always-present _id and _score meta columns. Return the DESCRIBE result.",
    "reference_sql": "DESCRIBE SELECT * FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double')"
  },
  {
    "name": "describe_typed_columns",
    "prompt": "Using this worker and an explicit fields spec (no live cluster), determine the DuckDB column types that es_search maps an Elasticsearch 'long' field named 'n' and a 'boolean' field named 'active' to, for an index named 'events'. Return the column schema via DESCRIBE.",
    "reference_sql": "DESCRIBE SELECT * FROM elasticsearch.main.es_search('http://localhost:9200', 'events', fields => 'n:long,active:boolean')"
  },
  {
    "name": "describe_temporal_and_object_columns",
    "prompt": "Using this worker and an explicit fields spec (no live cluster), determine the DuckDB column types that es_search assigns to an Elasticsearch 'date' field named 'created' and an 'object' field named 'meta', for an index named 'docs'. Return the full column schema via DESCRIBE SELECT *.",
    "reference_sql": "DESCRIBE SELECT * FROM elasticsearch.main.es_search('http://localhost:9200', 'docs', fields => 'created:date,meta:object')"
  }
]`

// IMPORTANT (gob-state gotcha): table-function scan state is gob-encoded by the
// SDK between NewState and Process AND between successive Process ticks when the
// scan resumes over the wire. State structs therefore hold ONLY exported,
// gob-encodable fields: no *http.Client, no arrow.Record, no interfaces/chans/
// funcs. The whole resumable deep-pagination story depends on this: the PIT id,
// the last hit's sort values (the search_after cursor) and the column plan all
// live as plain exported fields, and both the HTTP client and the Arrow batch
// are rebuilt inside Process from those fields each tick.

// ---------------------------------------------------------------------------
// es_search(endpoint, index, ...) -> rows
//
// THE worker: query an index as a SQL table using PIT + search_after for
// stateless, consistent deep pagination. One page per Process tick; the cursor
// (pit_id + last sort values) is the externalized scan state, gob-round-tripped
// between ticks. Projection pushdown via _source filtering; predicate pushdown
// via the query DSL (term/terms/range/exists), with a raw-DSL escape hatch.
// ---------------------------------------------------------------------------

type searchArgs struct {
	Endpoint    string `vgi:"pos=0,name=endpoint,doc=Cluster base URL, e.g. http://localhost:9200"`
	Index       string `vgi:"pos=1,name=index,doc=Index (or alias/wildcard) to search"`
	Fields      string `vgi:"default=,doc=Comma-separated source fields + types (name:estype,...). Empty = introspect the index mapping"`
	Query       string `vgi:"default=,doc=Raw query-DSL JSON escape hatch (merged under bool/filter with pushed predicates)"`
	Sort        string `vgi:"default=,doc=Sort spec: comma-separated field[:desc]. An _id tiebreaker is always appended"`
	Flavor      string `vgi:"default=opensearch,doc=PIT dialect: opensearch|elasticsearch"`
	KeepAlive   string `vgi:"default=1m,doc=PIT keep_alive duration (e.g. 1m, 30s)"`
	PageSize    int64  `vgi:"default=1000,doc=Hits per Process tick (search size)"`
	Username    string `vgi:"default=,doc=HTTP basic-auth username"`
	Password    string `vgi:"default=,doc=HTTP basic-auth password"`
	APIKey      string `vgi:"default=,doc=Authorization: ApiKey <value> credential"`
	InsecureTLS bool   `vgi:"default=false,doc=Skip TLS certificate verification (self-signed test clusters)"`
}

// searchState is the EXTERNALIZED, gob-encodable scan state. Every field is a
// plain exported value so the SDK serializes it between Process ticks. PitID +
// SortValues ARE the resumable search_after cursor.
type searchState struct {
	// Connection + plan (rebuilt into a client/query each tick — never store a client).
	Endpoint    string
	Index       string
	Flavor      string
	KeepAlive   string
	PageSize    int64
	Username    string
	Password    string
	APIKey      string
	InsecureTLS bool

	// ColumnsJSON is the resolved output column plan (name/source/estype) encoded
	// as JSON so it round-trips through gob without exporting arrow types.
	ColumnsJSON string
	// SourceIncludes is the _source projection list (projection pushdown).
	SourceIncludes []string
	// SortFieldsJSON is the deterministic sort spec, JSON-encoded.
	SortFieldsJSON string
	// QueryJSON is the server-side query DSL (predicate pushdown + raw escape
	// hatch), JSON-encoded; empty = match_all.
	QueryJSON string

	// Resumable cursor — the heart of the differentiator.
	PitID      string          // the open Point-In-Time id
	SortValues json.RawMessage // last hit's `sort` array (search_after for next page)
	Started    bool            // false until the first page has been fetched
	Done       bool            // set once Finish() has been signalled
	NoInput    bool            // required args were NULL — emit nothing
	OwnsPIT    bool            // true if this scan opened the PIT (and must close it)
}

// SearchFunction implements es_search.
type SearchFunction struct{}

var _ vgi.TypedTableFunc[searchState] = (*SearchFunction)(nil)

func (f *SearchFunction) Name() string { return "es_search" }

func (f *SearchFunction) Metadata() vgi.FunctionMetadata {
	return vgi.FunctionMetadata{
		Description:        "Query an Elasticsearch/OpenSearch index as a SQL table: consistent deep pagination via PIT + search_after (cursor in externalized scan state), with _source projection pushdown and query-DSL predicate pushdown",
		Stability:          vgi.StabilityVolatile,
		ProjectionPushdown: true,
		FilterPushdown:     true,
		Categories:         []string{"elasticsearch", "opensearch", "search", "api"},
		Tags: map[string]string{
			"vgi.title": "Elasticsearch / OpenSearch Index Search",
			// VGI413: names one of the schema's vgi.categories registry entries.
			"vgi.category":            "Search",
			"vgi.doc_llm":             "Query an Elasticsearch or OpenSearch index as a SQL table. Opens a Point-In-Time and pages through every matching document with search_after for consistent, resumable deep pagination over millions of hits. Pushes down column projection (via _source filtering) and predicates (term/terms/range/exists via the query DSL), supports both the OpenSearch and Elasticsearch PIT dialects, basic-auth and API-key credentials, a raw query-DSL escape hatch, and explicit sort. Positional args: endpoint, index. Two meta columns (_id VARCHAR, _score DOUBLE) are always present; one column per source field is derived from the index mapping or the explicit fields spec.",
			"vgi.doc_md":              "Query an Elasticsearch/OpenSearch index as a SQL table over Apache Arrow.\n\n`es_search(endpoint, index, ...)` performs consistent, resumable deep pagination using a Point-In-Time plus `search_after` cursor (the externalized scan state), with `_source` projection pushdown, `term`/`terms`/`range`/`exists` predicate pushdown, a raw query-DSL escape hatch, explicit sort, basic-auth / API-key credentials, and both PIT dialects. Always emits `_id` and `_score`; one column per source field.",
			"vgi.keywords":            `["elasticsearch","opensearch","es_search","search","full-text search","index","query DSL","point in time","PIT","search_after","deep pagination","cursor","projection pushdown","predicate pushdown","api key","scroll","lucene"]`,
			"vgi.executable_examples": `[{"description":"Bind the es_search schema for an index using an explicit fields spec (no live cluster needed to resolve the column shape: _id, _score, and the declared source fields).","sql":"DESCRIBE SELECT * FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double');"},{"description":"Bind the schema for reading only selected columns — the document _id plus one chosen source field — using an explicit fields spec so no live cluster is contacted. DESCRIBE returns just the projected columns.","sql":"DESCRIBE SELECT _id, level FROM elasticsearch.main.es_search('http://localhost:9200', 'logs', fields => 'level:keyword');"}]`,
			// es_search has a DYNAMIC output schema (resolved at bind from the
			// index mapping or the `fields` spec), so document the shape: two
			// always-present meta columns plus one column per indexed source field.
			"vgi.result_columns_md": "| column | type | description |\n" +
				"|---|---|---|\n" +
				"| `_id` | VARCHAR | The Elasticsearch/OpenSearch document `_id`. Always present. |\n" +
				"| `_score` | DOUBLE | The relevance score for the hit (NULL when sorting suppresses scoring). Always present. |\n" +
				"| _<source field>_ | _(mapped from ES type)_ | One column per indexed source field — either the explicit `fields` spec or, when omitted, the introspected index mapping. ES `keyword`/`text`→VARCHAR, `long`/`integer`→BIGINT, `double`/`float`→DOUBLE, `boolean`→BOOLEAN, `date`→TIMESTAMP (UTC, microseconds); `object`/`nested` are emitted as JSON VARCHAR. |",
		},
		Examples: []vgi.CatalogExample{
			{
				SQL:         "DESCRIBE SELECT _id, _score, name, price FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double');",
				Description: "Bind the column schema for the `products` index from an explicit `fields` spec. The _id/_score meta columns are always present; each declared source field becomes a typed column (here `name` VARCHAR, `price` DOUBLE).",
			},
			{
				SQL:         "DESCRIBE SELECT name, price FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double') WHERE price > 100 ORDER BY price DESC;",
				Description: "Bind the schema for a projected, filtered, sorted read of the `products` index. At runtime `name`/`price` project down via _source filtering and `price > 100` pushes into the Elasticsearch query DSL; the explicit `fields` spec lets the column shape resolve without a live cluster.",
			},
			{
				SQL:         "DESCRIBE SELECT level FROM elasticsearch.main.es_search('https://es.example.com', 'logs-*', fields => 'level:keyword', api_key => 'BASE64APIKEY', flavor => 'elasticsearch', query => '{\"term\":{\"level\":\"error\"}}');",
				Description: "Bind the schema for an error-log scan over the `logs-*` index pattern on an Elasticsearch cluster, using API-key auth, the Elasticsearch PIT dialect, and a raw query-DSL escape hatch. The explicit `fields` spec resolves the column shape without contacting the cluster.",
			},
		},
	}
}

func (f *SearchFunction) ArgumentSpecs() []vgi.ArgSpec { return vgi.DeriveArgSpecs(searchArgs{}) }

// OnBind resolves the output schema. If `fields` was given it is parsed; else the
// index mapping is introspected. Meta columns (_id, _score) are always present.
func (f *SearchFunction) OnBind(params *vgi.BindParams) (*vgi.BindResponse, error) {
	if params == nil || params.Args == nil {
		return vgi.BindSchema(SchemaFromColumns(MetaColumns()))
	}
	// endpoint/index are POSITIONAL (pos=0/pos=1) so read them by index; named
	// options (fields, auth) are read by name. Positional args live in
	// Arguments.Positional, named ones in Arguments.Named — mixing them up yields
	// an empty value and a meta-only schema.
	endpoint, _ := params.Args.GetScalarString(0)
	index, _ := params.Args.GetScalarString(1)
	fields, _ := params.Args.GetScalarString("fields")

	cols := MetaColumns()
	srcCols, err := resolveColumns(context.Background(), params.Args, endpoint, index, fields)
	if err != nil {
		return nil, err
	}
	cols = append(cols, srcCols...)
	return vgi.BindSchema(SchemaFromColumns(cols))
}

// resolveColumns produces the source-derived columns either from an explicit
// `fields` spec ("name:estype,other:long") or by introspecting the index
// mapping over HTTP.
func resolveColumns(ctx context.Context, args *vgi.Arguments, endpoint, index, fields string) ([]Column, error) {
	if strings.TrimSpace(fields) != "" {
		return parseFieldsSpec(fields), nil
	}
	if endpoint == "" || index == "" {
		return nil, nil
	}
	opts := optsFromArgs(args, endpoint, index)
	props, err := GetMapping(ctx, opts, index)
	if err != nil {
		return nil, fmt.Errorf("es_search: introspect mapping for %q: %w", index, err)
	}
	return ColumnsFromMapping(props), nil
}

// parseFieldsSpec parses "name[:estype],..." into columns. A missing type
// defaults to keyword (VARCHAR).
func parseFieldsSpec(spec string) []Column {
	var cols []Column
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, esType := part, "keyword"
		if i := strings.IndexByte(part, ':'); i >= 0 {
			name = strings.TrimSpace(part[:i])
			esType = strings.TrimSpace(part[i+1:])
		}
		if name == "" {
			continue
		}
		at, _ := esTypeToArrow(esType)
		cols = append(cols, Column{Name: name, SourcePath: name, ArrowType: at, ESType: esType})
	}
	return cols
}

// projectColumns selects columns by ProjectionIDs, preserving ProjectionIDs
// order so the emitted batch aligns with the SDK's projected OutputSchema. A nil
// projectionIDs means "all columns" (in their natural order).
func projectColumns(allCols []Column, projectionIDs []int32) []Column {
	if projectionIDs == nil {
		out := make([]Column, len(allCols))
		copy(out, allCols)
		return out
	}
	out := make([]Column, 0, len(projectionIDs))
	for _, id := range projectionIDs {
		if int(id) >= 0 && int(id) < len(allCols) {
			out = append(out, allCols[id])
		}
	}
	return out
}

// optsFromArgs builds client Options from auth args. index is unused here but
// kept for symmetry with callers that pass it.
func optsFromArgs(args *vgi.Arguments, endpoint, _ string) Options {
	get := func(k string) string { v, _ := args.GetScalarString(k); return v }
	getBool := func(k string) bool { v, _ := args.GetScalarBool(k); return v }
	flavor := FlavorOpenSearch
	if strings.EqualFold(get("flavor"), string(FlavorElasticsearch)) {
		flavor = FlavorElasticsearch
	}
	return Options{
		Endpoint:    endpoint,
		Flavor:      flavor,
		Username:    get("username"),
		Password:    get("password"),
		APIKey:      get("apikey"),
		InsecureTLS: getBool("insecuretls"),
	}
}

func (s *searchState) options() Options {
	flavor := FlavorOpenSearch
	if strings.EqualFold(s.Flavor, string(FlavorElasticsearch)) {
		flavor = FlavorElasticsearch
	}
	return Options{
		Endpoint:    s.Endpoint,
		Flavor:      flavor,
		Username:    s.Username,
		Password:    s.Password,
		APIKey:      s.APIKey,
		InsecureTLS: s.InsecureTLS,
	}
}

// NewState resolves the full scan plan (columns, projection, sort, predicate
// pushdown) but performs NO network I/O except mapping introspection at bind —
// the PIT is opened and the first page fetched in the first Process tick, so even
// page one flows through the same resumable cursor path (mirrors vgi-graphql).
func (f *SearchFunction) NewState(params *vgi.ProcessParams) (*searchState, error) {
	var args searchArgs
	if err := vgi.BindArgs(params.Args, &args); err != nil {
		return nil, err
	}
	if isNullArg(params.Args, 0) || isNullArg(params.Args, 1) {
		return &searchState{NoInput: true}, nil
	}

	// Resolve the FULL column set (meta + source), then apply projection pushdown:
	// keep only the columns DuckDB actually requested (ProjectionIDs index the
	// bound schema).
	allCols := MetaColumns()
	srcCols, err := resolveColumns(context.Background(), params.Args, args.Endpoint, args.Index, args.Fields)
	if err != nil {
		return nil, err
	}
	allCols = append(allCols, srcCols...)

	// Apply projection pushdown. CRITICAL: emit columns in ProjectionIDs ORDER,
	// not allCols order — the SDK validates the emitted batch positionally against
	// the projected OutputSchema (which follows ProjectionIDs), so a different
	// order surfaces as an arrow "type mismatch" on a misaligned column.
	cols := projectColumns(allCols, params.ProjectionIDs)
	if len(cols) == 0 {
		// Projection asked for nothing concrete; keep _id so rows still flow.
		cols = []Column{MetaColumns()[0]}
	}

	colsJSON, _ := json.Marshal(cols)
	sortFields := parseSortSpec(args.Sort)
	sortJSON, _ := json.Marshal(sortFields)

	// Predicate pushdown: build a query DSL from the static pushdown filters and
	// merge it with the raw-DSL escape hatch. Columns fully pushed to the server
	// are logged; DuckDB still re-applies all filters (AutoApplyFilters is off),
	// so an imperfect pushdown is a performance choice, never a correctness risk.
	var pf *vgi.PushdownFilters
	if params.PushdownFilters != nil {
		pf, _ = vgi.DeserializeFilters(params.PushdownFilters, params.JoinKeys)
	}
	query, pushed := buildMergedQuery(pf, args.Query)
	if len(pushed) > 0 {
		cols := make([]string, 0, len(pushed))
		for c := range pushed {
			cols = append(cols, c)
		}
		slog.Debug("es_search predicate pushdown", "index", args.Index, "pushed_columns", cols)
	}
	queryJSON := ""
	if query != nil {
		b, _ := json.Marshal(query)
		queryJSON = string(b)
	}

	return &searchState{
		Endpoint:       args.Endpoint,
		Index:          args.Index,
		Flavor:         args.Flavor,
		KeepAlive:      args.KeepAlive,
		PageSize:       args.PageSize,
		Username:       args.Username,
		Password:       args.Password,
		APIKey:         args.APIKey,
		InsecureTLS:    args.InsecureTLS,
		ColumnsJSON:    string(colsJSON),
		SourceIncludes: SourceIncludes(cols),
		SortFieldsJSON: string(sortJSON),
		QueryJSON:      queryJSON,
	}, nil
}

// buildMergedQuery combines pushed-down predicates with a raw query-DSL escape
// hatch. The raw query (if any) and the pushdown bool query are AND-merged under
// a top-level bool/filter so both constrain the result set.
func buildMergedQuery(pf *vgi.PushdownFilters, rawQuery string) (map[string]any, map[string]struct{}) {
	pushQ, pushed := BuildQueryFromFilters(pf)

	var raw map[string]any
	if strings.TrimSpace(rawQuery) != "" {
		if err := json.Unmarshal([]byte(rawQuery), &raw); err != nil {
			// A malformed escape hatch is ignored (logged); pushdown still applies.
			slog.Warn("es_search: ignoring invalid raw query DSL", "error", err)
			raw = nil
		}
	}

	switch {
	case pushQ == nil && raw == nil:
		return nil, pushed
	case pushQ == nil:
		return raw, pushed
	case raw == nil:
		return pushQ, pushed
	default:
		return map[string]any{
			"bool": map[string]any{"filter": []any{pushQ, raw}},
		}, pushed
	}
}

// parseSortSpec parses "field[:desc],..." into SortField. Empty yields nil (the
// _shard_doc tiebreaker is applied in sortToDSL).
func parseSortSpec(spec string) []SortField {
	var out []SortField
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		field, desc := part, false
		if i := strings.IndexByte(part, ':'); i >= 0 {
			field = strings.TrimSpace(part[:i])
			desc = strings.EqualFold(strings.TrimSpace(part[i+1:]), "desc")
		}
		out = append(out, SortField{Field: field, Desc: desc})
	}
	return out
}

// done reports whether the scan is exhausted.
func (s *searchState) done() bool {
	return s.NoInput || s.Done
}

// columns decodes the gob-rehydrated column plan.
func (s *searchState) columns() ([]Column, error) {
	var cols []Column
	if err := json.Unmarshal([]byte(s.ColumnsJSON), &cols); err != nil {
		return nil, fmt.Errorf("es_search: decode column plan: %w", err)
	}
	// Re-attach the concrete Arrow types (lost across JSON: only ESType survived).
	for i := range cols {
		reattachArrowType(&cols[i])
	}
	return cols, nil
}

// sortFields decodes the gob-rehydrated sort plan.
func (s *searchState) sortFields() []SortField {
	var sf []SortField
	_ = json.Unmarshal([]byte(s.SortFieldsJSON), &sf)
	return sf
}

// query decodes the gob-rehydrated query DSL (nil = match_all).
func (s *searchState) query() map[string]any {
	if s.QueryJSON == "" {
		return nil
	}
	var q map[string]any
	_ = json.Unmarshal([]byte(s.QueryJSON), &q)
	return q
}

// NextPage opens the PIT (first tick) or reuses it, runs ONE search resuming from
// the externalized SortValues cursor, advances PitID + SortValues in place, and
// returns that page's hits. The HTTP client and request body are rebuilt from
// the plain gob-rehydrated fields — nothing non-serializable ever lives in state.
// This single network tick is the differentiator; the unit test drives it
// directly, gob-round-tripping the state between calls, to prove the cursor
// survives a VGI batch boundary with no dupes and no drops.
func (s *searchState) NextPage(ctx context.Context) ([]Hit, error) {
	opts := s.options()

	if s.PitID == "" {
		pit, err := OpenPIT(ctx, opts, s.Index, s.KeepAlive)
		if err != nil {
			return nil, err
		}
		s.PitID = pit
		s.OwnsPIT = true
	}

	body, err := BuildSearchBody(s.PitID, s.KeepAlive, s.PageSize, s.sortFields(), s.SortValues, s.SourceIncludes, s.query())
	if err != nil {
		return nil, err
	}
	resp, err := Search(ctx, opts, body)
	if err != nil {
		return nil, err
	}

	s.Started = true
	hits := resp.Hits.Hits
	// The cluster may refresh the pit_id; carry the latest forward.
	if resp.PitID != "" {
		s.PitID = resp.PitID
	}
	if len(hits) > 0 {
		// Advance the search_after cursor to the LAST hit's sort values. This is
		// what round-trips through gob to resume the next page.
		s.SortValues = hits[len(hits)-1].Sort
	}
	// A short page (fewer than PageSize) means the scan is exhausted.
	if int64(len(hits)) < s.PageSize {
		s.closePIT(ctx)
		s.Done = true
	}
	return hits, nil
}

// closePIT best-effort releases the PIT this scan opened. Failures are logged
// (the PIT expires on its own via keep_alive) — cleanup must not fail a query.
func (s *searchState) closePIT(ctx context.Context) {
	if !s.OwnsPIT || s.PitID == "" {
		return
	}
	if err := ClosePIT(ctx, s.options(), s.PitID); err != nil {
		slog.Debug("es_search: PIT close failed (will expire via keep_alive)", "error", err)
	}
	s.OwnsPIT = false
}

// Process fetches ONE page, emits its hits as a record batch, and advances the
// cursor in state. When the page is short the scan finishes on the next tick.
func (f *SearchFunction) Process(ctx context.Context, _ *vgi.ProcessParams, state *searchState, out *vgirpc.OutputCollector) error {
	if state.done() {
		return out.Finish()
	}

	cols, err := state.columns()
	if err != nil {
		return err
	}
	hits, err := state.NextPage(ctx)
	if err != nil {
		return err
	}

	schema := SchemaFromColumns(cols)
	batch := BuildBatch(schema, cols, hits)
	defer batch.Release()
	if err := out.Emit(batch); err != nil {
		return err
	}
	// If NextPage marked the scan done (short page), Finish on the next tick by
	// leaving state.Done set; the SDK calls Process again and done() short-circuits.
	return nil
}

// NewSearchFunction builds the registerable table function.
func NewSearchFunction() vgi.TableFunction {
	return vgi.AsTableFunction[searchState](&SearchFunction{})
}

// isNullArg reports whether positional argument pos is present and NULL.
func isNullArg(args *vgi.Arguments, pos int) bool {
	if args == nil {
		return true
	}
	col, err := args.GetColumn(pos)
	if err != nil {
		return false
	}
	return col.Len() == 0 || col.IsNull(0)
}

// Register registers all Elasticsearch table functions on the worker.
func Register(w *vgi.Worker) {
	w.RegisterTable(NewSearchFunction())
}
