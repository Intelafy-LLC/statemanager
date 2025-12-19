package statemanager

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// inMemoryStore is an in-memory implementation of Store.
// A root instance manages jobs; job-scoped instances operate on a single job.
type inMemoryStore struct {
	root  *inMemoryStore
	job   *inMemoryJob
	jobID string

	mu   sync.Mutex // protects jobs map (root only)
	jobs map[string]*inMemoryJob
}

type inMemoryJob struct {
	numTasks int

	mu          sync.RWMutex
	cache       map[string]State
	versions    map[string]int64
	subscribers map[chan StateEvent]struct{}
	closed      bool
}

// Ensure inMemoryStore implements Store and Cleaner interfaces.
var (
	_ Store   = (*inMemoryStore)(nil)
	_ Cleaner = (*inMemoryStore)(nil)
)

// NewInMemoryStore constructs an unbound in-memory store backend.
func NewInMemoryStore() Store {
	root := &inMemoryStore{}
	root.root = root
	root.jobs = make(map[string]*inMemoryJob)
	return root
}

// NewJob creates a job and returns a job-scoped store.
func (s *inMemoryStore) NewJob(ctx context.Context, jobID string, numTasks int) (Store, int, error) {
	_ = ctx
	if numTasks <= 0 {
		return nil, 0, ErrNotSupported
	}
	root := s.rootStore()
	root.mu.Lock()
	defer root.mu.Unlock()
	if _, exists := root.jobs[jobID]; exists {
		return nil, 0, ErrNotSupported
	}
	job := &inMemoryJob{
		numTasks:    numTasks,
		cache:       make(map[string]State),
		versions:    make(map[string]int64),
		subscribers: make(map[chan StateEvent]struct{}),
	}
	root.jobs[jobID] = job
	return &inMemoryStore{root: root, job: job, jobID: jobID}, numTasks, nil
}

// OpenJob opens an existing job.
func (s *inMemoryStore) OpenJob(ctx context.Context, jobID string) (Store, int, error) {
	_ = ctx
	root := s.rootStore()
	root.mu.Lock()
	defer root.mu.Unlock()
	job, ok := root.jobs[jobID]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return &inMemoryStore{root: root, job: job, jobID: jobID}, job.numTasks, nil
}

// SetState writes a state, bumping version per {tag,task} key.
func (s *inMemoryStore) SetState(ctx context.Context, state State) error {
	job := s.job
	if job == nil {
		return ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	key := stateKey(state)

	job.mu.Lock()
	if job.closed {
		job.mu.Unlock()
		return ErrNotSupported
	}
	prevVer := job.versions[key]
	state.Version = prevVer + 1
	if state.Timestamp.IsZero() {
		state.Timestamp = time.Now().UTC()
	}
	job.cache[key] = state
	job.versions[key] = state.Version
	subs := copySubscribers(job.subscribers)
	job.mu.Unlock()

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
	job := s.job
	if job == nil {
		return State{}, ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	default:
	}
	key := tag + ":" + strconv.Itoa(taskID)
	job.mu.RLock()
	defer job.mu.RUnlock()
	st, ok := job.cache[key]
	if !ok {
		return State{}, ErrNotFound
	}
	return st, nil
}

// ListStatesForTag lists states for a tag.
func (s *inMemoryStore) ListStatesForTag(ctx context.Context, tag string) ([]State, error) {
	job := s.job
	if job == nil {
		return nil, ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	out := make([]State, 0)
	for _, st := range job.cache {
		if st.Tag == tag {
			out = append(out, st)
		}
	}
	return out, nil
}

// ListAllStates lists all states.
func (s *inMemoryStore) ListAllStates(ctx context.Context) ([]State, error) {
	job := s.job
	if job == nil {
		return nil, ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	out := make([]State, 0, len(job.cache))
	for _, st := range job.cache {
		out = append(out, st)
	}
	return out, nil
}

// Subscribe registers a new subscriber and returns its channel.
func (s *inMemoryStore) Subscribe(ctx context.Context) (<-chan StateEvent, error) {
	job := s.job
	if job == nil {
		return nil, ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ch := make(chan StateEvent, 16)
	job.mu.Lock()
	if job.closed {
		job.mu.Unlock()
		return nil, ErrNotSupported
	}
	job.subscribers[ch] = struct{}{}
	job.mu.Unlock()

	go func() {
		<-ctx.Done()
		job.mu.Lock()
		delete(job.subscribers, ch)
		close(ch)
		job.mu.Unlock()
	}()

	return ch, nil
}

// Close stops the store and closes internal channels.
func (s *inMemoryStore) Close() error {
	job := s.job
	if job == nil {
		return nil
	}
	job.mu.Lock()
	if job.closed {
		job.mu.Unlock()
		return nil
	}
	job.closed = true
	for ch := range job.subscribers {
		close(ch)
	}
	job.subscribers = nil
	job.mu.Unlock()
	return nil
}

// DeleteJob removes job metadata and task states.
func (s *inMemoryStore) DeleteJob(ctx context.Context) error {
	job := s.job
	if job == nil {
		return ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	job.mu.Lock()
	job.cache = make(map[string]State)
	job.versions = make(map[string]int64)
	job.mu.Unlock()
	return nil
}

// MarkJobDeleted tombstones a job for GC (here, clears cache).
func (s *inMemoryStore) MarkJobDeleted(ctx context.Context) error {
	return s.DeleteJob(ctx)
}

func (s *inMemoryStore) rootStore() *inMemoryStore {
	if s.root != nil {
		return s.root
	}
	return s
}

func copySubscribers(src map[chan StateEvent]struct{}) map[chan StateEvent]struct{} {
	copy := make(map[chan StateEvent]struct{}, len(src))
	for ch := range src {
		copy[ch] = struct{}{}
	}
	return copy
}
