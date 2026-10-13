package imdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	schemaVersion = 1
	applicationID = 0x494d4442
)

const schema = `
CREATE TABLE titles (
	imdb_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK(kind IN ('movie','show')),
	title_type TEXT NOT NULL,
	primary_title TEXT NOT NULL,
	original_title TEXT NOT NULL,
	year INTEGER,
	end_year INTEGER,
	runtime INTEGER,
	adult INTEGER,
	genres TEXT,
	rating REAL,
	votes INTEGER
) WITHOUT ROWID;
CREATE TABLE names (
	id INTEGER PRIMARY KEY,
	imdb_id TEXT NOT NULL REFERENCES titles(imdb_id),
	title TEXT NOT NULL,
	name_key TEXT NOT NULL,
	fuzzy_key TEXT NOT NULL,
	source TEXT NOT NULL,
	region TEXT NOT NULL,
	language TEXT NOT NULL,
	types TEXT,
	attributes TEXT,
	is_original INTEGER
);
CREATE TABLE name_grams (
	gram TEXT NOT NULL,
	name_id INTEGER NOT NULL REFERENCES names(id),
	PRIMARY KEY(gram,name_id)
) WITHOUT ROWID;
CREATE TABLE catalog_info (
	singleton INTEGER PRIMARY KEY CHECK(singleton=1),
	info TEXT NOT NULL
);
`

const indexes = `
CREATE INDEX names_by_key ON names(name_key,imdb_id);
CREATE INDEX names_by_fuzzy_key ON names(fuzzy_key,id);
CREATE INDEX names_by_title ON names(imdb_id);
`

func openDB(ctx context.Context, path string, write bool) (*sql.DB, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return nil, fmt.Errorf("imdb: invalid database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	params := url.Values{}
	params.Set("mode", "ro")
	params.Add("_pragma", "busy_timeout(1000)")
	params.Add("_pragma", "foreign_keys(1)")
	if write {
		params.Set("mode", "rwc")
		params.Set("_txlock", "immediate")
		params.Add("_pragma", "synchronous(FULL)")
	}
	u.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("imdb: open database: %w", err)
	}
	return db, nil
}

type sqlReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readInfo(ctx context.Context, db sqlReader) (Info, error) {
	var app, version int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return Info{}, err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return Info{}, err
	}
	if app != applicationID || version != schemaVersion {
		return Info{}, ErrSchema
	}
	var data string
	if err := db.QueryRowContext(ctx, "SELECT info FROM catalog_info WHERE singleton=1").Scan(&data); err != nil {
		return Info{}, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	var info Info
	if err := json.Unmarshal([]byte(data), &info); err != nil {
		return Info{}, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	if info.SchemaVersion != schemaVersion || info.NormalizationVersion != normalizationVersion || info.Generation == "" {
		return Info{}, ErrSchema
	}
	return info, nil
}

// Empty databases are eligible for creation; arbitrary or legacy databases are not.
func validateWriter(ctx context.Context, db sqlReader) (bool, error) {
	var app, version, tables int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return false, err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	if app == 0 && version == 0 {
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Scan(&tables); err != nil {
			return false, err
		}
		if tables == 0 {
			return true, nil
		}
	}
	_, err := readInfo(ctx, db)
	return false, err
}
