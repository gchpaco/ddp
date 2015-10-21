package ddp

import (
	"encoding/hex"
	"github.com/Sirupsen/logrus"
	"io"
	"sync"
	"time"
)

// Gather statistics about a DDP connection.

// ---------------------------------------------------------
// io utilities
//
// This is generic - should be moved into a stand alone lib
// ---------------------------------------------------------

// ReaderProxy provides common tooling for structs that manage an io.Reader.
type ReaderProxy struct {
	reader io.Reader
}

// NewReaderProxy creates a new proxy for the provided reader.
func NewReaderProxy(reader io.Reader) *ReaderProxy {
	return &ReaderProxy{reader}
}

// SetReader sets the reader on the proxy.
func (r *ReaderProxy) SetReader(reader io.Reader) {
	r.reader = reader
}

// WriterProxy provides common tooling for structs that manage an io.Writer.
type WriterProxy struct {
	writer io.Writer
}

// NewWriterProxy creates a new proxy for the provided writer.
func NewWriterProxy(writer io.Writer) *WriterProxy {
	return &WriterProxy{writer}
}

// SetWriter sets the writer on the proxy.
func (w *WriterProxy) SetWriter(writer io.Writer) {
	w.writer = writer
}

// Logging data types
const (
	DataByte = iota // data is raw []byte
	DataText        // data is string data
)

// Logger logs data from i/o sources.
type Logger struct {
	// Active is true if the logger should be logging reads
	Active bool
	// Truncate is >0 to indicate the number of characters to truncate output
	Truncate int

	logger *logrus.Entry
	dtype  int
}

// NewLogger creates a new i/o logger.
func NewLogger(logger *logrus.Entry, active bool, dataType int, truncate int) Logger {
	return Logger{logger: logger, Active: active, dtype: dataType, Truncate: truncate}
}

// Log logs the current i/o operation and returns the read and error for
// easy call chaining.
func (l *Logger) Log(p []byte, n int, err error) (int, error) {
	if l.Active && err == nil {
		limit := n
		truncated := false
		if l.Truncate > 0 && l.Truncate < limit {
			limit = l.Truncate
			truncated = true
		}
		switch l.dtype {
		case DataText:
			if truncated {
				l.logger.Printf("[%d] %s...", n, string(p[:limit]))
			} else {
				l.logger.Printf("[%d] %s", n, string(p[:limit]))
			}
		case DataByte:
			fallthrough
		default:
			l.logger.Println(hex.Dump(p[:limit]))
		}
	}
	return n, err
}

// ReaderLogger logs data from any io.Reader.
// ReaderLogger wraps a Reader and passes data to the actual data consumer.
type ReaderLogger struct {
	Logger
	ReaderProxy
}

// NewReaderDataLogger creates an active binary data logger with a default
// log.Logger and a '->' prefix.
func NewReaderDataLogger(reader io.Reader) *ReaderLogger {
	return NewReaderLogger(reader, log.WithField("direction", "read"), true, DataByte, 0)
}

// NewReaderTextLogger creates an active binary data logger with a default
// log.Logger and a '->' prefix.
func NewReaderTextLogger(reader io.Reader) *ReaderLogger {
	return NewReaderLogger(reader, log.WithField("direction", "read"), true, DataText, 80)
}

// NewReaderLogger creates a Reader logger for the provided parameters.
func NewReaderLogger(reader io.Reader, logger *logrus.Entry, active bool, dataType int, truncate int) *ReaderLogger {
	return &ReaderLogger{ReaderProxy: *NewReaderProxy(reader), Logger: NewLogger(logger, active, dataType, truncate)}
}

// Read logs the read bytes and passes them to the wrapped reader.
func (r *ReaderLogger) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	return r.Log(p, n, err)
}

// WriterLogger logs data from any io.Writer.
// WriterLogger wraps a Writer and passes data to the actual data producer.
type WriterLogger struct {
	Logger
	WriterProxy
}

