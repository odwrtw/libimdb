package imdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

type candidate struct {
	match  Match // Only identity, ranking and filter fields until final hydration.
	folded string
	tied   []string // Equally ranked folded names awaiting raw-name tie-breaks.
}

type candidateQuery struct {
	query string
	args  []any
	typo  bool
}

// Candidates returns bounded exact, normalized, prefix and substring suggestions.
// Substrings require three folded Unicode characters. Whole-title typo search
// (four or more characters) runs only when direct results do not fill Limit.
// Retrieval is heuristic; Truncated reports budget/result clipping, not recall.
// Use Resolve, never candidate rank or count, for authoritative identity.
func (c *Catalog) Candidates(ctx context.Context, q Query, opts SearchOptions) (SearchResult, error) {
	if err := validateQuery(q); err != nil {
		return SearchResult{}, err
	}
	if opts.Limit == 0 {
		opts.Limit = 20
	}
	if opts.Budget == 0 {
		opts.Budget = 1000
	}
	if opts.Limit < 1 || opts.Limit > 100 || opts.Budget < 1 || opts.Budget > 10000 {
		return SearchResult{}, ErrInvalidQuery
	}
	tx, info, err := c.read(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result := SearchResult{Matches: make([]Match, 0), Generation: info.Generation}
	found := make(map[string]candidate)
	key, folded := nameKey(q.Title), fuzzyKey(q.Title)
	run := func(stage candidateQuery, budget int) (int, error) {
		matches, inspected, clipped, err := candidateStage(ctx, tx, q, key, folded, stage, budget)
		if err != nil {
			return 0, err
		}
		result.Truncated = result.Truncated || clipped
		for _, m := range matches {
			old, exists := found[m.match.Title.ID]
			rank := compareMatches(m.match, old.match)
			if !exists || rank < 0 {
				found[m.match.Title.ID] = m
			} else if rank == 0 && m.folded != old.folded && !slices.Contains(old.tied, m.folded) {
				old.tied = append(old.tied, m.folded)
				found[m.match.Title.ID] = old
			}
		}
		return inspected, nil
	}
	if _, err := run(exactCandidatesQuery(q, key, folded, opts.Budget), opts.Budget); err != nil {
		return SearchResult{}, err
	}
	if folded != "" {
		if _, err := run(prefixCandidatesQuery(q, key, folded, opts.Budget), opts.Budget); err != nil {
			return SearchResult{}, err
		}
	}
	var seeds []gramCount
	if utf8.RuneCountInString(folded) >= 3 {
		g := grams(folded)
		seeds, err = candidateGrams(ctx, tx, q.Kind, g)
		if err != nil {
			return SearchResult{}, err
		}
		// A substring must contain every query gram. Missing grams prove absence;
		// otherwise the rarest gram supplies all possible substring entries.
		if len(seeds) == len(g) {
			if _, err := run(gramCandidatesQuery(q, key, folded, seeds[0].gram, opts.Budget, false), opts.Budget); err != nil {
				return SearchResult{}, err
			}
		}
	}
	if len(found) < opts.Limit && utf8.RuneCountInString(folded) >= 4 {
		// Inspect at most three rare posting streams, sharing one stage budget.
		// Unlike overlap aggregation, SQL never reads all common-gram postings.
		remaining := opts.Budget
		for _, seed := range seeds[:min(3, len(seeds))] {
			if remaining == 0 {
				result.Truncated = true
				break
			}
			inspected, err := run(gramCandidatesQuery(q, key, folded, seed.gram, remaining, true), remaining)
			if err != nil {
				return SearchResult{}, err
			}
			remaining -= inspected
		}
	}
	ordered := make([]candidate, 0, len(found))
	for _, m := range found {
		ordered = append(ordered, m)
	}
	slices.SortFunc(ordered, func(a, b candidate) int { return compareMatches(a.match, b.match) })
	if len(ordered) > opts.Limit {
		result.Truncated = true
		ordered = ordered[:opts.Limit]
	}
	// Decode genres and alias metadata only for the final distinct IMDb IDs.
	for _, m := range ordered {
		where := "n.fuzzy_key=?"
		var names any = m.folded
		if len(m.tied) != 0 {
			data, err := json.Marshal(append([]string{m.folded}, m.tied...))
			if err != nil {
				return SearchResult{}, err
			}
			where = "n.fuzzy_key IN (SELECT value FROM json_each(?))"
			names = string(data)
		}
		row := tx.QueryRowContext(ctx, "SELECT "+namedColumns+" FROM names n JOIN titles t ON t.imdb_id=n.imdb_id WHERE n.imdb_id=? AND "+where+" ORDER BY CASE WHEN n.title=? THEN 0 ELSE 1 END, CASE WHEN n.name_key=? THEN 0 ELSE 1 END,n.source,n.title,n.id LIMIT 1", m.match.Title.ID, names, q.Title, key)
		n, err := scanNamedTitle(row)
		if err != nil {
			return SearchResult{}, err
		}
		if !scoreMatch(q, &n.match, n.key, n.folded, key, folded) {
			return SearchResult{}, ErrSchema
		}
		result.Matches = append(result.Matches, n.match)
	}
	slices.SortFunc(result.Matches, compareMatches)
	return result, nil
}

func exactCandidatesQuery(q Query, key, folded string, budget int) candidateQuery {
	return candidateQuery{
		query: `WITH seeds AS MATERIALIZED (
			SELECT DISTINCT imdb_id FROM names WHERE name_key=? ORDER BY imdb_id LIMIT ?
		) SELECT t.imdb_id,t.kind,t.year,t.adult,t.votes,?,?,1,
		EXISTS(SELECT 1 FROM names n WHERE n.imdb_id=t.imdb_id AND n.name_key=? AND n.title=?)
		FROM seeds JOIN titles t ON t.imdb_id=seeds.imdb_id ORDER BY t.imdb_id`,
		args: []any{key, budget + 1, folded, utf8.RuneCountInString(folded), key, q.Title},
	}
}

func prefixCandidatesQuery(q Query, key, folded string, budget int) candidateQuery {
	where := "fuzzy_key>=? AND fuzzy_key<?"
	args := []any{folded, folded + "\U0010ffff"}
	if q.Kind != "" {
		where += " AND kind=?"
		args = append(args, q.Kind)
	}
	args = append(args, budget+1)
	return compactCandidatesQuery(q, key, folded, "SELECT id FROM search_names WHERE "+where+" ORDER BY fuzzy_key,id LIMIT ?", args, "s.fuzzy_key,s.id")
}

func gramCandidatesQuery(q Query, key, folded, gram string, budget int, typo bool) candidateQuery {
	where := "gram=?"
	args := []any{gram}
	if q.Kind != "" {
		where += " AND kind=?"
		args = append(args, q.Kind)
	}
	args = append(args, budget+1)
	stage := compactCandidatesQuery(q, key, folded, "SELECT search_id AS id FROM search_grams WHERE "+where+" ORDER BY kind,search_id LIMIT ?", args, "s.kind,s.id")
	stage.typo = typo
	return stage
}

func compactCandidatesQuery(q Query, key, folded, seeds string, args []any, order string) candidateQuery {
	return candidateQuery{
		query: "WITH seeds AS MATERIALIZED (" + seeds + `)
		SELECT s.imdb_id,t.kind,t.year,t.adult,t.votes,s.fuzzy_key,s.rune_length,
		CASE WHEN s.fuzzy_key=? THEN EXISTS(SELECT 1 FROM names n WHERE n.imdb_id=s.imdb_id AND n.fuzzy_key=s.fuzzy_key AND n.name_key=?) ELSE 0 END,
		CASE WHEN s.fuzzy_key=? THEN EXISTS(SELECT 1 FROM names n WHERE n.imdb_id=s.imdb_id AND n.fuzzy_key=s.fuzzy_key AND n.name_key=? AND n.title=?) ELSE 0 END
		FROM seeds JOIN search_names s ON s.id=seeds.id JOIN titles t ON t.imdb_id=s.imdb_id ORDER BY ` + order,
		args: append(args, folded, key, folded, key, q.Title),
	}
}

func candidateStage(ctx context.Context, tx *sql.Tx, q Query, key, folded string, stage candidateQuery, budget int) ([]candidate, int, bool, error) {
	rows, err := tx.QueryContext(ctx, stage.query, stage.args...)
	if err != nil {
		return nil, 0, false, queryError(err)
	}
	defer func() { _ = rows.Close() }()
	matches := make([]candidate, 0)
	count := 0
	queryLength := utf8.RuneCountInString(folded)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, count, false, err
		}
		if count == budget {
			return matches, count, true, nil
		}
		count++ // Includes entries rejected by year/adult/substring/typo filters.
		var n candidate
		var length int
		var normalized, exact bool
		t := &n.match.Title
		if err := rows.Scan(&t.ID, &t.Kind, &t.Year, &t.Adult, &t.Votes, &n.folded, &length, &normalized, &exact); err != nil {
			return nil, count, false, err
		}
		if (q.Kind != "" && q.Kind != t.Kind) || (q.Year != 0 && (t.Year == nil || *t.Year != q.Year)) || (q.ExcludeAdult && t.Adult != nil && *t.Adult) {
			continue
		}
		direct := normalized || strings.HasPrefix(n.folded, folded) || (queryLength >= 3 && strings.Contains(n.folded, folded))
		if !stage.typo && !direct {
			continue
		}
		if stage.typo && !direct && (length == 0 || length > 512 || float64(abs(queryLength-length))/float64(max(queryLength, length)) > 0.3) {
			continue
		}
		if exact {
			n.match.Name.Title = q.Title
		}
		var nameKey string
		if normalized {
			nameKey = key
		}
		if scoreMatch(q, &n.match, nameKey, n.folded, key, folded) {
			n.match.Name.Title = "" // Defer raw-name tie-breaks until hydration.
			matches = append(matches, n)
		}
	}
	return matches, count, false, rows.Err()
}

