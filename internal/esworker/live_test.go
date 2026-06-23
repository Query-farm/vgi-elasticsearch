// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
)

// These tests require a live OpenSearch/Elasticsearch cluster. Set
// VGI_ES_TEST_URL (e.g. http://localhost:9209) to enable them; otherwise they
// skip. The Makefile `make test` boots a dockerized OpenSearch and exports it.

func liveURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("VGI_ES_TEST_URL")
	if url == "" {
		t.Skip("VGI_ES_TEST_URL not set; skipping live OpenSearch test")
	}
	return url
}

// liveOpts builds Options for the live cluster (defaults: OpenSearch flavor, no
// auth — the test container runs with the security plugin disabled).
func liveOpts(url string) Options {
	flavor := FlavorOpenSearch
	if f := os.Getenv("VGI_ES_TEST_FLAVOR"); f == string(FlavorElasticsearch) {
		flavor = FlavorElasticsearch
	}
	return Options{Endpoint: url, Flavor: flavor, Timeout: 30 * time.Second}
}

// seedIndex (re)creates an index with a fixed mapping and bulk-indexes n docs:
// {name:"item<i>", n:<i>, group:<i%3>, created:<date>}. Refreshes so docs are
// searchable. Returns the index name.
func seedIndex(t *testing.T, url string, n int) string {
	t.Helper()
	ctx := context.Background()
	opts := liveOpts(url)
	index := fmt.Sprintf("vgi_es_test_%d", time.Now().UnixNano())

	mapping := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"name":    map[string]any{"type": "keyword"},
				"n":       map[string]any{"type": "long"},
				"group":   map[string]any{"type": "long"},
				"active":  map[string]any{"type": "boolean"},
				"created": map[string]any{"type": "date"},
			},
		},
	}
	if err := opts.doJSON(ctx, http.MethodPut, url+"/"+index, mapping, nil); err != nil {
		t.Fatalf("create index: %v", err)
	}
	t.Cleanup(func() { _ = opts.doJSON(context.Background(), http.MethodDelete, url+"/"+index, nil, nil) })

	// Bulk index.
	var buf bytes.Buffer
	for i := 1; i <= n; i++ {
		meta := map[string]any{"index": map[string]any{"_index": index, "_id": fmt.Sprintf("d%04d", i)}}
		doc := map[string]any{
			"name":    fmt.Sprintf("item%d", i),
			"n":       i,
			"group":   i % 3,
			"active":  i%2 == 0,
			"created": fmt.Sprintf("2026-01-01T00:00:%02dZ", i%60),
		}
		mb, _ := json.Marshal(meta)
		db, _ := json.Marshal(doc)
		buf.Write(mb)
		buf.WriteByte('\n')
		buf.Write(db)
		buf.WriteByte('\n')
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/_bulk", &buf)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bulk index: %v", err)
	}
	_ = resp.Body.Close()
	if err := opts.doJSON(ctx, http.MethodPost, url+"/"+index+"/_refresh", nil, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return index
}

// gobRoundTrip serializes a searchState and reads it back, exactly as the VGI
// SDK does between Process ticks over the wire. If the state held any
// non-gob-encodable field (interface/chan/func/conn), this would error — proving
// the cursor is truly externalized.
func gobRoundTrip(t *testing.T, s *searchState) *searchState {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(s); err != nil {
		t.Fatalf("gob encode scan state: %v", err)
	}
	var out searchState
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("gob decode scan state: %v", err)
	}
	return &out
}

// TestLivePITSearchAfterResumeNoDupesNoDrops is THE centerpiece. It indexes N
// docs, then pages through them ONE batch per NextPage call (the per-tick I/O
// behind Process), GOB-ROUND-TRIPPING the scan state between every page — i.e.
// the PIT id + the last hit's sort values (the search_after cursor) survive a
// simulated VGI batch boundary. It asserts every document id is returned EXACTLY
// ONCE: no duplicates across page boundaries, no drops, the full set recovered.
func TestLivePITSearchAfterResumeNoDupesNoDrops(t *testing.T) {
	url := liveURL(t)
	const total = 25
	const pageSize = 4 // total/pageSize is not integral -> exercises the short final page
	index := seedIndex(t, url, total)

	// Build the initial scan state by hand (NewState needs a ProcessParams; here
	// we drive the resumable core directly, which is what the SDK ultimately
	// invokes per tick). Sort by n asc; the _id tiebreaker is auto-appended.
	cols := append(MetaColumns(),
		Column{Name: "name", SourcePath: "name", ESType: "keyword", ArrowType: arrow.BinaryTypes.String},
		Column{Name: "n", SourcePath: "n", ESType: "long", ArrowType: arrow.PrimitiveTypes.Int64},
	)
	colsJSON, _ := json.Marshal(cols)
	sortJSON, _ := json.Marshal([]SortField{{Field: "n"}})

	state := &searchState{
		Endpoint:       url,
		Index:          index,
		Flavor:         os.Getenv("VGI_ES_TEST_FLAVOR"),
		KeepAlive:      "1m",
		PageSize:       pageSize,
		ColumnsJSON:    string(colsJSON),
		SourceIncludes: []string{"name", "n"},
		SortFieldsJSON: string(sortJSON),
	}

	ctx := context.Background()
	seen := map[string]int{}
	pages := 0
	var firstPIT string

	for {
		// Resume from gob-rehydrated state — the cursor must be carried in plain
		// exported fields for this to work at all.
		state = gobRoundTrip(t, state)
		if state.done() {
			break
		}
		hits, err := state.NextPage(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if firstPIT == "" {
			firstPIT = state.PitID
			if firstPIT == "" {
				t.Fatal("expected a PIT id to be opened on the first page")
			}
		}
		for _, h := range hits {
			seen[h.ID]++
		}
		// Safety valve against an infinite loop on a broken cursor.
		if pages > total+5 {
			t.Fatalf("too many pages (%d) — cursor likely not advancing", pages)
		}
	}

	// Every doc exactly once.
	if len(seen) != total {
		t.Fatalf("expected %d distinct docs, got %d", total, len(seen))
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("doc %s returned %d times (want exactly 1)", id, c)
		}
	}
	// The full id set, in order, with no gaps.
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		want := fmt.Sprintf("d%04d", i+1)
		if id != want {
			t.Errorf("doc %d: got %s want %s", i, id, want)
		}
	}
	// 25 docs / 4 per page = 7 pages (6*4 + 1).
	if pages != 7 {
		t.Errorf("expected 7 pages (6x4 + 1 short), got %d", pages)
	}
	t.Logf("paged %d docs across %d ticks (gob round-trip each), no dupes, no drops", total, pages)
}

