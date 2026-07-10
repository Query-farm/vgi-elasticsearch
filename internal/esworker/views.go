// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
)

// TypeMappingRow documents one Elasticsearch/OpenSearch field-type to DuckDB
// column-type mapping, as applied by esTypeToArrow. It is surfaced verbatim by
// the browsable es_type_mapping reference view.
type TypeMappingRow struct {
	ESType     string // the Elasticsearch/OpenSearch field type
	DuckDBType string // the DuckDB column type es_search emits for it
	Category   string // a coarse grouping (string/integer/float/boolean/temporal/json)
	Notes      string // a short human note (no apostrophes — kept SQL-literal-safe)
}

// TypeMappingRows is the canonical ES-type -> DuckDB-type reference. It is kept
// in lockstep with esTypeToArrow (types.go): TestTypeMappingRowsMatchArrow
// asserts every row here resolves through esTypeToArrow to the DuckDB type it
// claims, so this metadata can never silently drift from the worker's behaviour.
func TypeMappingRows() []TypeMappingRow {
	return []TypeMappingRow{
		{"keyword", "VARCHAR", "string", "Exact-value string. The default when a fields-spec entry omits its type."},
		{"text", "VARCHAR", "string", "Full-text field, surfaced as its raw string value."},
		{"ip", "VARCHAR", "string", "IP address, surfaced as text."},
		{"constant_keyword", "VARCHAR", "string", "Single-value keyword, surfaced as text."},
		{"wildcard", "VARCHAR", "string", "Wildcard keyword, surfaced as text."},
		{"version", "VARCHAR", "string", "Semantic-version keyword, surfaced as text."},
		{"long", "BIGINT", "integer", "64-bit signed integer."},
		{"integer", "BIGINT", "integer", "32-bit integer, widened to BIGINT."},
		{"short", "BIGINT", "integer", "16-bit integer, widened to BIGINT."},
		{"byte", "BIGINT", "integer", "8-bit integer, widened to BIGINT."},
		{"unsigned_long", "BIGINT", "integer", "Unsigned 64-bit integer, surfaced as BIGINT."},
		{"double", "DOUBLE", "float", "64-bit floating point."},
		{"float", "DOUBLE", "float", "32-bit float, widened to DOUBLE."},
		{"half_float", "DOUBLE", "float", "16-bit float, widened to DOUBLE."},
		{"scaled_float", "DOUBLE", "float", "Scaled float, surfaced as DOUBLE."},
		{"boolean", "BOOLEAN", "boolean", "Boolean value."},
		{"date", "TIMESTAMP WITH TIME ZONE", "temporal", "Parsed to a UTC microsecond timestamp."},
		{"date_nanos", "TIMESTAMP WITH TIME ZONE", "temporal", "Parsed to a UTC microsecond timestamp."},
		{"object", "VARCHAR", "json", "Nested structure, surfaced as raw JSON text."},
		{"nested", "VARCHAR", "json", "Array of objects, surfaced as raw JSON text."},
		{"geo_point", "VARCHAR", "json", "Geo point, surfaced as raw JSON text."},
		{"geo_shape", "VARCHAR", "json", "Geo shape, surfaced as raw JSON text."},
	}
}

// sqlQuote renders s as a single-quoted SQL string literal, doubling any
// embedded single quote.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// typeMappingViewDefinition builds the self-contained (no cluster, no base
// table) SELECT that backs the es_type_mapping view, from TypeMappingRows so the
// view and the worker's type mapping cannot drift.
func typeMappingViewDefinition() string {
	var b strings.Builder
	b.WriteString("SELECT es_type, duckdb_type, category, notes FROM (VALUES\n")
	rows := TypeMappingRows()
	for i, r := range rows {
		b.WriteString("  (")
		b.WriteString(sqlQuote(r.ESType))
		b.WriteString(", ")
		b.WriteString(sqlQuote(r.DuckDBType))
		b.WriteString(", ")
		b.WriteString(sqlQuote(r.Category))
		b.WriteString(", ")
		b.WriteString(sqlQuote(r.Notes))
		b.WriteString(")")
		if i < len(rows)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(") AS t(es_type, duckdb_type, category, notes)")
	return b.String()
}

// TypeMappingView is the browsable reference view (elasticsearch.main.
// es_type_mapping). It documents, one row per Elasticsearch/OpenSearch field
// type, the DuckDB column type es_search produces — a cheap, cluster-free
// discovery entry point so an agent can SELECT real data before it knows the
// es_search arguments (VGI146). Its rows come from TypeMappingRows, the same
// source esTypeToArrow is tested against.
func TypeMappingView() vgi.CatalogView {
	return vgi.CatalogView{
		Name:       "es_type_mapping",
		Definition: typeMappingViewDefinition(),
		Comment:    "Reference: how each Elasticsearch/OpenSearch field type maps to a DuckDB column type in es_search (one row per ES type). Browsable with no cluster.",
		ColumnComments: map[string]string{
			"es_type":     "The Elasticsearch/OpenSearch field type (as it appears in the index mapping or an es_search `fields` spec entry).",
			"duckdb_type": "The DuckDB column type es_search emits for this ES type.",
			"category":    "Coarse grouping of the ES type: string, integer, float, boolean, temporal, or json.",
			"notes":       "A short human note on how the value is surfaced.",
		},
		Tags: map[string]string{
			"vgi.title":           "Elasticsearch / OpenSearch Type Mapping",
			"vgi.category":        "Reference",
			"vgi.doc_llm":         "Reference lookup of how es_search maps every Elasticsearch/OpenSearch field type to a DuckDB column type. One row per ES type with columns es_type, duckdb_type, category, and notes. Browse it to predict the column type es_search will produce for a given field (keyword/text -> VARCHAR, long/integer -> BIGINT, double/float -> DOUBLE, boolean -> BOOLEAN, date -> TIMESTAMP WITH TIME ZONE, object/nested/geo -> VARCHAR holding raw JSON) without contacting a cluster.",
			"vgi.doc_md":          "# es_type_mapping\n\nA cluster-free reference view: one row per Elasticsearch/OpenSearch field type, giving the DuckDB column type `es_search` emits for it. Use it to predict a column's type before you run a search.\n\n| ES type family | DuckDB type |\n|---|---|\n| `keyword`, `text`, `ip`, … | `VARCHAR` |\n| `long`, `integer`, `short`, `byte` | `BIGINT` |\n| `double`, `float`, `half_float` | `DOUBLE` |\n| `boolean` | `BOOLEAN` |\n| `date`, `date_nanos` | `TIMESTAMP WITH TIME ZONE` (UTC) |\n| `object`, `nested`, `geo_*` | `VARCHAR` (raw JSON) |",
			"vgi.keywords":        `["elasticsearch","opensearch","type mapping","es type","duckdb type","schema","reference","keyword","date","object","json","es_search"]`,
			"vgi.example_queries": `[{"description":"List the ES types that es_search surfaces as raw JSON text (object/nested/geo).","sql":"SELECT es_type, duckdb_type FROM elasticsearch.main.es_type_mapping WHERE category = 'json' ORDER BY es_type"},{"description":"How many distinct DuckDB types does es_search map ES field types onto, and which ES types feed each?","sql":"SELECT duckdb_type, count(*) AS es_types, string_agg(es_type, ', ' ORDER BY es_type) AS from_es FROM elasticsearch.main.es_type_mapping GROUP BY duckdb_type ORDER BY es_types DESC"}]`,
			// VGI123 classifying tags use BARE keys; reuse the schema's vocabulary.
			"domain": "search",
			"topic":  "elasticsearch-opensearch-pagination",
		},
	}
}
