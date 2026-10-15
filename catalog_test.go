package imdb

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixtures() map[Dataset]string {
	return map[Dataset]string{
		Basics: headers[Basics] + "\n" +
			"tt1\tmovie\tThe Matrix\tThe Matrix\t0\t1999\t\\N\t136\tAction,Sci-Fi\n" +
			"tt2\tmovie\tDune\tDune\t0\t1984\t\\N\t137\tSci-Fi\n" +
			"tt3\tmovie\tDune\tDune\t0\t2021\t\\N\t155\tSci-Fi\n" +
			"tt4\ttvSeries\tDune\tDune\t0\t2000\t2000\t\\N\t\\N\n" +
			"tt5\tmovie\tAmélie\tLe Fabuleux Destin d'Amélie Poulain\t0\t2001\t\\N\t122\tComedy,Romance\n" +
			"tt6\ttvMiniSeries\tUnknown Year\tUnknown Year\t\\N\t\\N\t\\N\t\\N\t\\N\n" +
			"tt7\ttvMovie\tTelefilm\tTelefilm\t0\t2005\t\\N\t90\tDrama\n" +
			"tt8\ttvEpisode\tExcluded Episode\tExcluded Episode\t0\t2000\t\\N\t22\tDrama\n" +
			"tt9\tshort\tExcluded Short\tExcluded Short\t0\t2000\t\\N\t5\tDrama\n" +
			"tt10\tmovie\tDark\tDark\t0\t2015\t\\N\t90\tDrama\n" +
			"tt11\tmovie\tDark\tDark\t0\t2015\t\\N\t91\tDrama\n" +
			"tt12\tmovie\tAdult Film\tAdult Film\t1\t2001\t\\N\t90\tAdult\n",
		Aliases: headers[Aliases] + "\n" +
			"tt1\t1\tLa Matrice\tFR\tfr\timdbDisplay\t\\N\t0\n" +
			"tt1\t2\tLa Matrice\tCA\tfr\talternative\t\\N\t0\n" +
			"tt5\t1\tAmelie\tUS\ten\timdbDisplay\t\\N\t0\n" +
			"tt8\t1\tExcluded Alias\tUS\ten\t\\N\t\\N\t\\N\n",
		Ratings: headers[Ratings] + "\n" +
			"tt1\t8.7\t2100000\n" +
			"tt3\t8.0\t1000000\n" +
			"tt8\t9.1\t100\n",
	}
}

func compressed(t testing.TB, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := io.WriteString(gz, text); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixtureSource(t testing.TB, data map[Dataset]string) Source {
	t.Helper()
	files := make(map[Dataset][]byte)
	for d, text := range data {
		files[d] = compressed(t, text)
	}
	return SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
		if err := ctx.Err(); err != nil {
			return nil, SourceMetadata{}, err
		}
		b, ok := files[d]
		if !ok {
			return nil, SourceMetadata{}, fmt.Errorf("missing fixture %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), SourceMetadata{URL: "fixture:" + string(d)}, nil
	})
}

func newCatalog(t testing.TB) (string, *Catalog, Info) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog ?#é.sqlite")
	info, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, fixtures())})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return path, c, info
}

