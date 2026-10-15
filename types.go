package imdb

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound means no retained title satisfies the lookup or resolution.
	ErrNotFound = errors.New("imdb: title not found")
	// ErrAmbiguous is matched by AmbiguousError via errors.Is.
	ErrAmbiguous = errors.New("imdb: ambiguous title")
	// ErrInvalidInput is matched by ValidationError via errors.Is.
	ErrInvalidInput = errors.New("imdb: invalid input")
	// ErrSchema means the database is foreign, uninitialized, or incompatible.
	ErrSchema = errors.New("imdb: unsupported or uninitialized catalog")
	// ErrViewClosed means a View was used outside its callback or is uninitialized.
	ErrViewClosed = errors.New("imdb: read view closed")
)

// ValidationError identifies an invalid argument or option. Field is a Go field
// name (for example, Query.Title or UpdateOptions.Attempts), or an argument name.
// Problem describes the constraint without echoing potentially sensitive input.
type ValidationError struct {
	Field   string `json:"field"`
	Problem string `json:"problem"`
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrInvalidInput, e.Field, e.Problem)
}

func (e *ValidationError) Unwrap() error { return ErrInvalidInput }

// AmbiguousError reports conflicting titles sharing a conservatively normalized
// name under the query's filters. Titles contains at most 20 distinct titles,
// ordered by IMDb ID, not popularity. Truncated means more conflicts exist.
// All titles and Generation belong to the same snapshot. None is selected.
type AmbiguousError struct {
	Titles     []Title `json:"titles"`
	Truncated  bool    `json:"truncated"`
	Generation string  `json:"generation"`
}

func (e *AmbiguousError) Error() string { return ErrAmbiguous.Error() }

func (e *AmbiguousError) Unwrap() error { return ErrAmbiguous }

// Kind groups retained IMDb title types into movies and series.
type Kind string

const (
	Movie Kind = "movie" // movie and tvMovie
	Show  Kind = "show"  // tvSeries and tvMiniSeries
)

// Title contains IMDb facts, not provider enrichment or application state.
// Nil numeric/boolean fields and nil Genres are unavailable. An empty
// OriginalTitle is unavailable. StartYear is the release or series start year.
// Only movie, tvMovie, tvSeries, and tvMiniSeries title types are retained.
type Title struct {
	ID             string   `json:"imdb_id"`
	Kind           Kind     `json:"kind"`
	TitleType      string   `json:"title_type"`
	PrimaryTitle   string   `json:"primary_title"`
	OriginalTitle  string   `json:"original_title,omitempty"`
	StartYear      *int     `json:"start_year"`
	EndYear        *int     `json:"end_year"`
	RuntimeMinutes *int     `json:"runtime_minutes"`
	Adult          *bool    `json:"is_adult"`
	Genres         []string `json:"genres"`
	Rating         *float64 `json:"rating"`
	Votes          *int64   `json:"votes"`
}

// NameSource identifies the dataset role of a name, not its language or region.
type NameSource string

const (
	PrimaryName  NameSource = "primary"
	OriginalName NameSource = "original"
	AliasName    NameSource = "alias"
)

// Name is a primary title, distinct original title, or raw alias. Empty Region
// and Language and nil Types and Attributes mean unavailable or not applicable.
// IsOriginal is IMDb's alias isOriginalTitle field; it is nil for primary and
// original names and is not a general test for whether a name is original.
type Name struct {
	Title      string     `json:"title"`
	Source     NameSource `json:"source"`
	Region     string     `json:"region,omitempty"`
	Language   string     `json:"language,omitempty"`
	Types      []string   `json:"types,omitempty"`
	Attributes []string   `json:"attributes,omitempty"`
	IsOriginal *bool      `json:"is_original"`
}

// Query takes a title, not a filename. Title must be valid UTF-8 without NUL,
// nonempty after normalization, at most 1024 bytes and 256 normalized Unicode
// code points. Empty Kind searches both kinds. StartYear zero is unspecified;
// a positive value is a hard constraint excluding unknown years. ExcludeAdult
// excludes only titles explicitly marked adult, not those with unknown flags.
type Query struct {
	Title        string
	Kind         Kind
	StartYear    int
	ExcludeAdult bool
}

// MatchMethod describes how a name matched, not whether its identity is unique.
type MatchMethod string

const (
	Exact      MatchMethod = "exact"
	Normalized MatchMethod = "normalized"
	Prefix     MatchMethod = "prefix"
	Substring  MatchMethod = "substring"
	Fuzzy      MatchMethod = "fuzzy"
)

// Resolution is a unique identity accepted by Resolve. Method is Exact or
// Normalized; Name is the matching name. Generation identifies its snapshot.
type Resolution struct {
	Title      Title       `json:"title"`
	Name       Name        `json:"matched_name"`
	Method     MatchMethod `json:"method"`
	Generation string      `json:"generation"`
}

// Candidate is a suggestion, never proof of identity, even for Exact matches.
// Score is ranking similarity, not a confidence probability. Use Resolve for
// automated identity selection rather than candidate count, method, or score.
type Candidate struct {
	Title  Title       `json:"title"`
	Name   Name        `json:"matched_name"`
	Method MatchMethod `json:"method"`
	Score  float64     `json:"score"`
}

// SearchOptions bounds heuristic retrieval. Zero values select defaults.
// Budget counts distinct IDs in the exact stage, folded names in prefix and
// substring stages, and postings across all selected typo streams combined.
// It applies before year/adult and similarity filters; different names for one
// title can consume multiple entries. Each stream may read one extra row to
// detect clipping. Budgets bound these stages, not overall query time.
type SearchOptions struct {
	Limit  int // default 20, range 1–100
	Budget int // entries/postings inspected per stage, before filters; default 1000, range 1–10000
}

// SearchResult contains distinct-ID suggestions ordered by method, descending
// score, descending votes, ID, then matched name. Matches is non-nil on success.
// Neither clipping flag measures recall or promises exhaustive retrieval.
type SearchResult struct {
	Matches         []Candidate `json:"matches"`
	LimitTruncated  bool        `json:"limit_truncated"`  // retrieved matches were omitted to satisfy Limit
	BudgetExhausted bool        `json:"budget_exhausted"` // a retrieval stage was clipped by Budget
	Generation      string      `json:"generation"`
}

// Counts reports retained catalog rows. Names includes repeated raw aliases.
type Counts struct {
	Movies  int64 `json:"movies"`
	Shows   int64 `json:"shows"`
	Names   int64 `json:"names"`
	Ratings int64 `json:"ratings"`
}

// Info describes one committed catalog generation and its dataset provenance.
// UpdatedAt is the UTC time recorded immediately before commit.
type Info struct {
	SchemaVersion        int             `json:"schema_version"`
	NormalizationVersion int             `json:"normalization_version"`
	Generation           string          `json:"generation"`
	UpdatedAt            time.Time       `json:"updated_at"`
	Counts               Counts          `json:"counts"`
	Sources              []DatasetReport `json:"sources"`
}
