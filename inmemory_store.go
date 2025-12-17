package statemanager

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// inMemoryStore is an in-memory implementation of Store.
// It is intended for local testing and validation of Manager behavior.
type inMemoryStore struct {
	jobID    string
	numTasks int

	mu          sync.RWMutex
	cache       map[string]State
	versions    map[string]int64
	subscribers map[chan StateEvent]struct{}
	closed      bool

	updates chan StateEvent // legacy broadcast channel (kept for simplicity)

	stop func()
}

// Ensure inMemoryStore implements Store and Cleaner interfaces.
var (
	_ Store   = (*inMemoryStore)(nil)
	_ Cleaner = (*inMemoryStore)(nil)
)

// NewInMemoryStore constructs an in-memory store for a job, returning the authoritative numTasks.
func NewInMemoryStore(ctx context.Context, jobID string, numTasks int) (Store, int, error) {
	_ = ctx

	s := &inMemoryStore{
		jobID:       jobID,
		numTasks:    numTasks,
		cache:       make(map[string]State),
		versions:    make(map[string]int64),
		subscribers: make(map[chan StateEvent]struct{}),
		updates:     make(chan StateEvent, 16),
	}

	return s, numTasks, nil
}

// SetState writes a state, bumping version per {tag,task} key.
func (s *inMemoryStore) SetState(ctx context.Context, state State) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	key := stateKey(state)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrNotSupported
	}
	prevVer := s.versions[key]
	state.Version = prevVer + 1
	if state.Timestamp.IsZero() {
		state.Timestamp = time.Now().UTC()
	}
	s.cache[key] = state
	s.versions[key] = state.Version
	subs := s.copySubscribersLocked()
	s.mu.Unlock()

	// Broadcast to subscribers; block unless context is cancelled to avoid message loss.
	for ch := range subs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- StateEvent{State: state, Replay: false}:
		}
	}

	return nil
}

// GetState reads a single state.
func (s *inMemoryStore) GetState(ctx context.Context, taskID int, tag string) (State, error) {
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	default:
	}
	key := tag + ":" + strconv.Itoa(taskID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.cache[key]
	if !ok {
		return State{}, ErrNotFound
	}
	return st, nil
}

// ListStatesForTag lists states for a tag.
func (s *inMemoryStore) ListStatesForTag(ctx context.Context, tag string) ([]State, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0)
	for _, st := range s.cache {
		if st.Tag == tag {
			out = append(out, st)
		}
	}
	return out, nil
}

// ListAllStates lists all states.
func (s *inMemoryStore) ListAllStates(ctx context.Context) ([]State, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, len(s.cache))
	for _, st := range s.cache {
		out = append(out, st)
	}
	return out, nil
}

// Subscribe registers a new subscriber and returns its channel.
func (s *inMemoryStore) Subscribe(ctx context.Context) (<-chan StateEvent, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ch := make(chan StateEvent, 16)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrNotSupported
	}
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()

	// Unregister on context cancel.
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		delete(s.subscribers, ch)
		close(ch)
		s.mu.Unlock()
	}()

	return ch, nil
}

// Close stops the store and closes internal channels.
func (s *inMemoryStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	for ch := range s.subscribers {
		close(ch)
	}
	s.subscribers = nil
	s.mu.Unlock()
	return nil
}

// DeleteJob removes job metadata and task states.
func (s *inMemoryStore) DeleteJob(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	s.mu.Lock()
	s.cache = make(map[string]State)
	s.versions = make(map[string]int64)
	s.mu.Unlock()
	return nil
}

// MarkJobDeleted tombstones a job for GC (here, clears cache).
func (s *inMemoryStore) MarkJobDeleted(ctx context.Context) error {
	return s.DeleteJob(ctx)
}

// copySubscribersLocked returns a shallow copy of current subscribers. Caller must hold s.mu.
func (s *inMemoryStore) copySubscribersLocked() map[chan StateEvent]struct{} {
	copy := make(map[chan StateEvent]struct{}, len(s.subscribers))
	for ch := range s.subscribers {
		copy[ch] = struct{}{}
	}
	return copy
}
