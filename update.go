package imdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

type UpdateOptions struct {
	Source     Source        // nil uses the official HTTPSource
	Attempts   int           // default 1, maximum 5; retries restart the entire transaction
	RetryDelay time.Duration // default one second
	// Progress receives synchronous snapshots, including lifecycle and retry
	// events. It must return promptly and must not reenter Update. Ordinary
	// counters are throttled to about four events/second; boundaries are always
	// reported. Nil disables reporting. Cancel the context to stop an update.
	Progress func(Progress)
}

// Update creates or refreshes a catalog in one transaction, without dataset
// temporary files or whole-download buffers. The catalog must be owned by this
// library or empty. Existing legacy/foreign databases are never overwritten.
// WAL is used for concurrent readers; SQLite's own transaction files are required.
// A failed update preserves previous committed data; a failed first creation may
// leave an empty, uninitialized database file.
func Update(ctx context.Context, path string, opts UpdateOptions) (Info, error) {
	if opts.Attempts == 0 {
		opts.Attempts = 1
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = time.Second
	}
	progress := &progressReporter{callback: opts.Progress, now: time.Now, current: Progress{Attempts: opts.Attempts}}
	fail := func(err error) (Info, error) {
		progress.phase(Failed, err)
		return Info{}, err
	}
	if opts.Attempts < 1 || opts.Attempts > 5 || opts.RetryDelay < 0 {
		return fail(fmt.Errorf("imdb: invalid update options"))
	}
	if opts.Source == nil {
		opts.Source = HTTPSource{}
	}
	for attempt := 1; ; attempt++ {
		progress.current.Attempt = attempt
		progress.phase(Preparing, nil)
		info, err := updateOnce(ctx, path, opts, progress)
		if err == nil {
			progress.phase(Done, nil)
			return info, nil
		}
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if attempt >= opts.Attempts || !retryable(err) {
			return fail(err)
		}
		progress.phase(Retrying, err)
		timer := time.NewTimer(opts.RetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail(ctx.Err())
		case <-timer.C:
		}
	}
}

func retryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.ErrUnexpectedEOF)
}

