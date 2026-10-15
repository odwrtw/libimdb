package imdb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestViewAcrossRefresh(t *testing.T) {
	path, c, old := newCatalog(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var updated Info
	var escaped *View
	err := c.View(ctx, func(v *View) error {
		escaped = v
		before, err := v.Resolve(ctx, Query{Title: "The Matrix"})
		if err != nil || before.Generation != old.Generation {
			t.Fatalf("initial resolution: %+v, %v", before, err)
		}
		data := fixtures()
		data[Basics] = strings.ReplaceAll(data[Basics], "The Matrix", "New Matrix")
		data[Basics] = strings.ReplaceAll(data[Basics], "1999", "2002")
		updated, err = Update(ctx, path, UpdateOptions{Source: fixtureSource(t, data)})
		if err != nil {
			return err
		}
		if updated.Generation == old.Generation {
			t.Fatal("refresh did not advance generation")
		}
		info, err := v.Info(ctx)
		if err != nil || info.Generation != old.Generation {
			t.Fatalf("view adopted new metadata: %+v, %v", info, err)
		}
		title, err := v.Lookup(ctx, "tt1")
		if err != nil || title.PrimaryTitle != "The Matrix" || *title.StartYear != 1999 {
			t.Fatalf("view adopted new facts: %+v, %v", title, err)
		}
		batch, err := v.LookupMany(ctx, []string{"tt1", "tt999"})
		if err != nil || len(batch) != 1 || batch["tt1"].PrimaryTitle != "The Matrix" {
			t.Fatalf("batch crossed generations: %+v, %v", batch, err)
		}
		names, err := v.Names(ctx, "tt1")
		if err != nil || len(names) == 0 || names[0].Title != "The Matrix" {
			t.Fatalf("names crossed generations: %+v, %v", names, err)
		}
		after, err := v.Resolve(ctx, Query{Title: "The Matrix"})
		if err != nil || after.Generation != before.Generation || after.Title.PrimaryTitle != before.Title.PrimaryTitle {
			t.Fatalf("resolution crossed generations: %+v, %v", after, err)
		}
		candidates, err := v.Candidates(ctx, Query{Title: "The Matrix"}, SearchOptions{})
		if err != nil || candidates.Generation != old.Generation || len(candidates.Matches) == 0 || candidates.Matches[0].Title.PrimaryTitle != "The Matrix" {
			t.Fatalf("candidates crossed generations: %+v, %v", candidates, err)
		}
		_, err = v.Resolve(ctx, Query{Title: "Dune"})
		var ambiguous *AmbiguousError
		if !errors.As(err, &ambiguous) || ambiguous.Generation != old.Generation {
			t.Fatalf("ambiguity crossed generations: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertClosedView(t, escaped)
	after, err := c.Resolve(ctx, Query{Title: "New Matrix"})
	if err != nil || after.Generation != updated.Generation || *after.Title.StartYear != 2002 {
		t.Fatalf("catalog did not adopt refresh: %+v, %v", after, err)
	}
}

func assertClosedView(t *testing.T, v *View) {
	t.Helper()
	ctx := t.Context()
	calls := []func() error{
		func() error { _, err := v.Info(ctx); return err },
		func() error { _, err := v.Lookup(ctx, "tt1"); return err },
		func() error { _, err := v.LookupMany(ctx, nil); return err },
		func() error { _, err := v.Names(ctx, "tt1"); return err },
		func() error { _, err := v.Resolve(ctx, Query{Title: "The Matrix"}); return err },
		func() error { _, err := v.Candidates(ctx, Query{Title: "The Matrix"}, SearchOptions{}); return err },
	}
	for i, call := range calls {
		if err := call(); !errors.Is(err, ErrViewClosed) {
			t.Fatalf("closed view call %d: %v", i, err)
		}
	}
}

func TestViewLifecycle(t *testing.T) {
	_, c, _ := newCatalog(t)
	assertClosedView(t, &View{})
	sentinel := errors.New("callback failure")
	for _, mode := range []string{"success", "error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			var escaped *View
			func() {
				if mode == "panic" {
					defer func() {
						if recovered := recover(); recovered != sentinel {
							t.Fatalf("panic not preserved: %v", recovered)
						}
					}()
				}
				err := c.View(t.Context(), func(v *View) error {
					escaped = v
					switch mode {
					case "error":
						return sentinel
					case "panic":
						panic(sentinel)
					}
					return nil
				})
				if (mode == "error" && !errors.Is(err, sentinel)) || (mode == "success" && err != nil) {
					t.Fatalf("callback result: %v", err)
				}
			}()
			if escaped == nil {
				t.Fatal("callback not invoked")
			}
			assertClosedView(t, escaped)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := c.Lookup(ctx, "tt1"); err != nil {
				t.Fatalf("view did not release connection: %v", err)
			}
		})
	}
}

func TestViewCancellation(t *testing.T) {
	_, c, _ := newCatalog(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.View(ctx, func(*View) error {
		t.Fatal("callback invoked after cancellation")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled View: %v", err)
	}
	err := c.View(t.Context(), func(v *View) error {
		if _, err := v.Lookup(ctx, "tt1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("per-call cancellation: %v", err)
		}
		_, err := v.Lookup(t.Context(), "tt1")
		return err
	})
	if err != nil {
		t.Fatalf("per-call cancellation invalidated view: %v", err)
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	err = c.View(ctx, func(v *View) error {
		cancel()
		if _, err := v.Info(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("view ignored parent cancellation: %v", err)
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback success swallowed cancellation: %v", err)
	}
}

func TestViewInfoOwnership(t *testing.T) {
	_, c, _ := newCatalog(t)
	if err := c.View(t.Context(), func(v *View) error {
		first, err := v.Info(t.Context())
		if err != nil {
			return err
		}
		first.Sources[0].URL = "changed"
		second, err := v.Info(t.Context())
		if err != nil {
			return err
		}
		if second.Sources[0].URL == "changed" {
			t.Fatal("caller changed view metadata through a result")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
