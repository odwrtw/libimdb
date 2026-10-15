package imdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Catalog is a concurrency-safe read-only handle. Each operation reads one
// transaction snapshot; later operations automatically observe committed updates.
// Close releases its resources. Do not share the database over a network filesystem.
type Catalog struct{ db *sql.DB }

// Open opens an existing catalog read-only and validates its schema. It never
// creates a missing file. Path must be a filesystem path, not a SQLite URI.
func Open(ctx context.Context, path string) (*Catalog, error) {
	db, err := openDB(ctx, path, false)
	if err != nil {
		return nil, err
	}
	c := &Catalog{db: db}
	if _, err := c.Info(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

// Close releases the catalog's resources and prevents new operations. Finish
// View callbacks before closing the Catalog.
func (c *Catalog) Close() error { return c.db.Close() }

func (c *Catalog) newView(ctx context.Context) (*View, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	info, err := readInfo(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &View{tx: tx, info: info, ctx: ctx}, nil
}

// Info returns metadata for the operation's committed snapshot.
func (c *Catalog) Info(ctx context.Context) (Info, error) {
	v, err := c.newView(ctx)
	if err != nil {
		return Info{}, err
	}
	defer v.close()
	return v.Info(ctx)
}

const titleColumns = "t.imdb_id,t.kind,t.title_type,t.primary_title,t.original_title,t.year,t.end_year,t.runtime,t.adult,t.genres,t.rating,t.votes"

type scanner interface{ Scan(...any) error }

func titleArgs(t *Title, genres *sql.NullString) []any {
	return []any{&t.ID, &t.Kind, &t.TitleType, &t.PrimaryTitle, &t.OriginalTitle, &t.StartYear, &t.EndYear, &t.RuntimeMinutes, &t.Adult, genres, &t.Rating, &t.Votes}
}

func decodeList(s sql.NullString, dst *[]string) error {
	if !s.Valid {
		return nil
	}
	return json.Unmarshal([]byte(s.String), dst)
}

func scanTitle(row scanner) (Title, error) {
	var t Title
	var genres sql.NullString
	if err := row.Scan(titleArgs(&t, &genres)...); err != nil {
		return Title{}, err
	}
	if err := decodeList(genres, &t.Genres); err != nil {
		return Title{}, err
	}
	return t, nil
}

// Lookup is like Catalog.Lookup, using this view's snapshot.
func (v *View) Lookup(ctx context.Context, id string) (Title, error) {
	if err := v.check(ctx); err != nil {
		return Title{}, err
	}
	if !validID(id) {
		return Title{}, invalidID("id")
	}
	t, err := scanTitle(v.tx.QueryRowContext(ctx, "SELECT "+titleColumns+" FROM titles t WHERE t.imdb_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Title{}, ErrNotFound
	}
	return t, err
}

// LookupMany is like Catalog.LookupMany, using this view's snapshot.
func (v *View) LookupMany(ctx context.Context, ids []string) (map[string]Title, error) {
	if err := v.check(ctx); err != nil {
		return nil, err
	}
	if len(ids) > 10000 {
		return nil, &ValidationError{Field: "ids", Problem: "at most 10000 IDs allowed"}
	}
	for i, id := range ids {
		if !validID(id) {
			return nil, invalidID(fmt.Sprintf("ids[%d]", i))
		}
	}
	result := make(map[string]Title)
	if len(ids) == 0 {
		return result, nil
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := v.tx.QueryContext(ctx, "SELECT "+titleColumns+" FROM titles t WHERE t.imdb_id IN (SELECT value FROM json_each(?))", string(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t, err := scanTitle(rows)
		if err != nil {
			return nil, err
		}
		result[t.ID] = t
	}
	return result, rows.Err()
}

const nameColumns = "n.title,n.source,n.region,n.language,n.types,n.attributes,n.is_original"

func nameArgs(n *Name, types, attrs *sql.NullString) []any {
	return []any{&n.Title, &n.Source, &n.Region, &n.Language, types, attrs, &n.IsOriginal}
}

// Names is like Catalog.Names, using this view's snapshot.
func (v *View) Names(ctx context.Context, id string) ([]Name, error) {
	if err := v.check(ctx); err != nil {
		return nil, err
	}
	if !validID(id) {
		return nil, invalidID("id")
	}
	rows, err := v.tx.QueryContext(ctx, "SELECT "+nameColumns+" FROM names n WHERE n.imdb_id=? ORDER BY n.id", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]Name, 0)
	for rows.Next() {
		var n Name
		var types, attrs sql.NullString
		if err := rows.Scan(nameArgs(&n, &types, &attrs)...); err != nil {
			return nil, err
		}
		if err := decodeList(types, &n.Types); err != nil {
			return nil, err
		}
		if err := decodeList(attrs, &n.Attributes); err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, ErrNotFound
	}
	return result, nil
}

func queryFilters(q Query) (string, []any) {
	var where string
	var args []any
	if q.Kind != "" {
		where += " AND t.kind=?"
		args = append(args, q.Kind)
	}
	if q.StartYear != 0 {
		where += " AND t.year=?"
		args = append(args, q.StartYear)
	}
	if q.ExcludeAdult {
		where += " AND coalesce(t.adult,0)=0"
	}
	return where, args
}

const ambiguityLimit = 20

func resolveIDsQuery(q Query) (string, []any) {
	where, filters := queryFilters(q)
	return "SELECT DISTINCT n.imdb_id FROM names n JOIN titles t ON t.imdb_id=n.imdb_id WHERE n.name_key=?" + where + " ORDER BY n.imdb_id LIMIT ?", append(append([]any{nameKey(q.Title)}, filters...), ambiguityLimit+1)
}

type namedTitle struct {
	match  Candidate
	key    string
	folded string
}

func scanNamedTitle(row scanner) (namedTitle, error) {
	var result namedTitle
	var genres, types, attrs sql.NullString
	args := titleArgs(&result.match.Title, &genres)
	args = append(args, nameArgs(&result.match.Name, &types, &attrs)...)
	args = append(args, &result.key, &result.folded)
	if err := row.Scan(args...); err != nil {
		return namedTitle{}, err
	}
	for _, item := range []struct {
		s   sql.NullString
		dst *[]string
	}{
		{genres, &result.match.Title.Genres}, {types, &result.match.Name.Types}, {attrs, &result.match.Name.Attributes},
	} {
		if err := decodeList(item.s, item.dst); err != nil {
			return namedTitle{}, err
		}
	}
	return result, nil
}

const namedColumns = titleColumns + "," + nameColumns + ",n.name_key,n.fuzzy_key"

// Resolve is like Catalog.Resolve, using this view's snapshot, including any
// conflicts returned in AmbiguousError.
func (v *View) Resolve(ctx context.Context, q Query) (Resolution, error) {
	if err := v.check(ctx); err != nil {
		return Resolution{}, err
	}
	if err := validateQuery(q); err != nil {
		return Resolution{}, err
	}
	key := nameKey(q.Title)
	query, args := resolveIDsQuery(q)
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Resolution{}, err
	}
	ids := make([]string, 0, ambiguityLimit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return Resolution{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return Resolution{}, err
	}
	if closeErr != nil {
		return Resolution{}, closeErr
	}
	if len(ids) == 0 {
		return Resolution{}, ErrNotFound
	}
	if len(ids) > 1 {
		conflicts := &AmbiguousError{Generation: v.info.Generation, Truncated: len(ids) > ambiguityLimit}
		for _, id := range ids[:min(len(ids), ambiguityLimit)] {
			title, err := v.Lookup(ctx, id)
			if err != nil {
				return Resolution{}, err
			}
			conflicts.Titles = append(conflicts.Titles, title)
		}
		return Resolution{}, conflicts
	}
	row := v.tx.QueryRowContext(ctx, "SELECT "+namedColumns+" FROM names n JOIN titles t ON t.imdb_id=n.imdb_id WHERE n.name_key=? AND t.imdb_id=? ORDER BY CASE WHEN n.title=? THEN 0 ELSE 1 END, CASE n.source WHEN 'primary' THEN 0 WHEN 'original' THEN 1 ELSE 2 END,n.id LIMIT 1", key, ids[0], q.Title)
	result, err := scanNamedTitle(row)
	if err != nil {
		return Resolution{}, err
	}
	method := Normalized
	if result.match.Name.Title == q.Title {
		method = Exact
	}
	return Resolution{Title: result.match.Title, Name: result.match.Name, Method: method, Generation: v.info.Generation}, nil
}

func queryError(err error) error {
	return fmt.Errorf("imdb: candidate query: %w", err)
}
