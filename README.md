# libimdb

A programmatic, CGO-free Go library for creating and querying a custom SQLite
catalog from IMDb's public datasets. Module: `github.com/odwrtw/libimdb`;
package name: `imdb`. Requires Go 1.26.1 or newer.

[Go documentation](https://pkg.go.dev/github.com/odwrtw/libimdb)

## What it owns

The catalog is authoritative for IMDb identity and available IMDb facts:

- Primary/original titles and alternative titles, with alias language/region.
- Movie/show kind and original IMDb title type.
- Start/end year, runtime, adult flag, genres, rating, and vote count.
- Dataset provenance, compressed-input checksums, and catalog generation.

Retained title types are `movie`, `tvMovie`, `tvSeries`, and `tvMiniSeries`.
Episodes, shorts, people, plots, images, exact air dates, and TMDB/TVDB IDs are
not included. Application state and provider enrichment belong elsewhere.
Unknown numeric/boolean fields use pointers; unknown genres are nil.
Unknown original titles, alias language, and alias region use empty strings.

## Update from Go

The same operation creates a missing catalog or refreshes an existing one:

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
defer cancel()

info, err := imdb.Update(ctx, "imdb.sqlite", imdb.UpdateOptions{
    Attempts: 2,
})
```

The default source is `https://datasets.imdbws.com/`, using
`title.basics.tsv.gz`, `title.akas.tsv.gz`, and `title.ratings.tsv.gz`.

Data flows directly from HTTP through gzip/TSV parsing into prepared SQLite
statements. Neither compressed/extracted dataset temporary files nor whole-file
buffers are used. Rows are limited to 1 MiB. Suggestion names are deduplicated by
IMDb ID and folded name; trigram inserts are batched per distinct entry. Raw
aliases and their metadata remain intact. All three datasets, search indexes,
metadata, and checksums commit in one transaction. Removed facts disappear on
successful refresh.

Any transfer, parse, checksum, insertion, close, cancellation, or commit failure
preserves the previous committed catalog. A failed first creation can leave an
empty file; it will not open as a valid catalog and can be retried.

Retries restart the **entire** transaction for transient network errors,
incomplete transfers, HTTP 429, or HTTP 5xx. The default is one attempt, with a
maximum of five. There is no incremental/conditional refresh yet. Download
speed is coupled to insert speed; caller deadlines and real-dataset throughput
measurements are important.

Supply `HTTPSource{Client: client, BaseURL: base}` to customize HTTP policy, or a
`Source`/`SourceFunc` to open existing gzip files or other streams. Update owns
and closes each stream. Sources must honor context cancellation; an arbitrary
blocking reader cannot be forcibly interrupted by the library.

### Update progress

The library is silent unless `UpdateOptions.Progress` is supplied:

```go
info, err := imdb.Update(ctx, "imdb.sqlite", imdb.UpdateOptions{
    Progress: func(p imdb.Progress) {
        if p.Dataset != "" {
            fmt.Fprintf(os.Stderr, "attempt %d: %s %s, %d compressed bytes, %d rows\n",
                p.Attempt, p.Phase, p.Dataset, p.BytesRead, p.Rows)
        } else {
            fmt.Fprintf(os.Stderr, "attempt %d: %s\n", p.Attempt, p.Phase)
        }
    },
})
```

The callback receives synchronous snapshots and must return promptly; slow
callbacks slow the import. Do not reenter `Update` from it. Cancel the update's
context to stop. Nil disables reporting. Ordinary counters are throttled to
about four notifications per second; phase boundaries and final dataset counters
are always delivered. There is no background heartbeat during blocking work.

`Phase` reports `Preparing`, `Opening`, `Importing`, `DatasetDone`, `Indexing`,
`Validating`, `Committing`, `Retrying`, `Done`, or `Failed`. Only dataset phases
carry `Dataset` and counters; update-wide events have an empty dataset. `Done`
means the catalog committed successfully. `DatasetDone` follows checksum
validation and successful stream closure, not merely reading the last byte.
`Retrying` and `Failed` carry `Err`; cancellation is a `Failed` event.

`Attempt` is one-based (zero if options are rejected before any attempt), and
`Attempts` is the maximum allowed. Counters reset on each dataset and full-update
retry. `Rows` excludes the TSV header and includes successfully processed
filtered records, not just retained catalog rows. `BytesRead` counts compressed
input. Gzip/parser read-ahead means bytes and rows advance independently, and
**100% of input bytes is not 100% of the database update**. Indexing, validation,
and commit have phases but no measurable percentage.

`TotalBytes` is advisory; zero means unknown. HTTP sources use a known content
length. Custom sources can supply `SourceInfo.SizeBytes`; known sizes are stored
as optional provenance (`size_bytes`); its absence means the size is unknown.

### Database safety and concurrency

- SQLite WAL, shared-memory/journal files, and SQLite-managed scratch storage
  are distinct from dataset temporary files and may be used.
- Use a local filesystem supporting SQLite locking, not a network filesystem.
- Only one writer is supported. SQLite lock waiting is bounded to one second;
  contention is an error, not an implicit competing import.
- Read-only catalog handles remain valid during refresh. Each operation reads
  one transaction snapshot; its next operation sees newly committed data.
- Multi-call application workflows needing one generation should compare
  `Info.Generation`, or use a single batch operation.
- Do not open this changing database with SQLite `immutable=1`.
- Read-only container deployments must mount the catalog directory with the
  required WAL/SHM files available. Validate the actual mount/checkpoint setup.
- Readers and refreshes require **schema v2**. Other schemas and foreign or
  unversioned databases are rejected with `ErrSchema`, never overwritten. To
  rebuild an incompatible catalog, use a new path and explicitly point consumers
  to it.
- SQLite refreshes do not compact the database automatically.

## Lookup and safe resolution

```go
catalog, err := imdb.Open(ctx, "imdb.sqlite")
if err != nil {
    return err
}
defer catalog.Close()

match, err := catalog.Resolve(ctx, imdb.Query{
    Title: "The Matrix",
    Kind:  imdb.Movie,
    Year:  1999,
})
if err != nil {
    // errors.Is(err, imdb.ErrNotFound) or imdb.ErrAmbiguous are normal
    // identity outcomes, distinct from catalog/operational failures.
    return err
}
id := match.Title.ID
```

`Lookup` returns facts for an ID. `LookupMany` returns a snapshot-consistent map
for up to 10,000 IDs, omitting absent IDs. `Names` returns primary/original/alias
names and their metadata. `Info` reports provenance and catalog generation.

`Query` takes a title, not a filename. Parse filenames outside this library.
Empty kind searches both kinds; year zero means unspecified. A supplied year is
a hard constraint, including for records with unknown years. Adult titles are
included by default; `ExcludeAdult` excludes those explicitly flagged adult.

Resolution normalizes Unicode NFC, Unicode case, and whitespace only. It never
removes accents, punctuation, or words. All matching primary/original/alias names
participate in ambiguity detection; even an exact primary spelling does not
override an identity collision. Repeated aliases for one ID are not ambiguity.
Popularity never selects an identity. Queries are limited to 1,024 bytes and
256 normalized Unicode code points.

## Broader suggestions

```go
result, err := catalog.Candidates(ctx, imdb.Query{
    Title: "matrix", Kind: imdb.Movie,
}, imdb.SearchOptions{Limit: 20})
```

Candidate search uses indexed normalized names and accent/punctuation-folded
prefixes and substrings. Substring matching starts at **three folded Unicode
characters**, so `matrix` finds `The Matrix` and interior fragments work too.
One- and two-character queries use exact/prefix matching only. SQL wildcards and
operators have no special meaning; punctuation is folded as described above.

When direct results do not fill `Limit`, queries of at least four folded
characters also retrieve whole-title typo suggestions using rune-aware edit
distance. Typo search inspects up to three rare trigram posting streams; it does
not aggregate every common-gram hit or scan every title. Substring retrieval
uses the least frequent query gram and checks literal containment. Kind-aware
indexes avoid reading the other kind's prefix/posting entries.

Results are deduplicated by IMDb ID, with full title and matched-alias metadata
loaded only for the final IDs. Ranking is deterministic: exact, normalized,
prefix, substring, fuzzy; then score, votes, ID and matched name. Shorter prefix
and substring matches score higher. Scores are ranking values, **not confidence
probabilities**. Lossy accent/punctuation equivalence remains `fuzzy`.

This is heuristic retrieval, not guaranteed exhaustive search or a globally
optimal popularity ranking. Default `Budget`: 1,000 entries/postings per stage;
maximum: 10,000. Exact retrieval counts distinct IDs; prefix/substring retrieval
counts compact entries; all selected typo streams share one posting budget.
Budgets apply **before** year/adult and similarity filters. Repeated raw aliases
do not consume extra entries, but different folded names and postings can.
Each stream may read one extra row to detect clipping. Default output `Limit`:
20; maximum: 100.

`Truncated` reports budget or output-limit clipping, even when filtering leaves
no returned matches. It does not measure overall typo recall: a title sharing
no selected gram may be missed. Skipping typo fallback because direct results
fill the limit is normal search policy, not clipping. Use context deadlines;
indexed safe-name ambiguity detection and final alias selection still process
raw matching aliases.

**Never infer a unique identity from candidates, their rank, or their count.**
Use `Resolve` for authoritative automated selection. Accent/punctuation folding
and fuzzy matches remain suggestions only.

## Search validation and benchmarks

Routine tests and `BenchmarkSearch`/`BenchmarkSearchUpdate` use synthetic,
duplicate-heavy datasets. Optional real-data checks open an existing **v2**
catalog read-only and never refresh it:

```sh
go test -run '^$' -bench '^(BenchmarkSearch|BenchmarkSearchUpdate)$' -benchmem .
LIBIMDB_BENCH_DB=/path/to/imdb.sqlite go test -run '^TestRealCatalogSearch$' \
    -bench '^BenchmarkRealCatalog$' -benchmem -v .
```

The real benchmark reports SQLite version, catalog generation and counts. Record
hardware, cache state and budgets when comparing runs; its timings are not a
latency guarantee. Existing refreshes reuse SQLite pages and do not automatically
shrink the database file, even when search postings become smaller.

## CLI

[GitHub releases](https://github.com/odwrtw/libimdb/releases) provide a static
Linux amd64 binary (`imdb-linux-amd64`). Releases are published after checks pass
on pushes to `master`, with UTC date/time tags and names (`YYYYMMDD-HHMMSS`).

For a daily refresh using the binary built by `./build.sh`, see the
[systemd user service and timer examples](dist/README.md).

```sh
go run ./cmd/imdb update -db imdb.sqlite
go run ./cmd/imdb update -db imdb.sqlite -input-dir /path/to/existing/gzip/files
go run ./cmd/imdb update -db imdb.sqlite -progress always  # progress even when redirected
go run ./cmd/imdb update -db imdb.sqlite -progress never   # suppress progress
go run ./cmd/imdb info -db imdb.sqlite
go run ./cmd/imdb query -db imdb.sqlite -title 'The Matrix' -kind movie -year 1999
go run ./cmd/imdb query -db imdb.sqlite -title 'matrix' -kind movie -candidates
go run ./cmd/imdb query -db imdb.sqlite -title 'The Matrx' -kind movie -candidates
```

Updates default to `-progress auto`: a live status on terminal stderr (except
`TERM=dumb`), showing the current file, compressed bytes/percentage when size is
known, rows, attempts, and database phases. `-input-dir` uses regular-file sizes.
Completed files and retries retain a summary line. Unknown sizes show bytes and
rows without an invented percentage.

`-progress always` enables progress on any stderr destination, using plain
newline records instead of terminal escapes when it is not an interactive
terminal. `-progress never` disables it. Progress is best-effort; display-write
failures do not abort the update.

Results remain JSON on stdout. With redirected stderr, default/`never` errors
remain JSON-only; `always` deliberately mixes human progress lines with the
final JSON error. The display ends before result/error output, and existing
signal cancellation and exit codes are unchanged.
