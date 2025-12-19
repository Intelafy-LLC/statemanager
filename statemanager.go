package statemanager

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"
)

// State represents the status of a single task for a specific tag.
type State struct {
	JobID         string         `slog:"jobID"`
	TaskID        int            `slog:"taskID"`
	Tag           string         `slog:"tag"`
	Status        int32          `slog:"status"`
	Message       string         `slog:"message"`
	Timestamp     time.Time      `slog:"timestamp"`
	Version       int64          `slog:"version"`       // monotonic per {JobID, Tag, TaskID}; used for ordering and dedup
	EventID       string         `slog:"eventID"`       // optional (MVP+): unique per write; idempotency token
	Payload       map[string]any `slog:"payload"`       // optional tag-specific data
	SchemaVersion int            `slog:"schemaVersion"` // schema version of the payload/state
}

// StateEvent wraps a State with delivery metadata (e.g., replay indicator).
type StateEvent struct {
	State  State
	Replay bool
}

// Errors returned by the manager/store.
var (
	ErrTaskMismatch = errors.New("state task does not match manager task")
	ErrNotSupported = errors.New("operation not supported by store")
	ErrNotFound     = errors.New("state not found")
	// ErrNotImplemented is returned by stubbed methods.
	ErrNotImplemented = errors.New("not implemented")
)

// Store abstracts backend access and job lifecycle.
//
// Unbound usage: call NewJob (create, fail if exists) or OpenJob (open, fail if missing)
// to obtain a job-scoped Store. Job-scoped instances implement the same interface; calling
// job operations on an unbound instance should return ErrNotSupported.
type Store interface {
	// NewJob creates a job and returns a job-scoped Store plus the authoritative numTasks.
	// It must fail if the job already exists or numTasks <= 0.
	NewJob(ctx context.Context, jobID string, numTasks int) (Store, int, error)
	// OpenJob opens an existing job and returns a job-scoped Store plus the authoritative numTasks.
	// It must fail if the job does not exist.
	OpenJob(ctx context.Context, jobID string) (Store, int, error)

	SetState(ctx context.Context, state State) error
	GetState(ctx context.Context, taskID int, tag string) (State, error)
	ListStatesForTag(ctx context.Context, tag string) ([]State, error)
	ListAllStates(ctx context.Context) ([]State, error)
	Subscribe(ctx context.Context) (<-chan StateEvent, error)
	Close() error
}

// Cleaner defines optional cleanup capabilities for a store implementation.
// Implementations that do not support cleanup can omit these methods.
type Cleaner interface {
	DeleteJob(ctx context.Context) error
	MarkJobDeleted(ctx context.Context) error
}

// Manager is the main entrypoint for interacting with the state management system.
type Manager struct {
	jobID     string
	taskIndex int
	numTasks  int
	store     Store
}

// JobID returns the job identifier for this manager.
func (m *Manager) JobID() string {
	return m.jobID
}

// TaskIndex returns the task index this manager is bound to.
func (m *Manager) TaskIndex() int {
	return m.taskIndex
}

// NumTasks returns the configured number of tasks for the job.
func (m *Manager) NumTasks() int {
	return m.numTasks
}

// NewManagerForNewJob creates the job (failing if it already exists) and binds a manager to it.
func NewManagerForNewJob(ctx context.Context, store Store, jobID string, taskIndex int, numTasks int) (*Manager, error) {
	jobStore, resolved, err := store.NewJob(ctx, jobID, numTasks)
	if err != nil {
		return nil, err
	}
	return &Manager{
		jobID:     jobID,
		taskIndex: taskIndex,
		numTasks:  resolved,
		store:     jobStore,
	}, nil
}

// NewManagerForExistingJob opens an existing job (failing if missing) and binds a manager to it.
func NewManagerForExistingJob(ctx context.Context, store Store, jobID string, taskIndex int) (*Manager, error) {
	jobStore, resolved, err := store.OpenJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return &Manager{
		jobID:     jobID,
		taskIndex: taskIndex,
		numTasks:  resolved,
		store:     jobStore,
	}, nil
}

// Close gracefully shuts down the manager. It closes all active subscription
// channels and closes the underlying store connection.
func (m *Manager) Close() error {
	if m == nil || m.store == nil {
		return nil
	}
	return m.store.Close()
}

