package statemanager

import (
	"context"
	"time"
)

// State represents the status of a single task for a specific tag.
type State struct {
	JobID     string
	TaskID    int
	Tag       string
	Status    int32
	Message   string
	Timestamp time.Time
}

// Store is the interface for backend data storage. A Store instance is scoped
// to a single job.
type Store interface {
	SetState(ctx context.Context, state State) error
	GetState(ctx context.Context, taskID int, tag string) (State, error)
	ListStatesForTag(ctx context.Context, tag string) ([]State, error)
	ListAllStates(ctx context.Context) ([]State, error)
	Subscribe(ctx context.Context) (<-chan State, error)
	Close() error
}

// Manager is the main entrypoint for interacting with the state management system.
type Manager struct {
	jobID    string
	numTasks int
	store    Store
}

// NewManager creates a new state manager. The numTasks parameter is the expected
// number of tasks for the job. If the job already exists in the store, the
// stored value for numTasks will be used.
func NewManager(jobID string, numTasks int, store Store) *Manager {
	panic("not implemented")
}

// Close gracefully shuts down the manager. It closes all active subscription
// channels and closes the underlying store connection.
func (m *Manager) Close() error {
	panic("not implemented")
}

// SetState persists the state for a specific task and tag.
func (m *Manager) SetState(ctx context.Context, state State) error {
	panic("not implemented")
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
	panic("not implemented")
}

// Subscribe returns a channel that emits state changes.
// The lifetime of the subscription is tied to the provided context.
func (m *Manager) Subscribe(ctx context.Context, opts ...Option) (<-chan State, error) {
	panic("not implemented")
}
