// Copyright 2026 Query Farm LLC - https://query.farm

// Command vgi-elasticsearch-worker is a VGI worker that queries an
// Elasticsearch/OpenSearch index as a SQL table, using Point-In-Time +
// search_after for consistent, resumable deep pagination. It speaks the VGI
// protocol over stdio (or HTTP with --http).
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
	httpMode := flag.Bool("http", false, "Run as an HTTP server instead of stdio")
	logFlags := vgi.RegisterLoggingFlags(flag.CommandLine)
	_ = flag.CommandLine.Parse(filterKnownFlags(os.Args[1:], map[string]bool{
		"log-level":  true,
		"log-format": true,
		"log-logger": true,
	}))
	if err := logFlags.Apply(); err != nil {
		log.Fatalf("logging flags: %v", err)
	}

	w := vgi.NewWorker(
		vgi.WithCatalogName(esworker.CatalogName),
		vgi.WithCatalogComment("Query Elasticsearch/OpenSearch indices as SQL tables (PIT + search_after deep pagination via externalized scan state)"),
		vgi.WithCatalogTags(map[string]string{
			"source": "vgi-elasticsearch",
		}),
	)
	esworker.Register(w)

	if *httpMode {
		if err := w.RunHttp("127.0.0.1:0"); err != nil {
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
