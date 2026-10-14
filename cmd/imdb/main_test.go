package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	imdb "github.com/odwrtw/libimdb"
)

func cliCatalog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	inputs := map[imdb.Dataset]string{
		imdb.Basics: "tconst\ttitleType\tprimaryTitle\toriginalTitle\tisAdult\tstartYear\tendYear\truntimeMinutes\tgenres\n" +
			"tt1\tmovie\tFilm\tFilm\t0\t2000\t\\N\t90\tDrama\n" +
			"tt2\tmovie\tFilm\tFilm\t0\t2001\t\\N\t90\tDrama\n",
		imdb.Aliases: "titleId\tordering\ttitle\tregion\tlanguage\ttypes\tattributes\tisOriginalTitle\n",
		imdb.Ratings: "tconst\taverageRating\tnumVotes\n",
	}
	for d, text := range inputs {
		f, err := os.Create(filepath.Join(dir, string(d)))
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		if _, err := io.WriteString(gz, text); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "imdb.sqlite")
	if code := execute(context.Background(), []string{"update", "-db", path, "-input-dir", dir}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("update exit %d", code)
	}
	return path
}

func TestCommands(t *testing.T) {
	path := cliCatalog(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"info", "-db", path}, 0},
		{[]string{"query", "-db", path, "-title", "Film", "-year", "2000"}, 0},
		{[]string{"query", "-db", path, "-title", "Film", "-candidates"}, 0},
		{[]string{"query", "-db", path, "-title", "ilm", "-kind", "movie", "-candidates"}, 0},
		{[]string{"query", "-db", path, "-title", "Film"}, 3},
		{[]string{"query", "-db", path, "-title", "missing"}, 2},
		{[]string{"query", "-db", path}, 4},
		{[]string{"info", "-db", path, "extra"}, 4},
		{[]string{"unknown"}, 4},
		{[]string{"query", "-unknown"}, 4},
		{[]string{"update", "-h"}, 0},
		{[]string{"query", "-h"}, 0},
		{[]string{"info", "-h"}, 0},
		{nil, 4},
	} {
		var out, stderr bytes.Buffer
		code := execute(context.Background(), tc.args, &out, &stderr)
		if code != tc.code {
			t.Fatalf("%v: exit %d, want %d: %s", tc.args, code, tc.code, stderr.String())
		}
		if code == 0 && !json.Valid(out.Bytes()) {
			t.Fatalf("non-JSON output: %s", out.String())
		}
		if code != 0 && !json.Valid(stderr.Bytes()) {
			t.Fatalf("non-JSON error: %s", stderr.String())
		}
	}
}

func TestSubstringQuery(t *testing.T) {
	path := cliCatalog(t)
	var out, stderr bytes.Buffer
	code := execute(context.Background(), []string{"query", "-db", path, "-title", "ilm", "-kind", "movie", "-candidates"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("substring query exit %d: %s", code, stderr.String())
	}
	var result imdb.SearchResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 2 || result.Matches[0].Method != imdb.Substring || result.Matches[0].Title.ID != "tt1" {
		t.Fatalf("substring CLI results: %+v", result)
	}
}

func TestRepeatedUpdate(t *testing.T) {
	path := cliCatalog(t)
	if code := execute(context.Background(), []string{"update", "-db", path, "-input-dir", filepath.Dir(path)}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("refresh exit %d", code)
	}
}
