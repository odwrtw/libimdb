package imdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Search/update benchmarks use duplicate-heavy synthetic data. Real-catalog
// benchmarks are opt-in and never refresh or modify their input catalog.
func searchDataset(size, aliases int) map[Dataset]string {
	var basics, akas strings.Builder
	basics.WriteString(headers[Basics] + "\n")
	akas.WriteString(headers[Aliases] + "\n")
	for i := range size {
		fmt.Fprintf(&basics, "tt%d\tmovie\tThe Synthetic Movie %d\tThe Synthetic Movie %d\t0\t2000\t\\N\t90\tDrama\n", i+1, i, i)
		for j := range aliases {
			fmt.Fprintf(&akas, "tt%d\t%d\tThe Synthetic Movie %d\tUS\ten\talternative\t\\N\t0\n", i+1, j+1, i)
		}
	}
	return map[Dataset]string{Basics: basics.String(), Aliases: akas.String(), Ratings: headers[Ratings] + "\n"}
}

func BenchmarkSearch(b *testing.B) {
	path := filepath.Join(b.TempDir(), "search.sqlite")
	if _, err := Update(b.Context(), path, UpdateOptions{Source: fixtureSource(b, searchDataset(1000, 12))}); err != nil {
		b.Fatal(err)
	}
	benchmarkSearch(b, path, []Query{
		{Title: "The Synthetic Movie 500", Kind: Movie, Year: 2000},
		{Title: "Synthetic Movie 500", Kind: Movie},
		{Title: "The Synthetic Movi 500", Kind: Movie},
		{Title: "The", Kind: Movie},
	})
}

func BenchmarkRealCatalog(b *testing.B) {
	path := os.Getenv("LIBIMDB_BENCH_DB")
	if path == "" {
		b.Skip("set LIBIMDB_BENCH_DB to a catalog using the current schema")
	}
	benchmarkSearch(b, path, []Query{
		{Title: "The Matrix", Kind: Movie, Year: 1999},
		{Title: "matrix", Kind: Movie},
		{Title: "The Matrx", Kind: Movie},
		{Title: "breaking bad", Kind: Show},
		{Title: "the", Kind: Movie},
		{Title: "it", Kind: Movie},
	})
}

func benchmarkSearch(b *testing.B, path string, queries []Query) {
	c, err := Open(b.Context(), path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var version string
	if err := c.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		b.Fatal(err)
	}
	info, err := c.Info(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("SQLite %s; schema %d; generation %s; counts %+v", version, info.SchemaVersion, info.Generation, info.Counts)
	for _, q := range queries {
		b.Run("resolve/"+q.Title, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, err := c.Resolve(context.Background(), q)
				if err != nil && err != ErrNotFound && err != ErrAmbiguous {
					b.Fatal(err)
				}
			}
		})
		b.Run("candidates/"+q.Title, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := c.Candidates(context.Background(), q, SearchOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSearchUpdate(b *testing.B) {
	path := filepath.Join(b.TempDir(), "search.sqlite")
	source := fixtureSource(b, searchDataset(1000, 12))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Update(b.Context(), path, UpdateOptions{Source: source}); err != nil {
			b.Fatal(err)
		}
	}
}
