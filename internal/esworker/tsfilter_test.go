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

var tsType = &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}

// tsBindSchema adds a TIMESTAMPTZ column (index 1) for timestamp predicates.
var tsBindSchema = arrow.NewSchema([]arrow.Field{
	{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	{Name: "created", Type: tsType, Nullable: true},
}, nil)

func tsArr(vs ...int64) arrow.Array {
	b := array.NewTimestampBuilder(memory.DefaultAllocator, tsType)
	for _, v := range vs {
		b.Append(arrow.Timestamp(v))
	}
	return b.NewArray()
}

func tsListArr(vs ...int64) arrow.Array {
	b := array.NewListBuilder(memory.DefaultAllocator, tsType)
	vb := b.ValueBuilder().(*array.TimestampBuilder)
	b.Append(true)
	for _, v := range vs {
		vb.Append(arrow.Timestamp(v))
	}
	return b.NewArray()
}

func deserializeTS(t *testing.T, exprJSON string, values ...arrow.Array) *vgi.PushdownFilters {
	t.Helper()
	batch := filterBatch(t, exprJSON, values...)
	defer batch.Release()
	pf, err := vgi.DeserializeFiltersWithSchema(batch, tsBindSchema, nil)
	if err != nil {
		t.Fatalf("DeserializeFiltersWithSchema: %v", err)
	}
	return pf
}

// hitsAt builds hits whose `created` source value is the given RFC3339 string
// ("" = field missing).
func hitsAt(vals ...string) []Hit {
	out := make([]Hit, len(vals))
	for i, v := range vals {
		src := map[string]any{"n": i}
		if v != "" {
			src["created"] = v
		}
		b, _ := json.Marshal(src)
		out[i] = Hit{ID: v, Source: b}
	}
	return out
}

func hitIDs(hits []Hit) []string {
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	return ids
}

// The regression: a TIMESTAMPTZ comparison is declined by the ES query builder,
// and DuckDB never re-checks pushed predicates, so it used to be dropped and
// every document came back. It must now be collected and applied to the hits.
func TestTimestampRangeIsAppliedToHits(t *testing.T) {
	const at20 = 1767225620000000 // 2026-01-01T00:00:20Z in microseconds
	pf := deserializeTS(t, cmpExpr("ge", 1, "created"), tsArr(at20))

	if q, _ := BuildQueryFromFilters(pf); q != nil {
		t.Fatalf("timestamp predicate unexpectedly pushed to ES: %v", q)
	}
	preds := timestampPredicates(pf)
	if len(preds) != 1 || preds[0].Column != "created" || preds[0].Op != vgi.OpGE {
		t.Fatalf("predicates = %+v", preds)
	}
	hits := hitsAt("2026-01-01T00:00:19Z", "2026-01-01T00:00:20Z", "2026-01-01T00:00:21.5Z", "")
	got := hitIDs(filterHitsByTimestamp(hits, preds))
	want := []string{"2026-01-01T00:00:20Z", "2026-01-01T00:00:21.5Z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestTimestampInAndNotEqual(t *testing.T) {
	const at20 = 1767225620000000
	in := deserializeTS(t,
		`{"node":"in","expression":{"node":"column_ref","column_index":1,"column_name":"created"},"set":{"kind":"literal","value_ref":0},"negated":false}`,
		tsListArr(at20, at20+1_000_000))
	hits := hitsAt("2026-01-01T00:00:20Z", "2026-01-01T00:00:21Z", "2026-01-01T00:00:22Z")
	if got := hitIDs(filterHitsByTimestamp(hits, timestampPredicates(in))); len(got) != 2 {
		t.Fatalf("IN kept %v", got)
	}
	ne := deserializeTS(t, cmpExpr("ne", 1, "created"), tsArr(at20))
	got := hitIDs(filterHitsByTimestamp(hitsAt("2026-01-01T00:00:20Z", "2026-01-01T00:00:21Z", ""), timestampPredicates(ne)))
	if !reflect.DeepEqual(got, []string{"2026-01-01T00:00:21Z"}) {
		t.Fatalf("!= kept %v (a missing value is NULL and must be dropped)", got)
	}
}

func TestSplitMicrosFloorsNanos(t *testing.T) {
	if us, ns := splitMicros(-1, arrow.Nanosecond); us != -1 || ns != 999 {
		t.Fatalf("splitMicros(-1ns) = %d,%d", us, ns)
	}
	// column 5µs vs literal 5µs+1ns: column is less.
	p := TimestampPredicate{Op: vgi.OpLT, Micros: []int64{5}, Nanos: []int64{1}}
	if !p.matches(5, true) {
		t.Fatal("5us < 5us+1ns should hold")
	}
}

func TestWithPredicateColumnsFetchesFilterOnlyColumn(t *testing.T) {
	got := withPredicateColumns(nil, []TimestampPredicate{{Column: "created"}})
	if !reflect.DeepEqual(got, []string{"created"}) {
		t.Fatalf("includes = %v", got)
	}
}
