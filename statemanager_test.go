package statemanager

import (
	"context"
	"errors"
	"testing"
	"time"
)

type storeFactory func(t *testing.T) Store

var defaultStores = []struct {
	name string
	make storeFactory
}{
	{
		name: "inmemory",
		make: func(t *testing.T) Store {
			t.Helper()
			return NewInMemoryStore()
		},
	},
}

var inMemoryFactory = func(t *testing.T) Store {
	t.Helper()
	return NewInMemoryStore()
}

type stubStore struct {
	jobID    string
	numTasks int
	closed   bool
}

func newStubStore(jobID string, numTasks int) *stubStore {
	return &stubStore{jobID: jobID, numTasks: numTasks}
}

func (s *stubStore) NewJob(ctx context.Context, jobID string, numTasks int) (Store, int, error) {
	return newStubStore(jobID, numTasks), numTasks, nil
}

func (s *stubStore) OpenJob(ctx context.Context, jobID string) (Store, int, error) {
	return newStubStore(jobID, 1), 1, nil
}

func (s *stubStore) JobID() string { return s.jobID }
func (s *stubStore) NumTasks() int { return s.numTasks }

func (s *stubStore) SetState(ctx context.Context, state State) error { return nil }
func (s *stubStore) GetState(ctx context.Context, taskID int, tag string) (State, error) {
	return State{}, ErrNotFound
}

func (s *stubStore) ListStatesForTag(ctx context.Context, tag string) ([]State, error) {
	return nil, nil
}
func (s *stubStore) ListAllStates(ctx context.Context) ([]State, error) { return nil, nil }
func (s *stubStore) Subscribe(ctx context.Context) (<-chan StateEvent, error) {
	ch := make(chan StateEvent)
	close(ch)
	return ch, nil
}
func (s *stubStore) Close() error { s.closed = true; return nil }

func newTestManagers(t *testing.T, factory storeFactory, jobID string, numTasks int, taskIndices ...int) ([]*Manager, JobScopedStore) {
	t.Helper()
	backend := factory(t)
	ctx := context.Background()
	jobStore, resolved, err := backend.NewJob(ctx, jobID, numTasks)
	if err != nil {
		t.Fatalf("failed to create job store: %v", err)
	}
	if resolved != numTasks {
		t.Fatalf("expected numTasks %d, got %d", numTasks, resolved)
	}
	js, ok := jobStore.(JobScopedStore)
	if !ok {
		t.Fatalf("job store does not implement JobScopedStore")
	}
	if inserted, err := InsertStore(ctx, js); err != nil || !inserted {
		t.Fatalf("insert store failed: inserted=%v err=%v", inserted, err)
	}
	managers := make([]*Manager, 0, len(taskIndices))
	for _, idx := range taskIndices {
		mgr, err := NewManager(jobID, idx)
		if err != nil {
			t.Fatalf("new manager failed: %v", err)
		}
		managers = append(managers, mgr)
	}
	return managers, js
}

func newTestManager(t *testing.T, factory storeFactory, jobID string, taskIndex int, numTasks int) (*Manager, JobScopedStore) {
	mgrs, store := newTestManagers(t, factory, jobID, numTasks, taskIndex)
	return mgrs[0], store
}

func TestSetStateRejectsOtherTask(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			CloseAllStores()
			mgrs, _ := newTestManagers(t, f.make, "job-1", 2, 0)
			mgr := mgrs[0]
			err := mgr.SetState(context.Background(), State{JobID: "job-1", TaskID: 1, Tag: "ingest"})
			if !errors.Is(err, ErrTaskMismatch) {
				t.Fatalf("expected ErrTaskMismatch, got %v", err)
			}
		})
	}
}

