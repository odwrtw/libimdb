package imdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dataset identifies one of the three required gzip-compressed IMDb TSV files.
type Dataset string

const (
	Basics  Dataset = "title.basics.tsv.gz"
	Aliases Dataset = "title.akas.tsv.gz"
	Ratings Dataset = "title.ratings.tsv.gz"
)

// SourceMetadata is supplied by a source, not computed or verified by Update.
// URL is a provenance location and may be a local path. Empty strings mean
// unavailable; SizeBytes <= 0 means unknown. SizeBytes is an advisory compressed
// size for progress, not an integrity assertion. LastModified is opaque metadata.
type SourceMetadata struct {
	URL          string `json:"url,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
}

// DatasetReport combines source metadata with facts computed by the importer.
// SHA256 hashes the consumed compressed input; it is not compared to an external
// expected digest. Rows excludes the header and includes filtered records.
type DatasetReport struct {
	SourceMetadata
	Dataset Dataset `json:"dataset"`
	SHA256  string  `json:"sha256"`
	Rows    int64   `json:"rows"`
}

// Source supplies complete gzip streams. Open and the returned reader must honor
// cancellation. On success, Open returns a non-nil reader owned and closed by
// Update; on error, the source must release any acquired resources. Calls are
// sequential within an update, but retries reopen datasets and require fresh
// complete streams starting at byte zero. Shared sources must handle concurrent
// independent updates if used that way. Source metadata is advisory.
type Source interface {
	Open(context.Context, Dataset) (io.ReadCloser, SourceMetadata, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(context.Context, Dataset) (io.ReadCloser, SourceMetadata, error)

// Open calls f with the requested dataset.
func (f SourceFunc) Open(ctx context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
	return f(ctx, d)
}

// HTTPSource streams official datasets. BaseURL and Client allow test servers or
// caller-managed HTTP policy. An empty BaseURL uses https://datasets.imdbws.com/;
// a custom BaseURL must be absolute HTTP(S), without query or fragment. A nil
// Client uses a two-hour per-request timeout; callers should also supply an
// overall context deadline.
type HTTPSource struct {
	BaseURL string
	Client  *http.Client
}

// HTTPError reports a non-200 download response. Update may retry 429 and 5xx.
type HTTPError struct {
	URL        string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("imdb: download %s: HTTP %d", e.URL, e.StatusCode)
}

// Open downloads d without transparent HTTP decompression. The caller owns the
// returned body on success. Invalid datasets or BaseURL return ValidationError;
// non-200 responses return HTTPError after closing the response body.
func (s HTTPSource) Open(ctx context.Context, d Dataset) (io.ReadCloser, SourceMetadata, error) {
	if d != Basics && d != Aliases && d != Ratings {
		return nil, SourceMetadata{}, &ValidationError{Field: "dataset", Problem: "must be Basics, Aliases, or Ratings"}
	}
	base := s.BaseURL
	if base == "" {
		base = "https://datasets.imdbws.com/"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return nil, SourceMetadata{}, &ValidationError{Field: "HTTPSource.BaseURL", Problem: "must be an absolute HTTP(S) URL without query or fragment"}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + string(d)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, SourceMetadata{}, err
	}
	// The dataset itself is gzip. Prevent transparent HTTP decompression.
	req.Header.Set("Accept-Encoding", "identity")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Hour}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, SourceMetadata{}, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, SourceMetadata{}, &HTTPError{URL: u.String(), StatusCode: resp.StatusCode}
	}
	return resp.Body, SourceMetadata{
		URL: resp.Request.URL.String(), ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"), SizeBytes: max(resp.ContentLength, 0),
	}, nil
}
