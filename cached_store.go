package statemanager

import (
	"context"
	"sync"
)

// CachedStore decorates a JobScopedStore to provide replayable subscriptions.
// It keeps an in-memory cache keyed by {tag,taskID} and emits replay before live events.
type CachedStore struct {
	inner JobScopedStore

	mu          sync.RWMutex
	cache       map[string]State
	subscribers map[chan StateEvent]struct{}
	closed      bool
}

// NewCachedStore wraps a job-scoped store with caching and replay support.
func NewCachedStore(ctx context.Context, store JobScopedStore) (*CachedStore, error) {
	if store == nil {
		return nil, ErrNotSupported
	}
	cs := &CachedStore{
		inner:       store,
		cache:       make(map[string]State),
		subscribers: make(map[chan StateEvent]struct{}),
	}
	if err := cs.warmCache(ctx); err != nil {
		return nil, err
	}
	return cs, nil
}

// JobID returns the bound job identifier.
func (c *CachedStore) JobID() string { return c.inner.JobID() }

// NumTasks returns the configured number of tasks.
func (c *CachedStore) NumTasks() int { return c.inner.NumTasks() }

// NewJob should not be called on a job-scoped cached store.
func (c *CachedStore) NewJob(ctx context.Context, jobID string, numTasks int) (Store, int, error) {
	return nil, 0, ErrNotSupported
}

// OpenJob should not be called on a job-scoped cached store.
func (c *CachedStore) OpenJob(ctx context.Context, jobID string) (Store, int, error) {
	return nil, 0, ErrNotSupported
}

// SetState delegates and updates cache.
func (c *CachedStore) SetState(ctx context.Context, state State) error {
	if err := c.inner.SetState(ctx, state); err != nil {
		return err
	}
	c.updateCache(state)
	c.broadcast(StateEvent{State: state, Replay: false})
	return nil
}

// GetState delegates.
func (c *CachedStore) GetState(ctx context.Context, taskID int, tag string) (State, error) {
	return c.inner.GetState(ctx, taskID, tag)
}

// ListStatesForTag delegates.
func (c *CachedStore) ListStatesForTag(ctx context.Context, tag string) ([]State, error) {
	return c.inner.ListStatesForTag(ctx, tag)
}

// ListAllStates delegates.
func (c *CachedStore) ListAllStates(ctx context.Context) ([]State, error) {
	return c.inner.ListAllStates(ctx)
}

// Subscribe replays cached state then streams live updates from inner Subscribe.
func (c *CachedStore) Subscribe(ctx context.Context) (<-chan StateEvent, error) {
	innerCh, err := c.inner.Subscribe(ctx)
	if err != nil {
		return nil, err
	}

	out := make(chan StateEvent, 16)
	cacheSnapshot := c.snapshot()

	go func() {
		defer close(out)
		// Replay
		sortStates(cacheSnapshot)
		for _, st := range cacheSnapshot {
			select {
			case <-ctx.Done():
				return
			case out <- StateEvent{State: st, Replay: true}:
			}
		}

		// Live
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-innerCh:
				if !ok {
					return
				}
				c.updateCache(evt.State)
				select {
				case <-ctx.Done():
					return
				case out <- StateEvent{State: evt.State, Replay: false}:
				}
			}
		}
	}()

	return out, nil
}

// Close closes subscribers and the inner store.
func (c *CachedStore) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	for ch := range c.subscribers {
		close(ch)
	}
	c.subscribers = nil
	c.mu.Unlock()
	return c.inner.Close()
}

// broadcast is unused by Subscribe wrapper but kept for SetState fanout.
func (c *CachedStore) broadcast(evt StateEvent) {
	c.mu.RLock()
	for ch := range c.subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
	c.mu.RUnlock()
}

func (c *CachedStore) updateCache(st State) {
	key := stateKey(st)
	c.mu.Lock()
	prev, ok := c.cache[key]
	if !ok || st.Version > prev.Version {
		c.cache[key] = st
	}
	c.mu.Unlock()
}

func (c *CachedStore) snapshot() []State {
	c.mu.RLock()
	out := make([]State, 0, len(c.cache))
	for _, st := range c.cache {
		out = append(out, st)
	}
	c.mu.RUnlock()
	return out
}

func (c *CachedStore) warmCache(ctx context.Context) error {
	states, err := c.inner.ListAllStates(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for _, st := range states {
		c.cache[stateKey(st)] = st
	}
	c.mu.Unlock()
	return nil
}
