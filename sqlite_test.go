package imdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestRejectInvalidCatalog(t *testing.T) {
	for _, change := range []string{
		"PRAGMA application_id=0",
		fmt.Sprintf("PRAGMA user_version=%d", schemaVersion-1),
		fmt.Sprintf("PRAGMA user_version=%d", schemaVersion+1),
		"UPDATE catalog_info SET info=json_set(info,'$.schema_version',0)",
		"UPDATE catalog_info SET info=json_set(info,'$.normalization_version',99)",
		"UPDATE catalog_info SET info=json_set(info,'$.generation','')",
	} {
		t.Run(change, func(t *testing.T) {
			path, c, old := newCatalog(t)
			db, err := openDB(context.Background(), path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(change); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := db.QueryRow("SELECT info FROM catalog_info").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Info(context.Background()); !errors.Is(err, ErrSchema) {
				t.Fatalf("invalid catalog readable: %v", err)
			}
			if _, err := Update(context.Background(), path, UpdateOptions{Source: fixtureSource(t, fixtures())}); !errors.Is(err, ErrSchema) {
				t.Fatalf("invalid catalog accepted for refresh: %v", err)
			}
			var after, title string
			var names int64
			if err := db.QueryRow("SELECT info FROM catalog_info").Scan(&after); err != nil || before != after {
				t.Fatalf("rejected catalog metadata changed: %v", err)
			}
			if err := db.QueryRow("SELECT primary_title FROM titles WHERE imdb_id='tt1'").Scan(&title); err != nil || title != "The Matrix" {
				t.Fatalf("rejected catalog title changed: %q %v", title, err)
			}
			if err := db.QueryRow("SELECT count(*) FROM names").Scan(&names); err != nil || names != old.Counts.Names {
				t.Fatalf("rejected catalog names changed: %d %v", names, err)
			}
		})
	}
}

func TestRefreshFailureRestoresSchemaAndSearch(t *testing.T) {
	for _, mode := range []string{"parse", "index cancellation", "validation cancellation", "commit cancellation"} {
		t.Run(mode, func(t *testing.T) {
			path, c, old := newCatalog(t)
			const schemaQuery = "SELECT group_concat(sql,char(10)) FROM (SELECT sql FROM sqlite_schema ORDER BY name)"
			var before string
			if err := c.db.QueryRow(schemaQuery).Scan(&before); err != nil {
				t.Fatal(err)
			}
			q := Query{Title: "matrix", Kind: Movie}
			search, err := c.Candidates(context.Background(), q, SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			data := fixtures()
			data[Basics] = strings.ReplaceAll(data[Basics], "The Matrix", "New Name")
			if mode == "parse" {
				data[Ratings] = "bad header\n"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err = Update(ctx, path, UpdateOptions{Source: fixtureSource(t, data), Progress: func(p Progress) {
				if (mode == "index cancellation" && p.Phase == Indexing) || (mode == "validation cancellation" && p.Phase == Validating) || (mode == "commit cancellation" && p.Phase == Committing) {
					cancel()
				}
			}})
			if err == nil || (strings.Contains(mode, "cancellation") && !errors.Is(err, context.Canceled)) {
				t.Fatalf("expected refresh failure: %v", err)
			}
			current, err := c.Info(context.Background())
			if err != nil || !reflect.DeepEqual(old, current) {
				t.Fatalf("metadata changed: %+v %v", current, err)
			}
			var after string
			if err := c.db.QueryRow(schemaQuery).Scan(&after); err != nil || before != after {
				t.Fatalf("schema/index rollback failed: %v\nbefore=%s\nafter=%s", err, before, after)
			}
			got, err := c.Candidates(context.Background(), q, SearchOptions{})
			if err != nil || !reflect.DeepEqual(search, got) {
				t.Fatalf("search changed: %+v %v", got, err)
			}
		})
	}
}
