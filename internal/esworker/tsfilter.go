// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"encoding/json"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/scalar"
)

// DuckDB removes every predicate it pushes into a table function from its own
// plan and never re-checks the rows the function returns. BuildQueryFromFilters
// cannot translate a TIMESTAMPTZ comparison into the ES query exactly (the
// cluster indexes `date` at millisecond precision and may parse the source with
// a mapping format the worker does not, while the SQL column is parsed from
// `_source`), so it declines it. Before this file existed a declined timestamp
// predicate was simply lost: `WHERE created >= TIMESTAMPTZ '…'` returned every
// document. The worker now applies these predicates itself, to each page's hits,
// against the same parsed value the column carries.

// TimestampPredicate is one pushed `column <op> literal` (or `column IN (…)`)
// predicate on a timestamp column. Literals are kept exactly as a microsecond
// value plus a nanosecond remainder (0..999) so a nanosecond literal still
// compares correctly against the microsecond column. Plain exported fields: it
// lives (JSON-encoded) in the gob-encoded scan state.
type TimestampPredicate struct {
	Column string
	Op     vgi.ComparisonOp // OpEQ/NE/GT/GE/LT/LE; OpEQ with several literals = IN
	Micros []int64
	Nanos  []int64
}

// timestampPredicates collects the pushed timestamp comparisons and IN lists
// (top level or under AND) that the ES query builder declines.
func timestampPredicates(pf *vgi.PushdownFilters) []TimestampPredicate {
	if pf == nil {
		return nil
	}
	var out []TimestampPredicate
	var visit func(f vgi.Filter)
	visit = func(f vgi.Filter) {
		switch ft := f.(type) {
		case *vgi.AndFilter:
			for _, c := range ft.Children {
				visit(c)
			}
		case *vgi.ConstantFilter:
			ts, ok := ft.Value.(*scalar.Timestamp)
			if !ok || !ts.IsValid() {
				return
			}
			us, ns := splitMicros(int64(ts.Value), ts.Type.(*arrow.TimestampType).Unit)
			out = append(out, TimestampPredicate{Column: ft.ColumnName(), Op: ft.Op, Micros: []int64{us}, Nanos: []int64{ns}})
		case *vgi.InFilter:
			arr, ok := ft.Values.(*array.Timestamp)
			if !ok {
				return
			}
			unit := arr.DataType().(*arrow.TimestampType).Unit
			p := TimestampPredicate{Column: ft.ColumnName(), Op: vgi.OpEQ}
			for i := 0; i < arr.Len(); i++ {
				if arr.IsNull(i) {
					continue
				}
				us, ns := splitMicros(int64(arr.Value(i)), unit)
				p.Micros = append(p.Micros, us)
				p.Nanos = append(p.Nanos, ns)
			}
			out = append(out, p)
		}
	}
	for _, f := range pf.Filters {
		visit(f)
	}
	return out
}

// splitMicros converts a timestamp value in unit to (microseconds, nanosecond
// remainder 0..999), flooring so negative (pre-1970) values stay ordered.
func splitMicros(v int64, unit arrow.TimeUnit) (int64, int64) {
	switch unit {
	case arrow.Second:
		return v * 1_000_000, 0
	case arrow.Millisecond:
		return v * 1_000, 0
	case arrow.Nanosecond:
		us := v / 1_000
		ns := v % 1_000
		if ns < 0 {
			us--
			ns += 1_000
		}
		return us, ns
	default:
		return v, 0
	}
}

// matches reports whether a column value (microseconds, no sub-microsecond
// part) satisfies the predicate. A missing or unparseable value is SQL NULL and
// never matches.
func (p TimestampPredicate) matches(us int64, ok bool) bool {
	if !ok {
		return false
	}
	// cmp of (us, 0) against each literal (Micros[i], Nanos[i]).
	cmp := func(i int) int {
		switch {
		case us < p.Micros[i]:
			return -1
		case us > p.Micros[i]:
			return 1
		case p.Nanos[i] > 0:
			return -1
		}
		return 0
	}
	if len(p.Micros) == 0 {
		return false
	}
	switch p.Op {
	case vgi.OpEQ:
		for i := range p.Micros {
			if cmp(i) == 0 {
				return true
			}
		}
		return false
	case vgi.OpNE:
		return cmp(0) != 0
	case vgi.OpGT:
		return cmp(0) > 0
	case vgi.OpGE:
		return cmp(0) >= 0
	case vgi.OpLT:
		return cmp(0) < 0
	case vgi.OpLE:
		return cmp(0) <= 0
	}
	// An operator this file does not model: keep the row (never drop data the
	// predicate might accept). DuckDB only pushes the operators above.
	return true
}

// filterHitsByTimestamp drops the hits that fail any timestamp predicate,
// evaluating each against the value parseDate yields for the column — exactly
// what BuildBatch would put in the row.
func filterHitsByTimestamp(hits []Hit, preds []TimestampPredicate) []Hit {
	if len(preds) == 0 {
		return hits
	}
	kept := hits[:0:0]
	for _, h := range hits {
		var src map[string]json.RawMessage
		if len(h.Source) > 0 {
			_ = json.Unmarshal(h.Source, &src)
		}
		keep := true
		for _, p := range preds {
			raw, present := src[p.Column]
			var us int64
			ok := false
			if present {
				var ts arrow.Timestamp
				ts, ok = parseDate(raw)
				us = int64(ts)
			}
			if !p.matches(us, ok) {
				keep = false
				break
			}
		}
		if keep {
			kept = append(kept, h)
		}
	}
	return kept
}

// withPredicateColumns appends each predicate column missing from the _source
// include list, so a filter-only column (WHERE created >= … with created not
// selected) is still fetched for evaluation.
func withPredicateColumns(includes []string, preds []TimestampPredicate) []string {
	seen := map[string]bool{}
	for _, f := range includes {
		seen[f] = true
	}
	for _, p := range preds {
		if !seen[p.Column] {
			seen[p.Column] = true
			includes = append(includes, p.Column)
		}
	}
	return includes
}
