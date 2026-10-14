package imdb_test

import (
	"context"
	"errors"
	"fmt"

	imdb "github.com/odwrtw/libimdb"
)

func ExampleCatalog_Candidates() {
	catalog, err := imdb.Open(context.Background(), "imdb.sqlite")
	if err != nil {
		return
	}
	defer func() { _ = catalog.Close() }()

	result, err := catalog.Candidates(context.Background(), imdb.Query{
		Title: "matrix", Kind: imdb.Movie,
	}, imdb.SearchOptions{Limit: 20})
	if err != nil {
		return
	}
	for _, match := range result.Matches {
		fmt.Println(match.Title.ID, match.Title.PrimaryTitle, match.Method)
	}
	// These are suggestions, never proof of a unique identity.
	fmt.Println("clipped:", result.Truncated)
}

func ExampleCatalog_Resolve() {
	// Populate this catalog with imdb.Update before opening it.
	catalog, err := imdb.Open(context.Background(), "imdb.sqlite")
	if err != nil {
		return
	}
	defer func() { _ = catalog.Close() }()

	match, err := catalog.Resolve(context.Background(), imdb.Query{
		Title: "The Matrix", Kind: imdb.Movie, Year: 1999,
	})
	switch {
	case errors.Is(err, imdb.ErrAmbiguous):
		fmt.Println("more identity information is required")
	case errors.Is(err, imdb.ErrNotFound):
		fmt.Println("no safe identity match")
	case err != nil:
		fmt.Println("catalog failure:", err)
	default:
		fmt.Println(match.Title.ID)
	}
}
