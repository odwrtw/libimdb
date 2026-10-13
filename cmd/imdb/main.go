package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	imdb "github.com/odwrtw/libimdb"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

var errHelp = errors.New("help requested")

func execute(ctx context.Context, args []string, out, stderr io.Writer) int {
	if err := run(ctx, args, out, stderr); err != nil {
		if errors.Is(err, errHelp) {
			return 0
		}
		code, kind := 1, "operational"
		switch {
		case errors.Is(err, imdb.ErrNotFound):
			code, kind = 2, "not_found"
		case errors.Is(err, imdb.ErrAmbiguous):
			code, kind = 3, "ambiguous"
		case errors.Is(err, imdb.ErrInvalidQuery):
			code, kind = 4, "invalid_input"
		case errors.Is(err, imdb.ErrSchema):
			code, kind = 5, "schema"
		}
		_ = json.NewEncoder(stderr).Encode(map[string]string{"kind": kind, "error": err.Error()})
		return code
	}
	return 0
}

func run(ctx context.Context, args []string, out, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: usage: imdb update|query|info [flags]", imdb.ErrInvalidQuery)
	}
	command := args[0]
	if command != "update" && command != "query" && command != "info" {
		return fmt.Errorf("%w: unknown command %q", imdb.ErrInvalidQuery, command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("db", "imdb.sqlite", "catalog path")
	var result any
	switch command {
	case "update":
		dir := flags.String("input-dir", "", "directory of existing gzip datasets; otherwise download official datasets")
		base := flags.String("base-url", "", "dataset base URL; defaults to https://datasets.imdbws.com/")
		attempts := flags.Int("attempts", 1, "maximum full-update attempts (1-5)")
		timeout := flags.Duration("timeout", 2*time.Hour, "overall update deadline")
		progressMode := flags.String("progress", "auto", "progress on stderr: auto (terminal only), always, or never")
		if err := parseFlags(flags, args[1:], out); err != nil {
			return err
		}
		if flags.NArg() != 0 || (*dir != "" && *base != "") || *timeout <= 0 {
			return imdb.ErrInvalidQuery
		}
		if *progressMode != "auto" && *progressMode != "always" && *progressMode != "never" {
			return fmt.Errorf("%w: progress must be auto, always, or never", imdb.ErrInvalidQuery)
		}
		var source imdb.Source = imdb.HTTPSource{BaseURL: *base}
		if *dir != "" {
			source = imdb.SourceFunc(func(ctx context.Context, d imdb.Dataset) (io.ReadCloser, imdb.SourceInfo, error) {
				if err := ctx.Err(); err != nil {
					return nil, imdb.SourceInfo{}, err
				}
				filename := filepath.Join(*dir, string(d))
				f, err := os.Open(filename)
				if err != nil {
					return nil, imdb.SourceInfo{}, err
				}
				stat, err := f.Stat()
				if err != nil {
					_ = f.Close()
					return nil, imdb.SourceInfo{}, err
				}
				var size int64
				if stat.Mode().IsRegular() {
					size = stat.Size()
				}
				return f, imdb.SourceInfo{URL: filename, SizeBytes: size}, nil
			})
		}
		updateCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		display := newProgressDisplay(stderr, *progressMode, terminalWriter(stderr))
		defer display.finish()
		opts := imdb.UpdateOptions{Source: source, Attempts: *attempts}
		if display != nil {
			opts.Progress = display.report
		}
		info, err := imdb.Update(updateCtx, *path, opts)
		display.finish()
		if err != nil {
			return err
		}
		result = info
	case "query":
		title := flags.String("title", "", "title to resolve or search")
		kind := flags.String("kind", "", "movie or show; empty searches both")
		year := flags.Int("year", 0, "exact release/series start year; zero means unspecified")
		candidates := flags.Bool("candidates", false, "return suggestions instead of accepting an identity")
		limit := flags.Int("limit", 20, "maximum candidates (1-100)")
		excludeAdult := flags.Bool("exclude-adult", false, "exclude titles explicitly marked adult")
		if err := parseFlags(flags, args[1:], out); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return imdb.ErrInvalidQuery
		}
		c, err := imdb.Open(ctx, *path)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		q := imdb.Query{Title: *title, Kind: imdb.Kind(*kind), Year: *year, ExcludeAdult: *excludeAdult}
		if *candidates {
			r, err := c.Candidates(ctx, q, imdb.SearchOptions{Limit: *limit})
			if err != nil {
				return err
			}
			result = r
		} else {
			m, err := c.Resolve(ctx, q)
			if err != nil {
				return err
			}
			result = m
		}
	case "info":
		if err := parseFlags(flags, args[1:], out); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return imdb.ErrInvalidQuery
		}
		c, err := imdb.Open(ctx, *path)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		info, err := c.Info(ctx)
		if err != nil {
			return err
		}
		result = info
	}
	return json.NewEncoder(out).Encode(result)
}

func parseFlags(flags *flag.FlagSet, args []string, out io.Writer) error {
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		options := make([]map[string]string, 0)
		flags.VisitAll(func(f *flag.Flag) {
			options = append(options, map[string]string{"name": f.Name, "default": f.DefValue, "description": f.Usage})
		})
		if err := json.NewEncoder(out).Encode(map[string]any{"command": flags.Name(), "flags": options}); err != nil {
			return err
		}
		return errHelp
	}
	if err != nil {
		return fmt.Errorf("%w: %w", imdb.ErrInvalidQuery, err)
	}
	return nil
}
