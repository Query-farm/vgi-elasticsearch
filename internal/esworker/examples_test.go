// Copyright 2026 Query Farm LLC - https://query.farm

package esworker

import (
	"encoding/json"
	"testing"
)

// The composed examples must reach the function's native examples within the
// first five (all the vgi-lint agent simulator shows) and its described list.
func TestComposedExamplesPlacement(t *testing.T) {
	md := (&SearchFunction{}).Metadata()
	first := md.Examples
	if len(first) > 5 {
		first = first[:5]
	}
	var described []describedExample
	if err := json.Unmarshal([]byte(md.Tags["vgi.example_queries"]), &described); err != nil {
		t.Fatalf("function vgi.example_queries is not valid JSON: %v", err)
	}
	for _, want := range ComposedExamples {
		inNative := false
		for _, ex := range first {
			inNative = inNative || ex.SQL == want.SQL
		}
		if !inNative {
			t.Errorf("composed example not in the first 5 native examples: %s", want.SQL)
		}
		inDescribed := false
		for _, ex := range described {
			inDescribed = inDescribed || (ex.SQL == want.SQL && ex.Description == want.Description)
		}
		if !inDescribed {
			t.Errorf("composed example missing from vgi.example_queries: %s", want.SQL)
		}
	}
}

func TestWithComposedExamplesKeepsBase(t *testing.T) {
	out := WithComposedExamples(`[{"description":"d","sql":"SELECT 'a<b' AS x"}]`)
	var got []describedExample
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1+len(ComposedExamples) || got[0].SQL != "SELECT 'a<b' AS x" {
		t.Fatalf("unexpected list: %s", out)
	}
}