type gramCount struct {
	gram  string
	count int64
}

func candidateGrams(ctx context.Context, tx *sql.Tx, kind Kind, g []string) ([]gramCount, error) {
	data, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	where := "gram IN (SELECT value FROM json_each(?))"
	args := []any{string(data)}
	if kind != "" {
		where += " AND kind=?"
		args = append(args, kind)
	}
	// This aggregates at most two tiny frequency rows per gram, not postings.
	rows, err := tx.QueryContext(ctx, "SELECT gram,sum(entries) FROM gram_counts WHERE "+where+" GROUP BY gram", args...)
	if err != nil {
		return nil, queryError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]gramCount, 0, len(g))
	for rows.Next() {
		var seed gramCount
		if err := rows.Scan(&seed.gram, &seed.count); err != nil {
			return nil, err
		}
		result = append(result, seed)
	}
	slices.SortFunc(result, func(a, b gramCount) int {
		if a.count < b.count {
			return -1
		}
		if a.count > b.count {
			return 1
		}
		return strings.Compare(a.gram, b.gram)
	})
	return result, rows.Err()
}

func scoreMatch(q Query, m *Match, key, folded, queryKey, queryFold string) bool {
	switch {
	case key == queryKey:
		m.Method, m.Score = Normalized, 1
		if m.Name.Title == q.Title {
			m.Method = Exact
		}
		return true
	case folded == queryFold && folded != "":
		m.Method, m.Score = Fuzzy, 1
		return true
	case queryFold != "" && strings.HasPrefix(folded, queryFold):
		m.Method = Prefix
		m.Score = 0.8 + 0.1*float64(utf8.RuneCountInString(queryFold))/float64(utf8.RuneCountInString(folded))
		return true
	case utf8.RuneCountInString(queryFold) >= 3 && strings.Contains(folded, queryFold):
		m.Method = Substring
		m.Score = float64(utf8.RuneCountInString(queryFold)) / float64(utf8.RuneCountInString(folded))
		return true
	}
	aLen, bLen := utf8.RuneCountInString(queryFold), utf8.RuneCountInString(folded)
	if aLen < 4 || bLen == 0 || bLen > 512 || float64(abs(aLen-bLen))/float64(max(aLen, bLen)) > 0.3 {
		return false
	}
	distance := editDistance([]rune(queryFold), []rune(folded))
	score := 1 - float64(distance)/float64(max(aLen, bLen))
	if score < 0.7 {
		return false
	}
	m.Method, m.Score = Fuzzy, score
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func methodRank(m Method) int {
	switch m {
	case Exact:
		return 0
	case Normalized:
		return 1
	case Prefix:
		return 2
	case Substring:
		return 3
	default:
		return 4
	}
}

func compareMatches(a, b Match) int {
	if n := methodRank(a.Method) - methodRank(b.Method); n != 0 {
		return n
	}
	if a.Score != b.Score {
		if a.Score > b.Score {
			return -1
		}
		return 1
	}
	var av, bv int64
	if a.Title.Votes != nil {
		av = *a.Title.Votes
	}
	if b.Title.Votes != nil {
		bv = *b.Title.Votes
	}
	if av != bv {
		if av > bv {
			return -1
		}
		return 1
	}
	if a.Title.ID != b.Title.ID {
		return strings.Compare(a.Title.ID, b.Title.ID)
	}
	if a.Name.Source != b.Name.Source {
		return strings.Compare(a.Name.Source, b.Name.Source)
	}
	return strings.Compare(a.Name.Title, b.Name.Title)
}