// NewWriterDataLogger creates an active binary data logger with a default
// log.Logger and a '->' prefix.
func NewWriterDataLogger(writer io.Writer) *WriterLogger {
	return NewWriterLogger(writer, log.WithField("direction", "write"), true, DataByte, 0)
}

// NewWriterTextLogger creates an active binary data logger with a default
// log.Logger and a '->' prefix.
func NewWriterTextLogger(writer io.Writer) *WriterLogger {
	return NewWriterLogger(writer, log.WithField("direction", "write"), true, DataText, 80)
}

// NewWriterLogger creates a Reader logger for the provided parameters.
func NewWriterLogger(writer io.Writer, logger *logrus.Entry, active bool, dataType int, truncate int) *WriterLogger {
	return &WriterLogger{WriterProxy: *NewWriterProxy(writer), Logger: NewLogger(logger, active, dataType, truncate)}
}

// Write logs the written bytes and passes them to the wrapped reader.
func (w *WriterLogger) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	return w.Log(p, n, err)
}

// Stats tracks statistics for i/o operations. Stats are produced from a
// of a running stats agent.
type Stats struct {
	// Bytes is the total number of bytes transferred.
	Bytes int64
	// Ops is the total number of i/o operations performed.
	Ops int64
	// Errors is the total number of i/o errors encountered.
	Errors int64
	// Runtime is the duration that stats have been gathered.
	Runtime time.Duration
}

// StatsTracker provides the basic tooling for tracking i/o stats.
type StatsTracker struct {
	bytes  int64
	ops    int64
	errors int64
	start  time.Time
	lock   *sync.Mutex
}

// NewStatsTracker create a new stats tracker with start time set to now.
func NewStatsTracker() *StatsTracker {
	return &StatsTracker{start: time.Now(), lock: new(sync.Mutex)}
}

// Op records an i/o operation. The parameters are passed through to
// allow easy chaining.
func (t *StatsTracker) Op(n int, err error) (int, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.ops++
	if err == nil {
		t.bytes += int64(n)
	} else {
		if err == io.EOF {
			// I don't think we should log EOF stats as an error
		} else {
			t.errors++
		}
	}

	return n, err
}

// Snapshot takes a snapshot of the current reader statistics.
func (t *StatsTracker) Snapshot() *Stats {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.snap()
}

// Reset sets all of the stats to initial values.
func (t *StatsTracker) Reset() *Stats {
	t.lock.Lock()
	defer t.lock.Unlock()

	stats := t.snap()
	t.bytes = 0
	t.ops = 0
	t.errors = 0
	t.start = time.Now()

	return stats
}

func (t *StatsTracker) snap() *Stats {
	return &Stats{Bytes: t.bytes, Ops: t.ops, Errors: t.errors, Runtime: time.Since(t.start)}
}

// ReaderStats tracks statistics on any io.Reader.
// ReaderStats wraps a Reader and passes data to the actual data consumer.
type ReaderStats struct {
	StatsTracker
	ReaderProxy
}

// NewReaderStats creates a ReaderStats object for the provided reader.
func NewReaderStats(reader io.Reader) *ReaderStats {
	return &ReaderStats{ReaderProxy: *NewReaderProxy(reader), StatsTracker: *NewStatsTracker()}
}

// Read passes through a read collecting statistics and logging activity.
func (r *ReaderStats) Read(p []byte) (int, error) {
	return r.Op(r.reader.Read(p))
}

// WriterStats tracks statistics on any io.Writer.
// WriterStats wraps a Writer and passes data to the actual data producer.
type WriterStats struct {
	StatsTracker
	WriterProxy
}

// NewWriterStats creates a WriterStats object for the provided writer.
func NewWriterStats(writer io.Writer) *WriterStats {
	return &WriterStats{WriterProxy: *NewWriterProxy(writer), StatsTracker: *NewStatsTracker()}
}

// Write passes through a write collecting statistics.
func (w *WriterStats) Write(p []byte) (int, error) {
	return w.Op(w.writer.Write(p))
}
