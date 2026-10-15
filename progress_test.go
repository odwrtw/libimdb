package imdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUpdateProgress(t *testing.T) {
	for _, mode := range []string{"known", "unknown", "chunked"} {
		t.Run(mode, func(t *testing.T) {
			data := fixtures()
			data[Ratings] = headers[Ratings] + "\n" // an empty optional dataset
			files := make(map[Dataset][]byte)
			for d, text := range data {
				files[d] = compressed(t, text)
			}
			closes := make(map[Dataset]int)
			source := SourceFunc(func(_ context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
				var reader io.Reader = bytes.NewReader(files[d])
				size := int64(len(files[d]))
				if mode == "unknown" {
					size = -1
				}
				if mode == "chunked" {
					reader = oneByteReader{reader}
				}
				return &closeReader{Reader: reader, close: func() error {
					closes[d]++
					return nil
				}}, SourceInfo{SizeBytes: size}, nil
			})
			var events []Progress
			path := filepath.Join(t.TempDir(), "progress.sqlite")
			info, err := Update(context.Background(), path, UpdateOptions{Source: source, Progress: func(p Progress) {
				events = append(events, p)
				if p.Phase == DatasetDone && closes[p.Dataset] != 1 {
					t.Errorf("completion before closure: %+v", p)
				}
				if p.Phase == Done {
					c, err := Open(context.Background(), path)
					if err != nil {
						t.Errorf("completion before commit: %v", err)
					} else if err := c.Close(); err != nil {
						t.Error(err)
					}
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			var phases []ProgressPhase
			completed := make(map[Dataset]Progress)
			var previous Progress
			for _, p := range events {
				if p.Attempt != 1 || p.Attempts != 1 || p.Err != nil {
					t.Fatalf("unexpected event: %+v", p)
				}
				if p.Phase != Importing {
					phases = append(phases, p.Phase)
				}
				if p.Dataset == "" && (p.Rows != 0 || p.BytesRead != 0 || p.TotalBytes != 0) {
					t.Fatalf("stale update-wide counters: %+v", p)
				}
				if p.Phase == Opening && (p.Rows != 0 || p.BytesRead != 0) {
					t.Fatalf("counters not reset: %+v", p)
				}
				if p.Dataset != "" && p.Dataset == previous.Dataset && (p.Rows < previous.Rows || p.BytesRead < previous.BytesRead) {
					t.Fatalf("nonmonotonic progress: %+v -> %+v", previous, p)
				}
				if p.Phase == DatasetDone {
					completed[p.Dataset] = p
				}
				previous = p
			}
			want := []ProgressPhase{Preparing, Opening, DatasetDone, Opening, DatasetDone, Opening, DatasetDone, Indexing, Validating, Committing, Done}
			if !reflect.DeepEqual(phases, want) {
				t.Fatalf("phases: %v, want %v", phases, want)
			}
			for _, s := range info.Sources {
				p, ok := completed[s.Dataset]
				rows := int64(strings.Count(data[s.Dataset], "\n") - 1)
				size := int64(len(files[s.Dataset]))
				total := size
				if mode == "unknown" {
					total = 0
				}
				hash := sha256.Sum256(files[s.Dataset])
				if !ok || p.Rows != rows || p.BytesRead != size || p.TotalBytes != total || s.SizeBytes != total || s.SHA256 != hex.EncodeToString(hash[:]) {
					t.Fatalf("wrong final counters/checksum: %+v, source %+v", p, s)
				}
			}
		})
	}
}

func TestProgressThrottling(t *testing.T) {
	now := time.Unix(1, 0)
	var events []Progress
	p := &progressReporter{callback: func(p Progress) { events = append(events, p) }, now: func() time.Time { return now }}
	p.opening(Basics)
	p.importing(100)
	for i := range 100 {
		p.rows(int64(i + 1))
		p.bytes(1)
	}
	if len(events) != 2 {
		t.Fatalf("unthrottled counters: %d", len(events))
	}
	now = now.Add(progressInterval)
	p.rows(101)
	if len(events) != 3 || events[2].Rows != 101 || events[2].BytesRead != 100 {
		t.Fatalf("missing counter snapshot: %+v", events)
	}
	p.current.Phase = DatasetDone
	p.emit(true)
	if len(events) != 4 || events[3].Phase != DatasetDone {
		t.Fatal("boundary was throttled")
	}
	p.callback = nil
	p.now = func() time.Time { t.Fatal("disabled reporter accessed clock"); return time.Time{} }
	p.rows(102)
	p.bytes(1)
	p.emit(true)
}

type readWithError struct{}

func (readWithError) Read(p []byte) (int, error) {
	return copy(p, "abc"), io.ErrUnexpectedEOF
}

func TestProgressReadWithError(t *testing.T) {
	p := &progressReporter{callback: func(Progress) {}, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := contextReader{ctx: ctx, reader: readWithError{}, progress: p}
	n, err := r.Read(make([]byte, 10))
	if n != 3 || !errors.Is(err, io.ErrUnexpectedEOF) || p.current.BytesRead != 3 {
		t.Fatalf("read: n=%d err=%v bytes=%d", n, err, p.current.BytesRead)
	}
	cancel()
	if n, err := r.Read(make([]byte, 10)); n != 0 || !errors.Is(err, context.Canceled) || p.current.BytesRead != 3 {
		t.Fatalf("cancelled read: n=%d err=%v bytes=%d", n, err, p.current.BytesRead)
	}
}

func TestHTTPProgressSize(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			files := make(map[Dataset][]byte)
			for d, text := range fixtures() {
				files[d] = compressed(t, text)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("transparent decompression allowed")
				}
				if !known {
					w.(http.Flusher).Flush() // force chunked transfer without a content length
				}
				if _, err := w.Write(files[Dataset(strings.TrimPrefix(r.URL.Path, "/"))]); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			_, err := Update(context.Background(), filepath.Join(t.TempDir(), "http.sqlite"), UpdateOptions{
				Source: HTTPSource{BaseURL: server.URL, Client: server.Client()}, Progress: func(p Progress) {
					if p.Phase == DatasetDone {
						size := int64(len(files[p.Dataset]))
						total := int64(0)
						if known {
							total = size
						}
						if p.BytesRead != size || p.TotalBytes != total {
							t.Errorf("HTTP progress: %+v", p)
						}
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgressRetry(t *testing.T) {
	path, c, old := newCatalog(t)
	base := fixtureSource(t, fixtures())
	calls := make(map[Dataset]int)
	var events []Progress
	source := SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
		calls[d]++
		if d == Aliases && calls[d] == 1 {
			return nil, SourceInfo{}, &HTTPError{StatusCode: 503}
		}
		return base.Open(ctx, d)
	})
	info, err := Update(context.Background(), path, UpdateOptions{Source: source, Attempts: 2, RetryDelay: time.Millisecond, Progress: func(p Progress) {
		events = append(events, p)
		if p.Phase == Retrying {
			var httpErr *HTTPError
			if !errors.As(p.Err, &httpErr) || p.Attempt != 1 || p.Attempts != 2 {
				t.Errorf("retry event: %+v", p)
			}
			current, err := c.Info(context.Background())
			if err != nil || current.Generation != old.Generation {
				t.Error("retry changed committed generation")
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls[Basics] != 2 || calls[Aliases] != 2 || calls[Ratings] != 1 || info.Generation == old.Generation {
		t.Fatalf("retry did not restart: %v", calls)
	}
	var preparing, retries int
	for _, p := range events {
		if p.Phase == Preparing {
			preparing++
			if p.Attempt != preparing || p.Dataset != "" || p.BytesRead != 0 || p.Rows != 0 {
				t.Fatalf("attempt not reset: %+v", p)
			}
		}
		if p.Phase == Retrying {
			retries++
		}
	}
	if preparing != 2 || retries != 1 || events[len(events)-1].Phase != Done || events[len(events)-1].Attempt != 2 {
		t.Fatalf("retry lifecycle: %+v", events)
	}
}

func TestProgressPartialTransferRetry(t *testing.T) {
	for _, token := range []string{"header", "record"} {
		for _, mode := range []string{"recover", "exhausted", "cancel"} {
			t.Run(token+"/"+mode, func(t *testing.T) {
				path, c, old := newCatalog(t)
				oldTitle, err := c.Lookup(t.Context(), "tt1")
				if err != nil {
					t.Fatal(err)
				}
				data := fixtures()
				data[Basics] = strings.ReplaceAll(data[Basics], "The Matrix", "Updated Matrix")
				prefix := "titleId\torder"
				if token == "record" {
					prefix = headers[Aliases] + "\ntt1\t1\tLa"
				}
				partial, complete := interruptedGzip(t, prefix, strings.TrimPrefix(data[Aliases], prefix))
				files := make(map[Dataset][]byte)
				for d, text := range data {
					files[d] = compressed(t, text)
				}
				opens, closes := make(map[Dataset]int), make(map[Dataset]int)
				source := SourceFunc(func(_ context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
					opens[d]++
					input := files[d]
					if d == Aliases {
						input = partial
						if mode == "recover" && opens[d] == 2 {
							input = complete
						}
					}
					closed := false
					return &closeReader{Reader: bytes.NewReader(input), close: func() error {
						if closed {
							t.Errorf("%s stream closed more than once", d)
						}
						closed = true
						closes[d]++
						return nil
					}}, SourceInfo{}, nil
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				delay := time.Millisecond
				if mode == "cancel" {
					delay = time.Hour // Cancellation must interrupt the retry wait.
				}
				var events []Progress
				info, err := Update(ctx, path, UpdateOptions{Source: source, Attempts: 2, RetryDelay: delay, Progress: func(p Progress) {
					events = append(events, p)
					if p.Phase == DatasetDone {
						if closes[p.Dataset] != opens[p.Dataset] {
							t.Errorf("completion before stream closure: %+v", p)
						}
						if p.Dataset == Aliases && (mode != "recover" || p.Attempt != 2) {
							t.Errorf("failed dataset reported completion: %+v", p)
						}
					}
					if p.Phase == Retrying {
						if !errors.Is(p.Err, io.ErrUnexpectedEOF) || p.Attempt != 1 {
							t.Errorf("lost retry error or wrong attempt: %+v", p)
						}
						current, readErr := c.Info(t.Context())
						if readErr != nil || !reflect.DeepEqual(current, old) {
							t.Errorf("retry changed committed metadata: %+v %v", current, readErr)
						}
						if mode == "cancel" {
							cancel()
						}
					}
				}})
				wantAttempts, wantRatings, wantPhase := 2, 0, Failed
				switch mode {
				case "recover":
					wantRatings, wantPhase = 1, Done
					if err != nil || info.Generation == old.Generation {
						t.Fatalf("recovery failed: %+v %v", info, err)
					}
				case "exhausted":
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("lost final transfer error: %v", err)
					}
				case "cancel":
					wantAttempts = 1
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("lost cancellation: %v", err)
					}
				}
				if opens[Basics] != wantAttempts || opens[Aliases] != wantAttempts || opens[Ratings] != wantRatings || !reflect.DeepEqual(opens, closes) {
					t.Fatalf("wrong restart/closure counts: opens=%v closes=%v", opens, closes)
				}
				phases := make(map[ProgressPhase]int)
				for _, p := range events {
					phases[p.Phase]++
				}
				last := events[len(events)-1]
				if phases[Preparing] != wantAttempts || phases[Retrying] != 1 || phases[Done]+phases[Failed] != 1 || last.Phase != wantPhase || last.Attempt != wantAttempts || !errors.Is(last.Err, err) {
					t.Fatalf("wrong retry lifecycle: %+v", events)
				}
				current, readErr := c.Info(t.Context())
				title, titleErr := c.Lookup(t.Context(), "tt1")
				if readErr != nil || titleErr != nil {
					t.Fatalf("read after update: %v %v", readErr, titleErr)
				}
				if mode == "recover" {
					if !reflect.DeepEqual(current, info) || title.PrimaryTitle != "Updated Matrix" {
						t.Fatalf("committed recovery not visible: %+v %+v", current, title)
					}
				} else if !reflect.DeepEqual(current, old) || !reflect.DeepEqual(title, oldTitle) {
					t.Fatalf("failed refresh changed committed catalog: %+v %+v", current, title)
				}
			})
		}
	}
}

func TestProgressFailures(t *testing.T) {
	for _, mode := range []string{"header", "checksum", "truncated", "close", "permanent", "import cancellation", "commit cancellation", "retry cancellation"} {
		t.Run(mode, func(t *testing.T) {
			path, c, old := newCatalog(t)
			files := make(map[Dataset][]byte)
			for d, text := range fixtures() {
				files[d] = compressed(t, text)
			}
			switch mode {
			case "header":
				files[Ratings] = compressed(t, "bad\n")
			case "checksum":
				files[Ratings][len(files[Ratings])-8] ^= 0xff
			case "truncated":
				files[Ratings] = files[Ratings][:len(files[Ratings])-5]
			}
			closes, opens := make(map[Dataset]int), make(map[Dataset]int)
			source := SourceFunc(func(_ context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
				opens[d]++
				if d == Ratings && (mode == "permanent" || mode == "retry cancellation") {
					status := 404
					if mode == "retry cancellation" {
						status = 503
					}
					return nil, SourceInfo{}, &HTTPError{StatusCode: status}
				}
				return &closeReader{Reader: bytes.NewReader(files[d]), close: func() error {
					closes[d]++
					if mode == "close" && d == Ratings {
						return errors.New("close failure")
					}
					return nil
				}}, SourceInfo{}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []Progress
			_, err := Update(ctx, path, UpdateOptions{Source: source, Attempts: 2, RetryDelay: time.Millisecond, Progress: func(p Progress) {
				events = append(events, p)
				if (mode == "import cancellation" && p.Phase == Importing) || (mode == "commit cancellation" && p.Phase == Committing) || (mode == "retry cancellation" && p.Phase == Retrying) {
					cancel()
				}
				if p.Phase == Failed {
					current, err := c.Info(context.Background())
					if err != nil || current.Generation != old.Generation {
						t.Error("failure changed committed generation")
					}
				}
			}})
			if err == nil || len(events) == 0 {
				t.Fatal("expected failure and events")
			}
			last := events[len(events)-1]
			if last.Phase != Failed || !errors.Is(last.Err, err) {
				t.Fatalf("missing final failure: %+v, returned %v", last, err)
			}
			if strings.Contains(mode, "cancellation") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			for _, p := range events {
				if p.Phase == Done || (p.Phase == DatasetDone && p.Dataset == Ratings && mode != "commit cancellation") {
					t.Fatalf("false completion: %+v", p)
				}
				if mode == "permanent" && p.Phase == Retrying {
					t.Fatal("permanent error retried")
				}
			}
			for d, count := range closes {
				if count != opens[d] {
					t.Fatalf("%s closed %d times, opened %d", d, count, opens[d])
				}
			}
			if mode == "retry cancellation" && opens[Basics] != 1 {
				t.Fatal("started another attempt after cancellation")
			}
		})
	}
}
