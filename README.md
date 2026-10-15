# libimdb

A programmatic, CGO-free Go library for creating and querying a custom SQLite
catalog from IMDb's public datasets. Module: `github.com/odwrtw/libimdb`;
package name: `imdb`. Requires Go 1.26.1 or newer.

[Go documentation](https://pkg.go.dev/github.com/odwrtw/libimdb)

## What it owns

The catalog is authoritative for IMDb identity and available IMDb facts:

- Primary/original titles and alternative titles, with alias language/region.
- Movie/show kind and original IMDb title type.
- Start/end year, runtime in minutes, adult flag, genres, rating, and vote count.
- Dataset provenance, compressed-input checksums, and catalog generation.

Retained title types are `movie`, `tvMovie`, `tvSeries`, and `tvMiniSeries`.
Episodes, shorts, people, plots, images, exact air dates, and TMDB/TVDB IDs are
not included. Application state and provider enrichment belong elsewhere.
Unknown numeric/boolean fields use pointers; unknown genres are nil.
Unknown original titles, alias language, and alias region use empty strings.
Go fields use explicit names: `TitleType`, `StartYear`, `RuntimeMinutes`.
`Name.Source` is a `NameSource` (`PrimaryName`, `OriginalName`, or `AliasName`).
`Name.IsOriginal` is IMDb's alias `isOriginalTitle` metadata, not a general
original-name test; primary/original name records leave it nil.

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
`Source`/`SourceFunc` to open existing gzip files or other streams. `Source.Open`
returns `(io.ReadCloser, SourceMetadata, error)`. Source metadata supplies the
provenance location, ETag, last-modified value and advisory compressed size.
`Info.Sources` contains `DatasetReport` values combining that metadata with the
importer's dataset identity, row count, and computed SHA-256. The digest is not
an expected-checksum input or an external integrity assertion.

On successful open, Update owns and closes the stream. On error the source must
release any acquired resources. Calls are sequential within an update; retries
reopen datasets and require fresh, complete streams starting at byte zero.
Sources must honor context cancellation; an arbitrary blocking reader cannot
be forcibly interrupted by the library.

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
length. Custom sources can supply `SourceMetadata.SizeBytes`; known sizes are
stored as optional provenance (`size_bytes`); its absence means the size is unknown.

### Database safety and concurrency

- SQLite WAL, shared-memory/journal files, and SQLite-managed scratch storage
  are distinct from dataset temporary files and may be used.
- Use a local filesystem supporting SQLite locking, not a network filesystem.
- Only one writer is supported. SQLite lock waiting is bounded to one second;
  contention is an error, not an implicit competing import.
- Read-only catalog handles remain valid during refresh. Each operation reads
  one transaction snapshot; its next operation sees newly committed data.
- Use `Catalog.View` for multi-call workflows needing one generation; a separate
  `Info` call does not identify a previous operation's snapshot. `Resolution`,
  `SearchResult`, and `AmbiguousError` also report their own generation.
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
    Title:     "The Matrix",
    Kind:      imdb.Movie,
    StartYear: 1999,
})
if err != nil {
    // errors.Is(err, imdb.ErrNotFound) or imdb.ErrAmbiguous are normal
    // identity outcomes, distinct from catalog/operational failures.
    return err
}
id := match.Title.ID
```

`Resolve` returns a `Resolution`, with matched name, exact/normalized method,
and generation, but no ranking score. On ambiguity it returns a zero resolution
and `*AmbiguousError`, which matches `ErrAmbiguous` via `errors.Is`:

```go
var ambiguity *imdb.AmbiguousError
if errors.As(err, &ambiguity) {
    // Distinct safe-name conflicts, sorted by ID, never popularity.
    for _, title := range ambiguity.Titles {
        fmt.Println(title.ID, title.PrimaryTitle)
    }
    // At most 20 titles; Truncated indicates further conflicts exist.
    fmt.Println(ambiguity.Generation, ambiguity.Truncated)
}
```

These conflicts come from the same snapshot and filters as resolution. Broader
candidate search is not a replacement for this ambiguity information.

`Lookup` returns facts for an ID, or `ErrNotFound` for an absent or unsupported
title. IDs are `tt` followed by one or more ASCII digits. `LookupMany` returns a
snapshot-consistent map for up to 10,000 IDs, omitting absent IDs and collapsing
duplicates; empty input returns an empty map. `Names` returns raw names and
metadata in import order, not a preference order or a cross-generation ordering
guarantee. Distinct original names are included; an original identical to the
primary is not repeated as `OriginalName`. `Info` reports provenance and generation.

`Query` takes a title, not a filename. Parse filenames outside this library.
Empty kind searches both kinds; `StartYear` zero means unspecified. A supplied
year is a hard constraint, including for records with unknown years. Adult titles
are included by default; `ExcludeAdult` excludes those explicitly flagged adult.

Resolution normalizes Unicode NFC, Unicode case, and whitespace only. It never
removes accents, punctuation, or words. All matching primary/original/alias names
participate in ambiguity detection; even an exact primary spelling does not
override an identity collision. Repeated aliases for one ID are not ambiguity.
Popularity never selects an identity. Queries are limited to 1,024 bytes and
256 normalized Unicode code points.

### Consistent multi-call reads

```go
err = catalog.View(ctx, func(view *imdb.View) error {
    resolved, err := view.Resolve(ctx, imdb.Query{Title: "The Matrix"})
    if err != nil {
        return err
    }
    names, err := view.Names(ctx, resolved.Title.ID)
    if err != nil {
        return err
    }
    // resolved and names belong to one generation, even during a refresh.
    fmt.Println(resolved.Generation, names)
    return nil
})
```

The view also provides `Lookup`, `LookupMany`, `Candidates`, and `Info`. Its
lifetime is the callback, with cleanup on success, error, or panic. Do not copy,
retain, or use the view concurrently. Use the view rather than the same catalog
inside its callback: each catalog serializes operations on one connection.
Results remain usable after the callback; view calls then return `ErrViewClosed`.
Keep callbacks short because snapshots can retain WAL space. The view's parent
context governs its lifetime; individual calls can use shorter deadlines.

### Invalid input

Invalid queries, IDs, paths, HTTP source settings, and update/search options
return `*ValidationError`, matching `ErrInvalidInput`. Use `errors.As` to obtain
`Field` and `Problem`; do not parse error strings. Operational failures,
cancellation, not-found, and ambiguity are separate outcomes.

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

`SearchResult.Matches` contains `Candidate` values, a different type from
`Resolution`. Results are deduplicated by IMDb ID, with full title and
matched-alias metadata loaded only for the final IDs. Ranking is deterministic:
exact, normalized, prefix, substring, fuzzy; then score, votes, ID and matched name. Shorter prefix
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

`LimitTruncated` means retrieved matches were omitted to satisfy `Limit`;
increasing the limit may expose them. `BudgetExhausted` means a retrieval stage
was clipped by `Budget`, even when filtering leaves no returned matches;
increasing the budget may retrieve more. Both flags can be true. Neither measures
overall typo recall: a title sharing no selected gram may be missed. Skipping
typo fallback because direct results fill the limit is normal search policy,
not clipping. Use context deadlines; indexed safe-name ambiguity detection and
final alias selection still process raw matching aliases.

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

Results remain JSON on stdout. Resolutions include `generation` and no `score`;
title JSON uses `start_year` and `runtime_minutes`. Candidate results include
`limit_truncated` and `budget_exhausted`. JSON errors retain `kind` and `error`,
with optional `ambiguity` (titles, generation, truncation) or `validation`
(field, problem) details. `-year` sets `Query.StartYear`.

With redirected stderr, default/`never` errors remain JSON-only; `always`
deliberately mixes human progress lines with the
final JSON error. The display ends before result/error output, and existing
signal cancellation and exit codes are unchanged.