func TestStoreRegistryInsertAndGet(t *testing.T) {
	CloseAllStores()
	t.Cleanup(CloseAllStores)

	backend := NewInMemoryStore()
	ctx := context.Background()

	jobStore, numTasks, err := backend.NewJob(ctx, "job-reg", 1)
	if err != nil {
		t.Fatalf("new job store failed: %v", err)
	}
	if numTasks != 1 {
		t.Fatalf("expected numTasks=1, got %d", numTasks)
	}

	js, ok := jobStore.(JobScopedStore)
	if !ok {
		t.Fatalf("job store does not implement JobScopedStore")
	}

	inserted, err := InsertStore(ctx, js)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	if !inserted {
		t.Fatalf("expected first insert to report inserted=true")
	}

	inserted, err = InsertStore(ctx, js)
	if err != nil {
		t.Fatalf("second insert of same store failed: %v", err)
	}
	if inserted {
		t.Fatalf("expected idempotent insert to return inserted=false")
	}

	otherStore, _, err := backend.OpenJob(ctx, "job-reg")
	if err != nil {
		t.Fatalf("open job failed: %v", err)
	}
	if _, err := InsertStore(ctx, otherStore.(JobScopedStore)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists on different store, got %v", err)
	}

	got, ok := GetStore("job-reg")
	if !ok {
		t.Fatalf("expected store to be retrievable")
	}
	if got != js {
		t.Fatalf("expected retrieved store to match inserted instance")
	}

	RemoveStore("job-reg")
	if _, ok := GetStore("job-reg"); ok {
		t.Fatalf("expected store to be removed")
	}
}

func TestNewManagerUsesRegisteredStore(t *testing.T) {
	CloseAllStores()
	t.Cleanup(CloseAllStores)

	backend := NewInMemoryStore()
	ctx := context.Background()

	jobStore, numTasks, err := backend.NewJob(ctx, "job-hidden-reg", 2)
	if err != nil {
		t.Fatalf("new job store failed: %v", err)
	}
	if numTasks != 2 {
		t.Fatalf("expected numTasks=2, got %d", numTasks)
	}
	js := jobStore.(JobScopedStore)

	if inserted, err := InsertStore(ctx, js); err != nil || !inserted {
		t.Fatalf("insert store failed: inserted=%v err=%v", inserted, err)
	}

	mgr0, err := NewManager("job-hidden-reg", 0)
	if err != nil {
		t.Fatalf("NewManager task0 failed: %v", err)
	}
	mgr1, err := NewManager("job-hidden-reg", 1)
	if err != nil {
		t.Fatalf("NewManager task1 failed: %v", err)
	}

	if mgr0.numTasks != 2 || mgr1.numTasks != 2 {
		t.Fatalf("expected numTasks to be 2 for both managers, got %d and %d", mgr0.numTasks, mgr1.numTasks)
	}
	if mgr0.store != js || mgr1.store != js {
		t.Fatalf("managers should share the registered store instance")
	}
}

func TestSynchronizeSuccess(t *testing.T) {
	CloseAllStores()
	backend := NewInMemoryStore()
	ctx := context.Background()
	jobStore, _, err := backend.NewJob(ctx, "job-sync-ok", 2)
	if err != nil {
		t.Fatalf("new job store failed: %v", err)
	}
	if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
		t.Fatalf("insert store failed: %v inserted=%v", err, inserted)
	}
	mgr0, err := NewManager("job-sync-ok", 0)
	if err != nil {
		t.Fatalf("manager0 create failed: %v", err)
	}
	mgr1, err := NewManager("job-sync-ok", 1)
	if err != nil {
		t.Fatalf("manager1 open failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = mgr0.SetState(context.Background(), State{JobID: "job-sync-ok", TaskID: 0, Tag: "startup", Status: 10})
		_ = mgr1.SetState(context.Background(), State{JobID: "job-sync-ok", TaskID: 1, Tag: "startup", Status: 10})
		close(done)
	}()

	states, err := mgr0.Synchronize(ctx, "startup", 10)
	if err != nil {
		t.Fatalf("synchronize failed: %v", err)
	}
	<-done
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
	for i, st := range states {
		if st.Status < 10 {
			t.Fatalf("task %d status below target: %d", i, st.Status)
		}
		if st.Tag != "startup" {
			t.Fatalf("task %d unexpected tag: %s", i, st.Tag)
		}
	}
}

