// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"encoding/json"
	"fmt"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/scalar"
)

// BuildSearchBody assembles one _search request body for a PIT + search_after
// page. It is pure (no I/O) so it is fully unit-testable.
//
//   - pitID + keepAlive go into {"pit":{"id":...,"keep_alive":...}}.
//   - sort is a deterministic ordering so search_after is total-ordered; an _id
//     tiebreaker is always appended (see sortToDSL) unless the caller's sort
//     already ends in _id.
//   - searchAfter, when non-nil, is the previous page's last hit `sort` array
//     (raw JSON) — this is the externalized cursor.
//   - sourceFields, when non-empty, is the _source include list (projection
//     pushdown); empty means return the full _source.
//   - query, when non-nil, is the raw query DSL (predicate pushdown / escape
//     hatch); nil means match_all.
func BuildSearchBody(pitID, keepAlive string, size int64, sort []SortField, searchAfter json.RawMessage, sourceFields []string, query map[string]any) (map[string]any, error) {
	if pitID == "" {
		return nil, fmt.Errorf("elasticsearch: pit id is required to build a search body")
	}
	if size <= 0 {
		size = 1000
	}
	if keepAlive == "" {
		keepAlive = "1m"
	}

	body := map[string]any{
		"size": size,
		"pit": map[string]any{
			"id":         pitID,
			"keep_alive": keepAlive,
		},
		"sort":             sortToDSL(sort),
		"track_total_hits": false,
	}

	if query != nil {
		body["query"] = query
	} else {
		body["query"] = map[string]any{"match_all": map[string]any{}}
	}

	if len(sourceFields) > 0 {
		body["_source"] = sourceFields
	}

	if len(searchAfter) > 0 {
		var sa []any
		if err := json.Unmarshal(searchAfter, &sa); err != nil {
			return nil, fmt.Errorf("elasticsearch: invalid search_after cursor: %w", err)
		}
		body["search_after"] = sa
	}

	return body, nil
}

// SortField is one sort key in the deterministic ordering used for search_after.
type SortField struct {
	Field string // field name, or "_shard_doc" / "_id" / "_score"
	Desc  bool
}

// sortToDSL renders sort fields into ES sort-clause JSON, always ending in a
// deterministic _id tiebreaker so search_after deep pagination is total-ordered
// (no dupes/drops at page boundaries). _id is a stable, monotonic field present
// in every document and is supported on BOTH OpenSearch and Elasticsearch — we
// deliberately avoid _shard_doc, which Elasticsearch 8 supports but OpenSearch
// 2.x rejects ("No mapping found for [_shard_doc]"). If the caller already ends
// their sort on _id we do not append a second one.
func sortToDSL(sort []SortField) []any {
	tiebreaker := SortField{Field: "_id", Desc: false}
	if len(sort) == 0 || sort[len(sort)-1].Field != "_id" {
		sort = append(append([]SortField{}, sort...), tiebreaker)
	}
	out := make([]any, 0, len(sort))
	for _, s := range sort {
		order := "asc"
		if s.Desc {
			order = "desc"
		}
		out = append(out, map[string]any{
			s.Field: map[string]any{"order": order},
		})
	}
	return out
}