// DeleteJob deletes all job metadata and task states if the underlying store supports it.
func (m *Manager) DeleteJob(ctx context.Context) error {
	if c, ok := m.store.(Cleaner); ok {
		return c.DeleteJob(ctx)
	}
	return ErrNotSupported
}

// MarkJobDeleted marks a job as deleted/tombstoned if the underlying store supports it.
func (m *Manager) MarkJobDeleted(ctx context.Context) error {
	if c, ok := m.store.(Cleaner); ok {
		return c.MarkJobDeleted(ctx)
	}
	return ErrNotSupported
}

// SetState persists the state for a specific task and tag.
// It rejects writes where state.TaskID does not match the manager's taskIndex.
func (m *Manager) SetState(ctx context.Context, state State) error {
	if state.TaskID != m.taskIndex {
		return ErrTaskMismatch
	}
	return m.store.SetState(ctx, state)
}

// Option defines the signature for functions that modify GetState queries.
type Option func(*queryOptions)

type queryOptions struct {
	taskID *int
	tag    *string
}

// WithTaskID is an option to filter by a specific task ID.
func WithTaskID(id int) Option {
	return func(o *queryOptions) {
		o.taskID = &id
	}
}

// WithTag is an option to filter by a specific tag.
func WithTag(tag string) Option {
	return func(o *queryOptions) {
		o.tag = &tag
	}
}

// GetState retrieves state based on the provided options.
// It aggregates status according to the "lowest status wins" rule.
func (m *Manager) GetState(ctx context.Context, opts ...Option) (State, error) {
	qo := &queryOptions{}
	for _, opt := range opts {
		opt(qo)
	}

	// Case 1: specific task + tag.
	if qo.taskID != nil && qo.tag != nil {
		st, err := m.store.GetState(ctx, *qo.taskID, *qo.tag)
		if err != nil {
			return State{JobID: m.jobID, TaskID: *qo.taskID, Tag: *qo.tag, Status: 0}, err
		}
		return st, nil
	}

	// Case 2: aggregate for tag across all tasks (lowest status wins).
	if qo.tag != nil {
		states, err := m.store.ListStatesForTag(ctx, *qo.tag)
		if err != nil {
			return State{}, err
		}
		// Seed defaults for missing tasks.
		minState := State{JobID: m.jobID, Tag: *qo.tag, Status: 0, TaskID: 0}
		found := false
		statusByTask := make(map[int]State, len(states))
		for _, st := range states {
			statusByTask[st.TaskID] = st
		}
		for task := 0; task < m.numTasks; task++ {
			st, ok := statusByTask[task]
			if !ok {
				st = State{JobID: m.jobID, TaskID: task, Tag: *qo.tag, Status: 0}
			}
			if !found || st.Status < minState.Status {
				minState = st
				found = true
			}
		}
		return minState, nil
	}

	// Case 3: aggregate for a task across all its tags.
	if qo.taskID != nil {
		states, err := m.store.ListAllStates(ctx)
		if err != nil {
			return State{}, err
		}
		minState := State{JobID: m.jobID, TaskID: *qo.taskID, Status: 0}
		found := false
		for _, st := range states {
			if st.TaskID != *qo.taskID {
				continue
			}
			if !found || st.Status < minState.Status {
				minState = st
				found = true
			}
		}
		return minState, nil
	}

	// Case 4: global aggregate across all tasks and tags.
	states, err := m.store.ListAllStates(ctx)
	if err != nil {
		return State{}, err
	}
	minState := State{JobID: m.jobID, Status: 0}
	found := false
	for _, st := range states {
		if !found || st.Status < minState.Status {
			minState = st
			found = true
		}
	}
	return minState, nil
}