func TestSynchronizeTimeoutReturnsPartial(t *testing.T) {
	CloseAllStores()
	backend := NewInMemoryStore()
	ctx := context.Background()
	jobStore, _, err := backend.NewJob(ctx, "job-sync-timeout", 2)
	if err != nil {
		t.Fatalf("manager0 create failed: %v", err)
	}
	if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
		t.Fatalf("insert store failed: %v inserted=%v", err, inserted)
	}
	mgr0, err := NewManager("job-sync-timeout", 0)
	if err != nil {
		t.Fatalf("manager0 create failed: %v", err)
	}
	mgr1, err := NewManager("job-sync-timeout", 1)
	if err != nil {
		t.Fatalf("manager1 open failed: %v", err)
	}

	// Only one task reaches the target.
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sync-timeout", TaskID: 0, Tag: "startup", Status: 10}); err != nil {
		t.Fatalf("set state task0 failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	states, err := mgr1.Synchronize(ctx, "startup", 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
	if states[0].Status < 10 {
		t.Fatalf("task0 status should be at least target, got %d", states[0].Status)
	}
	if states[1].Status != 0 {
		t.Fatalf("task1 should be defaulted to 0 when missing, got %d", states[1].Status)
	}
}

func TestGetStateAggregations(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			numTasks := 3
			CloseAllStores()
			backend := f.make(t)
			ctx := context.Background()
			jobStore, _, err := backend.NewJob(ctx, "job-agg", numTasks)
			if err != nil {
				t.Fatalf("manager0 create: %v", err)
			}
			if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
				t.Fatalf("insert store failed: %v", err)
			}
			mgr0, err := NewManager("job-agg", 0)
			if err != nil {
				t.Fatalf("manager0 create: %v", err)
			}
			mgr1, err := NewManager("job-agg", 1)
			if err != nil {
				t.Fatalf("manager1 open: %v", err)
			}

			// Write states for tag "ingest".
			if err := mgr0.SetState(context.Background(), State{JobID: "job-agg", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
				t.Fatalf("set state t0 failed: %v", err)
			}
			if err := mgr1.SetState(context.Background(), State{JobID: "job-agg", TaskID: 1, Tag: "ingest", Status: -1}); err != nil {
				t.Fatalf("set state t1 failed: %v", err)
			}

			// Aggregate for tag should pick lowest (-1) from task 1.
			aggTag, err := mgr0.GetState(context.Background(), WithTag("ingest"))
			if err != nil {
				t.Fatalf("GetState WithTag failed: %v", err)
			}
			if aggTag.Status != -1 || aggTag.TaskID != 1 {
				t.Fatalf("expected status -1 from task 1, got status %d task %d", aggTag.Status, aggTag.TaskID)
			}

			// Task aggregate for task 1 should see its lowest (-1).
			aggTask, err := mgr0.GetState(context.Background(), WithTaskID(1))
			if err != nil {
				t.Fatalf("GetState WithTaskID failed: %v", err)
			}
			if aggTask.Status != -1 || aggTask.TaskID != 1 {
				t.Fatalf("expected status -1 for task 1, got status %d task %d", aggTask.Status, aggTask.TaskID)
			}

			// Specific task+tag fetch.
			st, err := mgr0.GetState(context.Background(), WithTaskID(0), WithTag("ingest"))
			if err != nil {
				t.Fatalf("GetState specific failed: %v", err)
			}
			if st.TaskID != 0 || st.Status != 1 {
				t.Fatalf("expected task 0 status 1, got task %d status %d", st.TaskID, st.Status)
			}

			// Global aggregate should pick lowest (-1).
			g, err := mgr0.GetState(context.Background())
			if err != nil {
				t.Fatalf("GetState global failed: %v", err)
			}
			if g.Status != -1 || g.TaskID != 1 {
				t.Fatalf("expected global lowest status -1 from task 1, got status %d task %d", g.Status, g.TaskID)
			}

			// Missing tag defaults to 0.
			missing, err := mgr0.GetState(context.Background(), WithTag("process"))
			if err != nil {
				t.Fatalf("GetState missing tag failed: %v", err)
			}
			if missing.Status != 0 {
				t.Fatalf("expected default status 0 for missing tag, got %d", missing.Status)
			}
		})
	}
}

