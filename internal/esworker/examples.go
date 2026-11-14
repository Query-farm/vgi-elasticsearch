// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Query-farm/vgi-go/vgi"
)

// Composed examples: queries that combine two es_search scans in one statement.
// The Elasticsearch query DSL has no joins, so joining one index to another is
// exactly what running es_search inside DuckDB adds. Each SQL is defined once
// here and fed to the function's Examples, the function's vgi.example_queries
// and the schema-level vgi.example_queries (see WithComposedExamples).
//
// Like every other es_search example, the published form is DESCRIBE <query>:
// with explicit `fields` specs it binds with no cluster, so catalog tooling
// (vgi-lint --execute) can run it anywhere; the query itself needs a cluster.
//
// The es_type_mapping view is not composed with es_search: es_search's output
// carries DuckDB column types, not the source ES types, so the two share no
// join key, and es_search cannot take a subquery-derived fields spec.
const (
	// ExampleCrossIndexRevenueSQL joins an orders index to a products index.
	ExampleCrossIndexRevenueSQL = "SELECT p.name, count(*) AS shipped_orders, sum(o.qty) AS units, sum(o.qty * p.price) AS revenue " +
		"FROM elasticsearch.main.es_search('http://localhost:9200', 'orders', fields => 'product_id:keyword,qty:long,status:keyword') AS o " +
		"JOIN elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'product_id:keyword,name:keyword,price:double') AS p " +
		"ON o.product_id = p.product_id WHERE o.status = 'shipped' GROUP BY p.name ORDER BY revenue DESC, p.name;"

	// ExampleOrphanOrdersSQL anti-joins an orders index against a products index.
	ExampleOrphanOrdersSQL = "SELECT o._id AS order_id, o.product_id, o.qty " +
		"FROM elasticsearch.main.es_search('http://localhost:9200', 'orders', fields => 'product_id:keyword,qty:long') AS o " +
		"ANTI JOIN elasticsearch.main.es_search('http://localhost:9200', 'products', fields => 'product_id:keyword') AS p " +
		"ON o.product_id = p.product_id ORDER BY o._id;"
)

// ComposedExamples are the es_search examples that combine two index scans.
var ComposedExamples = []vgi.CatalogExample{
	{
		SQL:         "DESCRIBE " + ExampleCrossIndexRevenueSQL,
		Description: "Bind the result schema (name, shipped_orders, units, revenue) of a join between two indices, which the Elasticsearch query DSL cannot do. Run without DESCRIBE, it returns one row per product that has shipped orders: its count of shipped orders, units shipped and revenue (sum of qty * price), from an `orders` index joined to a `products` index on product_id, highest revenue first. The `status = 'shipped'` filter is pushed into the orders search as a term query. The explicit `fields` specs let the schema resolve without a live cluster.",
	},
	{
		SQL:         "DESCRIBE " + ExampleOrphanOrdersSQL,
		Description: "Bind the result schema (order_id, product_id, qty) of a cross-index data-quality check. Run without DESCRIBE, it lists the orders whose product_id matches no document in the `products` index, using an anti-join between two es_search scans. The explicit `fields` specs let the schema resolve without a live cluster.",
	},
}

// describedExample is the {description, sql} shape of a vgi.example_queries
// entry.
type describedExample struct {
	Description string `json:"description"`
	SQL         string `json:"sql"`
}

// WithComposedExamples appends ComposedExamples to a vgi.example_queries JSON
// list, so the composed SQL is written once and every list stays in step. It
// panics on a malformed base list: the bases are compile-time constants, and a
// broken one must fail at worker start, not ship silently.
func WithComposedExamples(base string) string {
	var list []describedExample
	if err := json.Unmarshal([]byte(base), &list); err != nil {
		panic(fmt.Sprintf("esworker: malformed vgi.example_queries list: %v", err))
	}
	for _, ex := range ComposedExamples {
		list = append(list, describedExample{Description: ex.Description, SQL: ex.SQL})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(list); err != nil {
		panic(fmt.Sprintf("esworker: encode vgi.example_queries: %v", err))
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}
