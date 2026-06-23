// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
)

// Column describes one projected output column: its SQL name, the source path it
// reads from a hit (e.g. "_id", "_score", or a _source field), and its Arrow
// type. ESType is the original Elasticsearch field type (informational).
type Column struct {
	Name       string
	SourcePath string // "_id", "_score", or a _source field name
	// ArrowType is reconstructed from ESType after a gob/JSON round-trip, so it is
	// excluded from serialization (arrow.DataType is an interface and would not
	// unmarshal). Always repopulate via reattachArrowType after decoding.
	ArrowType arrow.DataType `json:"-"`
	ESType    string
}

// MetaColumns are the always-available document metadata columns. They are
// prepended to the source-derived columns so every es_search row carries _id and
// _score regardless of the index mapping.
func MetaColumns() []Column {
	return []Column{
		{Name: "_id", SourcePath: "_id", ArrowType: arrow.BinaryTypes.String, ESType: "keyword"},
		{Name: "_score", SourcePath: "_score", ArrowType: arrow.PrimitiveTypes.Float64, ESType: "float"},
	}
}

// timestampUTC is the Arrow type used for ES `date` fields. TIMESTAMPTZ returns
// REQUIRE an explicit arrow_type, which this concrete TimestampType provides.
var timestampUTC = &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}

// esTypeToArrow maps an Elasticsearch/OpenSearch field type to an Arrow type.
//
//	keyword, text, ip, constant_keyword  -> VARCHAR
//	long, integer, short, byte           -> BIGINT (int64)
//	double, float, half_float, scaled_float -> DOUBLE
//	boolean                              -> BOOLEAN
//	date, date_nanos                     -> TIMESTAMPTZ (UTC, explicit arrow_type)
//	object, nested, and everything else  -> VARCHAR holding the raw JSON
//
// object/nested fields are surfaced as JSON-in-VARCHAR in v1 (a STRUCT mapping
// would need full recursive mapping resolution; JSON keeps every field usable
// via DuckDB's json functions). The boolean second return is false for types we
// deliberately render as raw JSON.
func esTypeToArrow(esType string) (arrow.DataType, bool) {
	switch esType {
	case "keyword", "text", "ip", "constant_keyword", "wildcard", "version":
		return arrow.BinaryTypes.String, true
	case "long", "integer", "short", "byte", "unsigned_long":
		return arrow.PrimitiveTypes.Int64, true
	case "double", "float", "half_float", "scaled_float":
		return arrow.PrimitiveTypes.Float64, true
	case "boolean":
		return arrow.FixedWidthTypes.Boolean, true
	case "date", "date_nanos":
		return timestampUTC, true
	default:
		// object, nested, geo_point, geo_shape, completion, etc. -> raw JSON.
		return arrow.BinaryTypes.String, false
	}
}

// mappingProperty is the subset of an ES field-mapping entry we read.
type mappingProperty struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// ColumnsFromMapping turns an index mapping (field -> property JSON) into sorted
// source-derived columns. Object/nested fields (those without a scalar `type`
// but with sub-properties) are emitted as a single JSON VARCHAR column. The
// result is deterministic (sorted by name) so the bound schema is stable.
func ColumnsFromMapping(props map[string]json.RawMessage) []Column {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	cols := make([]Column, 0, len(names))
	for _, name := range names {
		var mp mappingProperty
		_ = json.Unmarshal(props[name], &mp)
		esType := mp.Type
		if esType == "" && len(mp.Properties) > 0 {
			esType = "object"
		}
		at, _ := esTypeToArrow(esType)
		cols = append(cols, Column{
			Name:       name,
			SourcePath: name,
			ArrowType:  at,
			ESType:     esType,
		})
	}
	return cols
}

// reattachArrowType restores a column's concrete Arrow type from its SourcePath
// (_id/_score) or ESType, after the type-less JSON/gob round-trip used to keep
// scan state serializable.
func reattachArrowType(c *Column) {
	switch c.SourcePath {
	case "_id":
		c.ArrowType = arrow.BinaryTypes.String
	case "_score":
		c.ArrowType = arrow.PrimitiveTypes.Float64
	default:
		c.ArrowType, _ = esTypeToArrow(c.ESType)
	}
}

// SchemaFromColumns builds an Arrow schema from columns in order.
func SchemaFromColumns(cols []Column) *arrow.Schema {
	fields := make([]arrow.Field, len(cols))
	for i, c := range cols {
		fields[i] = arrow.Field{Name: c.Name, Type: c.ArrowType, Nullable: true}
	}
	return arrow.NewSchema(fields, nil)
}

// isJSONColumn reports whether a column should carry raw JSON text rather than a
// decoded scalar (object/nested/geo/etc.).
func (c Column) isJSONColumn() bool {
	_, scalarType := esTypeToArrow(c.ESType)
	return !scalarType
}

// sourceField returns the dotted-path key used to pull this column's value from
// a hit's _source. Meta columns (_id/_score) have no source field.
func (c Column) sourceField() string {
	if c.SourcePath == "_id" || c.SourcePath == "_score" {
		return ""
	}
	return c.SourcePath
}

// SourceIncludes returns the _source include list (projection pushdown) for a
// set of columns: the source field names, excluding meta columns. An empty
// result means "no _source needed" (only meta columns projected).
func SourceIncludes(cols []Column) []string {
	var inc []string
	for _, c := range cols {
		if f := c.sourceField(); f != "" {
			inc = append(inc, f)
		}
	}
	return inc
}

// looksLikeDate reports whether a string is plausibly an ISO-8601/RFC3339 date,
// used to decide whether to parse a date column's value.
func looksLikeDate(s string) bool {
	return strings.Count(s, "-") >= 2 || strings.Contains(s, "T")
}