func TestSubscribeReplayAndLive(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			numTasks := 2
			CloseAllStores()
			backend := f.make(t)
			ctx := context.Background()
			jobStore, _, err := backend.NewJob(ctx, "job-sub", numTasks)
			if err != nil {
				t.Fatalf("manager0 create: %v", err)
			}
			if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
				t.Fatalf("insert store failed: %v", err)
			}
			mgr0, err := NewManager("job-sub", 0)
			if err != nil {
				t.Fatalf("manager0 create: %v", err)
			}
			mgr1, err := NewManager("job-sub", 1)
			if err != nil {
				t.Fatalf("manager1 open: %v", err)
			}

			// Seed initial state so snapshot includes it.
			if err := mgr0.SetState(context.Background(), State{JobID: "job-sub", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
				t.Fatalf("seed state failed: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			ch, err := mgr0.Subscribe(ctx, WithTag("ingest"))
			if err != nil {
				t.Fatalf("subscribe failed: %v", err)
			}

			// Expect replay event for task 0.
			evt := recvEvent(t, ch, time.Second)
			if !evt.Replay {
				t.Fatalf("expected replay event")
			}
			if evt.State.TaskID != 0 || evt.State.Status != 1 {
				t.Fatalf("unexpected replay state: %+v", evt.State)
			}

			// Publish live update from task 1.
			if err := mgr1.SetState(context.Background(), State{JobID: "job-sub", TaskID: 1, Tag: "ingest", Status: 2}); err != nil {
				t.Fatalf("live state failed: %v", err)
			}

			evt2 := recvEvent(t, ch, time.Second)
			if evt2.Replay {
				t.Fatalf("expected live event, got replay")
			}
			if evt2.State.TaskID != 1 || evt2.State.Status != 2 {
				t.Fatalf("unexpected live state: %+v", evt2.State)
			}
		})
	}
}

func TestSubscribeVersionOrdering(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			numTasks := 1
			CloseAllStores()
			backend := f.make(t)
			ctx := context.Background()
			jobStore, _, err := backend.NewJob(ctx, "job-sub-version", numTasks)
			if err != nil {
				t.Fatalf("manager create: %v", err)
			}
			if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
				t.Fatalf("insert store failed: %v", err)
			}
			mgr, err := NewManager("job-sub-version", 0)
			if err != nil {
				t.Fatalf("manager create: %v", err)
			}

			// Seed version 1
			if err := mgr.SetState(context.Background(), State{JobID: "job-sub-version", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
				t.Fatalf("seed failed: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ch, err := mgr.Subscribe(ctx, WithTag("ingest"))
			if err != nil {
				t.Fatalf("subscribe failed: %v", err)
			}

			// replay v1
			evt1 := recvEvent(t, ch, time.Second)
			// live v2
			if err := mgr.SetState(context.Background(), State{JobID: "job-sub-version", TaskID: 0, Tag: "ingest", Status: 2}); err != nil {
				t.Fatalf("live failed: %v", err)
			}
			evt2 := recvEvent(t, ch, time.Second)

			if !(evt1.State.Version < evt2.State.Version) {
				t.Fatalf("versions not increasing: v1=%d v2=%d", evt1.State.Version, evt2.State.Version)
			}
		})
	}
}

