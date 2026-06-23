// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Flavor selects the Point-In-Time API dialect. OpenSearch and Elasticsearch
// both implement search_after + PIT, but the endpoints differ slightly:
//
//	OpenSearch:    POST /<index>/_search/point_in_time?keep_alive=...   -> {"pit_id": ...}
//	               DELETE /_search/point_in_time  body {"pit_id":[...]}
//	               search body carries {"pit":{"id":...,"keep_alive":...}}
//	Elasticsearch: POST /<index>/_pit?keep_alive=...                    -> {"id": ...}
//	               DELETE /_pit  body {"id": ...}
//	               search body carries {"pit":{"id":...,"keep_alive":...}}, no index in URL
//
// The search request body is otherwise identical, which is why a single worker
// supports both flavors.
type Flavor string

const (
	// FlavorOpenSearch targets OpenSearch's _search/point_in_time endpoints.
	FlavorOpenSearch Flavor = "opensearch"
	// FlavorElasticsearch targets Elasticsearch's _pit endpoints.
	FlavorElasticsearch Flavor = "elasticsearch"
)

// Options carries everything needed to talk to a cluster for one request. It is
// rebuilt from plain gob-encodable scan-state fields on every Process tick — no
// live *http.Client ever lives in scan state.
type Options struct {
	// Endpoint is the cluster base URL, e.g. http://localhost:9200 (no trailing
	// path). Index is appended per request.
	Endpoint string
	// Flavor selects PIT dialect (opensearch|elasticsearch). Empty defaults to
	// opensearch.
	Flavor Flavor
	// Username/Password enable HTTP basic auth when both are set.
	Username string
	Password string
	// APIKey sets the "Authorization: ApiKey <APIKey>" header (Elasticsearch API
	// keys are already base64(id:api_key); pass the encoded value).
	APIKey string
	// InsecureTLS disables certificate verification (test clusters / self-signed).
	InsecureTLS bool
	// Timeout bounds a single HTTP request. Zero uses defaultTimeout.
	Timeout time.Duration
}

const (
	defaultTimeout = 30 * time.Second
	// maxBody caps a single response read (256 MiB) as a safety valve.
	maxBody = 256 << 20
)

// flavor returns the configured flavor, defaulting to OpenSearch.
func (o Options) flavor() Flavor {
	if o.Flavor == FlavorElasticsearch {
		return FlavorElasticsearch
	}
	return FlavorOpenSearch
}

// newHTTPClient builds a fresh client honoring the timeout and TLS setting. A
// new client per tick keeps scan state free of non-serializable connections.
func (o Options) newHTTPClient() *http.Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	tr := &http.Transport{}
	if o.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for test/self-signed clusters
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// applyAuth sets auth headers on a request from the configured credentials.
func (o Options) applyAuth(req *http.Request) {
	switch {
	case o.APIKey != "":
		req.Header.Set("Authorization", "ApiKey "+o.APIKey)
	case o.Username != "" || o.Password != "":
		cred := base64.StdEncoding.EncodeToString([]byte(o.Username + ":" + o.Password))
		req.Header.Set("Authorization", "Basic "+cred)
	}
}

