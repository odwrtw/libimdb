package imdb

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// interruptedGzip flushes a partial TSV token before finishing the same gzip
// member. The interrupted stream therefore ends inside a token, independently
// of the compressor's byte layout.
func interruptedGzip(t testing.TB, prefix, suffix string) (partial, complete []byte) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := io.WriteString(gz, prefix); err != nil {
		t.Fatal(err)
	}
	if err := gz.Flush(); err != nil {
		t.Fatal(err)
	}
	partial = bytes.Clone(b.Bytes())
	if _, err := io.WriteString(gz, suffix); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return partial, b.Bytes()
}

func TestPartialTSVTransferError(t *testing.T) {
	data := fixtures()
	for _, tc := range []struct {
		name     string
		dataset  Dataset
		prefix   string
		suffix   string
		parseErr string
	}{
		{"header", Basics, "tconst\ttitle", strings.TrimPrefix(data[Basics], "tconst\ttitle"), "unexpected TSV header"},
		{"fields", Basics, headers[Basics] + "\ntt1\tmovie\tThe", strings.TrimPrefix(data[Basics], headers[Basics]+"\ntt1\tmovie\tThe"), "unexpected field count"},
		{"UTF-8", Basics, headers[Basics] + "\ntt1\tmovie\tAm\xc3", "\xa9lie\tAmélie\t0\t2001\t\\N\t122\tComedy\n", "invalid text"},
		{"boolean", Aliases, headers[Aliases] + "\ntt1\t1\tLa Matrice\tFR\tfr\timdbDisplay\t\\N\t\\", "N\n", "invalid boolean"},
		{"votes", Ratings, headers[Ratings] + "\ntt1\t8.7\t", "2100000\n", "invalid vote count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial, _ := interruptedGzip(t, tc.prefix, tc.suffix)
			for _, mode := range []string{"interrupted", "malformed", "complete without newline"} {
				t.Run(mode, func(t *testing.T) {
					input := partial
					attempts := 1
					switch mode {
					case "malformed":
						input = compressed(t, tc.prefix)
						attempts = 2 // Fully transferred malformed TSV must not retry.
					case "complete without newline":
						input = compressed(t, strings.TrimSuffix(tc.prefix+tc.suffix, "\n"))
					}
					base := fixtureSource(t, data)
					opens := 0
					source := SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
						if d == tc.dataset {
							opens++
							return io.NopCloser(bytes.NewReader(input)), SourceInfo{}, nil
						}
						return base.Open(ctx, d)
					})
					_, err := Update(t.Context(), filepath.Join(t.TempDir(), "catalog.sqlite"), UpdateOptions{
						Source: source, Attempts: attempts, RetryDelay: time.Millisecond,
					})
					switch mode {
					case "interrupted":
						if !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatalf("lost transfer error: %v", err)
						}
					case "malformed":
						if err == nil || !strings.Contains(err.Error(), tc.parseErr) || retryable(err) {
							t.Fatalf("expected permanent %q error: %v", tc.parseErr, err)
						}
					case "complete without newline":
						if err != nil {
							t.Fatal(err)
						}
					}
					if opens != 1 {
						t.Fatalf("opened dataset %d times, want 1", opens)
					}
				})
			}
		})
	}
}