// Subscribe returns a channel that emits state changes.
// The lifetime of the subscription is tied to the provided context.
func (m *Manager) Subscribe(ctx context.Context, opts ...Option) (<-chan StateEvent, error) {
	qo := &queryOptions{}
	for _, opt := range opts {
		opt(qo)
	}

	// Prepare replay snapshot based on options.
	snapshot, err := m.snapshotStates(ctx, qo)
	if err != nil {
		return nil, err
	}

	storeCh, err := m.store.Subscribe(ctx)
	if err != nil {
		return nil, err
	}

	out := make(chan StateEvent, 4)
	seen := make(map[string]int64)

	// Emit replay in timestamp ascending order, deduplicated by increasing version per key.
	go func() {
		defer close(out)
		for _, st := range snapshot {
			key := stateKey(st)
			if v, ok := seen[key]; ok && st.Version <= v {
				continue
			}
			seen[key] = st.Version
			select {
			case out <- StateEvent{State: st, Replay: true}:
			case <-ctx.Done():
				return
			}
		}

		// Live phase: forward filtered updates, deduping by version.
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-storeCh:
				if !ok {
					return
				}
				if !matchesOptions(evt.State, qo) {
					continue
				}
				key := stateKey(evt.State)
				if v, ok := seen[key]; ok && evt.State.Version <= v {
					continue
				}
				seen[key] = evt.State.Version
				select {
				case out <- StateEvent{State: evt.State, Replay: false}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// snapshotStates builds the replay set based on options, sorted by timestamp ascending.
func (m *Manager) snapshotStates(ctx context.Context, qo *queryOptions) ([]State, error) {
	// Specific task+tag
	if qo.taskID != nil && qo.tag != nil {
		st, err := m.store.GetState(ctx, *qo.taskID, *qo.tag)
		if err != nil {
			return nil, err
		}
		return []State{st}, nil
	}

	// Tag-scoped snapshot across tasks.
	if qo.tag != nil {
		states, err := m.store.ListStatesForTag(ctx, *qo.tag)
		if err != nil {
			return nil, err
		}
		filtered := make([]State, 0, len(states))
		for _, st := range states {
			if st.Tag == *qo.tag {
				filtered = append(filtered, st)
			}
		}
		sortStates(filtered)
		return filtered, nil
	}

	// Task-scoped snapshot across tags.
	if qo.taskID != nil {
		states, err := m.store.ListAllStates(ctx)
		if err != nil {
			return nil, err
		}
		filtered := make([]State, 0, len(states))
		for _, st := range states {
			if st.TaskID == *qo.taskID {
				filtered = append(filtered, st)
			}
		}
		sortStates(filtered)
		return filtered, nil
	}

	// Global snapshot: all states.
	states, err := m.store.ListAllStates(ctx)
	if err != nil {
		return nil, err
	}
	sortStates(states)
	return states, nil
}

// matchesOptions reports whether a state matches subscription options.
func matchesOptions(st State, qo *queryOptions) bool {
	if qo.taskID != nil && st.TaskID != *qo.taskID {
		return false
	}
	if qo.tag != nil && st.Tag != *qo.tag {
		return false
	}
	return true
}

// Synchronize waits until all tasks have reached at least targetStatus for the given tag
// or the context expires. It returns a slice of length NumTasks containing the latest
// observed state per task (missing tasks are defaulted) and any error (including context
// timeout/cancellation).
func (m *Manager) Synchronize(ctx context.Context, tag string, targetStatus int32) ([]State, error) {
	ctxSub, cancel := context.WithCancel(ctx)
	defer cancel()

	states := make([]State, m.numTasks)
	for i := 0; i < m.numTasks; i++ {
		states[i] = State{JobID: m.jobID, TaskID: i, Tag: tag, Status: 0}
	}

	completed := func() bool {
		for i := 0; i < m.numTasks; i++ {
			if states[i].Status < targetStatus {
				return false
			}
		}
		return true
	}

	seed, err := m.store.ListStatesForTag(ctxSub, tag)
	if err != nil {
		return nil, err
	}
	for _, st := range seed {
		if st.TaskID >= 0 && st.TaskID < m.numTasks {
			states[st.TaskID] = st
		}
	}
	if completed() {
		return states, nil
	}

	ch, err := m.Subscribe(ctxSub, WithTag(tag))
	if err != nil {
		return nil, err
	}

	for {
		select {
		case <-ctxSub.Done():
			return states, ctx.Err()
		case evt, ok := <-ch:
			if !ok {
				return states, ctx.Err()
			}
			st := evt.State
			if st.TaskID >= 0 && st.TaskID < m.numTasks {
				states[st.TaskID] = st
				if completed() {
					return states, nil
				}
			}
		}
	}
}

// stateKey builds a key for deduplication per task+tag.
func stateKey(st State) string {
	return st.Tag + ":" + strconv.Itoa(st.TaskID)
}

// sortStates orders states by Timestamp ascending, tie-breaking by Version.
func sortStates(states []State) {
	sort.Slice(states, func(i, j int) bool {
		if states[i].Timestamp.Equal(states[j].Timestamp) {
			return states[i].Version < states[j].Version
		}
		return states[i].Timestamp.Before(states[j].Timestamp)
	})
}