func TestSubscribeContextCancelCloses(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			CloseAllStores()
			backend := f.make(t)
			ctx := context.Background()
			jobStore, _, err := backend.NewJob(ctx, "job-sub-cancel", 1)
			if err != nil {
				t.Fatalf("manager create: %v", err)
			}
			if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
				t.Fatalf("insert store failed: %v", err)
			}
			mgr, err := NewManager("job-sub-cancel", 0)
			if err != nil {
				t.Fatalf("manager create: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			ch, err := mgr.Subscribe(ctx, WithTag("ingest"))
			if err != nil {
				t.Fatalf("subscribe failed: %v", err)
			}
			cancel()

			select {
			case _, ok := <-ch:
				if ok {
					t.Fatalf("expected channel to close after cancel")
				}
			case <-time.After(time.Second):
				t.Fatalf("channel did not close after cancel")
			}
		})
	}
}

func TestReplayOrdering(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			CloseAllStores()
			backend := f.make(t)
			ctx := context.Background()
			jobStore, _, err := backend.NewJob(ctx, "job-replay-order", 2)
			if err != nil {
				t.Fatalf("manager0 create: %v", err)
			}
			if inserted, err := InsertStore(ctx, jobStore.(JobScopedStore)); err != nil || !inserted {
				t.Fatalf("insert store failed: %v", err)
			}
			mgr0, err := NewManager("job-replay-order", 0)
			if err != nil {
				t.Fatalf("manager0 create: %v", err)
			}
			mgr1, err := NewManager("job-replay-order", 1)
			if err != nil {
				t.Fatalf("manager1 open: %v", err)
			}

			t2 := time.Now().Add(2 * time.Second)
			t1 := time.Now()

			// Intentionally write later timestamp first.
			if err := mgr0.SetState(context.Background(), State{JobID: "job-replay-order", TaskID: 0, Tag: "ingest", Status: 1, Timestamp: t2}); err != nil {
				t.Fatalf("set state t0 failed: %v", err)
			}
			if err := mgr1.SetState(context.Background(), State{JobID: "job-replay-order", TaskID: 1, Tag: "ingest", Status: 2, Timestamp: t1}); err != nil {
				t.Fatalf("set state t1 failed: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ch, err := mgr0.Subscribe(ctx, WithTag("ingest"))
			if err != nil {
				t.Fatalf("subscribe failed: %v", err)
			}

			evts := collectN(t, ch, 2, time.Second)
			if !(evts[0].State.Timestamp.Before(evts[1].State.Timestamp) || evts[0].State.Timestamp.Equal(evts[1].State.Timestamp)) {
				t.Fatalf("events not ordered by timestamp")
			}
		})
	}
}

func TestGetStateMissingSpecific(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			mgr, _ := newTestManager(t, f.make, "job-missing", 0, 1)
			_, err := mgr.GetState(context.Background(), WithTaskID(0), WithTag("missing"))
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected ErrNotFound, got %v", err)
			}
		})
	}
}

func TestDeleteJobClearsState(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			mgr, store := newTestManager(t, f.make, "job-delete", 0, 1)
			if err := mgr.SetState(context.Background(), State{JobID: "job-delete", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
				t.Fatalf("set failed: %v", err)
			}
			if err := mgr.DeleteJob(context.Background()); err != nil {
				t.Fatalf("delete failed: %v", err)
			}
			_, err := mgr.GetState(context.Background(), WithTaskID(0), WithTag("ingest"))
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected ErrNotFound after delete, got %v", err)
			}
			// Also ensure store is emptied.
			states, err := store.ListAllStates(context.Background())
			if err != nil {
				t.Fatalf("ListAllStates after delete failed: %v", err)
			}
			if len(states) != 0 {
				t.Fatalf("expected no states after delete, got %d", len(states))
			}
		})
	}
}

