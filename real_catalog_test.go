package imdb

import (
	"context"
	"os"
	"testing"
	"time"
)

// Optional real-data quality check. It opens but never refreshes the catalog.
func TestRealCatalogSearch(t *testing.T) {
	path := os.Getenv("LIBIMDB_BENCH_DB")
	if path == "" {
		t.Skip("set LIBIMDB_BENCH_DB to a full IMDb catalog using the current schema")
	}
	c, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, tc := range []struct {
		q  Query
		id string
	}{
		{Query{Title: "matrix", Kind: Movie}, "tt0133093"},
		{Query{Title: "The Matrx", Kind: Movie}, "tt0133093"},
		{Query{Title: "breaking bad", Kind: Show}, "tt0903747"},
		{Query{Title: "king bad", Kind: Show}, "tt0903747"},
		{Query{Title: "it", Kind: Movie}, "tt1396484"},
	} {
		t.Run(tc.q.Title, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := c.Candidates(ctx, tc.q, SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range got.Matches {
				if m.Title.ID == tc.id {
					found = true
				}
				if m.Title.Kind != tc.q.Kind {
					t.Fatalf("wrong kind: %+v", m)
				}
			}
			if !found {
				t.Fatalf("known title %s absent: %+v", tc.id, got)
			}
		})
	}
	match, err := c.Resolve(t.Context(), Query{Title: "The Matrix", Kind: Movie, StartYear: 1999})
	if err != nil || match.Title.ID != "tt0133093" {
		t.Fatalf("safe real-data resolution: %+v %v", match, err)
	}
}
