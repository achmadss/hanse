package operations

import (
	"fmt"
	"sort"
	"sync"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// Store keeps operation records. The Gateway ships an in-memory
// implementation (NewMemoryStore); issue #17 replaces it with the SQLite
// history store behind this same interface.
type Store interface {
	// Insert stores a new record. An ID that already exists is an error.
	Insert(rec Record) error
	// Update replaces the record with the same ID.
	Update(rec Record) error
	// Get returns one record by operation ID.
	Get(id string) (Record, bool)
	// List returns the records that satisfy f, sorted by operation ID.
	List(f Filter) []Record
	// ByIdempotency returns the operation of one requester and
	// Idempotency-Key. An empty key never matches.
	ByIdempotency(requester, key string) (Record, bool)
}

// Recoverer is implemented by a Store that survives a restart. It fails
// every operation that a previous process left queued or running, with the
// gateway_restart code (docs/architecture.md section 9.6).
type Recoverer interface {
	RecoverInterrupted(now time.Time) (int, error)
}

// Pruner is implemented by a Store that keeps records for a limited time.
// It deletes only finished records older than the retention period; an
// active record is never deleted.
type Pruner interface {
	PurgeOld(now time.Time) (int64, error)
}

// MemoryStore is the in-memory Store of the current Gateway. It is safe
// for concurrent use. Records are kept in process memory, so a restart
// loses them; durability is issue #17.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
	// idempotency maps requester + separator + key to an operation ID.
	idempotency map[string]string
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records:     map[string]Record{},
		idempotency: map[string]string{},
	}
}

// Insert stores a new record.
func (s *MemoryStore) Insert(rec Record) error {
	if s == nil {
		return herrors.New(herrors.Internal, "the operation store is not available", nil)
	}
	if rec.ID == "" {
		return herrors.New(herrors.Internal, "an operation record needs an ID", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]Record{}
	}
	if _, dup := s.records[rec.ID]; dup {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("operation %q is stored twice", rec.ID), nil)
	}
	s.records[rec.ID] = rec.clone()
	if rec.IdempotencyKey != "" {
		if s.idempotency == nil {
			s.idempotency = map[string]string{}
		}
		s.idempotency[idempotencyIndex(rec.Requester, rec.IdempotencyKey)] = rec.ID
	}
	return nil
}

// Update replaces the record with the same ID.
func (s *MemoryStore) Update(rec Record) error {
	if s == nil {
		return herrors.New(herrors.Internal, "the operation store is not available", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[rec.ID]; !ok {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("operation %q is not stored", rec.ID), nil)
	}
	s.records[rec.ID] = rec.clone()
	return nil
}

// Get returns one record by operation ID.
func (s *MemoryStore) Get(id string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false
	}
	return rec.clone(), true
}

// List returns the records that satisfy f, sorted by operation ID
// (docs/architecture.md section 11.1: every list is sorted by ID).
func (s *MemoryStore) List(f Filter) []Record {
	if s == nil {
		return []Record{}
	}
	s.mu.RLock()
	out := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		if f.matches(rec) {
			out = append(out, rec.clone())
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ByIdempotency returns the operation of one requester and key.
func (s *MemoryStore) ByIdempotency(requester, key string) (Record, bool) {
	if s == nil || key == "" {
		return Record{}, false
	}
	s.mu.RLock()
	id, ok := s.idempotency[idempotencyIndex(requester, key)]
	if !ok {
		s.mu.RUnlock()
		return Record{}, false
	}
	rec, ok := s.records[id]
	s.mu.RUnlock()
	if !ok {
		return Record{}, false
	}
	return rec.clone(), true
}

// idempotencyIndex builds the lookup key of one requester and key. The
// NUL byte cannot appear in an ID or a header value, so two pairs never
// collide.
func idempotencyIndex(requester, key string) string {
	return requester + "\x00" + key
}
