package imdb

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateSearchPostings(t *testing.T) {
	data := fixtures()
	data[Basics] += "tt20\tmovie\txaaaaay\txaaaaay\t0\t2000\t\\N\t90\tDrama\n" +
		"tt21\ttvSeries\t君の名は。\t君の名は。\t0\t2000\t\\N\t90\tDrama\n" +
		"tt22\tmovie\tIt\tIt\t0\t2000\t\\N\t90\tDrama\n" +
		"tt23\tmovie\t...\t...\t0\t2000\t\\N\t90\tDrama\n" +
		"tt24\tmovie\t" + strings.Repeat("Long Unicode 名 title ", 100) + "\tOriginal\t0\t2000\t\\N\t90\tDrama\n"
	data[Aliases] += "tt20\t1\tXAAAAAY\tUS\ten\t\\N\t\\N\t0\n" +
		"tt20\t2\txaaaaay\tCA\ten\t\\N\t\\N\t0\n" +
		"tt21\t1\t君の名は。\tJP\tja\t\\N\t\\N\t1\n"
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	source := fixtureSource(t, data)
	for _, operation := range []string{"create", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			if _, err := Update(t.Context(), path, UpdateOptions{Source: source}); err != nil {
				t.Fatal(err)
			}
			c, err := Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			// Compare every posting against the original Go generator, including
			// Unicode, repeated grams, short/empty folds and duplicate raw names.
			type posting struct {
				gram string
				kind Kind
				id   int64
			}
			want := make(map[posting]bool)
			rows, err := c.db.Query("SELECT id,kind,fuzzy_key FROM search_names")
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id int64
				var kind Kind
				var key string
				if err := rows.Scan(&id, &kind, &key); err != nil {
					t.Fatal(err)
				}
				for _, gram := range grams(key) {
					want[posting{gram, kind, id}] = true
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			rows, err = c.db.Query("SELECT gram,kind,search_id FROM search_grams")
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[posting]bool)
			for rows.Next() {
				var p posting
				if err := rows.Scan(&p.gram, &p.kind, &p.id); err != nil {
					t.Fatal(err)
				}
				got[p] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("posting mismatch: got %d, want %d", len(got), len(want))
			}
			var differences int
			if err := c.db.QueryRow(`SELECT
				(SELECT count(*) FROM (SELECT gram,kind,count(*) FROM search_grams GROUP BY gram,kind
					EXCEPT SELECT gram,kind,entries FROM gram_counts)) +
				(SELECT count(*) FROM (SELECT gram,kind,entries FROM gram_counts
					EXCEPT SELECT gram,kind,count(*) FROM search_grams GROUP BY gram,kind))`).Scan(&differences); err != nil || differences != 0 {
				t.Fatalf("gram frequency mismatch: %d, %v", differences, err)
			}
			if err := c.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='pending_grams'").Scan(&differences); err != nil || differences != 0 {
				t.Fatalf("staging leaked into catalog: %d, %v", differences, err)
			}
		})
	}
}

