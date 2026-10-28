package imdb

import "testing"

func TestFuzzyKeyFromNameKey(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"AMÉLIE", "amelie"},
		{"Ame\u0301lie", "amelie"},
		{"Straße", "strasse"},
		{"İstanbul", "istanbul"},
		{"君の名は。", "君の名は"},
		{"  A\tB—C! ", "a b c"},
		{"...", ""},
		{"①", "①"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := fuzzyKeyFromNameKey(nameKey(tc.raw)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if got := fuzzyKey(tc.raw); got != tc.want {
				t.Fatalf("query key %q, want %q", got, tc.want)
			}
		})
	}
}
