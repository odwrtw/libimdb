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
	fmt.Println("limit clipped:", result.LimitTruncated, "budget exhausted:", result.BudgetExhausted)
}

func ExampleCatalog_Resolve() {
	// Populate this catalog with imdb.Update before opening it.
	catalog, err := imdb.Open(context.Background(), "imdb.sqlite")
	if err != nil {
		return
	}
	defer func() { _ = catalog.Close() }()

	match, err := catalog.Resolve(context.Background(), imdb.Query{
		Title: "The Matrix", Kind: imdb.Movie, StartYear: 1999,
	})
	var ambiguity *imdb.AmbiguousError
	switch {
	case errors.As(err, &ambiguity):
		for _, title := range ambiguity.Titles {
			fmt.Println(title.ID, title.PrimaryTitle)
		}
		fmt.Println("more conflicts:", ambiguity.Truncated)
	case errors.Is(err, imdb.ErrNotFound):
		fmt.Println("no safe identity match")
	case err != nil:
		fmt.Println("catalog failure:", err)
	default:
		fmt.Println(match.Title.ID, match.Generation)
	}
}

func ExampleCatalog_View() {
	ctx := context.Background()
	catalog, err := imdb.Open(ctx, "imdb.sqlite")
	if err != nil {
		return
	}
	defer func() { _ = catalog.Close() }()

	err = catalog.View(ctx, func(view *imdb.View) error {
		resolved, err := view.Resolve(ctx, imdb.Query{Title: "The Matrix"})
		if err != nil {
			return err
		}
		names, err := view.Names(ctx, resolved.Title.ID)
		if err != nil {
			return err
		}
		// Both results come from one generation, even across a refresh.
		fmt.Println(resolved.Generation, names)
		return nil
	})
	if err != nil {
		fmt.Println(err)
	}
}

func ExampleValidationError() {
	_, err := imdb.Open(context.Background(), "")
	var invalid *imdb.ValidationError
	if errors.As(err, &invalid) {
		fmt.Println(invalid.Field)
	}
	fmt.Println(errors.Is(err, imdb.ErrInvalidInput))
	// Output:
	// path
	// true
}
