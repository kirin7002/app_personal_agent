package turns

import (
	"context"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

// MemoryStore supports simulated single-process deployments and tests.
type MemoryStore struct {
	mu     sync.Mutex
	record map[recordKey]Record
	locks  map[contract.Scope]*scopeLock
}

type recordKey struct {
	scope contract.Scope
	id    string
}

type scopeLock struct {
	token   chan struct{}
	waiters int
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{record: make(map[recordKey]Record), locks: make(map[contract.Scope]*scopeLock)}
}

func (s *MemoryStore) LockScope(ctx context.Context, scope contract.Scope) (func(), error) {
	s.mu.Lock()
	lock := s.locks[scope]
	if lock == nil {
		lock = &scopeLock{token: make(chan struct{}, 1)}
		s.locks[scope] = lock
	}
	lock.waiters++
	s.mu.Unlock()

	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		lock.waiters--
		if lock.waiters == 0 {
			delete(s.locks, scope)
		}
	}
	select {
	case lock.token <- struct{}{}:
		return func() { <-lock.token; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func (s *MemoryStore) Get(_ context.Context, scope contract.Scope, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.record[recordKey{scope, id}]; ok {
		return record, nil
	}
	return Record{}, ErrNotFound
}

func (s *MemoryStore) Register(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := record.Context
	key := recordKey{binding.Scope, binding.VoiceTurnID}
	if _, ok := s.record[key]; ok {
		return conflict()
	}
	for existing, value := range s.record {
		if existing.scope == binding.Scope && value.Context.RequestID == binding.RequestID {
			return conflict()
		}
	}
	s.record[key] = record
	return nil
}

func (s *MemoryStore) Save(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey{record.Context.Scope, record.Context.VoiceTurnID}
	existing, ok := s.record[key]
	if !ok {
		return ErrNotFound
	}
	record.Cancelled = existing.Cancelled
	record.Closed = record.Closed || existing.Closed
	s.record[key] = record
	return nil
}

func (s *MemoryStore) Cancel(_ context.Context, scope contract.Scope, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey{scope, id}
	record := s.record[key]
	record.Cancelled, record.Closed = true, true
	s.record[key] = record
	return nil
}

func (s *MemoryStore) ResolveTurn(_ context.Context, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.record {
		if record.Context.TurnHandle == meta.TurnHandle {
			return resolve(record, meta, now)
		}
	}
	return contract.TurnContext{}, invalidTurn()
}
