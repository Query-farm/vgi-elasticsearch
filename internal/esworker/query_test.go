// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// --- sort / search body -----------------------------------------------------

func TestSortAlwaysEndsInIDTiebreaker(t *testing.T) {
	// No sort -> just _id.
	got := sortToDSL(nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 sort clause, got %d: %v", len(got), got)
	}
	if _, ok := got[0].(map[string]any)["_id"]; !ok {
		t.Errorf("default sort must be _id, got %v", got[0])
	}

	// Field sort -> field then _id appended.
	got = sortToDSL([]SortField{{Field: "n", Desc: true}})
	if len(got) != 2 {
		t.Fatalf("expected field+tiebreaker, got %d clauses", len(got))
	}
	if _, ok := got[1].(map[string]any)["_id"]; !ok {
		t.Errorf("tiebreaker must be _id, got %v", got[1])
	}
	if m := got[0].(map[string]any)["n"].(map[string]any); m["order"] != "desc" {
		t.Errorf("desc order lost: %v", m)
	}

	// Caller already ending in _id -> not duplicated.
	got = sortToDSL([]SortField{{Field: "_id"}})
	if len(got) != 1 {
		t.Errorf("explicit _id sort should not be duplicated, got %d", len(got))
	}
}

func TestBuildSearchBodyShape(t *testing.T) {
	after := json.RawMessage(`[42,"d7"]`)
	body, err := BuildSearchBody("PIT123", "2m", 50, []SortField{{Field: "n"}}, after, []string{"name", "n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if body["size"].(int64) != 50 {
		t.Errorf("size not propagated: %v", body["size"])
	}
	pit := body["pit"].(map[string]any)
	if pit["id"] != "PIT123" || pit["keep_alive"] != "2m" {
		t.Errorf("pit block wrong: %v", pit)
	}
	if _, ok := body["query"].(map[string]any)["match_all"]; !ok {
		t.Errorf("nil query must default to match_all, got %v", body["query"])
	}
	src := body["_source"].([]string)
	if !reflect.DeepEqual(src, []string{"name", "n"}) {
		t.Errorf("_source projection wrong: %v", src)
	}
	sa := body["search_after"].([]any)
	if len(sa) != 2 || sa[0].(float64) != 42 || sa[1].(string) != "d7" {
		t.Errorf("search_after cursor not decoded: %v", sa)
	}
}

func TestBuildSearchBodyRejectsBadCursor(t *testing.T) {
	if _, err := BuildSearchBody("PIT", "1m", 10, nil, json.RawMessage(`{bad`), nil, nil); err == nil {
		t.Error("expected error for malformed search_after cursor")
	}
	if _, err := BuildSearchBody("", "1m", 10, nil, nil, nil, nil); err == nil {
		t.Error("expected error for missing pit id")
	}
}

func TestBuildSearchBodyRawQuery(t *testing.T) {
	q := map[string]any{"term": map[string]any{"name": "x"}}
	body, _ := BuildSearchBody("P", "1m", 10, nil, nil, nil, q)
	if !reflect.DeepEqual(body["query"], q) {
		t.Errorf("raw query not used: %v", body["query"])
	}
	if _, ok := body["_source"]; ok {
		t.Errorf("empty projection should omit _source, got %v", body["_source"])
	}
}

// --- predicate pushdown -----------------------------------------------------
//
// The SDK's filter structs have unexported fields, so an external test builds
// filters via the wire format and DeserializeFilters — which also exercises the
// real deserialization path the worker uses in NewState.

// filterBatch builds a pushdown-filter RecordBatch from a JSON spec list and a
// set of value columns, mirroring what DuckDB sends. Column 0 is the JSON specs
// (carrying the vgi_filter_version metadata); columns 1..N are value_ref slots.
func filterBatch(t *testing.T, specsJSON string, values ...arrow.Array) arrow.RecordBatch {
	t.Helper()
	mem := memory.DefaultAllocator
	md := arrow.NewMetadata([]string{"vgi_filter_version"}, []string{"1"})
	fields := []arrow.Field{{Name: "specs", Type: arrow.BinaryTypes.String, Metadata: md}}
	cols := make([]arrow.Array, 0, len(values)+1)
	sb := array.NewStringBuilder(mem)
	sb.Append(specsJSON)
	cols = append(cols, sb.NewArray())
	for i, v := range values {
		fields = append(fields, arrow.Field{Name: "v" + string(rune('0'+i)), Type: v.DataType()})
		cols = append(cols, v)
	}
	schema := arrow.NewSchema(fields, nil)
	return array.NewRecordBatch(schema, cols, 1)
}

func strArr(vs ...string) arrow.Array {
	b := array.NewStringBuilder(memory.DefaultAllocator)
	for _, v := range vs {
		b.Append(v)
	}
	return b.NewArray()
}
func i64Arr(vs ...int64) arrow.Array {
	b := array.NewInt64Builder(memory.DefaultAllocator)
	for _, v := range vs {
		b.Append(v)
	}
	return b.NewArray()
}

func deserialize(t *testing.T, specsJSON string, values ...arrow.Array) *vgi.PushdownFilters {
	t.Helper()
	batch := filterBatch(t, specsJSON, values...)
	defer batch.Release()
	pf, err := vgi.DeserializeFilters(batch)
	if err != nil {
		t.Fatalf("DeserializeFilters: %v", err)
	}
	return pf
}

func TestPushdownEquality(t *testing.T) {
	pf := deserialize(t,
		`[{"type":"constant","column_name":"name","column_index":0,"op":"eq","value_ref":0}]`,
		strArr("alice"))
	q, pushed := BuildQueryFromFilters(pf)
	if _, ok := pushed["name"]; !ok {
		t.Fatalf("name should be pushed, got %v", pushed)
	}
	filter := q["bool"].(map[string]any)["filter"].([]any)
	term := filter[0].(map[string]any)["term"].(map[string]any)
	if term["name"] != "alice" {
		t.Errorf("term clause wrong: %v", term)
	}
}

func TestPushdownNotEqualGoesToMustNot(t *testing.T) {
	pf := deserialize(t,
		`[{"type":"constant","column_name":"name","column_index":0,"op":"ne","value_ref":0}]`,
		strArr("bob"))
	q, _ := BuildQueryFromFilters(pf)
	mn, ok := q["bool"].(map[string]any)["must_not"].([]any)
	if !ok || len(mn) != 1 {
		t.Fatalf("!= should map to must_not, got %v", q)
	}
}

func TestPushdownRange(t *testing.T) {
	pf := deserialize(t,
		`[{"type":"constant","column_name":"n","column_index":1,"op":"ge","value_ref":0}]`,
		i64Arr(3))
	q, pushed := BuildQueryFromFilters(pf)
	if _, ok := pushed["n"]; !ok {
		t.Fatal("n range should be pushed")
	}
	filter := q["bool"].(map[string]any)["filter"].([]any)
	rng := filter[0].(map[string]any)["range"].(map[string]any)["n"].(map[string]any)
	if rng["gte"].(int64) != 3 {
		t.Errorf("gte bound wrong: %v", rng)
	}
}

// strListArr builds a List<String> array of one row containing vs — the shape
// an IN filter's value_ref column takes (a list scalar).
func strListArr(vs ...string) arrow.Array {
	b := array.NewListBuilder(memory.DefaultAllocator, arrow.BinaryTypes.String)
	vb := b.ValueBuilder().(*array.StringBuilder)
	b.Append(true)
	for _, v := range vs {
		vb.Append(v)
	}
	return b.NewArray()
}

func TestPushdownIn(t *testing.T) {
	pf := deserialize(t,
		`[{"type":"in","column_name":"tag","column_index":2,"value_ref":0}]`,
		strListArr("a", "b"))
	q, pushed := BuildQueryFromFilters(pf)
	if _, ok := pushed["tag"]; !ok {
		t.Fatal("tag IN should be pushed")
	}
	terms := q["bool"].(map[string]any)["filter"].([]any)[0].(map[string]any)["terms"].(map[string]any)["tag"].([]any)
	if len(terms) != 2 || terms[0] != "a" {
		t.Errorf("terms values wrong: %v", terms)
	}
}

func TestPushdownEmptyReturnsNil(t *testing.T) {
	q, pushed := BuildQueryFromFilters(nil)
	if q != nil || len(pushed) != 0 {
		t.Errorf("nil filters should produce no query, got %v / %v", q, pushed)
	}
	q, _ = BuildQueryFromFilters(&vgi.PushdownFilters{})
	if q != nil {
		t.Errorf("empty filters should produce no query, got %v", q)
	}
}

// --- type mapping -----------------------------------------------------------

func TestESTypeToArrow(t *testing.T) {
	cases := map[string]arrow.DataType{
		"keyword": arrow.BinaryTypes.String,
		"text":    arrow.BinaryTypes.String,
		"long":    arrow.PrimitiveTypes.Int64,
		"integer": arrow.PrimitiveTypes.Int64,
		"double":  arrow.PrimitiveTypes.Float64,
		"float":   arrow.PrimitiveTypes.Float64,
		"boolean": arrow.FixedWidthTypes.Boolean,
	}
	for es, want := range cases {
		got, scalarOK := esTypeToArrow(es)
		if !arrow.TypeEqual(got, want) {
			t.Errorf("%s -> %v, want %v", es, got, want)
		}
		if !scalarOK {
			t.Errorf("%s should be a scalar type", es)
		}
	}
	// date -> explicit UTC timestamp (TIMESTAMPTZ).
	got, _ := esTypeToArrow("date")
	tt, ok := got.(*arrow.TimestampType)
	if !ok || tt.TimeZone != "UTC" {
		t.Errorf("date must map to UTC TimestampType, got %v", got)
	}
	// object/nested -> raw JSON VARCHAR (scalarOK=false).
	got, scalarOK := esTypeToArrow("object")
	if !arrow.TypeEqual(got, arrow.BinaryTypes.String) || scalarOK {
		t.Errorf("object should map to JSON VARCHAR, got %v scalarOK=%v", got, scalarOK)
	}
}

func TestColumnsFromMapping(t *testing.T) {
	props := map[string]json.RawMessage{
		"name":    json.RawMessage(`{"type":"keyword"}`),
		"n":       json.RawMessage(`{"type":"long"}`),
		"created": json.RawMessage(`{"type":"date"}`),
		"meta":    json.RawMessage(`{"properties":{"a":{"type":"keyword"}}}`),
	}
	cols := ColumnsFromMapping(props)
	if len(cols) != 4 {
		t.Fatalf("expected 4 cols, got %d", len(cols))
	}
	// Sorted by name: created, meta, n, name.
	if cols[0].Name != "created" || cols[3].Name != "name" {
		t.Errorf("columns not sorted: %v", colNames(cols))
	}
	byName := map[string]Column{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	if byName["meta"].ESType != "object" || !byName["meta"].isJSONColumn() {
		t.Errorf("object field should be a JSON column: %+v", byName["meta"])
	}
}

func TestSourceIncludesSkipsMeta(t *testing.T) {
	cols := append(MetaColumns(), Column{Name: "name", SourcePath: "name", ESType: "keyword"})
	inc := SourceIncludes(cols)
	if len(inc) != 1 || inc[0] != "name" {
		t.Errorf("_id/_score must be excluded from _source includes, got %v", inc)
	}
}

func colNames(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}
