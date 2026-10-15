package imdb

import (
	"context"
	"database/sql"
	"slices"
)

// View is a read-only snapshot scoped to a Catalog.View callback. Its zero value
// is closed. Do not copy it, retain it, or use it concurrently. Do not call the
// same Catalog from its callback: a Catalog serializes operations on one
// connection. Results remain valid after the callback; the View itself does not.
type View struct {
	tx   *sql.Tx
	info Info
	ctx  context.Context
}

// View runs fn synchronously against one committed generation, releasing the
// snapshot even on callback error or panic. It returns fn's error, or the
// context error if fn succeeds after cancellation. A nil fn is invalid input.
// Use the supplied View for all reads inside fn, not the Catalog. Keep callbacks
// short: long-lived snapshots can prevent SQLite from reclaiming WAL space.
func (c *Catalog) View(ctx context.Context, fn func(*View) error) error {
	if fn == nil {
		return &ValidationError{Field: "fn", Problem: "must not be nil"}
	}
	v, err := c.newView(ctx)
	if err != nil {
		return err
	}
	defer v.close()
	if err := fn(v); err != nil {
		return err
	}
	return ctx.Err()
}

func (v *View) close() {
	_ = v.tx.Rollback()
	v.tx = nil
}

func (v *View) check(ctx context.Context) error {
	if v.tx == nil {
		return ErrViewClosed
	}
	if err := v.ctx.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// Info returns metadata for this view's generation. The returned data can be
// modified by the caller without changing the view's metadata.
func (v *View) Info(ctx context.Context) (Info, error) {
	if err := v.check(ctx); err != nil {
		return Info{}, err
	}
	info := v.info
	info.Sources = slices.Clone(info.Sources)
	return info, nil
}

// Lookup returns the retained title with id, or ErrNotFound. IDs must be "tt"
// followed by one or more ASCII digits; invalid IDs return ValidationError.
// Valid IDs for unsupported title types are treated as not found.
func (c *Catalog) Lookup(ctx context.Context, id string) (Title, error) {
	v, err := c.newView(ctx)
	if err != nil {
		return Title{}, err
	}
	defer v.close()
	return v.Lookup(ctx, id)
}

// LookupMany returns a non-nil map of found titles, omitting absent IDs and
// collapsing duplicates. Empty input returns an empty map. All results belong
// to one snapshot. At most 10000 IDs are allowed; any invalid ID rejects the
// entire request with ValidationError. ID syntax is the same as for Lookup.
func (c *Catalog) LookupMany(ctx context.Context, ids []string) (map[string]Title, error) {
	v, err := c.newView(ctx)
	if err != nil {
		return nil, err
	}
	defer v.close()
	return v.LookupMany(ctx, ids)
}

// Names returns primary/original/alias names without deduplication. Ordering is
// dataset import order within this snapshot, not a preference ranking or stable
// across generations. An original name identical to the primary is not repeated
// as an OriginalName. It returns ErrNotFound for an absent title and
// ValidationError for invalid ID syntax (see Lookup).
func (c *Catalog) Names(ctx context.Context, id string) ([]Name, error) {
	v, err := c.newView(ctx)
	if err != nil {
		return nil, err
	}
	defer v.close()
	return v.Names(ctx, id)
}

// Resolve accepts only a unique identity after Unicode NFC, case and whitespace
// normalization. It never uses fuzzy or popularity ranking. All matching names
// participate in ambiguity detection, even when a primary name is an exact
// spelling match. StartYear never falls back to another or unknown year.
// No match returns ErrNotFound; multiple identities return *AmbiguousError,
// matching ErrAmbiguous. Invalid queries return *ValidationError. On any error
// the returned Resolution is zero; ambiguity never selects a winner.
func (c *Catalog) Resolve(ctx context.Context, q Query) (Resolution, error) {
	v, err := c.newView(ctx)
	if err != nil {
		return Resolution{}, err
	}
	defer v.close()
	return v.Resolve(ctx, q)
}

// Candidates returns bounded exact, normalized, prefix, substring and whole-title
// typo suggestions. Substrings require three folded Unicode characters; typos
// require four and run only when direct matches do not fill Limit. Retrieval is
// heuristic: clipping flags do not measure recall. Invalid queries or options
// return *ValidationError. No suggestions returns an empty, non-nil Matches.
// Use Resolve, never candidate rank or count, for authoritative identity.
func (c *Catalog) Candidates(ctx context.Context, q Query, opts SearchOptions) (SearchResult, error) {
	v, err := c.newView(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	defer v.close()
	return v.Candidates(ctx, q, opts)
}
