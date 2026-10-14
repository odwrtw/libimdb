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

func (c *Catalog) Close() error { return c.db.Close() }

func (c *Catalog) read(ctx context.Context) (*sql.Tx, Info, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, Info{}, err
	}
	info, err := readInfo(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return nil, Info{}, err
	}
	return tx, info, nil
}

func (c *Catalog) Info(ctx context.Context) (Info, error) {
	tx, info, err := c.read(ctx)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return info, nil
}

const titleColumns = "t.imdb_id,t.kind,t.title_type,t.primary_title,t.original_title,t.year,t.end_year,t.runtime,t.adult,t.genres,t.rating,t.votes"

type scanner interface{ Scan(...any) error }

func titleArgs(t *Title, genres *sql.NullString) []any {
	return []any{&t.ID, &t.Kind, &t.Type, &t.PrimaryTitle, &t.OriginalTitle, &t.Year, &t.EndYear, &t.Runtime, &t.Adult, genres, &t.Rating, &t.Votes}
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

func (c *Catalog) Lookup(ctx context.Context, id string) (Title, error) {
	if !validID(id) {
		return Title{}, ErrInvalidQuery
	}
	tx, _, err := c.read(ctx)
	if err != nil {
		return Title{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := scanTitle(tx.QueryRowContext(ctx, "SELECT "+titleColumns+" FROM titles t WHERE t.imdb_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Title{}, ErrNotFound
	}
	return t, err
}

// LookupMany returns found titles keyed by ID, omitting absent IDs. All results
// belong to one snapshot. At most 10000 IDs may be requested.
func (c *Catalog) LookupMany(ctx context.Context, ids []string) (map[string]Title, error) {
	if len(ids) > 10000 {
		return nil, ErrInvalidQuery
	}
	for _, id := range ids {
		if !validID(id) {
			return nil, ErrInvalidQuery
		}
	}
	result := make(map[string]Title)
	if len(ids) == 0 {
		return result, nil
	}
	tx, _, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+titleColumns+" FROM titles t WHERE t.imdb_id IN (SELECT value FROM json_each(?))", string(data))
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

func (c *Catalog) Names(ctx context.Context, id string) ([]Name, error) {
	if !validID(id) {
		return nil, ErrInvalidQuery
	}
	tx, _, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, "SELECT "+nameColumns+" FROM names n WHERE n.imdb_id=? ORDER BY n.id", id)
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
	if q.Year != 0 {
		where += " AND t.year=?"
		args = append(args, q.Year)
	}
	if q.ExcludeAdult {
		where += " AND coalesce(t.adult,0)=0"
	}
	return where, args
}

func resolveIDsQuery(q Query) (string, []any) {
	where, filters := queryFilters(q)
	return "SELECT DISTINCT n.imdb_id FROM names n JOIN titles t ON t.imdb_id=n.imdb_id WHERE n.name_key=?" + where + " LIMIT 2", append([]any{nameKey(q.Title)}, filters...)
}

type namedTitle struct {
	match  Match
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

// Resolve never uses fuzzy or popularity ranking. All aliases sharing the safe
// normalized key participate in ambiguity detection, even if a primary name is
// an exact spelling match. A specified year never falls back to another year.
func (c *Catalog) Resolve(ctx context.Context, q Query) (Match, error) {
	if err := validateQuery(q); err != nil {
		return Match{}, err
	}
	tx, _, err := c.read(ctx)
	if err != nil {
		return Match{}, err
	}
	defer func() { _ = tx.Rollback() }()
	key := nameKey(q.Title)
	query, args := resolveIDsQuery(q)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Match{}, err
	}
	ids := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return Match{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return Match{}, err
	}
	if closeErr != nil {
		return Match{}, closeErr
	}
	switch len(ids) {
	case 0:
		return Match{}, ErrNotFound
	case 2:
		return Match{}, ErrAmbiguous
	}
	row := tx.QueryRowContext(ctx, "SELECT "+namedColumns+" FROM names n JOIN titles t ON t.imdb_id=n.imdb_id WHERE n.name_key=? AND t.imdb_id=? ORDER BY CASE WHEN n.title=? THEN 0 ELSE 1 END, CASE n.source WHEN 'primary' THEN 0 WHEN 'original' THEN 1 ELSE 2 END,n.id LIMIT 1", key, ids[0], q.Title)
	result, err := scanNamedTitle(row)
	if err != nil {
		return Match{}, err
	}
	result.match.Method = Normalized
	if result.match.Name.Title == q.Title {
		result.match.Method = Exact
	}
	result.match.Score = 1
	return result.match, nil
}

func queryError(err error) error {
	return fmt.Errorf("imdb: candidate query: %w", err)
}
