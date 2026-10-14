# libimdb

A programmatic, CGO-free Go library for creating and querying a custom SQLite
catalog from IMDb's public datasets. Module: `github.com/odwrtw/libimdb`;
package name: `imdb`. Requires Go 1.26.1 or newer.

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
buffers are used. Rows are limited to 1 MiB. Trigram posting inserts are batched
per name. All three datasets, search indexes, metadata, and checksums commit in
one transaction. Removed facts disappear on successful refresh.

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
as optional provenance (`size_bytes`), and older catalogs remain readable.

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
- Unknown schemas, foreign databases, and tapis's old unversioned database are
  rejected, never overwritten. For the initial migration, build at a new path
  and explicitly point consumers to it.
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
    Title: "The Matrx", Kind: imdb.Movie,
}, imdb.SearchOptions{Limit: 20})
```

Candidate search uses indexed normalized names, accent/punctuation-folded
prefixes, and overlapping trigrams, followed by rune-aware edit-distance
scoring. It does not scan every catalog title for fuzzy similarity.
Short names do not use typo scoring. Each candidate exposes its matched name,
method, and score; the score is a ranking value, **not a confidence probability**.

This is heuristic retrieval, not guaranteed exhaustive fuzzy search. Names with
no shared trigram may not be retrieved. `Truncated` reports per-stage budget or
output-limit clipping, not overall fuzzy-search recall. Default budget: 1,000
names per stage; maximum: 10,000. Output limit: default 20, maximum 100.
SQL work on common grams can still be substantial; use context deadlines.
Trigram overlap orders the bounded retrieval before edit scoring. Names are
deduplicated by IMDb ID and ordered deterministically by match method,
score, votes, ID, and matched name.

**Never infer a unique identity from candidates, their rank, or their count.**
Use `Resolve` for authoritative automated selection. Accent/punctuation folding
and fuzzy matches remain suggestions only.

## CLI

For a daily refresh using the binary built by `./build.sh`, see the
[systemd user service and timer examples](dist/README.md).

```sh
go run ./cmd/imdb update -db imdb.sqlite
go run ./cmd/imdb update -db imdb.sqlite -input-dir /path/to/existing/gzip/files
go run ./cmd/imdb update -db imdb.sqlite -progress always  # progress even when redirected
go run ./cmd/imdb update -db imdb.sqlite -progress never   # suppress progress
go run ./cmd/imdb info -db imdb.sqlite
go run ./cmd/imdb query -db imdb.sqlite -title 'The Matrix' -kind movie -year 1999
go run ./cmd/imdb query -db imdb.sqlite -title 'The Matrx' -candidates
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
