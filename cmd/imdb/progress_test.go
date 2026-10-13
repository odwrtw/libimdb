package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	imdb "github.com/odwrtw/libimdb"
)

func TestProgressModes(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		terminal bool
		enabled  bool
	}{
		{"auto", false, false}, {"auto", true, true},
		{"always", false, true}, {"always", true, true},
		{"never", false, false}, {"never", true, false},
	} {
		t.Run(fmt.Sprintf("%s/terminal=%v", tc.mode, tc.terminal), func(t *testing.T) {
			var out bytes.Buffer
			d := newProgressDisplay(&out, tc.mode, tc.terminal)
			if (d != nil) != tc.enabled {
				t.Fatalf("display: %+v", d)
			}
			if d != nil {
				d.report(imdb.Progress{Phase: imdb.Preparing, Attempt: 1, Attempts: 2})
				d.finish()
				if !strings.HasSuffix(out.String(), "\n") || strings.Contains(out.String(), "\x1b") != tc.terminal {
					t.Fatalf("wrong display mode: %q", out.String())
				}
			} else {
				d.finish() // disabled display cleanup is safe
			}
		})
	}
	if terminalWriter(&bytes.Buffer{}) {
		t.Fatal("buffer detected as terminal")
	}
}

func TestFormatProgress(t *testing.T) {
	for _, tc := range []struct {
		p    imdb.Progress
		want string
	}{
		{imdb.Progress{Phase: imdb.Opening, Dataset: imdb.Basics}, "opening title.basics.tsv.gz"},
		{imdb.Progress{Phase: imdb.Importing, Dataset: imdb.Basics, BytesRead: 1024, TotalBytes: 2048, Rows: 30}, "importing title.basics.tsv.gz 1.0KiB/2.0KiB 50% 30 rows"},
		{imdb.Progress{Phase: imdb.Importing, Dataset: imdb.Aliases, BytesRead: 123, Rows: 10}, "importing title.akas.tsv.gz 123B 10 rows"},
		{imdb.Progress{Phase: imdb.DatasetDone, Dataset: imdb.Ratings, BytesRead: 10, TotalBytes: 10}, "imported title.ratings.tsv.gz 10B/10B 100% 0 rows"},
		{imdb.Progress{Phase: imdb.Importing, Dataset: imdb.Basics, BytesRead: 20, TotalBytes: 10}, "importing title.basics.tsv.gz 20B/10B 100% 0 rows"},
		{imdb.Progress{Phase: imdb.Retrying, Err: io.ErrUnexpectedEOF}, "retrying full update"},
		{imdb.Progress{Phase: imdb.Failed, Err: context.Canceled}, "cancelled"},
		{imdb.Progress{Phase: imdb.Failed, Err: context.DeadlineExceeded}, "deadline exceeded"},
		{imdb.Progress{Phase: imdb.Failed, Err: errors.New("failure")}, "failed"},
		{imdb.Progress{Phase: imdb.Indexing}, "indexing"},
		{imdb.Progress{Phase: imdb.Validating}, "validating"},
		{imdb.Progress{Phase: imdb.Committing}, "committing"},
		{imdb.Progress{Phase: imdb.Done}, "done"},
	} {
		t.Run(string(tc.p.Phase)+"/"+string(tc.p.Dataset), func(t *testing.T) {
			tc.p.Attempt, tc.p.Attempts = 2, 3
			if got := formatProgress(tc.p); got != "[2/3] "+tc.want {
				t.Fatalf("%q, want %q", got, tc.want)
			}
		})
	}
}

func TestProgressRedrawAndCleanup(t *testing.T) {
	for _, phase := range []imdb.ProgressPhase{imdb.DatasetDone, imdb.Retrying, imdb.Done, imdb.Failed} {
		t.Run(string(phase), func(t *testing.T) {
			var out bytes.Buffer
			d := newProgressDisplay(&out, "always", true)
			first := imdb.Progress{Phase: imdb.Importing, Dataset: imdb.Basics, BytesRead: 1024, TotalBytes: 2048, Rows: 30, Attempt: 1, Attempts: 2}
			last := imdb.Progress{Phase: phase, Attempt: 2, Attempts: 2}
			d.report(first)
			d.report(last)
			d.finish()
			d.finish()
			want := "\r\x1b[2K" + formatProgress(first) + "\r\x1b[2K" + formatProgress(last) + "\n"
			if out.String() != want {
				t.Fatalf("redraw/cleanup: %q, want %q", out.String(), want)
			}
		})
	}
}

type failedWriter struct{ calls int }

