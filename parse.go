package imdb

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxRowBytes = 1024 * 1024

var headers = map[Dataset]string{
	Basics:  "tconst\ttitleType\tprimaryTitle\toriginalTitle\tisAdult\tstartYear\tendYear\truntimeMinutes\tgenres",
	Aliases: "titleId\tordering\ttitle\tregion\tlanguage\ttypes\tattributes\tisOriginalTitle",
	Ratings: "tconst\taverageRating\tnumVotes",
}

func parseGzip(ctx context.Context, d Dataset, input io.Reader, w *datasetWriter, progress *progressReporter) (int64, error) {
	gz, err := gzip.NewReader(input)
	if err != nil {
		return 0, err
	}
	defer func() { _ = gz.Close() }()
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 64*1024), maxRowBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("missing TSV header")
	}
	if scanner.Text() != headers[d] {
		return 0, fmt.Errorf("unexpected TSV header")
	}
	var rows int64
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return rows, err
		}
		rows++
		line := scanner.Text()
		if !utf8.ValidString(line) || strings.ContainsRune(line, 0) {
			return rows, fmt.Errorf("line %d: invalid text", rows+1)
		}
		row := strings.Split(line, "\t")
		if len(row) != strings.Count(headers[d], "\t")+1 {
			return rows, fmt.Errorf("line %d: unexpected field count", rows+1)
		}
		if !validID(row[0]) {
			return rows, fmt.Errorf("line %d: invalid IMDb ID", rows+1)
		}
		var err error
		switch d {
		case Basics:
			err = w.basics(ctx, row)
		case Aliases:
			err = w.alias(ctx, row)
		case Ratings:
			err = w.ratings(ctx, row)
		}
		if err != nil {
			return rows, fmt.Errorf("line %d: %w", rows+1, err)
		}
		progress.rows(rows)
	}
	if err := scanner.Err(); err != nil {
		return rows, err
	}
	if err := ctx.Err(); err != nil {
		return rows, err
	}
	return rows, gz.Close()
}

func validID(id string) bool {
	if !strings.HasPrefix(id, "tt") || len(id) < 3 {
		return false
	}
	for _, c := range id[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func titleKind(t string) Kind {
	switch t {
	case "movie", "tvMovie":
		return Movie
	case "tvSeries", "tvMiniSeries":
		return Show
	default:
		return ""
	}
}

func textField(s string) string {
	if s == "\\N" {
		return ""
	}
	return s
}

func intField(s string) (*int, error) {
	if s == "\\N" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid nonnegative integer %q", s)
	}
	return &n, nil
}

func boolField(s string) (*bool, error) {
	if s == "\\N" {
		return nil, nil
	}
	if s != "0" && s != "1" {
		return nil, fmt.Errorf("invalid boolean %q", s)
	}
	b := s == "1"
	return &b, nil
}

func listField(s, sep string) []string {
	if s == "\\N" {
		return nil
	}
	return strings.Split(s, sep)
}

func (w *datasetWriter) basics(ctx context.Context, r []string) error {
	kind := titleKind(r[1])
	if kind == "" {
		return nil
	}
	if textField(r[2]) == "" || nameKey(r[2]) == "" {
		return fmt.Errorf("missing primary title")
	}
	adult, err := boolField(r[4])
	if err != nil {
		return err
	}
	year, err := intField(r[5])
	if err != nil {
		return err
	}
	end, err := intField(r[6])
	if err != nil {
		return err
	}
	runtime, err := intField(r[7])
	if err != nil {
		return err
	}
	genres, err := jsonField(listField(r[8], ","))
	if err != nil {
		return err
	}
	_, err = w.title.ExecContext(ctx, r[0], kind, r[1], r[2], textField(r[3]), year, end, runtime, adult, genres)
	if err != nil {
		return err
	}
	if kind == Movie {
		w.counts.Movies++
	} else {
		w.counts.Shows++
	}
	if err := w.addName(ctx, r[0], Name{Title: r[2], Source: "primary"}); err != nil {
		return err
	}
	if textField(r[3]) != "" && r[3] != r[2] {
		return w.addName(ctx, r[0], Name{Title: r[3], Source: "original"})
	}
	return nil
}

func (w *datasetWriter) alias(ctx context.Context, r []string) error {
	ordering, err := intField(r[1])
	if err != nil {
		return err
	}
	if ordering == nil || *ordering == 0 || textField(r[2]) == "" || nameKey(r[2]) == "" {
		return fmt.Errorf("invalid alias")
	}
	original, err := boolField(r[7])
	if err != nil {
		return err
	}
	return w.addName(ctx, r[0], Name{Title: r[2], Source: "alias", Region: textField(r[3]), Language: textField(r[4]),
		Types: listField(r[5], "\x02"), Attributes: listField(r[6], "\x02"), IsOriginal: original})
}

func (w *datasetWriter) ratings(ctx context.Context, r []string) error {
	rating, err := strconv.ParseFloat(r[1], 64)
	if err != nil || math.IsNaN(rating) || math.IsInf(rating, 0) || rating < 0 || rating > 10 {
		return fmt.Errorf("invalid rating %q", r[1])
	}
	votes, err := strconv.ParseInt(r[2], 10, 64)
	if err != nil || votes < 0 {
		return fmt.Errorf("invalid vote count %q", r[2])
	}
	result, err := w.rating.ExecContext(ctx, rating, votes, r[0])
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists bool
		if err := w.exists.QueryRowContext(ctx, r[0]).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("duplicate rating for %s", r[0])
		}
	}
	w.counts.Ratings += n
	return nil
}
