package imdb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Inputs are generated and compressed outside the timed loop. All cases use
// cancellable contexts, like the CLI. No live downloads or existing catalogs.
func updateBenchmarkDataset(size int, mode string) map[Dataset]string {
	var basics, aliases, ratings strings.Builder
	basics.WriteString(headers[Basics] + "\n")
	aliases.WriteString(headers[Aliases] + "\n")
	ratings.WriteString(headers[Ratings] + "\n")
	for i := 1; i <= size; i++ {
		kind := "movie"
		if i%4 == 0 {
			kind = "tvSeries"
		}
		name := fmt.Sprintf("Synthetic Catalog Movie %06d", i)
		if i%10 == 0 {
			name += " Amélie 君の名は。"
		}
		if i%100 == 0 {
			name += strings.Repeat(" long title", 20)
		}
		fmt.Fprintf(&basics, "tt%d\t%s\t%s\t%s\t0\t2000\t\\N\t90\tDrama\n", i, kind, name, name)
		fmt.Fprintf(&ratings, "tt%d\t7.5\t1000\n", i)
		if mode == "basics" {
			continue
		}
		aliasID := i
		if mode == "excluded" {
			aliasID += size
			fmt.Fprintf(&basics, "tt%d\ttvEpisode\tExcluded Episode\tExcluded Episode\t0\t2000\t\\N\t20\tDrama\n", aliasID)
		}
		for j := 1; j <= 12; j++ {
			alias := name
			if mode == "distinct" {
				alias += fmt.Sprintf(" alias %02d", j)
			}
			fmt.Fprintf(&aliases, "tt%d\t%d\t%s\tUS\ten\talternative\t\\N\t0\n", aliasID, j, alias)
		}
	}
	return map[Dataset]string{Basics: basics.String(), Aliases: aliases.String(), Ratings: ratings.String()}
}

func BenchmarkUpdate(b *testing.B) {
	for _, mode := range []string{"basics", "duplicates", "distinct", "excluded"} {
		b.Run(mode, func(b *testing.B) {
			source := fixtureSource(b, updateBenchmarkDataset(1000, mode))
			for _, operation := range []string{"create", "refresh"} {
				b.Run(operation, func(b *testing.B) {
					path := filepath.Join(b.TempDir(), "catalog.sqlite")
					if operation == "refresh" {
						if _, err := Update(b.Context(), path, UpdateOptions{Source: source}); err != nil {
							b.Fatal(err)
						}
					}
					phases := make(map[ProgressPhase]time.Duration)
					var previous ProgressPhase
					var last time.Time
					opts := UpdateOptions{Source: source, Progress: func(p Progress) {
						if p.Phase == previous {
							return
						}
						now := time.Now()
						if previous != "" {
							phases[previous] += now.Sub(last)
						}
						previous, last = p.Phase, now
					}}
					b.ReportAllocs()
					for b.Loop() {
						if operation == "create" {
							b.StopTimer()
							for _, suffix := range []string{"", "-wal", "-shm"} {
								if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
									b.Fatal(err)
								}
							}
							b.StartTimer()
						}
						previous = ""
						if _, err := Update(b.Context(), path, opts); err != nil {
							b.Fatal(err)
						}
					}
					stat, err := os.Stat(path)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(stat.Size()), "db-bytes")
					for _, phase := range []ProgressPhase{Importing, Indexing, Validating, Committing} {
						b.ReportMetric(float64(phases[phase].Nanoseconds())/float64(b.N), string(phase)+"-ns/op")
					}
				})
			}
		})
	}
}
