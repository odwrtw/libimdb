package imdb

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"unicode/utf8"
)

type candidateQuery struct {
	join  string
	where string
	order string
	args  []any
}

// Candidates searches indexed normalized names, lossy prefixes, and overlapping
// trigrams. It is heuristic, not an exhaustive fuzzy scan. Truncated explicitly
// reports budget/result clipping; callers must never infer uniqueness from it.
// Resolve is the separate authoritative identity operation.
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
	found := make(map[string]Match)
	key, folded := nameKey(q.Title), fuzzyKey(q.Title)
	stages := []candidateQuery{{where: "n.name_key=?", order: "n.id", args: []any{key}}}
	if folded != "" {
		stages = append(stages, candidateQuery{
			where: "n.fuzzy_key>=? AND n.fuzzy_key<?", order: "n.fuzzy_key,n.id",
			args: []any{folded, folded + "\U0010ffff"},
		})
		if utf8.RuneCountInString(folded) >= 4 {
			g := grams(folded)
			args := make([]any, len(g))
			for i := range g {
				args[i] = g[i]
			}
			stages = append(stages, candidateQuery{
				join:  "JOIN (SELECT name_id,count(*) AS overlap FROM name_grams WHERE gram IN (" + placeholders(len(g)) + ") GROUP BY name_id) hits ON hits.name_id=n.id",
				where: "1", order: "hits.overlap DESC,n.id", args: args,
			})
		}
	}
	for _, stage := range stages {
		matches, clipped, err := candidateStage(ctx, tx, q, key, folded, stage, opts.Budget)
		if err != nil {
			return SearchResult{}, err
		}
		result.Truncated = result.Truncated || clipped
		for _, m := range matches {
			old, exists := found[m.Title.ID]
			if !exists || compareMatches(m, old) < 0 {
				found[m.Title.ID] = m
			}
		}
	}
	for _, m := range found {
		result.Matches = append(result.Matches, m)
	}
	slices.SortFunc(result.Matches, compareMatches)
	if len(result.Matches) > opts.Limit {
		result.Truncated = true
		result.Matches = result.Matches[:opts.Limit]
	}
	return result, nil
}

func candidateStage(ctx context.Context, tx *sql.Tx, q Query, key, folded string, stage candidateQuery, budget int) ([]Match, bool, error) {
	args := append(stage.args, filterArgs(q)...)
	args = append(args, budget+1)
	rows, err := tx.QueryContext(ctx, "SELECT "+namedColumns+" FROM names n "+stage.join+" JOIN titles t ON t.imdb_id=n.imdb_id WHERE ("+stage.where+")"+queryFilter+" ORDER BY "+stage.order+" LIMIT ?", args...)
	if err != nil {
		return nil, false, queryError(err)
	}
	defer func() { _ = rows.Close() }()
	matches := make([]Match, 0)
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		count++
		if count > budget {
			return matches, true, nil
		}
		n, err := scanNamedTitle(rows)
		if err != nil {
			return nil, false, err
		}
		if scoreMatch(q, &n.match, n.key, n.folded, key, folded) {
			matches = append(matches, n.match)
		}
	}
	return matches, false, rows.Err()
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
	default:
		return 3
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
