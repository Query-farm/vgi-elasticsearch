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
	httpAddr := flag.String("http-addr", "127.0.0.1:0", "HTTP listen address (ignored unless --http). Default is an ephemeral loopback port; the container entrypoint passes 0.0.0.0:$PORT.")
	unixPath := flag.String("unix", "", "Serve the AF_UNIX launcher transport on this socket path instead of stdio")
	logFlags := vgi.RegisterLoggingFlags(flag.CommandLine)
	_ = flag.CommandLine.Parse(filterKnownFlags(os.Args[1:], map[string]bool{
		"log-level":  true,
		"log-format": true,
		"log-logger": true,
		"unix":       true,
		"http-addr":  true,
	}))
	if err := logFlags.Apply(); err != nil {
		log.Fatalf("logging flags: %v", err)
	}

	sourceURL := "https://github.com/Query-farm/vgi-elasticsearch"
	w := vgi.NewWorker(
		vgi.WithCatalogName(esworker.CatalogName),
		vgi.WithCatalogComment("Query Elasticsearch/OpenSearch indices as SQL tables (PIT + search_after deep pagination via externalized scan state)"),
		vgi.WithCatalogTags(map[string]string{
			"source":       "vgi-elasticsearch",
			"vgi.title":    "Elasticsearch / OpenSearch Search Connector",
			"vgi.keywords": `["elasticsearch","opensearch","search","full-text search","index","query DSL","point in time","PIT","search_after","deep pagination","cursor","projection pushdown","predicate pushdown","api key","lucene","es_search"]`,
			"vgi.doc_llm":  "Query an Elasticsearch or OpenSearch index as a SQL table. The es_search table function opens a Point-In-Time and pages through every matching document with search_after, giving consistent, resumable deep pagination over millions of hits. It pushes down column projection (via _source filtering) and predicates (term/terms/range/exists via the query DSL), supports both the OpenSearch and Elasticsearch PIT dialects, basic-auth/API-key credentials, a raw query-DSL escape hatch, and explicit sort. Use it to read, filter, and join Elasticsearch/OpenSearch data from DuckDB SQL.",
			"vgi.doc_md":   "# Elasticsearch & OpenSearch for DuckDB\n\n![Elasticsearch logo](https://upload.wikimedia.org/wikipedia/commons/thumb/f/f4/Elasticsearch_logo.svg/250px-Elasticsearch_logo.svg.png)\n\nQuery [Elasticsearch](https://www.elastic.co) and [OpenSearch](https://opensearch.org) indices as live SQL tables over Apache Arrow — full-text search, the query DSL, and consistent deep pagination, straight from DuckDB SQL with no application code.\n\nThis VGI worker turns any Elasticsearch or OpenSearch cluster into queryable tables inside DuckDB. It is built for data engineers, analysts, and anyone who needs to read, filter, join, and export search-engine data without standing up a separate ETL pipeline or learning a client library. Point it at a cluster endpoint and an index (or an index pattern such as `logs-*`) and select the fields you want; the worker handles authentication, mapping introspection, pagination, and the translation of SQL filters into Elasticsearch query DSL for you. It works with both the commercial Elasticsearch distribution from Elastic and the Apache-2.0 licensed OpenSearch fork.\n\nUnder the hood the worker speaks the native Elasticsearch/OpenSearch REST API and JSON [query DSL](https://www.elastic.co/guide/en/elasticsearch/reference/current/query-dsl.html) directly over HTTP. Large result sets are streamed using a [Point-In-Time (PIT)](https://www.elastic.co/guide/en/elasticsearch/reference/current/point-in-time-api.html) snapshot combined with a `search_after` cursor, and that cursor is carried as externalized scan state — so a scan resumes cleanly across batch boundaries with no duplicated and no dropped documents, even across millions of hits. The `flavor` option selects the correct PIT dialect for OpenSearch versus Elasticsearch. See the [Elasticsearch documentation](https://www.elastic.co/docs) and the [OpenSearch documentation](https://opensearch.org/docs/latest/) for cluster-side details.\n\nReads support projection pushdown (only the requested fields are fetched via `_source` filtering), predicate pushdown (`term`, `terms`, `range`, and `exists` filters are compiled into the query DSL), a raw query-DSL escape hatch for anything more advanced, explicit `sort`, and basic-auth or API-key credentials. Every row carries the document `_id` and relevance `_score` alongside one typed column per source field. Typical use cases include exploring and aggregating log or product indices, joining search hits against local DuckDB tables or Parquet files, and exporting filtered subsets — for example projecting `name` and `price` from a `products` index, keeping only rows where `price` exceeds a threshold, and ordering by price. See the `es_search` function's examples for ready-to-run queries, and browse the `es_type_mapping` view to see how each Elasticsearch field type becomes a DuckDB column type.",
			// VGI152/VGI920 agent-check suite. es_search is a connector, but its
			// column shape resolves offline from a `fields` spec, so the tasks
			// exercise that cluster-free DESCRIBE surface (see AgentTestTasks).
			"vgi.agent_test_tasks":   esworker.AgentTestTasks,
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
				"vgi.title":   "Elasticsearch / OpenSearch — main",
				"vgi.doc_llm": "Elasticsearch/OpenSearch search functions. The es_search table function reads an index as a SQL table with consistent deep pagination (PIT + search_after), projection and predicate pushdown, sort, a raw query-DSL escape hatch, and basic-auth / API-key credentials.",
				"vgi.doc_md": "# elasticsearch.main\n\n" +
					"Elasticsearch and OpenSearch search over Apache Arrow. This schema turns a live cluster index into a SQL table you can read, filter, join, and export directly from DuckDB.\n\n" +
					"## Capabilities\n\n" +
					"- **Consistent deep pagination** using a Point-In-Time snapshot plus a `search_after` cursor carried as externalized scan state, so a scan resumes cleanly across batch boundaries with no duplicated or dropped documents.\n" +
					"- **Projection and predicate pushdown**: only requested fields are fetched via `_source` filtering, and `term`/`terms`/`range`/`exists` filters compile into the query DSL, with a raw query-DSL escape hatch for anything more advanced.\n" +
					"- **Both dialects and authentication**: the OpenSearch and Elasticsearch PIT dialects, plus basic-auth and API-key credentials.\n\n" +
					"## When to use it\n\n" +
					"Reach for this schema to explore or aggregate log and product indices, join search hits against local DuckDB tables or Parquet files, or export filtered subsets — without writing application code.",
				"vgi.keywords": `["elasticsearch","opensearch","es_search","search","full-text search","index","query DSL","point in time","PIT","search_after","deep pagination","projection pushdown","predicate pushdown","api key","lucene"]`,
				// VGI413 category registry: an ordered list of navigation sections;
				// each object carries a matching `vgi.category` naming one of these.
				"vgi.categories": `[
  {"name": "Search", "description": "Query an Elasticsearch/OpenSearch index as a SQL table with PIT + search_after deep pagination, projection and predicate pushdown, sort, a raw query-DSL escape hatch, and basic-auth / API-key credentials."},
  {"name": "Reference", "description": "Cluster-free reference data about the connector itself, such as the Elasticsearch/OpenSearch field-type to DuckDB column-type mapping es_search applies."}
]`,
				// VGI123 classifying tags use BARE keys (not vgi.-namespaced).
				"domain":   "search",
				"category": "full-text-search",
				"topic":    "elasticsearch-opensearch-pagination",
				// VGI515 described-example list: each example carries a human-readable
				// description alongside its SQL (a JSON [{description,sql}] array).
				"vgi.example_queries": `[{"description":"Bind the column schema for the products index from an explicit fields spec: the _id/_score meta columns plus one typed column per declared source field.","sql":"DESCRIBE SELECT _id, _score, name, price FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double');"},{"description":"Bind the schema for a projected, filtered, sorted read of the products index — name/price project down via _source filtering and price > 100 pushes into the query DSL.","sql":"DESCRIBE SELECT name, price FROM elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'name:keyword,price:double') WHERE price > 100 ORDER BY price DESC;"},{"description":"Bind the schema for an error-log scan over the logs-* index pattern on an Elasticsearch cluster, using API-key auth, the Elasticsearch PIT dialect, and a raw query-DSL escape hatch.","sql":"DESCRIBE SELECT level FROM elasticsearch.main.es_search('https://es.example.com', 'logs-*', fields => 'level:keyword', api_key => 'BASE64APIKEY', flavor => 'elasticsearch', query => '{\"term\":{\"level\":\"error\"}}');"}]`,
			},
		}),
	)
	esworker.Register(w)

	// Browsable, cluster-free discovery entry point: an agent can SELECT the
	// ES-type -> DuckDB-type reference before it knows the es_search arguments
	// (VGI146). Its rows are generated from esworker.TypeMappingRows.
	w.RegisterCatalogView("main", esworker.TypeMappingView())

	if *httpMode {
		if err := w.RunHttp(*httpAddr); err != nil {
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
