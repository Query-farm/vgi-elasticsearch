// Copyright 2026 Query Farm LLC - https://query.farm

// Command seed creates a fixed test index in a running OpenSearch/Elasticsearch
// cluster and bulk-indexes a deterministic set of documents. It is used by the
// Makefile to prepare data for the haybarn SQL E2E suite.
//
// Usage: seed --url http://127.0.0.1:9200 --index vgi_es_e2e --count 25
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:9200", "cluster base URL")
	index := flag.String("index", "vgi_es_e2e", "index name to create")
	count := flag.Int("count", 25, "number of documents to index")
	flag.Parse()

	if err := run(*url, *index, *count); err != nil {
		log.Fatalf("seed: %v", err)
	}
	fmt.Printf("seeded %d docs into %s/%s\n", *count, *url, *index)
}

func run(url, index string, count int) error {
	// Delete any prior index (idempotent).
	_, _ = do(http.MethodDelete, url+"/"+index, nil, "application/json")

	mapping := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"name":    map[string]any{"type": "keyword"},
				"n":       map[string]any{"type": "long"},
				"grp":     map[string]any{"type": "long"},
				"active":  map[string]any{"type": "boolean"},
				"score":   map[string]any{"type": "double"},
				"created": map[string]any{"type": "date"},
				"meta":    map[string]any{"type": "object"},
			},
		},
	}
	mb, _ := json.Marshal(mapping)
	if body, err := do(http.MethodPut, url+"/"+index, mb, "application/json"); err != nil {
		return fmt.Errorf("create index: %v (%s)", err, body)
	}

	var buf bytes.Buffer
	for i := 1; i <= count; i++ {
		meta := map[string]any{"index": map[string]any{"_index": index, "_id": fmt.Sprintf("d%04d", i)}}
		doc := map[string]any{
			"name":    fmt.Sprintf("item%d", i),
			"n":       i,
			"grp":     i % 3,
			"active":  i%2 == 0,
			"score":   float64(i) / 2.0,
			"created": fmt.Sprintf("2026-01-01T00:00:%02dZ", i%60),
			"meta":    map[string]any{"idx": i, "label": fmt.Sprintf("L%d", i)},
		}
		mj, _ := json.Marshal(meta)
		dj, _ := json.Marshal(doc)
		buf.Write(mj)
		buf.WriteByte('\n')
		buf.Write(dj)
		buf.WriteByte('\n')
	}
	if body, err := do(http.MethodPost, url+"/_bulk", buf.Bytes(), "application/x-ndjson"); err != nil {
		return fmt.Errorf("bulk index: %v (%s)", err, body)
	}
	if _, err := do(http.MethodPost, url+"/"+index+"/_refresh", nil, "application/json"); err != nil {
		return fmt.Errorf("refresh: %v", err)
	}
	return nil
}

func do(method, url string, body []byte, contentType string) (string, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 && method != http.MethodDelete {
		return string(raw), fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(raw), nil
}
