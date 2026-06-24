// Copyright 2026 Query Farm LLC - https://query.farm

// Command vgi-elasticsearch-worker is a VGI worker that queries an
// Elasticsearch/OpenSearch index as a SQL table, using Point-In-Time +
// search_after for consistent, resumable deep pagination. It speaks the VGI
// protocol over stdio (or HTTP with --http, or the AF_UNIX launcher transport
// with --unix <path>).
package main

import (
	"flag"
	"log"
	"os"
	"strings"

	"github.com/Query-farm/vgi-elasticsearch/internal/esworker"
	"github.com/Query-farm/vgi-go/vgi"
)

func main() {
	// Accept --http for HTTP transport and --unix for the AF_UNIX launcher
	// transport; default is stdio. Unknown launcher flags are tolerated (the
	// VGI extension varies argv to key its worker cache), so we filter to flags
	// we actually define before parsing.
	httpMode := flag.Bool("http", false, "Run as an HTTP server instead of stdio")
	unixPath := flag.String("unix", "", "Serve the AF_UNIX launcher transport on this socket path instead of stdio")
	logFlags := vgi.RegisterLoggingFlags(flag.CommandLine)
	_ = flag.CommandLine.Parse(filterKnownFlags(os.Args[1:], map[string]bool{
		"log-level":  true,
		"log-format": true,
		"log-logger": true,
		"unix":       true,
	}))
	if err := logFlags.Apply(); err != nil {
		log.Fatalf("logging flags: %v", err)
	}

	sourceURL := "https://github.com/Query-farm/vgi-elasticsearch"
	w := vgi.NewWorker(
		vgi.WithCatalogName(esworker.CatalogName),
		vgi.WithCatalogComment("Query Elasticsearch/OpenSearch indices as SQL tables (PIT + search_after deep pagination via externalized scan state)"),
		vgi.WithCatalogTags(map[string]string{
			"source":                 "vgi-elasticsearch",
			"vgi.title":              "Elasticsearch / OpenSearch Search Connector",
			"vgi.keywords":           "elasticsearch, opensearch, search, full-text search, index, query DSL, point in time, PIT, search_after, deep pagination, cursor, projection pushdown, predicate pushdown, api key, lucene, es_search",
			"vgi.description_llm":    "Query an Elasticsearch or OpenSearch index as a SQL table. The es_search table function opens a Point-In-Time and pages through every matching document with search_after, giving consistent, resumable deep pagination over millions of hits. It pushes down column projection (via _source filtering) and predicates (term/terms/range/exists via the query DSL), supports both the OpenSearch and Elasticsearch PIT dialects, basic-auth/API-key credentials, a raw query-DSL escape hatch, and explicit sort. Use it to read, filter, and join Elasticsearch/OpenSearch data from DuckDB SQL.",
			"vgi.description_md":     "# elasticsearch\n\nQuery Elasticsearch/OpenSearch indices as SQL tables over Apache Arrow.\n\nThe `es_search(endpoint, index, ...)` table function performs consistent, resumable deep pagination using a Point-In-Time plus `search_after` cursor (the cursor is the externalized scan state, so a scan survives batch boundaries with no duplicates and no drops). It supports projection pushdown (`_source` filtering), predicate pushdown (`term`/`terms`/`range`/`exists` query DSL), a raw query-DSL escape hatch, explicit sort, basic-auth / API-key credentials, and both OpenSearch and Elasticsearch PIT dialects.\n\nTable: `es_search`.",
			"vgi.author":             "Query.Farm",
			"vgi.copyright":          "Copyright 2026 Query Farm LLC - https://query.farm",
			"vgi.license":            "MIT",
			"vgi.support_contact":    "https://github.com/Query-farm/vgi-elasticsearch/issues",
			"vgi.support_policy_url": "https://github.com/Query-farm/vgi-elasticsearch/blob/main/README.md",
		}),
		vgi.WithCatalogInfo(vgi.CatalogInfo{
			Name:      esworker.CatalogName,
			SourceURL: &sourceURL,
		}),
		vgi.WithSchemaComments(map[string]string{
			"main": "Elasticsearch/OpenSearch search functions.",
		}),
		vgi.WithSchemaTags(map[string]map[string]string{
			"main": {
				"vgi.title":           "Elasticsearch / OpenSearch — main",
				"vgi.description_llm": "Elasticsearch/OpenSearch search functions. The es_search table function reads an index as a SQL table with consistent deep pagination (PIT + search_after), projection and predicate pushdown, sort, a raw query-DSL escape hatch, and basic-auth / API-key credentials.",
				"vgi.description_md":  "Elasticsearch/OpenSearch search functions over Apache Arrow. Contains the `es_search` table function for reading an index as a SQL table with PIT + `search_after` deep pagination, projection/predicate pushdown, sort, and authentication.",
				"vgi.keywords":        "elasticsearch, opensearch, es_search, search, full-text search, index, query DSL, point in time, PIT, search_after, deep pagination, projection pushdown, predicate pushdown, api key, lucene",
				"vgi.source_url":      "https://github.com/Query-farm/vgi-elasticsearch/blob/main/internal/esworker/functions.go",
				// VGI123 classifying tags use BARE keys (not vgi.-namespaced).
				"domain":   "search",
				"category": "full-text-search",
				"topic":    "elasticsearch-opensearch-pagination",
				// VGI506 representative example queries (a plain string of SQL).
				"vgi.example_queries": "DESCRIBE SELECT _id, _score, name, price FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double');\n" +
					"DESCRIBE SELECT name, price FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double') WHERE price > 100 ORDER BY price DESC;\n" +
					"DESCRIBE SELECT level FROM elasticsearch.main.es_search('https://es.example.com', 'logs-*', fields => 'level:keyword', api_key => 'BASE64APIKEY', flavor => 'elasticsearch', query => '{\"term\":{\"level\":\"error\"}}');",
			},
		}),
	)
	esworker.Register(w)

	if *httpMode {
		if err := w.RunHttp("127.0.0.1:0"); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *unixPath != "" {
		// AF_UNIX launcher transport: serve on the given socket path. The SDK
		// prints "UNIX:<path>" once listening; idleTimeout=0 disables the
		// self-shutdown timer (the launcher/CI owns the process lifecycle).
		if err := w.RunUnix(*unixPath, 0); err != nil {
			log.Fatal(err)
		}
		return
	}
	w.RunStdio()
}

// filterKnownFlags drops argv tokens for flags this binary doesn't define, so
// launcher-injected differentiation flags don't abort flag parsing. Flags named
// in valueFlags consume the following token as their value.
func filterKnownFlags(args []string, valueFlags map[string]bool) []string {
	defined := map[string]bool{}
	flag.CommandLine.VisitAll(func(f *flag.Flag) { defined[f.Name] = true })
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		hasInlineValue := strings.ContainsRune(name, '=')
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if !defined[name] {
			continue
		}
		out = append(out, a)
		if valueFlags[name] && !hasInlineValue && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}
