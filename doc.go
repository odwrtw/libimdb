// Package imdb provides a CGO-free SQLite catalog of IMDb titles and facts.
// Update creates or refreshes the catalog atomically from streaming gzip datasets.
// Catalog.Resolve returns a Resolution only for unique exact or conservatively
// normalized names; AmbiguousError reports bounded conflicts without a winner.
// Catalog.Candidates returns heuristic Candidate suggestions, never identities.
// Catalog.View scopes multi-call reads to one committed generation. Invalid input
// returns ValidationError matching ErrInvalidInput. Readers and refreshes require
// schema v2.
// IMDb data is subject to separate personal, non-commercial licensing terms.
package imdb
