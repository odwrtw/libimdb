package imdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolutionAndAmbiguity(t *testing.T) {
	_, c, info := newCatalog(t)
	resolved, err := c.Resolve(t.Context(), Query{Title: "The Matrix"})
	if err != nil || resolved.Generation != info.Generation || resolved.Method != Exact {
		t.Fatalf("resolution: %+v, %v", resolved, err)
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if _, exists := object["score"]; exists {
		t.Fatal("a resolution must not expose a candidate ranking score")
	}
	var title map[string]json.RawMessage
	if err := json.Unmarshal(object["title"], &title); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"start_year", "runtime_minutes", "title_type"} {
		if _, exists := title[field]; !exists {
			t.Fatalf("title missing %s: %s", field, data)
		}
	}
	if _, exists := title["year"]; exists {
		t.Fatal("obsolete year field")
	}

	resolved, err = c.Resolve(t.Context(), Query{Title: "Dune"})
	var ambiguous *AmbiguousError
	if !errors.Is(err, ErrAmbiguous) || !errors.As(err, &ambiguous) || !reflect.DeepEqual(resolved, Resolution{}) {
		t.Fatalf("ambiguity selected an identity: %+v, %v", resolved, err)
	}
	if ambiguous.Generation != info.Generation || ambiguous.Truncated || len(ambiguous.Titles) != 3 {
		t.Fatalf("conflicts: %+v", ambiguous)
	}
	for i, id := range []string{"tt2", "tt3", "tt4"} {
		if ambiguous.Titles[i].ID != id || ambiguous.Titles[i].PrimaryTitle != "Dune" || ambiguous.Titles[i].StartYear == nil {
			t.Fatalf("conflict %d: %+v", i, ambiguous.Titles[i])
		}
	}
	_, err = c.Resolve(t.Context(), Query{Title: "Dune", Kind: Movie, StartYear: 2021})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguityBound(t *testing.T) {
	for _, count := range []int{2, 20, 21} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			data := map[Dataset]string{
				Basics: headers[Basics] + "\n", Aliases: headers[Aliases] + "\n", Ratings: headers[Ratings] + "\n",
			}
			for i := count; i > 0; i-- { // Deliberately not import order.
				data[Basics] += fmt.Sprintf("tt%03d\tmovie\tCollision\tCollision\t0\t2000\t\\N\t90\tDrama\n", i)
				data[Aliases] += fmt.Sprintf("tt%03d\t1\tCOLLISION\tUS\ten\t\\N\t\\N\t0\n", i)
			}
			c := catalogFromData(t, data)
			_, err := c.Resolve(t.Context(), Query{Title: "Collision"})
			var ambiguous *AmbiguousError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("expected typed ambiguity, got %v", err)
			}
			if len(ambiguous.Titles) != min(count, 20) || ambiguous.Truncated != (count > 20) || ambiguous.Generation == "" {
				t.Fatalf("conflicts: %+v", ambiguous)
			}
			for i, title := range ambiguous.Titles {
				if title.ID != fmt.Sprintf("tt%03d", i+1) {
					t.Fatalf("conflicts not distinct and ID-ordered: %+v", ambiguous.Titles)
				}
			}
		})
	}
}

func TestValidationErrors(t *testing.T) {
	_, c, _ := newCatalog(t)
	ctx := t.Context()
	queryError := func(q Query) func() error {
		return func() error { _, err := c.Resolve(ctx, q); return err }
	}
	checks := []struct {
		name, field string
		call        func() error
	}{
		{"empty title", "Query.Title", queryError(Query{})},
		{"whitespace", "Query.Title", queryError(Query{Title: " \t\n"})},
		{"utf8", "Query.Title", queryError(Query{Title: "\xff"})},
		{"nul", "Query.Title", queryError(Query{Title: "private\x00title"})},
		{"bytes", "Query.Title", queryError(Query{Title: strings.Repeat("界", 400)})},
		{"code points", "Query.Title", queryError(Query{Title: strings.Repeat("x", 257)})},
		{"year", "Query.StartYear", queryError(Query{Title: "Film", StartYear: -1})},
		{"kind", "Query.Kind", queryError(Query{Title: "Film", Kind: "episode"})},
		{"id", "id", func() error { _, err := c.Lookup(ctx, "invalid"); return err }},
		{"names", "id", func() error { _, err := c.Names(ctx, "invalid"); return err }},
		{"batch id", "ids[1]", func() error { _, err := c.LookupMany(ctx, []string{"tt1", "bad"}); return err }},
		{"batch size", "ids", func() error { _, err := c.LookupMany(ctx, make([]string, 10001)); return err }},
		{"limit", "SearchOptions.Limit", func() error {
			_, err := c.Candidates(ctx, Query{Title: "Film"}, SearchOptions{Limit: -1})
			return err
		}},
		{"budget", "SearchOptions.Budget", func() error {
			_, err := c.Candidates(ctx, Query{Title: "Film"}, SearchOptions{Budget: 10001})
			return err
		}},
		{"view callback", "fn", func() error { return c.View(ctx, nil) }},
		{"open path", "path", func() error { _, err := Open(ctx, ""); return err }},
		{"update path", "path", func() error { _, err := Update(ctx, "\x00", UpdateOptions{}); return err }},
		{"attempts", "UpdateOptions.Attempts", func() error {
			_, err := Update(ctx, filepath.Join(t.TempDir(), "unused.sqlite"), UpdateOptions{Attempts: 6})
			return err
		}},
		{"retry delay", "UpdateOptions.RetryDelay", func() error {
			_, err := Update(ctx, filepath.Join(t.TempDir(), "unused.sqlite"), UpdateOptions{RetryDelay: -1})
			return err
		}},
		{"base URL", "HTTPSource.BaseURL", func() error {
			_, _, err := (HTTPSource{BaseURL: ":bad"}).Open(ctx, Basics)
			return err
		}},
		{"dataset", "dataset", func() error {
			_, _, err := (HTTPSource{}).Open(ctx, "bad")
			return err
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var invalid *ValidationError
			if !errors.Is(err, ErrInvalidInput) || !errors.As(err, &invalid) {
				t.Fatalf("expected validation error, got %v", err)
			}
			if invalid.Field != tc.field || invalid.Problem == "" || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("validation details: %+v", invalid)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("input leaked in error: %v", err)
			}
		})
	}
	// Context and identity outcomes are not validation errors.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Lookup(ctx, "tt1")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cancellation misclassified: %v", err)
	}
}

func TestCandidateClippingReasons(t *testing.T) {
	_, c, _ := newCatalog(t)
	for _, tc := range []struct {
		name          string
		q             Query
		opts          SearchOptions
		limit, budget bool
		count         int
	}{
		{"limit", Query{Title: "Dune"}, SearchOptions{Limit: 1}, true, false, 1},
		{"budget", Query{Title: "Dune"}, SearchOptions{Budget: 1}, false, true, 1},
		{"both", Query{Title: "Dune"}, SearchOptions{Limit: 1, Budget: 2}, true, true, 1},
		{"filtered", Query{Title: "Dune", StartYear: 1900}, SearchOptions{Budget: 1}, false, true, 0},
		{"complete", Query{Title: "Dune"}, SearchOptions{Limit: 3}, false, false, 3},
		{"no matches", Query{Title: "zzzzzzzz"}, SearchOptions{}, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Candidates(t.Context(), tc.q, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.LimitTruncated != tc.limit || got.BudgetExhausted != tc.budget || len(got.Matches) != tc.count || got.Matches == nil {
				t.Fatalf("unexpected clipping: %+v", got)
			}
		})
	}
}
