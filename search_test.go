package imdb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func catalogFromData(t testing.TB, data map[Dataset]string) *Catalog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "search.sqlite")
	if _, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, data)}); err != nil {
		t.Fatal(err)
	}
	c, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSubstringCandidates(t *testing.T) {
	data := fixtures()
	data[Basics] += "tt20\tmovie\t君の名は。\t君の名は。\t0\t2016\t\\N\t106\tAnimation\n" +
		"tt21\tmovie\tIt\tIt\t0\t2017\t\\N\t135\tHorror\n" +
		"tt22\tmovie\tLittle\tLittle\t0\t2019\t\\N\t109\tComedy\n" +
		"tt23\tmovie\t...\t...\t0\t2020\t\\N\t90\tDrama\n"
	c := catalogFromData(t, data)
	for _, tc := range []struct {
		q      Query
		id     string
		method Method
		source string
	}{
		{Query{Title: "matrix", Kind: Movie}, "tt1", Substring, "primary"},
		{Query{Title: "fabuleux", Kind: Movie}, "tt5", Substring, "original"},
		{Query{Title: "matrice", Kind: Movie}, "tt1", Substring, "alias"},
		{Query{Title: "AMEL", Kind: Movie}, "tt5", Prefix, "alias"},
		{Query{Title: "の名は", Kind: Movie}, "tt20", Substring, "primary"},
		{Query{Title: "名は", Kind: Movie}, "", "", ""},
		{Query{Title: "It", Kind: Movie}, "tt21", Exact, "primary"},
		{Query{Title: "...", Kind: Movie}, "tt23", Exact, "primary"},
		{Query{Title: "matrix", Kind: Show}, "", "", ""},
		{Query{Title: "matrix", Kind: Movie, Year: 2000}, "", "", ""},
	} {
		t.Run(tc.q.Title+string(tc.q.Kind)+tc.source, func(t *testing.T) {
			got, err := c.Candidates(context.Background(), tc.q, SearchOptions{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if tc.id == "" {
				if len(got.Matches) != 0 {
					t.Fatalf("unexpected matches: %+v", got)
				}
				return
			}
			if len(got.Matches) != 1 || got.Matches[0].Title.ID != tc.id || got.Matches[0].Method != tc.method || got.Matches[0].Name.Source != tc.source {
				t.Fatalf("unexpected match: %+v", got)
			}
			if got.Matches[0].Title.Runtime == nil || got.Matches[0].Title.Genres == nil {
				t.Fatalf("metadata not hydrated: %+v", got.Matches[0])
			}
			if tc.method == Substring {
				if _, err := c.Resolve(context.Background(), tc.q); !errors.Is(err, ErrNotFound) {
					t.Fatalf("substring accepted as identity: %v", err)
				}
			}
		})
	}
}

func TestCandidateRankingAndTypoFallback(t *testing.T) {
	data := fixtures()
	data[Basics] += "tt30\tmovie\tmatrix\tmatrix\t0\t2000\t\\N\t90\tDrama\n" +
		"tt31\tmovie\tMATRIX\tMATRIX\t0\t2000\t\\N\t90\tDrama\n" +
		"tt32\tmovie\tMatrix Reloaded\tMatrix Reloaded\t0\t2000\t\\N\t90\tDrama\n" +
		"tt33\tmovie\tmatrx\tmatrx\t0\t2000\t\\N\t90\tDrama\n"
	c := catalogFromData(t, data)
	q := Query{Title: "matrix", Kind: Movie}
	got, err := c.Candidates(context.Background(), q, SearchOptions{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"tt30", "tt31", "tt32", "tt1"}
	wantMethods := []Method{Exact, Normalized, Prefix, Substring}
	if len(got.Matches) != 4 || got.Truncated {
		t.Fatalf("direct results: %+v", got)
	}
	for i, m := range got.Matches {
		if m.Title.ID != wantIDs[i] || m.Method != wantMethods[i] {
			t.Fatalf("wrong ranking: %+v", got)
		}
	}
	for range 5 {
		again, err := c.Candidates(context.Background(), q, SearchOptions{Limit: 4})
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatalf("unstable results: %+v %v", again, err)
		}
	}
	all, err := c.Candidates(context.Background(), q, SearchOptions{})
	if err != nil || len(all.Matches) != 5 || all.Matches[4].Title.ID != "tt33" || all.Matches[4].Method != Fuzzy {
		t.Fatalf("missing typo fallback: %+v %v", all, err)
	}
}

func TestCandidateMatchedNameRanking(t *testing.T) {
	for _, tc := range []struct {
		name     string
		primary  string
		original string
		aliases  []string
		want     string
		method   Method
	}{
		{"prefix source", "Matrix A", "Matrix A", []string{"Matrix B", "Matrix CC"}, "Matrix B", Prefix},
		{"prefix raw title", "Unrelated", "Unrelated", []string{"Matrix á", "Matrix Z"}, "Matrix Z", Prefix},
		{"substring source", "X Matrix A", "X Matrix A", []string{"X Matrix B"}, "X Matrix B", Substring},
		{"substring raw title", "Unrelated", "Unrelated", []string{"X Matrix á", "X Matrix Z"}, "X Matrix Z", Substring},
		{"fuzzy source", "Matriy", "Matriy", []string{"Matriz"}, "Matriz", Fuzzy},
		{"fuzzy raw title", "Unrelated", "Unrelated", []string{"Matríy", "MatriZ"}, "MatriZ", Fuzzy},
		{"exact before source", "Matrix", "Matrix A", []string{"Matrix B"}, "Matrix", Exact},
		{"normalized before source", "MATRIX", "Matrix A", []string{"Matrix B"}, "MATRIX", Normalized},
		{"better score replaces ties", "Matrix AAA", "Matrix AAA", []string{"Matrix BBB", "Matrix Z"}, "Matrix Z", Prefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[Dataset]string{
				Basics:  headers[Basics] + fmt.Sprintf("\ntt1\tmovie\t%s\t%s\t0\t2000\t\\N\t90\tDrama\n", tc.primary, tc.original),
				Aliases: headers[Aliases] + "\n",
				Ratings: headers[Ratings] + "\n",
			}
			for i, title := range tc.aliases {
				data[Aliases] += fmt.Sprintf("tt1\t%d\t%s\tUS\ten\talternative\t\\N\t0\n", i+1, title)
			}
			c := catalogFromData(t, data)
			q := Query{Title: "Matrix", Kind: Movie}
			names, err := c.Names(context.Background(), "tt1")
			if err != nil {
				t.Fatal(err)
			}
			var best Match
			for _, name := range names {
				m := Match{Title: Title{ID: "tt1"}, Name: name}
				if scoreMatch(q, &m, nameKey(name.Title), fuzzyKey(name.Title), nameKey(q.Title), fuzzyKey(q.Title)) &&
					(best.Method == "" || compareMatches(m, best) < 0) {
					best = m
				}
			}
			if best.Name.Title != tc.want || best.Method != tc.method {
				t.Fatalf("invalid ranking fixture: %+v", best)
			}
			for _, limit := range []int{1, 20} {
				got, err := c.Candidates(context.Background(), q, SearchOptions{Limit: limit})
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Matches) != 1 || got.Truncated {
					t.Fatalf("limit %d: %+v", limit, got)
				}
				m := got.Matches[0]
				if m.Title.ID != "tt1" || m.Method != best.Method || m.Score != best.Score || !reflect.DeepEqual(m.Name, best.Name) {
					t.Fatalf("limit %d: matched name %+v, want %+v", limit, m, best)
				}
			}
		})
	}
}