func TestSubscribeEmptyThenLive(t *testing.T) {
	for _, f := range defaultStores {
		t.Run(f.name, func(t *testing.T) {
			CloseAllStores()
			mgr, _ := newTestManager(t, f.make, "job-sub-empty", 0, 1)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ch, err := mgr.Subscribe(ctx, WithTag("ingest"))
			if err != nil {
				t.Fatalf("subscribe failed: %v", err)
			}

			// No replay events expected; ensure none arrive within a short interval.
			select {
			case evt := <-ch:
				t.Fatalf("expected no replay events, got %+v", evt)
			case <-time.After(100 * time.Millisecond):
			}

			if err := mgr.SetState(context.Background(), State{JobID: "job-sub-empty", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
				t.Fatalf("live set failed: %v", err)
			}

			evt := recvEvent(t, ch, time.Second)
			if evt.State.Status != 1 {
				t.Fatalf("expected live status 1, got %d", evt.State.Status)
			}
		})
	}
}

func TestSubscribeFiltersByTag(t *testing.T) {
	numTasks := 2
	CloseAllStores()
	mgrs, _ := newTestManagers(t, inMemoryFactory, "job-sub-filter-tag", numTasks, 0, 1)
	mgr0, mgr1 := mgrs[0], mgrs[1]

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch1, err := mgr0.Subscribe(ctx, WithTag("ingest"))
	if err != nil {
		t.Fatalf("subscribe1 failed: %v", err)
	}
	ch2, err := mgr0.Subscribe(ctx, WithTag("ingest"))
	if err != nil {
		t.Fatalf("subscribe2 failed: %v", err)
	}

	// Emit events for multiple tags; subscribers should only see ingest.
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-filter-tag", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
		t.Fatalf("set ingest t0 failed: %v", err)
	}
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-filter-tag", TaskID: 0, Tag: "process", Status: 3}); err != nil {
		t.Fatalf("set process t0 failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-filter-tag", TaskID: 1, Tag: "ingest", Status: 2}); err != nil {
		t.Fatalf("set ingest t1 failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-filter-tag", TaskID: 1, Tag: "export", Status: 4}); err != nil {
		t.Fatalf("set export t1 failed: %v", err)
	}

	e1 := collectN(t, ch1, 2, time.Second)
	e2 := collectN(t, ch2, 2, time.Second)

	assertOnlyIngest(t, e1)
	assertOnlyIngest(t, e2)
	assertTasksSeen(t, e1, []int{0, 1})
	assertTasksSeen(t, e2, []int{0, 1})
}

func TestSubscribeFiltersByTask(t *testing.T) {
	numTasks := 2
	CloseAllStores()
	mgrs, _ := newTestManagers(t, inMemoryFactory, "job-sub-filter-task", numTasks, 0, 1)
	mgr0, mgr1 := mgrs[0], mgrs[1]

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch0, err := mgr0.Subscribe(ctx, WithTaskID(0))
	if err != nil {
		t.Fatalf("subscribe task0 failed: %v", err)
	}
	ch1, err := mgr0.Subscribe(ctx, WithTaskID(1))
	if err != nil {
		t.Fatalf("subscribe task1 failed: %v", err)
	}

	// Emit events across tasks; subscribers should only see their task.
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-filter-task", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
		t.Fatalf("set t0 ingest failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-filter-task", TaskID: 1, Tag: "ingest", Status: 2}); err != nil {
		t.Fatalf("set t1 ingest failed: %v", err)
	}
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-filter-task", TaskID: 0, Tag: "export", Status: 3}); err != nil {
		t.Fatalf("set t0 export failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-filter-task", TaskID: 1, Tag: "export", Status: 4}); err != nil {
		t.Fatalf("set t1 export failed: %v", err)
	}

	evts0 := collectN(t, ch0, 2, time.Second)
	evts1 := collectN(t, ch1, 2, time.Second)

	assertOnlyTask(t, evts0, 0)
	assertOnlyTask(t, evts1, 1)
}

