package imdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSourceMetadataAndDatasetReports(t *testing.T) {
	data := fixtures()
	base := fixtureSource(t, data)
	source := SourceFunc(func(ctx context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
		body, _, err := base.Open(ctx, d)
		return body, SourceMetadata{
			URL: "fixture:" + string(d), ETag: "test-etag", LastModified: "opaque", SizeBytes: -1,
		}, err
	})
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	info, err := Update(t.Context(), path, UpdateOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Sources) != 3 {
		t.Fatalf("reports: %+v", info.Sources)
	}
	for _, report := range info.Sources {
		hash := sha256.Sum256(compressed(t, data[report.Dataset]))
		wantMetadata := SourceMetadata{URL: "fixture:" + string(report.Dataset), ETag: "test-etag", LastModified: "opaque"}
		if report.SourceMetadata != wantMetadata || report.SHA256 != hex.EncodeToString(hash[:]) || report.Rows != int64(strings.Count(data[report.Dataset], "\n")-1) {
			t.Fatalf("report: %+v", report)
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"dataset", "url", "etag", "last_modified", "sha256", "rows"} {
			if _, ok := object[field]; !ok {
				t.Fatalf("missing flat provenance field %s: %s", field, encoded)
			}
		}
	}
	c, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	stored, err := c.Info(t.Context())
	if err != nil || !reflect.DeepEqual(stored, info) {
		t.Fatalf("report round trip: %+v, %v", stored, err)
	}
}
