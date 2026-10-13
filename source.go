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

type Dataset string

const (
	Basics  Dataset = "title.basics.tsv.gz"
	Aliases Dataset = "title.akas.tsv.gz"
	Ratings Dataset = "title.ratings.tsv.gz"
)

type SourceInfo struct {
	Dataset      Dataset `json:"dataset"`
	URL          string  `json:"url,omitempty"`
	ETag         string  `json:"etag,omitempty"`
	LastModified string  `json:"last_modified,omitempty"`
	SHA256       string  `json:"sha256"` // compressed input bytes
	Rows         int64   `json:"rows"`   // excluding header, including filtered records
}

// Source supplies complete gzip streams. Open and the returned reader must honor
// cancellation; Update owns and closes each returned reader.
type Source interface {
	Open(context.Context, Dataset) (io.ReadCloser, SourceInfo, error)
}

type SourceFunc func(context.Context, Dataset) (io.ReadCloser, SourceInfo, error)

func (f SourceFunc) Open(ctx context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
	return f(ctx, d)
}

// HTTPSource streams official datasets. BaseURL and Client allow test servers or
// caller-managed HTTP policy. A nil Client uses a two-hour per-request timeout;
// callers should also supply an overall context deadline.
type HTTPSource struct {
	BaseURL string
	Client  *http.Client
}

type HTTPError struct {
	URL        string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("imdb: download %s: HTTP %d", e.URL, e.StatusCode)
}

func (s HTTPSource) Open(ctx context.Context, d Dataset) (io.ReadCloser, SourceInfo, error) {
	if d != Basics && d != Aliases && d != Ratings {
		return nil, SourceInfo{}, fmt.Errorf("imdb: unknown dataset %q", d)
	}
	base := s.BaseURL
	if base == "" {
		base = "https://datasets.imdbws.com/"
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, SourceInfo{}, err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, SourceInfo{}, fmt.Errorf("imdb: invalid dataset base URL")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + string(d)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, SourceInfo{}, err
	}
	// The dataset itself is gzip. Prevent transparent HTTP decompression.
	req.Header.Set("Accept-Encoding", "identity")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Hour}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, SourceInfo{}, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, SourceInfo{}, &HTTPError{URL: u.String(), StatusCode: resp.StatusCode}
	}
	return resp.Body, SourceInfo{
		Dataset: d, URL: resp.Request.URL.String(), ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}
