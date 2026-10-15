package imdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizedAliasCollision(t *testing.T) {
	data := fixtures()
	data[Aliases] += "tt2\t1\tTHE MATRIX\tUS\ten\t\\N\t\\N\t\\N\n"
	path := filepath.Join(t.TempDir(), "collision.sqlite")
	if _, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, data)}); err != nil {
		t.Fatal(err)
	}
	c, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Resolve(context.Background(), Query{Title: "The Matrix"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("primary name overrode alias collision: %v", err)
	}
	m, err := c.Resolve(context.Background(), Query{Title: "The Matrix", Year: 1999})
	if err != nil || m.Title.ID != "tt1" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestDuplicateRating(t *testing.T) {
	data := fixtures()
	data[Ratings] += "tt1\t9.0\t1\n"
	path := filepath.Join(t.TempDir(), "duplicate.sqlite")
	if _, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, data)}); err == nil {
		t.Fatal("duplicate rating accepted")
	}
}

func TestBusyWriter(t *testing.T) {
	path, c, old := newCatalog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := fixtureSource(t, fixtures())
	blocked, release := make(chan struct{}), make(chan struct{})
	source := SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
		if d == Ratings {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, SourceInfo{}, ctx.Err()
			}
		}
		return base.Open(ctx, d)
	})
	done := make(chan error, 1)
	go func() { _, err := Update(ctx, path, UpdateOptions{Source: source}); done <- err }()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := Update(ctx, path, UpdateOptions{Source: base}); err == nil {
		close(release)
		t.Fatal("competing writer succeeded")
	}
	info, err := c.Info(ctx)
	if err != nil || info.Generation != old.Generation {
		close(release)
		t.Fatalf("contended update changed catalog: %+v %v", info, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPPermanentFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	_, err := Update(context.Background(), filepath.Join(t.TempDir(), "http.sqlite"), UpdateOptions{
		Source: HTTPSource{BaseURL: server.URL, Client: server.Client()}, Attempts: 5, RetryDelay: time.Millisecond,
	})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 404 || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestHTTPInterruptedBody(t *testing.T) {
	path, c, old := newCatalog(t)
	files := map[Dataset][]byte{}
	for d, text := range fixtures() {
		files[d] = compressed(t, text)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := Dataset(strings.TrimPrefix(r.URL.Path, "/"))
		data := files[d]
		if d == Ratings {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)+100))
			data = data[:len(data)/2]
		}
		if _, err := w.Write(data); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	if _, err := Update(context.Background(), path, UpdateOptions{Source: HTTPSource{BaseURL: server.URL, Client: server.Client()}}); err == nil {
		t.Fatal("interrupted body committed")
	}
	info, err := c.Info(context.Background())
	if err != nil || info.Generation != old.Generation {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestHTTPInterruptedBodyRetry(t *testing.T) {
	path, c, old := newCatalog(t)
	data := fixtures()
	data[Basics] = strings.ReplaceAll(data[Basics], "The Matrix", "Updated Matrix")
	files := make(map[Dataset][]byte)
	for d, text := range data {
		files[d] = compressed(t, text)
	}
	prefix := headers[Aliases] + "\ntt1\t1\tLa"
	partial, complete := interruptedGzip(t, prefix, strings.TrimPrefix(data[Aliases], prefix))
	files[Aliases] = complete
	var mu sync.Mutex
	hits := make(map[Dataset]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := Dataset(strings.TrimPrefix(r.URL.Path, "/"))
		mu.Lock()
		hits[d]++
		attempt := hits[d]
		mu.Unlock()
		body := files[d]
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if d == Aliases && attempt == 1 {
			body = partial
		}
		if _, err := w.Write(body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	retries := 0
	info, err := Update(t.Context(), path, UpdateOptions{
		Source: HTTPSource{BaseURL: server.URL, Client: server.Client()}, Attempts: 2, RetryDelay: time.Millisecond,
		Progress: func(p Progress) {
			if p.Phase == Retrying {
				retries++
				if !errors.Is(p.Err, io.ErrUnexpectedEOF) {
					t.Errorf("lost HTTP transfer error: %v", p.Err)
				}
			}
		},
	})
	if err != nil || info.Generation == old.Generation || retries != 1 {
		t.Fatalf("HTTP recovery failed: generation=%s retries=%d err=%v", info.Generation, retries, err)
	}
	mu.Lock()
	if hits[Basics] != 2 || hits[Aliases] != 2 || hits[Ratings] != 1 {
		t.Errorf("HTTP retry did not restart the whole update: %v", hits)
	}
	mu.Unlock()
	current, err := c.Info(t.Context())
	if err != nil || current.Generation != info.Generation {
		t.Fatalf("recovered generation not visible: %+v %v", current, err)
	}
	title, err := c.Lookup(t.Context(), "tt1")
	if err != nil || title.PrimaryTitle != "Updated Matrix" || title.Rating == nil || *title.Rating != 8.7 {
		t.Fatalf("recovered facts: %+v %v", title, err)
	}
}

func TestOptionsAndSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unused.sqlite")
	for _, opts := range []UpdateOptions{{Attempts: -1}, {Attempts: 6}, {RetryDelay: -1}} {
		if _, err := Update(context.Background(), path, opts); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	for _, base := range []string{"file:///data", "https://example.org/?x=1", ":bad"} {
		if _, _, err := (HTTPSource{BaseURL: base}).Open(context.Background(), Basics); err == nil {
			t.Fatal("invalid base URL accepted")
		}
	}
	if _, _, err := (HTTPSource{}).Open(context.Background(), "bad"); err == nil {
		t.Fatal("unknown dataset accepted")
	}
	if _, err := Update(context.Background(), path, UpdateOptions{Source: SourceFunc(func(context.Context, Dataset) (io.ReadCloser, SourceInfo, error) {
		return nil, SourceInfo{}, nil
	})}); err == nil {
		t.Fatal("nil stream accepted")
	}
}

func FuzzNormalization(f *testing.F) {
	for _, s := range []string{"Amélie", "Ame\u0301lie", "  THE\tMATRIX ", "İstanbul", "Straße", "君の名は。", "..."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			t.Skip()
		}
		if !utf8.ValidString(s) {
			if err := validateQuery(Query{Title: s}); err != ErrInvalidQuery {
				t.Fatalf("invalid UTF-8 accepted: %v", err)
			}
			return
		}
		key := nameKey(s)
		if nameKey(key) != key {
			t.Fatalf("non-idempotent key for %q", s)
		}
		folded := fuzzyKey(s)
		if fuzzyKey(folded) != folded {
			t.Fatalf("non-idempotent fuzzy key for %q", s)
		}
	})
}

func BenchmarkCatalog(b *testing.B) {
	ctx := context.Background()
	var basics strings.Builder
	basics.WriteString(headers[Basics] + "\n")
	for i := range 1000 {
		fmt.Fprintf(&basics, "tt%d\tmovie\tSynthetic Movie %d\tSynthetic Movie %d\t0\t2000\t\\N\t90\tDrama\n", i+1, i, i)
	}
	source := fixtureSource(b, map[Dataset]string{Basics: basics.String(), Aliases: headers[Aliases] + "\n", Ratings: headers[Ratings] + "\n"})
	path := filepath.Join(b.TempDir(), "scale.sqlite")
	if _, err := Update(ctx, path, UpdateOptions{Source: source}); err != nil {
		b.Fatal(err)
	}
	c, err := Open(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	b.Run("resolve", func(b *testing.B) {
		for b.Loop() {
			if _, err := c.Resolve(ctx, Query{Title: "Synthetic Movie 500", Year: 2000}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("candidates", func(b *testing.B) {
		for b.Loop() {
			got, err := c.Candidates(ctx, Query{Title: "Synthetic Movi 500"}, SearchOptions{Budget: 100})
			if err != nil || len(got.Matches) == 0 {
				b.Fatalf("%+v %v", got, err)
			}
		}
	})
	b.Run("update", func(b *testing.B) {
		for b.Loop() {
			if _, err := Update(ctx, path, UpdateOptions{Source: source}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// A streaming reader is not required to implement io.ByteReader.
func TestChunkedReader(t *testing.T) {
	files := map[Dataset][]byte{}
	for d, text := range fixtures() {
		files[d] = compressed(t, text)
	}
	source := SourceFunc(func(_ context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
		return io.NopCloser(oneByteReader{bytes.NewReader(files[d])}), SourceInfo{}, nil
	})
	if _, err := Update(context.Background(), filepath.Join(t.TempDir(), "chunked.sqlite"), UpdateOptions{Source: source}); err != nil {
		t.Fatal(err)
	}
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}
