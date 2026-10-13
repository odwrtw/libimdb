package imdb

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const normalizationVersion = 1

// Safe normalization does not remove punctuation, accents, or words.
func nameKey(s string) string {
	return norm.NFC.String(cases.Fold().String(strings.Join(strings.Fields(s), " ")))
}

// Lossy keys are used only for candidate suggestions, never for Resolve.
func fuzzyKey(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(nameKey(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func grams(s string) []string {
	runes := []rune(s)
	seen := make(map[string]bool)
	out := make([]string, 0)
	for i := 0; i+3 <= len(runes); i++ {
		g := string(runes[i : i+3])
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

func validateQuery(q Query) error {
	if !utf8.ValidString(q.Title) || strings.ContainsRune(q.Title, 0) ||
		len(q.Title) > 1024 || utf8.RuneCountInString(nameKey(q.Title)) > 256 || nameKey(q.Title) == "" || q.Year < 0 ||
		(q.Kind != "" && q.Kind != Movie && q.Kind != Show) {
		return ErrInvalidQuery
	}
	return nil
}

// editDistance is rune-aware and only used on the bounded candidate set.
func editDistance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	next := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, x := range a {
		next[0] = i + 1
		for j, y := range b {
			cost := 0
			if x != y {
				cost = 1
			}
			next[j+1] = min(prev[j+1]+1, next[j]+1, prev[j]+cost)
		}
		prev, next = next, prev
	}
	return prev[len(b)]
}
