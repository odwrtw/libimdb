package imdb

import (
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("imdb: title not found")
	ErrAmbiguous    = errors.New("imdb: ambiguous title")
	ErrInvalidQuery = errors.New("imdb: invalid query")
	ErrSchema       = errors.New("imdb: unsupported or uninitialized catalog")
)

type Kind string

const (
	Movie Kind = "movie"
	Show  Kind = "show"
)

// Title contains IMDb facts, not provider enrichment. Nil fields are unavailable.
// Year is the release year, or the series start year. Runtime is in minutes.
type Title struct {
	ID            string   `json:"imdb_id"`
	Kind          Kind     `json:"kind"`
	Type          string   `json:"title_type"`
	PrimaryTitle  string   `json:"primary_title"`
	OriginalTitle string   `json:"original_title,omitempty"`
	Year          *int     `json:"year"`
	EndYear       *int     `json:"end_year"`
	Runtime       *int     `json:"runtime_minutes"`
	Adult         *bool    `json:"is_adult"`
	Genres        []string `json:"genres"`
	Rating        *float64 `json:"rating"`
	Votes         *int64   `json:"votes"`
}

type Name struct {
	Title      string   `json:"title"`
	Source     string   `json:"source"` // primary, original, or alias
	Region     string   `json:"region,omitempty"`
	Language   string   `json:"language,omitempty"`
	Types      []string `json:"types,omitempty"`
	Attributes []string `json:"attributes,omitempty"`
	IsOriginal *bool    `json:"is_original"`
}

// Query takes a title, not a filename. Empty Kind searches both kinds.
// Year zero means unspecified. Adult titles are included unless explicitly excluded.
type Query struct {
	Title        string
	Kind         Kind
	Year         int
	ExcludeAdult bool
}

type Method string

const (
	Exact      Method = "exact"
	Normalized Method = "normalized"
	Prefix     Method = "prefix"
	Fuzzy      Method = "fuzzy"
)

type Match struct {
	Title  Title   `json:"title"`
	Name   Name    `json:"matched_name"`
	Method Method  `json:"method"`
	Score  float64 `json:"score"` // ranking similarity, not a probability
}

type SearchOptions struct {
	Limit  int // default 20, maximum 100
	Budget int // names per indexed search stage; default 1000, maximum 10000
}

type SearchResult struct {
	Matches    []Match `json:"matches"`
	Truncated  bool    `json:"truncated"` // candidate budget or result limit exhausted
	Generation string  `json:"generation"`
}

type Counts struct {
	Movies  int64 `json:"movies"`
	Shows   int64 `json:"shows"`
	Names   int64 `json:"names"`
	Ratings int64 `json:"ratings"`
}

type Info struct {
	SchemaVersion        int          `json:"schema_version"`
	NormalizationVersion int          `json:"normalization_version"`
	Generation           string       `json:"generation"`
	UpdatedAt            time.Time    `json:"updated_at"`
	Counts               Counts       `json:"counts"`
	Sources              []SourceInfo `json:"sources"`
}
