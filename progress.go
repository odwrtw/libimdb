package imdb

import "time"

// ProgressPhase identifies the current operation. Only Opening, Importing, and
// DatasetDone carry a Dataset and per-dataset counters; other phases are update-wide.
type ProgressPhase string

const (
	Preparing   ProgressPhase = "preparing"
	Opening     ProgressPhase = "opening"
	Importing   ProgressPhase = "importing"
	DatasetDone ProgressPhase = "dataset_done"
	Indexing    ProgressPhase = "indexing"
	Validating  ProgressPhase = "validating"
	Committing  ProgressPhase = "committing"
	Retrying    ProgressPhase = "retrying"
	Done        ProgressPhase = "done"
	Failed      ProgressPhase = "failed"
)

// Progress is a snapshot of a streaming update. Counters reset for each dataset
// and full-update attempt. BytesRead counts compressed input, not decompressed
// TSV bytes; buffering means bytes and rows advance independently. Reaching
// TotalBytes does not imply dataset validation or catalog commit has completed.
type Progress struct {
	Dataset    Dataset
	Rows       int64 // successfully processed records, excluding header, including filtered records
	Phase      ProgressPhase
	Attempt    int // one-based; zero if options were rejected before any attempt
	Attempts   int // maximum attempts, not necessarily all used
	BytesRead  int64
	TotalBytes int64 // source-supplied compressed size; zero means unknown, totals are advisory
	Err        error // non-nil only for Retrying and Failed, including cancellation
}

const progressInterval = 250 * time.Millisecond

type progressReporter struct {
	callback func(Progress)
	current  Progress
	last     time.Time
	now      func() time.Time
}

func (p *progressReporter) phase(phase ProgressPhase, err error) {
	p.current = Progress{Phase: phase, Attempt: p.current.Attempt, Attempts: p.current.Attempts, Err: err}
	p.emit(true)
}

func (p *progressReporter) opening(d Dataset) {
	p.current = Progress{Phase: Opening, Dataset: d, Attempt: p.current.Attempt, Attempts: p.current.Attempts}
	p.emit(true)
}

func (p *progressReporter) importing(size int64) {
	p.current.Phase = Importing
	p.current.TotalBytes = max(size, 0)
	p.emit(true)
}

func (p *progressReporter) rows(rows int64) {
	if p.callback != nil {
		p.current.Rows = rows
		p.emit(false)
	}
}

func (p *progressReporter) bytes(n int) {
	if p.callback != nil && n > 0 {
		p.current.BytesRead += int64(n)
		p.emit(false)
	}
}

func (p *progressReporter) emit(force bool) {
	if p.callback == nil {
		return
	}
	now := p.now()
	if force || now.Sub(p.last) >= progressInterval {
		p.last = now
		p.callback(p.current)
	}
}