func TestCompactSearchPreservesNamesAndBudget(t *testing.T) {
	c := catalogFromData(t, searchDataset(3, 500))
	var entries, postings int
	if err := c.db.QueryRow("SELECT count(*) FROM search_names").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := c.db.QueryRow("SELECT entries FROM gram_counts WHERE gram='the' AND kind='movie'").Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if entries != 3 || postings != 3 {
		t.Fatalf("raw aliases indexed repeatedly: entries=%d postings=%d", entries, postings)
	}
	names, err := c.Names(context.Background(), "tt1")
	if err != nil || len(names) != 501 || names[1].Region != "US" || names[1].Types[0] != "alternative" {
		t.Fatalf("lost alias metadata: count=%d err=%v", len(names), err)
	}
	got, err := c.Candidates(context.Background(), Query{Title: "The Synthetic Movie 0", Kind: Movie}, SearchOptions{Limit: 1, Budget: 1})
	if err != nil || len(got.Matches) != 1 || got.Matches[0].Title.ID != "tt1" || got.Truncated {
		t.Fatalf("duplicates consumed budget: %+v %v", got, err)
	}
	filtered, err := c.Candidates(context.Background(), Query{Title: "The", Kind: Movie, Year: 1900}, SearchOptions{Budget: 1})
	if err != nil || len(filtered.Matches) != 0 || !filtered.Truncated {
		t.Fatalf("filtering concealed clipping: %+v %v", filtered, err)
	}
}