func TestAliasMembershipCache(t *testing.T) {
	data := fixtures()
	data[Aliases] = headers[Aliases] + "\n" +
		"tt8\t1\tExcluded First\tUS\ten\t\\N\t\\N\t0\n" +
		"tt8\t2\tExcluded Second\tUS\ten\t\\N\t\\N\t0\n" +
		"tt1\t1\tAmélie\tFR\tfr\talternative\t\\N\t0\n" +
		"tt1\t2\tAmélie\tCA\tfr\timdbDisplay\t\\N\t1\n" +
		"tt999\t1\tMissing\tUS\ten\t\\N\t\\N\t0\n" +
		"tt8\t3\tExcluded Again\tUS\ten\t\\N\t\\N\t0\n" +
		"tt1\t3\tLast Alias\tUS\ten\t\\N\t\\N\t0\n"
	c := catalogFromData(t, data)
	names, err := c.Names(t.Context(), "tt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 4 || names[1].Title != "Amélie" || names[1].Region != "FR" || names[2].Region != "CA" || names[3].Title != "Last Alias" {
		t.Fatalf("alias order or metadata lost: %+v", names)
	}
	// A primary spelling must not override another title's raw alias.
	if _, err := c.Resolve(t.Context(), Query{Title: "Amélie"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("alias collision lost: %v", err)
	}
	for _, id := range []string{"tt8", "tt999"} {
		if _, err := c.Names(t.Context(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("retained excluded alias %s: %v", id, err)
		}
	}
}

func TestExcludedAliasValidation(t *testing.T) {
	for _, row := range []string{
		"tt8\t0\tInvalid\tUS\ten\t\\N\t\\N\t0",
		"tt8\t1\t\\N\tUS\ten\t\\N\t\\N\t0",
		"tt8\t1\t \u00a0 \tUS\ten\t\\N\t\\N\t0",
		"tt8\t1\tInvalid\tUS\ten\t\\N\t\\N\t2",
	} {
		t.Run(row, func(t *testing.T) {
			path, c, old := newCatalog(t)
			data := fixtures() // Ends with a valid excluded tt8 alias, priming the cache.
			data[Aliases] += row + "\n"
			if _, err := Update(t.Context(), path, UpdateOptions{Source: fixtureSource(t, data)}); err == nil {
				t.Fatal("invalid excluded alias accepted")
			}
			current, err := c.Info(t.Context())
			if err != nil || !reflect.DeepEqual(current, old) {
				t.Fatalf("failed import changed catalog: %+v, %v", current, err)
			}
		})
	}
}

func TestGramStagingLifecycle(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(ctx, filepath.Join(t.TempDir(), "staging.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for pragma, want := range map[string]int{"temp_store": 1, "foreign_keys": 1, "synchronous": 2} {
		var got int
		if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, want %d: %v", pragma, got, want, err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	w, err := newDatasetWriter(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.basics(ctx, []string{"tt1", "movie", "abcabc", "abcabc", "0", "2000", "\\N", "90", "Drama"}); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"search_grams": 0, "temp.pending_grams": 3} {
		var got int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, want %d: %v", table, got, want, err)
		}
	}
	if _, err := tx.ExecContext(ctx, indexes); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM search_grams").Scan(&count); err != nil || count != 3 {
		t.Fatalf("materialized postings: %d, %v", count, err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_temp_schema WHERE name='pending_grams'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("staging not removed: %d, %v", count, err)
	}
}

func TestPostingFailuresRollBack(t *testing.T) {
	for _, mode := range []string{"staging", "materializing"} {
		t.Run(mode, func(t *testing.T) {
			path, c, old := newCatalog(t)
			ctx := t.Context()
			q := Query{Title: "matrix", Kind: Movie}
			before, err := c.Candidates(ctx, q, SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			db, err := openDB(ctx, path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			w, err := newDatasetWriter(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			defer w.close()
			if _, err := tx.ExecContext(ctx, "UPDATE titles SET primary_title='Uncommitted'; DELETE FROM gram_counts; DELETE FROM search_grams"); err != nil {
				t.Fatal(err)
			}
			if mode == "staging" {
				if _, err := tx.ExecContext(ctx, "DROP TABLE temp.pending_grams"); err != nil {
					t.Fatal(err)
				}
				_, err = w.gram.ExecContext(ctx, Movie, 1, `["aaa"]`)
			} else {
				// Staging deliberately has no foreign key or B-tree maintenance.
				// The final insertion must still enforce all catalog constraints.
				if _, err := w.gram.ExecContext(ctx, Movie, 1, `["aaa"]`); err != nil {
					t.Fatal(err)
				}
				if _, err := w.gram.ExecContext(ctx, Movie, 999999, `["zzz"]`); err != nil {
					t.Fatal(err)
				}
				_, err = tx.ExecContext(ctx, indexes)
			}
			if err == nil {
				t.Fatal("expected posting failure")
			}
			if mode == "materializing" && !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Fatalf("expected final foreign-key enforcement, got %v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			current, err := c.Info(ctx)
			if err != nil || !reflect.DeepEqual(current, old) {
				t.Fatalf("metadata changed: %+v, %v", current, err)
			}
			title, err := c.Lookup(ctx, "tt1")
			if err != nil || title.PrimaryTitle != "The Matrix" {
				t.Fatalf("facts changed: %+v, %v", title, err)
			}
			var count int
			if err := c.db.QueryRow("SELECT count(*) FROM search_grams").Scan(&count); err != nil || count == 0 {
				t.Fatalf("postings lost: %d, %v", count, err)
			}
			after, err := c.Candidates(ctx, q, SearchOptions{})
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("search changed after rollback: %+v, %v", after, err)
			}
			// A fresh attempt can reuse the same writer connection after rollback.
			retryTx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = retryTx.Rollback() }()
			retryWriter, err := newDatasetWriter(ctx, retryTx)
			if err != nil {
				t.Fatalf("stale staging after rollback: %v", err)
			}
			retryWriter.close()
		})
	}
}