func updateOnce(ctx context.Context, path string, opts UpdateOptions, progress *progressReporter) (Info, error) {
	db, err := openDB(ctx, path, true)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = db.Close() }()
	if _, err := validateWriter(ctx, db); err != nil {
		return Info{}, err
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return Info{}, err
	}
	if mode != "wal" {
		return Info{}, fmt.Errorf("imdb: WAL unavailable: %s", mode)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = tx.Rollback() }()
	create, err := validateWriter(ctx, tx)
	if err != nil {
		return Info{}, err
	}
	if create {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return Info{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			DROP INDEX names_by_key;
			DROP INDEX names_by_fuzzy_key;
			DROP INDEX names_by_title;
			DELETE FROM name_grams;
			DELETE FROM names;
			DELETE FROM titles;
			DELETE FROM catalog_info;`); err != nil {
			return Info{}, err
		}
	}

	writer, err := newDatasetWriter(ctx, tx)
	if err != nil {
		return Info{}, err
	}
	defer writer.close()
	info := Info{SchemaVersion: schemaVersion, NormalizationVersion: normalizationVersion, Generation: rand.Text()}
	for _, d := range []Dataset{Basics, Aliases, Ratings} {
		src, err := importDataset(ctx, d, opts, writer, progress)
		if err != nil {
			return Info{}, fmt.Errorf("imdb: %s: %w", d, err)
		}
		info.Sources = append(info.Sources, src)
	}
	info.Counts = writer.counts
	if info.Counts.Movies+info.Counts.Shows == 0 {
		return Info{}, fmt.Errorf("imdb: empty title dataset")
	}
	progress.phase(Indexing, nil)
	if _, err := tx.ExecContext(ctx, indexes); err != nil {
		return Info{}, err
	}
	progress.phase(Validating, nil)
	var problem string
	if err := tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&problem); err != nil {
		return Info{}, err
	}
	if problem != "ok" {
		return Info{}, fmt.Errorf("imdb: integrity check: %s", problem)
	}
	info.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(info)
	if err != nil {
		return Info{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO catalog_info(singleton,info) VALUES(1,?)", string(data)); err != nil {
		return Info{}, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d; PRAGMA user_version=%d", applicationID, schemaVersion)); err != nil {
		return Info{}, err
	}
	progress.phase(Committing, nil)
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := tx.Commit(); err != nil {
		return Info{}, fmt.Errorf("imdb: commit: %w", err)
	}
	return info, nil
}

func importDataset(ctx context.Context, d Dataset, opts UpdateOptions, w *datasetWriter, progress *progressReporter) (SourceInfo, error) {
	progress.opening(d)
	if err := ctx.Err(); err != nil {
		return SourceInfo{}, err
	}
	body, info, err := opts.Source.Open(ctx, d)
	if err != nil {
		return SourceInfo{}, err
	}
	if body == nil {
		return SourceInfo{}, fmt.Errorf("source returned a nil stream")
	}
	closed, complete := false, false
	defer func() {
		if !closed {
			_ = body.Close()
		}
		if !complete {
			progress.emit(true) // final counters, even when parsing or closure failed
		}
	}()
	info.SizeBytes = max(info.SizeBytes, 0)
	progress.importing(info.SizeBytes)
	hash := sha256.New()
	reader := io.TeeReader(contextReader{ctx: ctx, reader: body, progress: progress}, hash)
	rows, err := parseGzip(ctx, d, reader, w, progress)
	if err != nil {
		return SourceInfo{}, err
	}
	// The parser consumes through EOF, including gzip checksum validation.
	closeErr := body.Close()
	closed = true
	if closeErr != nil {
		return SourceInfo{}, closeErr
	}
	info.Dataset, info.Rows = d, rows
	info.SHA256 = hex.EncodeToString(hash.Sum(nil))
	complete = true
	progress.current.Phase = DatasetDone
	progress.emit(true)
	return info, nil
}

type contextReader struct {
	ctx      context.Context
	reader   io.Reader
	progress *progressReporter
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.progress.bytes(n)
	return n, err
}

// Keep all database writes on this transaction; there are no nested DB queries.
type datasetWriter struct {
	title  *sql.Stmt
	name   *sql.Stmt
	gram   *sql.Stmt
	rating *sql.Stmt
	exists *sql.Stmt
	counts Counts
}

func newDatasetWriter(ctx context.Context, tx *sql.Tx) (*datasetWriter, error) {
	w := &datasetWriter{}
	for _, p := range []struct {
		target **sql.Stmt
		query  string
	}{
		{&w.title, "INSERT INTO titles(imdb_id,kind,title_type,primary_title,original_title,year,end_year,runtime,adult,genres) VALUES(?,?,?,?,?,?,?,?,?,?)"},
		{&w.name, "INSERT INTO names(imdb_id,title,name_key,fuzzy_key,source,region,language,types,attributes,is_original) SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM titles WHERE imdb_id=?)"},
		{&w.gram, "INSERT INTO name_grams(gram,name_id) SELECT value,? FROM json_each(?)"},
		{&w.rating, "UPDATE titles SET rating=?,votes=? WHERE imdb_id=? AND rating IS NULL"},
		{&w.exists, "SELECT EXISTS(SELECT 1 FROM titles WHERE imdb_id=?)"},
	} {
		stmt, err := tx.PrepareContext(ctx, p.query)
		if err != nil {
			w.close()
			return nil, err
		}
		*p.target = stmt
	}
	return w, nil
}

func (w *datasetWriter) close() {
	for _, stmt := range []*sql.Stmt{w.title, w.name, w.gram, w.rating, w.exists} {
		if stmt != nil {
			_ = stmt.Close()
		}
	}
}

func (w *datasetWriter) addName(ctx context.Context, id string, n Name) error {
	types, err := jsonField(n.Types)
	if err != nil {
		return err
	}
	attrs, err := jsonField(n.Attributes)
	if err != nil {
		return err
	}
	folded := fuzzyKey(n.Title)
	result, err := w.name.ExecContext(ctx, id, n.Title, nameKey(n.Title), folded, n.Source, n.Region, n.Language, types, attrs, n.IsOriginal, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return nil
	}
	w.counts.Names++
	nameID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	data, err := json.Marshal(grams(folded))
	if err != nil {
		return err
	}
	_, err = w.gram.ExecContext(ctx, nameID, string(data))
	return err
}

func jsonField(v []string) (any, error) {
	if v == nil {
		return nil, nil
	}
	data, err := json.Marshal(v)
	return string(data), err
}
