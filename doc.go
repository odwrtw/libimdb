// Package imdb provides a CGO-free SQLite catalog of IMDb titles and facts.
// Update creates or refreshes the catalog atomically from streaming gzip datasets.
// Catalog.Resolve accepts only unique exact or conservatively normalized names;
// Catalog.Candidates returns broader suggestions without accepting their identity.
// IMDb data is subject to separate personal, non-commercial licensing terms.
package imdb
