package imdb

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedFuzzyRetrievalAtScale(t *testing.T) {
	var basics strings.Builder
	basics.WriteString(headers[Basics] + "\n")
	for i := range 1000 {
		fmt.Fprintf(&basics, "tt%d\tmovie\tSynthetic Movie %d\tSynthetic Movie %d\t0\t2000\t\\N\t90\tDrama\n", i+1, i, i)
	}
	path := filepath.Join(t.TempDir(), "scale.sqlite")
	ctx := context.Background()
	source := fixtureSource(t, map[Dataset]string{Basics: basics.String(), Aliases: headers[Aliases] + "\n", Ratings: headers[Ratings] + "\n"})
	if _, err := Update(ctx, path, UpdateOptions{Source: source}); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	got, err := c.Candidates(ctx, Query{Title: "Synthetic Movi 500"}, SearchOptions{Budget: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got.Matches {
		if m.Title.ID == "tt501" {
			found = true
		}
	}
	if !found || !got.Truncated {
		t.Fatalf("target not retained in clipped retrieval: %+v", got)
	}
	if _, err := c.Resolve(ctx, Query{Title: "Synthetic Movi 500"}); err != ErrNotFound {
		t.Fatalf("fuzzy identity accepted: %v", err)
	}
}
