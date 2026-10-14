// Package imdb provides a CGO-free SQLite catalog of IMDb titles and facts.
// Update creates or refreshes the catalog atomically from streaming gzip datasets.
// Catalog.Resolve accepts only unique exact or conservatively normalized names;
// Catalog.Candidates returns bounded prefix, substring and typo suggestions,
// without accepting their identity. Readers and refreshes require schema v2.
// IMDb data is subject to separate personal, non-commercial licensing terms.
package imdb