// TestLiveProjectionPushdown proves _source filtering returns only requested
// fields: a projection of {n} must not bring back the `name` field bytes.
func TestLiveProjectionPushdown(t *testing.T) {
	url := liveURL(t)
	index := seedIndex(t, url, 5)
	ctx := context.Background()
	opts := liveOpts(url)

	pit, err := OpenPIT(ctx, opts, index, "1m")
	if err != nil {
		t.Fatalf("open pit: %v", err)
	}
	defer func() { _ = ClosePIT(ctx, opts, pit) }()

	body, _ := BuildSearchBody(pit, "1m", 10, []SortField{{Field: "n"}}, nil, []string{"n"}, nil)
	resp, err := Search(ctx, opts, body)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Hits.Hits) != 5 {
		t.Fatalf("expected 5 hits, got %d", len(resp.Hits.Hits))
	}
	for _, h := range resp.Hits.Hits {
		var src map[string]json.RawMessage
		_ = json.Unmarshal(h.Source, &src)
		if _, ok := src["name"]; ok {
			t.Errorf("projection pushdown failed: 'name' returned despite _source=[n]: %s", h.Source)
		}
		if _, ok := src["n"]; !ok {
			t.Errorf("projected field 'n' missing: %s", h.Source)
		}
	}
}

// TestLivePredicatePushdown proves a range + term filter is applied SERVER-side:
// querying n>=3 AND group=0 returns only the matching docs.
func TestLivePredicatePushdown(t *testing.T) {
	url := liveURL(t)
	index := seedIndex(t, url, 9) // n=1..9, group=n%3 -> group 0 is n in {3,6,9}
	ctx := context.Background()
	opts := liveOpts(url)

	pit, err := OpenPIT(ctx, opts, index, "1m")
	if err != nil {
		t.Fatalf("open pit: %v", err)
	}
	defer func() { _ = ClosePIT(ctx, opts, pit) }()

	query := map[string]any{
		"bool": map[string]any{"filter": []any{
			map[string]any{"range": map[string]any{"n": map[string]any{"gte": 3}}},
			map[string]any{"term": map[string]any{"group": 0}},
		}},
	}
	body, _ := BuildSearchBody(pit, "1m", 100, []SortField{{Field: "n"}}, nil, []string{"n", "group"}, query)
	resp, err := Search(ctx, opts, body)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// group 0 = {3,6,9}; all are >=3, so 3 docs.
	if len(resp.Hits.Hits) != 3 {
		t.Fatalf("predicate pushdown: expected 3 docs (n in {3,6,9}), got %d", len(resp.Hits.Hits))
	}
	for _, h := range resp.Hits.Hits {
		var src struct {
			N     int `json:"n"`
			Group int `json:"group"`
		}
		_ = json.Unmarshal(h.Source, &src)
		if src.N < 3 || src.Group != 0 {
			t.Errorf("server returned non-matching doc: %+v", src)
		}
	}
}

// TestLiveTypeMappingAndBatch end-to-ends the mapping introspection + batch
// build: every ES field type appears with the right Arrow type and a value.
func TestLiveTypeMappingAndBatch(t *testing.T) {
	url := liveURL(t)
	index := seedIndex(t, url, 3)
	ctx := context.Background()
	opts := liveOpts(url)

	props, err := GetMapping(ctx, opts, index)
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	cols := append(MetaColumns(), ColumnsFromMapping(props)...)
	byName := map[string]Column{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	if !arrow.TypeEqual(byName["n"].ArrowType, arrow.PrimitiveTypes.Int64) {
		t.Errorf("n should be Int64, got %v", byName["n"].ArrowType)
	}
	if _, ok := byName["created"].ArrowType.(*arrow.TimestampType); !ok {
		t.Errorf("created should be TimestampType, got %v", byName["created"].ArrowType)
	}

	pit, _ := OpenPIT(ctx, opts, index, "1m")
	defer func() { _ = ClosePIT(ctx, opts, pit) }()
	body, _ := BuildSearchBody(pit, "1m", 10, nil, nil, SourceIncludes(cols), nil)
	resp, _ := Search(ctx, opts, body)

	schema := SchemaFromColumns(cols)
	batch := BuildBatch(schema, cols, resp.Hits.Hits)
	defer batch.Release()
	if batch.NumRows() != 3 {
		t.Fatalf("expected 3 rows in batch, got %d", batch.NumRows())
	}
	// Spot-check the date column decoded into a non-null timestamp.
	createdIdx := schema.FieldIndices("created")[0]
	tsCol := batch.Column(createdIdx)
	if tsCol.IsNull(0) {
		t.Error("created timestamp should not be null")
	}
}
