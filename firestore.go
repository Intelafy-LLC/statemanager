//go:build firestore
// +build firestore

package statemanager

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// firestoreStore is a placeholder Firestore-backed implementation of Store.
// It maintains an in-memory cache and a single listener stream per job (per task process).
type firestoreStore struct {
	jobID    string
	numTasks int

	mu          sync.RWMutex
	cache       map[string]State             // key: tag:taskID
	versions    map[string]int64             // monotonic per key
	subscribers map[chan StateEvent]struct{} // active subscriber chans
	closed      bool

	updates chan StateEvent // legacy broadcast channel (kept for simplicity)

	stop func()
}

// Ensure firestoreStore implements optional Cleaner interface.
var _ Cleaner = (*firestoreStore)(nil)

// NewFirestoreStore constructs a Firestore store for a job, returning the authoritative numTasks.
// TODO: wire Firestore client, job doc get-or-create, listener startup, cache warmup.
func NewFirestoreStore(ctx context.Context, projectID, jobID string, numTasks int) (Store, int, error) {
	_ = projectID

	fs := &firestoreStore{
		jobID:       jobID,
		numTasks:    numTasks,
		cache:       make(map[string]State),
		versions:    make(map[string]int64),
		subscribers: make(map[chan StateEvent]struct{}),
		updates:     make(chan StateEvent, 16),
	}

	return fs, numTasks, nil
}

// SetState writes a state. In this placeholder, it operates in-memory and bumps version per {tag,task} key.
func (f *firestoreStore) SetState(ctx context.Context, state State) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	key := stateKey(state)

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return ErrNotSupported
	}
	prevVer := f.versions[key]
	state.Version = prevVer + 1
	if state.Timestamp.IsZero() {
		state.Timestamp = time.Now().UTC()
	}
	f.cache[key] = state
	f.versions[key] = state.Version
	subs := f.copySubscribersLocked()
	f.mu.Unlock()

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
func (f *firestoreStore) GetState(ctx context.Context, taskID int, tag string) (State, error) {
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	default:
	}
	key := tag + ":" + strconv.Itoa(taskID)
	f.mu.RLock()
	defer f.mu.RUnlock()
	st, ok := f.cache[key]
	if !ok {
		return State{}, ErrNotFound
	}
	return st, nil
}

// ListStatesForTag lists states for a tag.
func (f *firestoreStore) ListStatesForTag(ctx context.Context, tag string) ([]State, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]State, 0)
	for _, st := range f.cache {
		if st.Tag == tag {
			out = append(out, st)
		}
	}
	return out, nil
}

// ListAllStates lists all states.
func (f *firestoreStore) ListAllStates(ctx context.Context) ([]State, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]State, 0, len(f.cache))
	for _, st := range f.cache {
		out = append(out, st)
	}
	return out, nil
}

// Subscribe registers a new subscriber and returns its channel.
func (f *firestoreStore) Subscribe(ctx context.Context) (<-chan StateEvent, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ch := make(chan StateEvent, 16)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, ErrNotSupported
	}
	f.subscribers[ch] = struct{}{}
	f.mu.Unlock()

	// Unregister on context cancel.
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		delete(f.subscribers, ch)
		close(ch)
		f.mu.Unlock()
	}()

	return ch, nil
}

// Close stops the listener and closes internal channels.
func (f *firestoreStore) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	if f.stop != nil {
		f.stop()
	}
	for ch := range f.subscribers {
		close(ch)
	}
	f.subscribers = nil
	f.mu.Unlock()
	return nil
}

// copySubscribersLocked returns a shallow copy of current subscribers. Caller must hold f.mu.
func (f *firestoreStore) copySubscribersLocked() map[chan StateEvent]struct{} {
	copy := make(map[chan StateEvent]struct{}, len(f.subscribers))
	for ch := range f.subscribers {
		copy[ch] = struct{}{}
	}
	return copy
}

// DeleteJob removes job metadata and task states (in-memory placeholder).
func (f *firestoreStore) DeleteJob(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	f.mu.Lock()
	f.cache = make(map[string]State)
	f.versions = make(map[string]int64)
	f.mu.Unlock()
	return nil
}

// MarkJobDeleted tombstones a job for GC (in-memory placeholder: clears cache).
func (f *firestoreStore) MarkJobDeleted(ctx context.Context) error {
	return f.DeleteJob(ctx)
}
