package operations

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Stream names of one log entry, docs/architecture.md section 10.
const (
	// StreamStderr is a line the handler wrote to stderr.
	StreamStderr = "stderr"
	// StreamSystem is a Gateway or node agent event.
	StreamSystem = "system"
)

// DefaultLogLimit is the most log data stored for one operation. It is the
// documented default of limits.log_total (docs/configuration.md section 3).
const DefaultLogLimit = 10 << 20

// DefaultLogLineLimit is the longest log line before the line is split. It
// is the documented default of limits.log_line (docs/configuration.md
// section 3).
const DefaultLogLineLimit = 16 << 10

// LogEntry is one line of an operation's log. Seq is assigned by the log
// store: it starts at 1 and grows by one with every entry of one operation.
type LogEntry struct {
	Seq    int64
	Time   time.Time
	Stream string
	Line   string
}

// LogStore keeps operation logs. The Gateway ships an in-memory
// implementation (NewMemoryLogStore) and the SQLite history store.
type LogStore interface {
	// AppendLog stores one entry and returns it with the assigned Seq.
	// An empty Stream means StreamSystem; a zero Time means the store's
	// current time.
	AppendLog(operationID string, entry LogEntry) (LogEntry, error)
	// Logs returns the entries of one operation with a sequence number
	// greater than after, in sequence order. No entries is an empty list,
	// never nil, so a caller can range over it.
	Logs(operationID string, after int64) ([]LogEntry, error)
}

// MemoryLogStore is an in-memory LogStore. It keeps logs only for the life
// of the process; durability comes from the history store.
type MemoryLogStore struct {
	mu      sync.Mutex
	entries map[string][]LogEntry
}

// NewMemoryLogStore returns an empty in-memory log store.
func NewMemoryLogStore() *MemoryLogStore {
	return &MemoryLogStore{entries: map[string][]LogEntry{}}
}

// AppendLog stores one entry after the last one of the operation.
func (s *MemoryLogStore) AppendLog(operationID string, entry LogEntry) (LogEntry, error) {
	if operationID == "" {
		return LogEntry{}, fmt.Errorf("an operation log entry needs an operation ID")
	}
	if entry.Stream == "" {
		entry.Stream = StreamSystem
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string][]LogEntry{}
	}
	entry.Seq = int64(len(s.entries[operationID])) + 1
	s.entries[operationID] = append(s.entries[operationID], entry)
	return entry, nil
}