func TestCatalogFacts(t *testing.T) {
	_, c, info := newCatalog(t)
	if info.Counts != (Counts{Movies: 8, Shows: 2, Names: 14, Ratings: 2}) {
		t.Fatalf("counts: %+v", info.Counts)
	}
	if len(info.Sources) != 3 || info.Generation == "" || info.UpdatedAt.IsZero() {
		t.Fatalf("info: %+v", info)
	}
	for _, s := range info.Sources {
		hash := sha256.Sum256(compressed(t, fixtures()[s.Dataset]))
		if s.SHA256 != hex.EncodeToString(hash[:]) || s.Rows == 0 {
			t.Fatalf("source: %+v", s)
		}
	}
	m, err := c.Lookup(context.Background(), "tt1")
	if err != nil {
		t.Fatal(err)
	}
	if m.PrimaryTitle != "The Matrix" || *m.StartYear != 1999 || *m.RuntimeMinutes != 136 ||
		*m.Adult || *m.Rating != 8.7 || *m.Votes != 2100000 || strings.Join(m.Genres, ",") != "Action,Sci-Fi" {
		t.Fatalf("facts: %+v", m)
	}
	null, err := c.Lookup(context.Background(), "tt6")
	if err != nil {
		t.Fatal(err)
	}
	if null.StartYear != nil || null.EndYear != nil || null.RuntimeMinutes != nil || null.Adult != nil || null.Genres != nil || null.Rating != nil || null.Votes != nil {
		t.Fatalf("nulls: %+v", null)
	}
	for _, id := range []string{"tt8", "tt9", "tt999"} {
		if _, err := c.Lookup(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	names, err := c.Names(context.Background(), "tt1")
	if err != nil || len(names) != 3 {
		t.Fatalf("names: %+v %v", names, err)
	}
	if names[1].Language != "fr" || names[1].Region != "FR" || names[1].Types[0] != "imdbDisplay" || *names[1].IsOriginal {
		t.Fatalf("alias: %+v", names[1])
	}
	batch, err := c.LookupMany(context.Background(), []string{"tt1", "tt6", "tt8", "tt1"})
	if err != nil || len(batch) != 2 {
		t.Fatalf("batch: %+v %v", batch, err)
	}
}

func TestResolve(t *testing.T) {
	_, c, _ := newCatalog(t)
	tests := []struct {
		name   string
		q      Query
		id     string
		method MatchMethod
		err    error
	}{
		{"exact", Query{Title: "The Matrix"}, "tt1", Exact, nil},
		{"fold", Query{Title: "  THE\tMATRIX  ", Kind: Movie, StartYear: 1999}, "tt1", Normalized, nil},
		{"unicode", Query{Title: "AMÉLIE"}, "tt5", Normalized, nil},
		{"combining", Query{Title: "Ame\u0301lie"}, "tt5", Normalized, nil},
		{"original", Query{Title: "Le Fabuleux Destin d'Amélie Poulain"}, "tt5", Exact, nil},
		{"alias duplicates", Query{Title: "La Matrice"}, "tt1", Exact, nil},
		{"remake", Query{Title: "Dune", Kind: Movie, StartYear: 2021}, "tt3", Exact, nil},
		{"series", Query{Title: "Dune", Kind: Show}, "tt4", Exact, nil},
		{"unknown year", Query{Title: "Unknown Year"}, "tt6", Exact, nil},
		{"remakes", Query{Title: "Dune", Kind: Movie}, "", "", ErrAmbiguous},
		{"all kinds", Query{Title: "Dune"}, "", "", ErrAmbiguous},
		{"same year", Query{Title: "Dark", Kind: Movie, StartYear: 2015}, "", "", ErrAmbiguous},
		{"year constraint", Query{Title: "The Matrix", StartYear: 2000}, "", "", ErrNotFound},
		{"null year constraint", Query{Title: "Unknown Year", StartYear: 2000}, "", "", ErrNotFound},
		{"typo", Query{Title: "The Matrx"}, "", "", ErrNotFound},
		{"prefix", Query{Title: "The Mat"}, "", "", ErrNotFound},
		{"adult excluded", Query{Title: "Adult Film", ExcludeAdult: true}, "", "", ErrNotFound},
		{"adult included", Query{Title: "Adult Film"}, "tt12", Exact, nil},
		{"invalid", Query{Title: ""}, "", "", ErrInvalidInput},
		{"bad kind", Query{Title: "Dune", Kind: "episode"}, "", "", ErrInvalidInput},
		{"bad year", Query{Title: "Dune", StartYear: -1}, "", "", ErrInvalidInput},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Resolve(context.Background(), tc.q)
			if !errors.Is(err, tc.err) || got.Title.ID != tc.id || got.Method != tc.method {
				t.Fatalf("got %+v, %v; want %s %s %v", got, err, tc.id, tc.method, tc.err)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	_, c, info := newCatalog(t)
	for _, tc := range []struct {
		key, id string
		method  MatchMethod
	}{
		{"The Matrix", "tt1", Exact}, {"THE MATRIX", "tt1", Normalized},
		{"The Mat", "tt1", Prefix}, {"The Matrx", "tt1", Fuzzy},
		{"Le Fabuleux Destin d Amelie Poulain", "tt5", Fuzzy},
	} {
		t.Run(tc.key, func(t *testing.T) {
			got, err := c.Candidates(context.Background(), Query{Title: tc.key}, SearchOptions{})
			if err != nil || len(got.Matches) == 0 {
				t.Fatalf("%+v %v", got, err)
			}
			m := got.Matches[0]
			if m.Title.ID != tc.id || m.Method != tc.method || m.Score == 0 || got.Generation != info.Generation {
				t.Fatalf("%+v", got)
			}
		})
	}
	got, err := c.Candidates(context.Background(), Query{Title: "La Matrice"}, SearchOptions{})
	if err != nil || len(got.Matches) != 1 {
		t.Fatalf("dedup: %+v %v", got, err)
	}
	got, err = c.Candidates(context.Background(), Query{Title: "Dune", Kind: Movie}, SearchOptions{Limit: 1})
	if err != nil || len(got.Matches) != 1 || !got.LimitTruncated || got.BudgetExhausted {
		t.Fatalf("limit: %+v %v", got, err)
	}
	if _, err := c.Resolve(context.Background(), Query{Title: "Dune", Kind: Movie}); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	got, err = c.Candidates(context.Background(), Query{Title: "Dune"}, SearchOptions{Budget: 1})
	if err != nil || !got.BudgetExhausted || got.LimitTruncated {
		t.Fatalf("budget: %+v %v", got, err)
	}
	got, err = c.Candidates(context.Background(), Query{Title: "The Matrx", StartYear: 2000}, SearchOptions{})
	if err != nil || len(got.Matches) != 0 {
		t.Fatalf("year: %+v %v", got, err)
	}
}

func TestRefreshRemovesOldFacts(t *testing.T) {
	path, c, old := newCatalog(t)
	data := fixtures()
	data[Basics] = headers[Basics] + "\r\n" + "tt1\tmovie\tNew Name\tNew Name\t0\t2000\t\\N\t\\N\t\\N"
	data[Aliases] = headers[Aliases] + "\n"
	data[Ratings] = headers[Ratings] + "\n"
	info, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, data)})
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation == old.Generation {
		t.Fatal("unchanged generation")
	}
	m, err := c.Lookup(context.Background(), "tt1")
	if err != nil || m.PrimaryTitle != "New Name" || m.Rating != nil || m.Votes != nil {
		t.Fatalf("%+v %v", m, err)
	}
	for _, key := range []string{"The Matrix", "La Matrice", "Dune"} {
		if _, err := c.Resolve(context.Background(), Query{Title: key}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", key, err)
		}
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name() != filepath.Base(path) && f.Name() != filepath.Base(path)+"-wal" && f.Name() != filepath.Base(path)+"-shm" {
			t.Fatalf("unexpected file: %s", f.Name())
		}
	}
}

func TestFailedUpdateRollsBack(t *testing.T) {
	path, c, old := newCatalog(t)
	tests := map[string]func(map[Dataset]string){
		"header":       func(d map[Dataset]string) { d[Basics] = "bad\n" },
		"duplicate":    func(d map[Dataset]string) { d[Basics] += strings.Split(d[Basics], "\n")[1] + "\n" },
		"year":         func(d map[Dataset]string) { d[Basics] = strings.Replace(d[Basics], "1999", "invalid", 1) },
		"alias":        func(d map[Dataset]string) { d[Aliases] += "tt1\t1\tInvalid\tFR\tfr\t\\N\t\\N\t2\n" },
		"last dataset": func(d map[Dataset]string) { d[Ratings] += "tt1\tNaN\t1\n" },
		"fields":       func(d map[Dataset]string) { d[Ratings] += "tt1\t9.0\n" },
		"empty":        func(d map[Dataset]string) { d[Basics] = headers[Basics] + "\n" },
		"invalid utf8": func(d map[Dataset]string) { d[Basics] += "tt100\tmovie\t\xff\tTitle\t0\t2000\t\\N\t90\tDrama\n" },
		"oversized":    func(d map[Dataset]string) { d[Basics] += strings.Repeat("a", maxRowBytes+1) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d := maps.Clone(fixtures())
			mutate(d)
			if _, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, d)}); err == nil {
				t.Fatal("update succeeded")
			}
			info, err := c.Info(context.Background())
			if err != nil || info.Generation != old.Generation {
				t.Fatalf("old generation lost: %+v %v", info, err)
			}
			if _, err := c.Resolve(context.Background(), Query{Title: "The Matrix"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGzipFailureAndClose(t *testing.T) {
	path, c, old := newCatalog(t)
	for _, mode := range []string{"checksum", "truncated", "close"} {
		t.Run(mode, func(t *testing.T) {
			files := map[Dataset][]byte{}
			for d, text := range fixtures() {
				files[d] = compressed(t, text)
			}
			switch mode {
			case "checksum":
				files[Ratings][len(files[Ratings])-8] ^= 0xff
			case "truncated":
				files[Ratings] = files[Ratings][:len(files[Ratings])-5]
			}
			closes := make(map[Dataset]int)
			source := SourceFunc(func(_ context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
				return &closeReader{Reader: bytes.NewReader(files[d]), close: func() error {
					closes[d]++
					if mode == "close" && d == Ratings {
						return errors.New("close failure")
					}
					return nil
				}}, SourceMetadata{}, nil
			})
			if _, err := Update(context.Background(), path, UpdateOptions{Source: source}); err == nil {
				t.Fatal("unexpected success")
			}
			for d, n := range closes {
				if n != 1 {
					t.Fatalf("%s closed %d times", d, n)
				}
			}
			info, err := c.Info(context.Background())
			if err != nil || info.Generation != old.Generation {
				t.Fatalf("%+v %v", info, err)
			}
		})
	}
}

type closeReader struct {
	io.Reader
	close func() error
}

func (r *closeReader) Close() error { return r.close() }

func TestSchemaAndMissingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	if _, err := Open(ctx, missing); err == nil {
		t.Fatal("missing opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("reader created file: %v", err)
	}
	for _, table := range []string{"user_data", "sqliteXdata"} {
		t.Run(table, func(t *testing.T) {
			foreign := filepath.Join(dir, table+".sqlite")
			db, err := openDB(ctx, foreign, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("CREATE TABLE " + table + "(value TEXT); INSERT INTO " + table + " VALUES('keep')"); err != nil {
				t.Fatal(err)
			}
			if _, err := Update(ctx, foreign, UpdateOptions{Source: fixtureSource(t, fixtures())}); !errors.Is(err, ErrSchema) {
				t.Fatalf("foreign: %v", err)
			}
			var value string
			if err := db.QueryRow("SELECT value FROM " + table).Scan(&value); err != nil || value != "keep" {
				t.Fatalf("%s %v", value, err)
			}
			var objects int
			if err := db.QueryRow("SELECT count(*) FROM sqlite_schema").Scan(&objects); err != nil || objects != 1 {
				t.Fatalf("foreign schema changed: objects=%d err=%v", objects, err)
			}
		})
	}
	empty := filepath.Join(dir, "empty.sqlite")
	bad := fixtures()
	bad[Ratings] = "bad"
	if _, err := Update(ctx, empty, UpdateOptions{Source: fixtureSource(t, bad)}); err == nil {
		t.Fatal("bad create succeeded")
	}
	if _, err := Open(ctx, empty); !errors.Is(err, ErrSchema) {
		t.Fatalf("uninitialized: %v", err)
	}
	if _, err := Update(ctx, empty, UpdateOptions{Source: fixtureSource(t, fixtures())}); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadersDuringUpdate(t *testing.T) {
	path, c, old := newCatalog(t)
	base := fixtureSource(t, fixtures())
	atRatings, release := make(chan struct{}), make(chan struct{})
	source := SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
		if d == Ratings {
			close(atRatings)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, SourceMetadata{}, ctx.Err()
			}
		}
		return base.Open(ctx, d)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Update(ctx, path, UpdateOptions{Source: source}); done <- err }()
	select {
	case <-atRatings:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	info, err := c.Info(ctx)
	if err != nil || info.Generation != old.Generation {
		close(release)
		t.Fatalf("read uncommitted: %+v %v", info, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	info, err = c.Info(ctx)
	if err != nil || info.Generation == old.Generation {
		t.Fatalf("did not adopt commit: %+v %v", info, err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := c.Resolve(ctx, Query{Title: "The Matrix"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestCancellation(t *testing.T) {
	path, c, old := newCatalog(t)
	ctx, cancel := context.WithCancel(context.Background())
	base := fixtureSource(t, fixtures())
	source := SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
		if d == Ratings {
			cancel()
		}
		return base.Open(ctx, d)
	})
	if _, err := Update(ctx, path, UpdateOptions{Source: source}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil || info.Generation != old.Generation {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := c.Lookup(ctx, "tt1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHTTPAndRetry(t *testing.T) {
	files := map[Dataset][]byte{}
	for d, text := range fixtures() {
		files[d] = compressed(t, text)
	}
	hits := map[Dataset]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := Dataset(strings.TrimPrefix(r.URL.Path, "/"))
		hits[d]++
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("transparent gzip allowed")
		}
		if d == Aliases && hits[d] == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", "fixture")
		w.Header().Set("Last-Modified", "Tue, 13 Oct 2026 00:00:00 GMT")
		if _, err := w.Write(files[d]); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "http.sqlite")
	info, err := Update(context.Background(), path, UpdateOptions{
		Source: HTTPSource{BaseURL: server.URL, Client: server.Client()}, Attempts: 2, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits[Basics] != 2 || hits[Aliases] != 2 || hits[Ratings] != 1 {
		t.Fatalf("did not restart: %v", hits)
	}
	for _, s := range info.Sources {
		if s.ETag != "fixture" || !strings.HasPrefix(s.URL, server.URL) {
			t.Fatalf("%+v", s)
		}
	}
}

func TestReadOnlyAndUnknownVersion(t *testing.T) {
	path, c, _ := newCatalog(t)
	if _, err := c.db.Exec("DELETE FROM titles"); err == nil {
		t.Fatal("reader writable")
	}
	db, err := openDB(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup(context.Background(), "tt1"); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, fixtures())}); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
}

func TestSQLInjectionAndLimits(t *testing.T) {
	_, c, _ := newCatalog(t)
	for _, key := range []string{"' OR 1=1 --", "%", "_", "*; DELETE FROM titles"} {
		if _, err := c.Resolve(context.Background(), Query{Title: key}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: %v", key, err)
		}
		if _, err := c.Candidates(context.Background(), Query{Title: key}, SearchOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []Query{{Title: "\xff"}, {Title: "a\x00b"}, {Title: strings.Repeat("x", 257)}} {
		if _, err := c.Resolve(context.Background(), q); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	if _, err := c.Candidates(context.Background(), Query{Title: "Dune"}, SearchOptions{Budget: 10001}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := c.Lookup(context.Background(), "invalid"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := c.LookupMany(context.Background(), []string{"bad"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := c.Names(context.Background(), "tt999"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestResolveUsesNameIndex(t *testing.T) {
	_, c, _ := newCatalog(t)
	query, args := resolveIDsQuery(Query{Title: "Dune", Kind: Movie, StartYear: 2021, ExcludeAdult: true})
	rows, err := c.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "names_by_key")
		if strings.Contains(detail, "DISTINCT") || strings.Contains(detail, "SCAN n") {
			t.Fatalf("resolver should stream the covering name index: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("name lookup not indexed")
	}
}