// doJSON performs an HTTP request with an optional JSON body and decodes a JSON
// response into out (out may be nil to discard). It surfaces non-2xx responses
// (with an ES error body snippet) as Go errors.
func (o Options) doJSON(ctx context.Context, method, url string, body any, out any) error {
	if o.Endpoint == "" {
		return fmt.Errorf("elasticsearch: endpoint is required")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("elasticsearch: marshal request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return fmt.Errorf("elasticsearch: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	o.applyAuth(req)

	resp, err := o.newHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("elasticsearch: %s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("elasticsearch: read response from %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("elasticsearch: %s %s returned HTTP %d: %s",
			method, url, resp.StatusCode, snippet(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("elasticsearch: decode response from %s: %w (body: %s)", url, err, snippet(raw))
		}
	}
	return nil
}

// pitResponse covers both flavors' open-PIT responses ({"pit_id":...} on
// OpenSearch, {"id":...} on Elasticsearch).
type pitResponse struct {
	PitID string `json:"pit_id"`
	ID    string `json:"id"`
}

// OpenPIT opens a Point-In-Time for an index and returns its id. keepAlive is an
// ES duration string (e.g. "1m"). A PIT freezes the set of segments searched, so
// search_after pagination stays consistent even as the index changes underneath.
func OpenPIT(ctx context.Context, opts Options, index, keepAlive string) (string, error) {
	if index == "" {
		return "", fmt.Errorf("elasticsearch: index is required to open a PIT")
	}
	if keepAlive == "" {
		keepAlive = "1m"
	}
	var url string
	switch opts.flavor() {
	case FlavorElasticsearch:
		url = fmt.Sprintf("%s/%s/_pit?keep_alive=%s", trimEndpoint(opts.Endpoint), index, keepAlive)
	default:
		url = fmt.Sprintf("%s/%s/_search/point_in_time?keep_alive=%s", trimEndpoint(opts.Endpoint), index, keepAlive)
	}
	var pr pitResponse
	if err := opts.doJSON(ctx, http.MethodPost, url, nil, &pr); err != nil {
		return "", err
	}
	if pr.PitID != "" {
		return pr.PitID, nil
	}
	if pr.ID != "" {
		return pr.ID, nil
	}
	return "", fmt.Errorf("elasticsearch: open PIT returned no id")
}

// ClosePIT deletes a Point-In-Time. Errors are returned but callers typically
// log-and-ignore on cleanup (the PIT expires on its own via keep_alive).
func ClosePIT(ctx context.Context, opts Options, pitID string) error {
	if pitID == "" {
		return nil
	}
	var url string
	var body any
	switch opts.flavor() {
	case FlavorElasticsearch:
		url = fmt.Sprintf("%s/_pit", trimEndpoint(opts.Endpoint))
		body = map[string]any{"id": pitID}
	default:
		url = fmt.Sprintf("%s/_search/point_in_time", trimEndpoint(opts.Endpoint))
		body = map[string]any{"pit_id": []string{pitID}}
	}
	return opts.doJSON(ctx, http.MethodDelete, url, body, nil)
}

// Hit is one search result document. Source is the raw JSON of _source; Sort is
// the raw JSON array of sort values used as the search_after cursor for the next
// page. Both are kept raw so they remain gob-encodable in scan state.
type Hit struct {
	ID     string          `json:"_id"`
	Index  string          `json:"_index"`
	Score  *float64        `json:"_score"`
	Source json.RawMessage `json:"_source"`
	Sort   json.RawMessage `json:"sort"`
}

// SearchResponse is the subset of an ES/OpenSearch _search response we consume.
type SearchResponse struct {
	PitID string `json:"pit_id"`
	Hits  struct {
		Hits []Hit `json:"hits"`
	} `json:"hits"`
}

// Search runs a single _search against an open PIT and returns that page. The
// body must already contain {"pit":...,"search_after":...,"sort":...,"size":...,
// query, _source}. The PIT is in the body (not the URL), so the URL is just the
// cluster's /_search. Returns the page's hits plus the (possibly refreshed)
// pit_id the cluster echoes back.
func Search(ctx context.Context, opts Options, body map[string]any) (*SearchResponse, error) {
	url := fmt.Sprintf("%s/_search", trimEndpoint(opts.Endpoint))
	var sr SearchResponse
	if err := opts.doJSON(ctx, http.MethodPost, url, body, &sr); err != nil {
		return nil, err
	}
	return &sr, nil
}

// GetMapping fetches the field mapping for an index, returning the raw
// properties object (field name -> {type:..., properties:...}).
func GetMapping(ctx context.Context, opts Options, index string) (map[string]json.RawMessage, error) {
	if index == "" {
		return nil, fmt.Errorf("elasticsearch: index is required")
	}
	url := fmt.Sprintf("%s/%s/_mapping", trimEndpoint(opts.Endpoint), index)
	var raw map[string]struct {
		Mappings struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"mappings"`
	}
	if err := opts.doJSON(ctx, http.MethodGet, url, nil, &raw); err != nil {
		return nil, err
	}
	// A wildcard/alias index may return several concrete indices; merge their
	// properties (first writer wins on a name collision).
	merged := map[string]json.RawMessage{}
	for _, idx := range raw {
		for name, prop := range idx.Mappings.Properties {
			if _, ok := merged[name]; !ok {
				merged[name] = prop
			}
		}
	}
	return merged, nil
}

// trimEndpoint strips a trailing slash from a base endpoint URL.
func trimEndpoint(s string) string {
	return strings.TrimRight(s, "/")
}

// snippet returns a short single-line excerpt of a response body for error text.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