func TestCandidateFiltersAndCancellation(t *testing.T) {
	_, c, _ := newCatalog(t)
	for _, tc := range []struct {
		q    Query
		want int
	}{
		{Query{Title: "film", Kind: Movie}, 2},
		{Query{Title: "film", Kind: Movie, ExcludeAdult: true}, 1},
		{Query{Title: "unknown", Kind: Show, ExcludeAdult: true}, 1},
		{Query{Title: "unknown", Kind: Show, Year: 2000}, 0},
		{Query{Title: "Dune"}, 3},
		{Query{Title: "Dune", Kind: Show}, 1},
	} {
		got, err := c.Candidates(context.Background(), tc.q, SearchOptions{})
		if err != nil || len(got.Matches) != tc.want {
			t.Fatalf("%+v: %+v %v", tc.q, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Candidates(ctx, Query{Title: "matrix", Kind: Movie}, SearchOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search: %v", err)
	}
}

func queryPlan(t testing.TB, c *Catalog, query string, args ...any) string {
	t.Helper()
	rows, err := c.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

func TestRepeatedAndMissingGrams(t *testing.T) {
	data := fixtures()
	data[Basics] += "tt40\tmovie\txaaaaay\txaaaaay\t0\t2000\t\\N\t90\tDrama\n"
	c := catalogFromData(t, data)
	got, err := c.Candidates(context.Background(), Query{Title: "aaa", Kind: Movie}, SearchOptions{})
	if err != nil || len(got.Matches) != 1 || got.Matches[0].Title.ID != "tt40" || got.Matches[0].Method != Substring || got.Truncated {
		t.Fatalf("repeated grams: %+v %v", got, err)
	}
	var count int
	if err := c.db.QueryRow("SELECT entries FROM gram_counts WHERE gram='aaa' AND kind='movie'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated gram counted more than once: %d %v", count, err)
	}
	missing, err := c.Candidates(context.Background(), Query{Title: "zzz", Kind: Movie}, SearchOptions{})
	if err != nil || len(missing.Matches) != 0 || missing.Truncated {
		t.Fatalf("missing gram: %+v %v", missing, err)
	}
}

func TestCandidateQueryPlans(t *testing.T) {
	_, c, _ := newCatalog(t)
	key, folded := nameKey("The Matrix"), fuzzyKey("The Matrix")
	for _, tc := range []struct {
		stage candidateQuery
		index string
	}{
		{exactCandidatesQuery(Query{Title: "The Matrix"}, key, folded, 10), "names_by_key"},
		{prefixCandidatesQuery(Query{Title: "The Matrix"}, key, folded, 10), "search_by_prefix"},
		{prefixCandidatesQuery(Query{Title: "The Matrix", Kind: Movie}, key, folded, 10), "search_by_kind_prefix"},
		{gramCandidatesQuery(Query{Title: "The Matrix", Kind: Movie}, key, folded, "rix", 10, false), "SEARCH search_grams USING PRIMARY KEY (gram=? AND kind=?)"},
		{gramCandidatesQuery(Query{Title: "The Matrix"}, key, folded, "rix", 10, true), "SEARCH search_grams USING PRIMARY KEY (gram=?)"},
	} {
		plan := queryPlan(t, c, tc.stage.query, tc.stage.args...)
		if !strings.Contains(plan, tc.index) || !strings.Contains(plan, "MATERIALIZE seeds") || strings.Contains(plan, "SCAN search_grams") || strings.Contains(plan, "GROUP BY") {
			t.Fatalf("unbounded or unindexed retrieval:\n%s", plan)
		}
	}
}
