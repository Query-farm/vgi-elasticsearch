// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"encoding/json"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// BuildBatch turns a slice of hits into an Arrow record batch for the projected
// columns. Each column reads from the hit's _id, _score, or _source.<field>.
// Missing/null values become Arrow nulls. date columns parse RFC3339/epoch-ms
// into a UTC TimestampType (explicit arrow_type). object/nested columns carry
// the raw JSON text. The returned batch is owned by the caller (Release it).
func BuildBatch(schema *arrow.Schema, cols []Column, hits []Hit) arrow.RecordBatch {
	n := len(hits)
	// Pre-decode each hit's _source into a generic map once.
	sources := make([]map[string]json.RawMessage, n)
	for i, h := range hits {
		if len(h.Source) > 0 {
			_ = json.Unmarshal(h.Source, &sources[i])
		}
	}

	arrays := make([]arrow.Array, len(cols))
	for ci, col := range cols {
		arrays[ci] = buildColumn(col, hits, sources)
	}
	return array.NewRecordBatch(schema, arrays, int64(n))
}

// buildColumn materializes one column across all hits.
func buildColumn(col Column, hits []Hit, sources []map[string]json.RawMessage) arrow.Array {
	mem := memory.DefaultAllocator
	n := len(hits)

	switch col.SourcePath {
	case "_id":
		b := array.NewStringBuilder(mem)
		defer b.Release()
		for i := 0; i < n; i++ {
			if hits[i].ID == "" {
				b.AppendNull()
			} else {
				b.Append(hits[i].ID)
			}
		}
		return b.NewArray()
	case "_score":
		b := array.NewFloat64Builder(mem)
		defer b.Release()
		for i := 0; i < n; i++ {
			if hits[i].Score == nil {
				b.AppendNull()
			} else {
				b.Append(*hits[i].Score)
			}
		}
		return b.NewArray()
	}

	field := col.sourceField()
	rawAt := func(i int) (json.RawMessage, bool) {
		if sources[i] == nil {
			return nil, false
		}
		v, ok := sources[i][field]
		return v, ok
	}

	switch col.ArrowType.(type) {
	case *arrow.Int64Type:
		b := array.NewInt64Builder(mem)
		defer b.Release()
		for i := 0; i < n; i++ {
			raw, ok := rawAt(i)
			var v int64
			if ok && json.Unmarshal(raw, &v) == nil {
				b.Append(v)
			} else {
				b.AppendNull()
			}
		}
		return b.NewArray()
	case *arrow.Float64Type:
		b := array.NewFloat64Builder(mem)
		defer b.Release()
		for i := 0; i < n; i++ {
			raw, ok := rawAt(i)
			var v float64
			if ok && json.Unmarshal(raw, &v) == nil {
				b.Append(v)
			} else {
				b.AppendNull()
			}
		}
		return b.NewArray()
	case *arrow.BooleanType:
		b := array.NewBooleanBuilder(mem)
		defer b.Release()
		for i := 0; i < n; i++ {
			raw, ok := rawAt(i)
			var v bool
			if ok && json.Unmarshal(raw, &v) == nil {
				b.Append(v)
			} else {
				b.AppendNull()
			}
		}
		return b.NewArray()
	case *arrow.TimestampType:
		b := array.NewTimestampBuilder(mem, timestampUTC)
		defer b.Release()
		for i := 0; i < n; i++ {
			raw, ok := rawAt(i)
			if !ok {
				b.AppendNull()
				continue
			}
			if ts, parsed := parseDate(raw); parsed {
				b.Append(ts)
			} else {
				b.AppendNull()
			}
		}
		return b.NewArray()
	default:
		// VARCHAR: either a JSON object/nested field (raw JSON) or a string scalar.
		b := array.NewStringBuilder(mem)
		defer b.Release()
		for i := 0; i < n; i++ {
			raw, ok := rawAt(i)
			if !ok {
				b.AppendNull()
				continue
			}
			if col.isJSONColumn() {
				b.Append(string(raw))
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil {
				b.Append(s)
			} else {
				// Non-string scalar in a VARCHAR slot: keep the raw JSON text.
				b.Append(string(raw))
			}
		}
		return b.NewArray()
	}
}

// parseDate converts an ES date value (raw JSON) into an Arrow microsecond
// timestamp. Accepts RFC3339 strings, date-only strings, and epoch-millis
// numbers (ES's default numeric date representation).
func parseDate(raw json.RawMessage) (arrow.Timestamp, bool) {
	// Numeric: epoch milliseconds.
	var ms int64
	if json.Unmarshal(raw, &ms) == nil {
		return arrow.Timestamp(ms * 1000), true // ms -> us
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return 0, false
	}
	if !looksLikeDate(s) {
		return 0, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			ts, err := arrow.TimestampFromTime(t.UTC(), arrow.Microsecond)
			if err == nil {
				return ts, true
			}
		}
	}
	return 0, false
}