// Logs returns the entries after a sequence number, in sequence order.
func (s *MemoryLogStore) Logs(operationID string, after int64) ([]LogEntry, error) {
	if s == nil {
		return []LogEntry{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []LogEntry{}
	for _, entry := range s.entries[operationID] {
		if entry.Seq > after {
			out = append(out, entry)
		}
	}
	return out, nil
}

// LogSink appends lines to the operation logs, redacts the run's secrets,
// splits long lines, enforces the log limits, and delivers live lines to the
// followers of one operation (docs/architecture.md section 10). It is safe
// for concurrent use.
//
// A follower subscribes before the sink reads the backlog, so every entry
// reaches it exactly once, in sequence order. The sink keeps the recent
// state of an operation only while the run is open or a follower reads it;
// a finished operation is served from the store.
type LogSink struct {
	store     LogStore
	limit     int
	lineLimit int
	now       func() time.Time

	mu     sync.Mutex
	topics map[string]*logTopic
}

// logTopic is the state of one operation: the redaction terms of the run,
// the bytes stored so far, whether the cap was reached, and the live
// followers. Its mutex serializes appends, so the store sees one line at a
// time and the sequence numbers have no gaps.
type logTopic struct {
	id   string
	sink *LogSink

	mu        sync.Mutex
	active    bool
	loaded    bool
	total     int
	truncated bool
	terms     []string
	subs      map[*LogSubscription]struct{}
}

// NewLogSink returns a sink over store. A nil store keeps the logs in
// memory. A total limit of zero or less takes DefaultLogLimit, and a line
// limit of zero or less takes DefaultLogLineLimit.
func NewLogSink(store LogStore, limit, lineLimit int) *LogSink {
	if store == nil {
		store = NewMemoryLogStore()
	}
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	if lineLimit <= 0 {
		lineLimit = DefaultLogLineLimit
	}
	return &LogSink{
		store:     store,
		limit:     limit,
		lineLimit: lineLimit,
		now:       time.Now,
		topics:    map[string]*logTopic{},
	}
}

// Open starts the log of one run and records the values to remove from its
// lines: the secret argument values and the elevation password. Every line
// the run writes after Open passes through the terms.
func (s *LogSink) Open(operationID string, terms ...string) {
	if s == nil || operationID == "" {
		return
	}
	t := s.topic(operationID)
	t.mu.Lock()
	t.active = true
	t.terms = redactionTerms(terms)
	t.mu.Unlock()
}

// Release closes the log of one run. A follower that is still reading keeps
// the topic alive until it disconnects; the next run of the operation ID
// starts a fresh topic.
func (s *LogSink) Release(operationID string) {
	if s == nil || operationID == "" {
		return
	}
	t := s.topic(operationID)
	t.mu.Lock()
	t.active = false
	empty := len(t.subs) == 0
	t.mu.Unlock()
	if empty {
		s.drop(operationID, t)
	}
}

// Append stores one line and delivers it to every follower. The line is
// redacted first, then split at limits.log_line, so a secret that would
// straddle a chunk boundary is still removed and every chunk is its own
// entry with its own sequence number. A chunk that would exceed
// limits.log_total is dropped, and the run gets one system entry that says
// so; later lines are dropped too. A store failure drops the chunk, so a
// follower never sees a line that a resume cannot replay.
func (s *LogSink) Append(operationID, stream, line string) {
	if s == nil || operationID == "" {
		return
	}
	if stream != StreamStderr {
		stream = StreamSystem
	}
	t := s.topic(operationID)
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadTotal(t)
	if t.truncated {
		return
	}
	// One measure for both the cap check and the stored bytes: the
	// redacted text, which is what the store keeps. Redaction runs before
	// the split, so no chunk can hide a secret across a boundary.
	for _, chunk := range splitLine(redactLine(line, t.terms), s.lineLimit) {
		if t.total+len(chunk) > s.limit {
			t.truncated = true
			s.storeLocked(t, StreamSystem, truncationLine(s.limit))
			return
		}
		s.storeLocked(t, stream, chunk)
	}
}

// truncationLine is the one system entry of a run that reached
// limits.log_total (docs/handler-contract.md section 11).
func truncationLine(limit int) string {
	return fmt.Sprintf("log limit reached: %d bytes stored, further lines are dropped", limit)
}

// storeLocked stores one redacted line, then wakes the followers. The
// caller holds the topic mutex.
func (s *LogSink) storeLocked(t *logTopic, stream, line string) {
	entry, err := s.store.AppendLog(t.id, LogEntry{
		Time:   s.now().UTC(),
		Stream: stream,
		Line:   line,
	})
	if err != nil {
		return
	}
	t.total += len(entry.Line)
	for sub := range t.subs {
		sub.queue = append(sub.queue, entry)
		signal(sub.wake)
	}
}

// splitLine splits line into chunks of at most limit bytes, never inside a
// multi-byte UTF-8 rune (docs/handler-contract.md section 11). A line within
// the limit is returned unchanged. A single rune longer than the limit is
// kept whole, because splitting it would corrupt it.
func splitLine(line string, limit int) []string {
	if limit <= 0 || len(line) <= limit {
		return []string{line}
	}
	chunks := make([]string, 0, len(line)/limit+1)
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 {
			_, size := utf8.DecodeRuneInString(line)
			cut = size
		}
		chunks = append(chunks, line[:cut])
		line = line[cut:]
	}
	if line != "" {
		chunks = append(chunks, line)
	}
	return chunks
}

// loadTotal reads the bytes a previous process stored for this operation,
// so the cap holds across a restart. It runs once per topic.
func (s *LogSink) loadTotal(t *logTopic) {
	if t.loaded {
		return
	}
	t.loaded = true
	entries, err := s.store.Logs(t.id, 0)
	if err != nil {
		return
	}
	for _, entry := range entries {
		t.total += len(entry.Line)
	}
}

// Entries returns the stored log of one operation after a sequence number.
func (s *LogSink) Entries(operationID string, after int64) ([]LogEntry, error) {
	if s == nil {
		return []LogEntry{}, nil
	}
	return s.store.Logs(operationID, after)
}

// Follow subscribes a follower to one operation before reading the stored
// backlog. Every append waits for the topic mutex, so an entry is either in
// the backlog or in the subscription, never in neither and never in both.
func (s *LogSink) Follow(operationID string, after int64) (*LogSubscription, []LogEntry, error) {
	t := s.topic(operationID)
	t.mu.Lock()
	defer t.mu.Unlock()
	sub := &LogSubscription{topic: t, wake: make(chan struct{}, 1)}
	if t.subs == nil {
		t.subs = map[*LogSubscription]struct{}{}
	}
	t.subs[sub] = struct{}{}
	backlog, err := s.store.Logs(operationID, after)
	if err != nil {
		sub.closed = true
		delete(t.subs, sub)
		return nil, nil, err
	}
	return sub, backlog, nil
}

// topic returns the state of one operation, creating it when it is missing.
func (s *LogSink) topic(operationID string) *logTopic {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.topics[operationID]
	if !ok {
		t = &logTopic{id: operationID, sink: s, subs: map[*LogSubscription]struct{}{}}
		s.topics[operationID] = t
	}
	return t
}

// drop removes a topic from the map when it is still the one registered for
// the operation and nobody follows it.
func (s *LogSink) drop(operationID string, t *logTopic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.topics[operationID] == t {
		delete(s.topics, operationID)
	}
}

// signal wakes a follower without blocking the writer. The buffer of one
// keeps the signal while the reader drains.
func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// LogSubscription is one live follower of an operation log. It receives
// entries once, in sequence order. Close it to release the subscription.
type LogSubscription struct {
	topic *logTopic
	wake  chan struct{}

	// The fields below are guarded by topic.mu.
	queue  []LogEntry
	closed bool
}

// Wake returns a channel that a write signals. The receiver must call
// Drain to take the entries; a stale signal costs one extra loop only.
func (sub *LogSubscription) Wake() <-chan struct{} { return sub.wake }

// Drain takes every queued entry and consumes the pending signal. It never
// blocks.
func (sub *LogSubscription) Drain() []LogEntry {
	t := sub.topic
	t.mu.Lock()
	defer t.mu.Unlock()
	out := sub.queue
	sub.queue = nil
	select {
	case <-sub.wake:
	default:
	}
	return out
}

// Close stops the subscription and releases the topic when the run is over
// and no other follower reads it.
func (sub *LogSubscription) Close() {
	t := sub.topic
	t.mu.Lock()
	if sub.closed {
		t.mu.Unlock()
		return
	}
	sub.closed = true
	delete(t.subs, sub)
	empty := len(t.subs) == 0
	inactive := !t.active
	t.mu.Unlock()
	signal(sub.wake)
	if empty && inactive {
		t.sink.drop(t.id, t)
	}
}
