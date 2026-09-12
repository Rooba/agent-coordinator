package hostrunner

import (
	"bytes"
	"sync"
)

const (
	// maxToolNameBytes bounds the one provider-supplied string progress ever
	// carries, so a hostile stream cannot grow the record.
	maxToolNameBytes = 64
	// maxStreamLine is the longest event line worth assembling: the terminal
	// event is a report plus its envelope, so the stream must accept every
	// line the report parser would accept and nothing beyond it.
	maxStreamLine = defaultOutput
	// maxTurns bounds the counter so a malformed stream cannot report nonsense.
	maxTurns = 100000
)

// Progress is one liveness sample: how many model turns the task has taken and
// the NAME of the tool it last used. Tool arguments, tool results and page
// text are never sampled - an eyes task looks at a live browser session, so
// nothing derived from the page may leave the host.
type Progress struct {
	Turns int
	Tool  string
}

// ProgressFunc receives samples while a task runs. It is called from the
// runner's own goroutine and must not block: samples are coalesced, so a slow
// receiver loses intermediate ones rather than stalling the provider.
type ProgressFunc func(Progress)

// StreamEvent is what a provider's line decoder makes of one event: whether it
// opens a new model turn, the name of the tool that turn invoked, and whether
// the line carries the final report.
type StreamEvent struct {
	Turn  bool
	Tool  string
	Final bool
}

// StreamDecoder reads one event line. It must never return anything derived
// from page content or tool arguments.
type StreamDecoder func(line []byte) StreamEvent

// cleanToolName bounds a tool name and requires it to BE a name: anything
// carrying a character a name does not have is dropped whole rather than
// compacted, so page text can never ride in as a tool.
func cleanToolName(name string) string {
	if len(name) > maxToolNameBytes {
		name = name[:maxToolNameBytes]
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '-', c == '.', c == ':':
		default:
			return ""
		}
	}
	return name
}

// streamReader turns a provider's line-delimited event stream into progress
// samples while the process runs, and keeps the single line the report is
// decoded from - so a long stream is observed without ever being buffered
// whole.
type streamReader struct {
	decode StreamDecoder
	sink   *progressPump
	once   sync.Once

	mu      sync.Mutex
	pending []byte
	skip    bool // the current line blew the size bound; drop it at the newline
	final   []byte
	turns   int
	tool    string
}

func newStreamReader(decode StreamDecoder, report ProgressFunc) *streamReader {
	return &streamReader{decode: decode, sink: newProgressPump(report)}
}

func (r *streamReader) Write(data []byte) (int, error) {
	written := len(data)
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			r.buffer(data)
			break
		}
		r.buffer(data[:newline])
		r.flush()
		data = data[newline+1:]
	}
	return written, nil
}

func (r *streamReader) buffer(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.skip || len(r.pending)+len(chunk) > maxStreamLine {
		r.skip, r.pending = true, nil
		return
	}
	r.pending = append(r.pending, chunk...)
}

// flush decodes the assembled line and publishes what it learned.
func (r *streamReader) flush() {
	r.mu.Lock()
	line, skipped := r.pending, r.skip
	r.pending, r.skip = nil, false
	if skipped || len(bytes.TrimSpace(line)) == 0 {
		r.mu.Unlock()
		return
	}
	event := r.decode(line)
	if event.Final {
		r.final = append([]byte(nil), line...)
	}
	if tool := cleanToolName(event.Tool); tool != "" {
		r.tool = tool
	}
	if event.Turn && r.turns < maxTurns {
		r.turns++
	}
	sample, changed := Progress{Turns: r.turns, Tool: r.tool}, event.Turn || event.Tool != ""
	r.mu.Unlock()
	if changed {
		r.sink.offer(sample)
	}
}

// Close flushes a stream that ended without a trailing newline and stops
// publishing, so no sample outlives the task that produced it. It is safe to
// call more than once, which lets every path out of a run defer it.
func (r *streamReader) Close() {
	r.once.Do(func() {
		r.flush()
		r.sink.close()
	})
}

// Final is the line the provider marked as carrying its report, empty when it
// never emitted one.
func (r *streamReader) Final() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.final
}

// progressPump hands samples to the caller's callback on one goroutine, newest
// first and never blocking the producer: a sample is a snapshot of the whole
// counter, so dropping an intermediate one loses nothing.
type progressPump struct {
	mu      sync.Mutex
	closed  bool
	samples chan Progress
	done    chan struct{}
}

func newProgressPump(report ProgressFunc) *progressPump {
	p := &progressPump{done: make(chan struct{})}
	if report == nil {
		p.closed = true
		close(p.done)
		return p
	}
	p.samples = make(chan Progress, 1)
	go func() {
		defer close(p.done)
		for sample := range p.samples {
			report(sample)
		}
	}()
	return p
}

func (p *progressPump) offer(sample Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.samples <- sample:
	default:
		select {
		case <-p.samples: // the queued sample is already stale; this one replaces it
		default:
		}
		select {
		case p.samples <- sample:
		default:
		}
	}
}

func (p *progressPump) close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.samples)
	}
	p.mu.Unlock()
	<-p.done
}
