package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/mattn/go-isatty"
	imdb "github.com/odwrtw/libimdb"
)

func terminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && os.Getenv("TERM") != "dumb" && isatty.IsTerminal(f.Fd())
}

type progressDisplay struct {
	out      io.Writer
	terminal bool
	active   bool
}

func newProgressDisplay(out io.Writer, mode string, terminal bool) *progressDisplay {
	if mode == "never" || (mode == "auto" && !terminal) {
		return nil
	}
	return &progressDisplay{out: out, terminal: terminal}
}

func (d *progressDisplay) report(p imdb.Progress) {
	if d.out == nil {
		return
	}
	line := formatProgress(p)
	if d.terminal {
		line = "\r\x1b[2K" + line
	}
	d.active = d.terminal && p.Phase != imdb.DatasetDone && p.Phase != imdb.Retrying && p.Phase != imdb.Done && p.Phase != imdb.Failed
	if !d.active {
		line += "\n"
	}
	if _, err := io.WriteString(d.out, line); err != nil {
		// Progress is best-effort. Do not fail (or misreport) a committed update
		// because its status display is unwritable.
		d.out = nil
	}
}

func (d *progressDisplay) finish() {
	if d != nil && d.out != nil && d.active {
		_, _ = io.WriteString(d.out, "\n")
		d.active = false
	}
}

func formatProgress(p imdb.Progress) string {
	phase := string(p.Phase)
	switch p.Phase {
	case imdb.DatasetDone:
		phase = "imported"
	case imdb.Retrying:
		phase = "retrying full update"
	case imdb.Failed:
		if errors.Is(p.Err, context.Canceled) {
			phase = "cancelled"
		} else if errors.Is(p.Err, context.DeadlineExceeded) {
			phase = "deadline exceeded"
		}
	}
	line := fmt.Sprintf("[%d/%d] %s", p.Attempt, p.Attempts, phase)
	if p.Dataset != "" {
		line += " " + string(p.Dataset)
		if p.Phase != imdb.Opening {
			line += " " + formatBytes(p.BytesRead)
			if p.TotalBytes > 0 {
				percent := math.Min(100, math.Floor(100*float64(p.BytesRead)/float64(p.TotalBytes)))
				line += fmt.Sprintf("/%s %.0f%%", formatBytes(p.TotalBytes), percent)
			}
			line += fmt.Sprintf(" %d rows", p.Rows)
		}
	}
	return line
}

func formatBytes(n int64) string {
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	value, unit := float64(n), 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1f%s", value, units[unit])
}