// BuildQueryFromFilters maps a set of DuckDB pushdown filters onto an ES query
// DSL bool query. It returns the query (or nil if no filter is pushable) plus
// the set of column names that were FULLY pushed to the server. Columns not in
// that set must still be post-filtered in SQL by DuckDB.
//
// Pushdown rules (conservative — only push what ES evaluates identically):
//   - col = v            -> term  (or match_all-bypass)
//   - col IN (a,b,...)   -> terms
//   - col >/>=/</<= v    -> range
//   - col IS NULL        -> must_not exists
//   - col IS NOT NULL    -> exists
//   - AND of the above   -> combined into the same bool/filter context
//
// OR, struct, expression and join-key filters are NOT pushed (returned as
// not-pushed) and fall back to DuckDB post-filtering. fieldType maps a column to
// its ES type so we can choose `.keyword` sub-fields for text equality if needed
// (kept simple in v1: we trust the caller to target keyword/numeric fields).
func BuildQueryFromFilters(pf *vgi.PushdownFilters) (map[string]any, map[string]struct{}) {
	pushed := map[string]struct{}{}
	if pf == nil || len(pf.Filters) == 0 {
		return nil, pushed
	}

	var filterClauses []any
	var mustNot []any

	add := func(f vgi.Filter) bool {
		switch ft := f.(type) {
		case *vgi.ConstantFilter:
			clause, neg, ok := constantToClause(ft)
			if !ok {
				return false
			}
			if neg {
				mustNot = append(mustNot, clause)
			} else {
				filterClauses = append(filterClauses, clause)
			}
			return true
		case *vgi.InFilter:
			vals := arrayToGo(ft.Values)
			if vals == nil {
				return false
			}
			filterClauses = append(filterClauses, map[string]any{
				"terms": map[string]any{ft.ColumnName(): vals},
			})
			return true
		case *vgi.IsNullFilter:
			mustNot = append(mustNot, map[string]any{
				"exists": map[string]any{"field": ft.ColumnName()},
			})
			return true
		case *vgi.IsNotNullFilter:
			filterClauses = append(filterClauses, map[string]any{
				"exists": map[string]any{"field": ft.ColumnName()},
			})
			return true
		}
		return false
	}

	for _, f := range pf.Filters {
		switch ft := f.(type) {
		case *vgi.AndFilter:
			// Push each child we can; AND maps to multiple filter clauses. A child
			// is only marked pushed if it fully translated.
			for _, c := range ft.Children {
				if add(c) {
					pushed[c.ColumnName()] = struct{}{}
				}
			}
		default:
			if add(f) {
				pushed[f.ColumnName()] = struct{}{}
			}
		}
	}

	if len(filterClauses) == 0 && len(mustNot) == 0 {
		return nil, pushed
	}

	boolQ := map[string]any{}
	if len(filterClauses) > 0 {
		boolQ["filter"] = filterClauses
	}
	if len(mustNot) > 0 {
		boolQ["must_not"] = mustNot
	}
	return map[string]any{"bool": boolQ}, pushed
}

// constantToClause renders a ConstantFilter into an ES query clause. It returns
// (clause, negated, ok): negated clauses belong in must_not (e.g. !=).
func constantToClause(cf *vgi.ConstantFilter) (map[string]any, bool, bool) {
	field := cf.ColumnName()
	val := scalarGo(cf.Value)
	if val == nil {
		return nil, false, false
	}
	switch cf.Op {
	case vgi.OpEQ:
		return map[string]any{"term": map[string]any{field: val}}, false, true
	case vgi.OpNE:
		return map[string]any{"term": map[string]any{field: val}}, true, true
	case vgi.OpGT:
		return map[string]any{"range": map[string]any{field: map[string]any{"gt": val}}}, false, true
	case vgi.OpGE:
		return map[string]any{"range": map[string]any{field: map[string]any{"gte": val}}}, false, true
	case vgi.OpLT:
		return map[string]any{"range": map[string]any{field: map[string]any{"lt": val}}}, false, true
	case vgi.OpLE:
		return map[string]any{"range": map[string]any{field: map[string]any{"lte": val}}}, false, true
	}
	return nil, false, false
}

// scalarGo unwraps an Arrow scalar into a plain Go value suitable for JSON
// encoding into a query DSL. Unsupported/complex scalars return nil so the
// caller declines the pushdown (DuckDB then post-filters).
func scalarGo(s scalar.Scalar) any {
	if s == nil || !s.IsValid() {
		return nil
	}
	switch v := s.(type) {
	case *scalar.Int8:
		return v.Value
	case *scalar.Int16:
		return v.Value
	case *scalar.Int32:
		return v.Value
	case *scalar.Int64:
		return v.Value
	case *scalar.Uint8:
		return v.Value
	case *scalar.Uint16:
		return v.Value
	case *scalar.Uint32:
		return v.Value
	case *scalar.Uint64:
		return v.Value
	case *scalar.Float32:
		return v.Value
	case *scalar.Float64:
		return v.Value
	case *scalar.String:
		return string(v.Value.Bytes())
	case *scalar.LargeString:
		return string(v.Value.Bytes())
	case *scalar.Boolean:
		return v.Value
	default:
		return nil
	}
}

// arrayToGo converts an Arrow array (an IN filter's value set) into a Go slice
// for a terms query. Returns nil if any element is of an unsupported type.
func arrayToGo(arr arrow.Array) []any {
	if arr == nil {
		return nil
	}
	out := make([]any, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		if arr.IsNull(i) {
			continue
		}
		s, err := scalar.GetScalar(arr, i)
		if err != nil {
			return nil
		}
		g := scalarGo(s)
		if g == nil {
			return nil
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