func (w *failedWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("unwritable display")
}

func TestProgressWriterFailure(t *testing.T) {
	w := &failedWriter{}
	d := newProgressDisplay(w, "always", true)
	d.report(imdb.Progress{Phase: imdb.Preparing})
	d.report(imdb.Progress{Phase: imdb.Done})
	d.finish()
	if w.calls != 1 {
		t.Fatalf("failed display not disabled: %d writes", w.calls)
	}
	path := cliCatalog(t)
	var out bytes.Buffer
	if code := execute(context.Background(), []string{"update", "-db", path, "-input-dir", filepath.Dir(path), "-progress", "always"}, &out, w); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("progress failure affected update: exit %d, output %q", code, out.String())
	}
}

func TestCLIUpdateProgress(t *testing.T) {
	path := cliCatalog(t)
	for _, mode := range []string{"auto", "always", "never"} {
		t.Run(mode, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := execute(context.Background(), []string{"update", "-db", path, "-input-dir", filepath.Dir(path), "-progress", mode}, &out, &stderr); code != 0 {
				t.Fatalf("update exit %d: %s", code, stderr.String())
			}
			var info imdb.Info
			if err := json.Unmarshal(out.Bytes(), &info); err != nil {
				t.Fatalf("stdout is not one JSON result: %v: %q", err, out.String())
			}
			for _, s := range info.Sources {
				stat, err := os.Stat(filepath.Join(filepath.Dir(path), string(s.Dataset)))
				if err != nil {
					t.Fatal(err)
				}
				if s.SizeBytes != stat.Size() {
					t.Fatalf("wrong local size: %+v, want %d", s, stat.Size())
				}
				if mode == "always" && !strings.Contains(stderr.String(), string(s.Dataset)) {
					t.Fatalf("missing current dataset: %q", stderr.String())
				}
			}
			if mode == "always" {
				for _, text := range []string{"100%", "2 rows", "indexing", "validating", "committing", "done\n"} {
					if !strings.Contains(stderr.String(), text) {
						t.Fatalf("missing %q: %q", text, stderr.String())
					}
				}
				if strings.ContainsAny(stderr.String(), "\r\x1b") {
					t.Fatalf("nonterminal control characters: %q", stderr.String())
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("unexpected redirected progress: %q", stderr.String())
			}
		})
	}
}

func TestCLIProgressErrorsAndHelp(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := execute(context.Background(), []string{"update", "-db", filepath.Join(t.TempDir(), "missing.sqlite"), "-input-dir", t.TempDir(), "-progress", mode}, &out, &stderr)
			wantCode := 1
			if mode == "invalid" {
				wantCode = 4
			}
			if code != wantCode || out.Len() != 0 {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), stderr.String())
			}
			text := stderr.String()
			if mode == "always" {
				if !strings.Contains(text, "failed\n{\"") || strings.ContainsAny(text, "\r\x1b") {
					t.Fatalf("progress not cleaned before error: %q", text)
				}
				text = strings.TrimSpace(text[strings.LastIndex(text, "\n{")+1:])
			}
			if !json.Valid([]byte(text)) {
				t.Fatalf("non-JSON error: %q", text)
			}
		})
	}
	var out, stderr bytes.Buffer
	if code := execute(context.Background(), []string{"update", "-h"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), `"name":"progress"`) || stderr.Len() != 0 {
		t.Fatalf("help: exit %d, stdout %s, stderr %s", code, out.String(), stderr.String())
	}
}

func TestCLIHTTPProgressRetryAndUnknownSize(t *testing.T) {
	path := cliCatalog(t)
	files := make(map[imdb.Dataset][]byte)
	for _, d := range []imdb.Dataset{imdb.Basics, imdb.Aliases, imdb.Ratings} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(path), string(d)))
		if err != nil {
			t.Fatal(err)
		}
		files[d] = data
	}
	var aliasCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := imdb.Dataset(strings.TrimPrefix(r.URL.Path, "/"))
		if d == imdb.Aliases && aliasCalls.Add(1) == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.(http.Flusher).Flush()
		if _, err := w.Write(files[d]); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	code := execute(context.Background(), []string{"update", "-db", path, "-base-url", server.URL, "-attempts", "2", "-progress", "always"}, &out, &stderr)
	if code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("HTTP update: exit %d, stdout %s, stderr %s", code, out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "[1/2] retrying full update\n[2/2] preparing") || strings.Contains(stderr.String(), "%") || strings.ContainsAny(stderr.String(), "\r\x1b") {
		t.Fatalf("retry/unknown total output: %q", stderr.String())
	}
}