func TestSubscribeAllEvents(t *testing.T) {
	numTasks := 2
	CloseAllStores()
	mgrs, _ := newTestManagers(t, inMemoryFactory, "job-sub-all", numTasks, 0, 1)
	mgr0, mgr1 := mgrs[0], mgrs[1]

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := mgr0.Subscribe(ctx)
	if err != nil {
		t.Fatalf("subscribe all failed: %v", err)
	}

	// Emit multiple events across tasks and tags.
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-all", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
		t.Fatalf("set t0 ingest failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-all", TaskID: 1, Tag: "ingest", Status: 2}); err != nil {
		t.Fatalf("set t1 ingest failed: %v", err)
	}
	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-all", TaskID: 0, Tag: "export", Status: 3}); err != nil {
		t.Fatalf("set t0 export failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-all", TaskID: 1, Tag: "export", Status: 4}); err != nil {
		t.Fatalf("set t1 export failed: %v", err)
	}

	evts := collectN(t, ch, 4, time.Second)
	assertTasksSeen(t, evts, []int{0, 1})
	assertTagsSeen(t, evts, []string{"ingest", "export"})
}

func TestManagerAccessorsAndClose(t *testing.T) {
	CloseAllStores()
	s := newStubStore("job-access", 5)
	if inserted, err := InsertStore(context.Background(), s); err != nil || !inserted {
		t.Fatalf("insert store failed: %v inserted=%v", err, inserted)
	}
	mgr, err := NewManager("job-access", 2)
	if err != nil {
		t.Fatalf("manager create failed: %v", err)
	}

	if mgr.JobID() != "job-access" {
		t.Fatalf("unexpected JobID: %s", mgr.JobID())
	}
	if mgr.TaskIndex() != 2 {
		t.Fatalf("unexpected TaskIndex: %d", mgr.TaskIndex())
	}
	if mgr.NumTasks() != 5 {
		t.Fatalf("unexpected NumTasks: %d", mgr.NumTasks())
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if !s.closed {
		t.Fatalf("expected stub store to be closed")
	}

	var nilMgr *Manager
	if err := nilMgr.Close(); err != nil {
		t.Fatalf("nil manager close should be nil error, got %v", err)
	}
}

func TestManagerCleanupUnsupported(t *testing.T) {
	CloseAllStores()
	s := newStubStore("job-cleanup", 1)
	if inserted, err := InsertStore(context.Background(), s); err != nil || !inserted {
		t.Fatalf("insert store failed: %v inserted=%v", err, inserted)
	}
	mgr, err := NewManager("job-cleanup", 0)
	if err != nil {
		t.Fatalf("manager create failed: %v", err)
	}
	if err := mgr.DeleteJob(context.Background()); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
	if err := mgr.MarkJobDeleted(context.Background()); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestMarkJobDeletedClearsState(t *testing.T) {
	CloseAllStores()
	backend := NewInMemoryStore()
	ctx := context.Background()
	jobStoreIface, _, err := backend.NewJob(ctx, "job-mark-delete", 1)
	if err != nil {
		t.Fatalf("job create failed: %v", err)
	}
	jobStore := jobStoreIface.(JobScopedStore)
	if inserted, err := InsertStore(ctx, jobStore); err != nil || !inserted {
		t.Fatalf("insert store failed: %v inserted=%v", err, inserted)
	}
	mgr, err := NewManager("job-mark-delete", 0)
	if err != nil {
		t.Fatalf("manager create failed: %v", err)
	}

	if err := mgr.SetState(context.Background(), State{JobID: "job-mark-delete", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if err := mgr.MarkJobDeleted(context.Background()); err != nil {
		t.Fatalf("MarkJobDeleted failed: %v", err)
	}
	states, err := mgr.store.ListAllStates(context.Background())
	if err != nil {
		t.Fatalf("ListAllStates after mark delete failed: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("expected no states after mark delete, got %d", len(states))
	}
}

func TestInMemoryStoreCloseClosesSubscribers(t *testing.T) {
	backend := NewInMemoryStore()
	jobStoreIface, _, err := backend.NewJob(context.Background(), "job-store-close", 1)
	if err != nil {
		t.Fatalf("job create failed: %v", err)
	}
	store := jobStoreIface.(*inMemoryStore)

	ch, err := store.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("double close failed: %v", err)
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected subscriber channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatalf("subscriber channel not closed after store close")
	}
}

func TestSubscribeSpecificTaskAndTagSnapshot(t *testing.T) {
	CloseAllStores()
	mgrs, _ := newTestManagers(t, inMemoryFactory, "job-sub-specific", 2, 0, 1)
	mgr0, mgr1 := mgrs[0], mgrs[1]

	if err := mgr0.SetState(context.Background(), State{JobID: "job-sub-specific", TaskID: 0, Tag: "ingest", Status: 1}); err != nil {
		t.Fatalf("seed task0 failed: %v", err)
	}
	if err := mgr1.SetState(context.Background(), State{JobID: "job-sub-specific", TaskID: 1, Tag: "ingest", Status: 2}); err != nil {
		t.Fatalf("seed task1 failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, err := mgr0.Subscribe(ctx, WithTaskID(1), WithTag("ingest"))
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	evt := recvEvent(t, ch, time.Second)
	if !evt.Replay {
		t.Fatalf("expected replay event")
	}
	if evt.State.TaskID != 1 || evt.State.Tag != "ingest" {
		t.Fatalf("unexpected state in replay: %+v", evt.State)
	}

	select {
	case e := <-ch:
		t.Fatalf("expected only one replay event, got %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSortStatesTiebreaksVersion(t *testing.T) {
	when := time.Now()
	states := []State{
		{TaskID: 0, Tag: "ingest", Version: 2, Timestamp: when},
		{TaskID: 0, Tag: "ingest", Version: 1, Timestamp: when},
	}

	sortStates(states)
	if states[0].Version != 1 {
		t.Fatalf("expected lower version first when timestamps equal, got %d", states[0].Version)
	}
}

func assertOnlyIngest(t *testing.T, evts []StateEvent) {
	t.Helper()
	for _, e := range evts {
		if e.State.Tag != "ingest" {
			t.Fatalf("expected only ingest tag, got %s", e.State.Tag)
		}
	}
}

func assertOnlyTask(t *testing.T, evts []StateEvent, taskID int) {
	t.Helper()
	for _, e := range evts {
		if e.State.TaskID != taskID {
			t.Fatalf("expected only task %d, got %d", taskID, e.State.TaskID)
		}
	}
}

func assertTasksSeen(t *testing.T, evts []StateEvent, taskIDs []int) {
	t.Helper()
	seen := make(map[int]bool)
	for _, e := range evts {
		seen[e.State.TaskID] = true
	}
	for _, id := range taskIDs {
		if !seen[id] {
			t.Fatalf("expected to see task %d", id)
		}
	}
}

func assertTagsSeen(t *testing.T, evts []StateEvent, tags []string) {
	t.Helper()
	seen := make(map[string]bool)
	for _, e := range evts {
		seen[e.State.Tag] = true
	}
	for _, tag := range tags {
		if !seen[tag] {
			t.Fatalf("expected to see tag %s", tag)
		}
	}
}

func collectN(t *testing.T, ch <-chan StateEvent, n int, timeout time.Duration) []StateEvent {
	t.Helper()
	evts := make([]StateEvent, 0, n)
	deadline := time.After(timeout)
	for len(evts) < n {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before collecting %d events (got %d)", n, len(evts))
			}
			evts = append(evts, evt)
		case <-deadline:
			t.Fatalf("timed out collecting %d events, got %d", n, len(evts))
		}
	}
	return evts
}

func recvEvent(t *testing.T, ch <-chan StateEvent, timeout time.Duration) StateEvent {
	t.Helper()
	select {
	case evt, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed unexpectedly")
		}
		return evt
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for event")
		return StateEvent{}
	}
}
