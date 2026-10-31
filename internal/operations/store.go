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
	// Get returns one record by operation ID. The bool reports whether
	// the operation exists; a read failure is the error, so a caller can
	// never mistake a broken store for a missing operation.
	Get(id string) (Record, bool, error)
	// List returns the records that satisfy f, newest first: by creation
	// time descending, then by ID descending. A positive Limit returns
	// only the newest records. A read failure is an error, never an empty
	// list.
	List(f Filter) ([]Record, error)
	// ByIdempotency returns the operation of one requester and
	// Idempotency-Key. An empty key never matches. The bool reports
	// whether a record matched; a read failure is the error, so a retry
	// never looks like a new request when the lookup failed.
	ByIdempotency(requester, key string) (Record, bool, error)
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
func (s *MemoryStore) Get(id string) (Record, bool, error) {
	if s == nil {
		return Record{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false, nil
	}
	return rec.clone(), true, nil
}

// List returns the records that satisfy f, newest first: by creation time
// descending, then by ID descending, and at most f.Limit records when it is
// positive (docs/architecture.md section 11.1).
func (s *MemoryStore) List(f Filter) ([]Record, error) {
	if s == nil {
		return []Record{}, nil
	}
	s.mu.RLock()
	out := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		if f.matches(rec) {
			out = append(out, rec.clone())
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ByIdempotency returns the operation of one requester and key.
func (s *MemoryStore) ByIdempotency(requester, key string) (Record, bool, error) {
	if s == nil || key == "" {
		return Record{}, false, nil
	}
	s.mu.RLock()
	id, ok := s.idempotency[idempotencyIndex(requester, key)]
	if !ok {
		s.mu.RUnlock()
		return Record{}, false, nil
	}
	rec, ok := s.records[id]
	s.mu.RUnlock()
	if !ok {
		return Record{}, false, nil
	}
	return rec.clone(), true, nil
}

// idempotencyIndex builds the lookup key of one requester and key. The
// NUL byte cannot appear in an ID or a header value, so two pairs never
// collide.
func idempotencyIndex(requester, key string) string {
	return requester + "\x00" + key
}
