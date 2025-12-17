//go:build firestore
// +build firestore

package statemanager

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultCollectionPrefix = "jobs"

// firestoreStore is a Firestore-backed implementation of Store scoped to a single job.
type firestoreStore struct {
	client   *firestore.Client
	jobDoc   *firestore.DocumentRef
	states   *firestore.CollectionRef
	jobID    string
	numTasks int

	mu          sync.Mutex
	subscribers map[chan StateEvent]context.CancelFunc
	closed      bool
}

// Ensure firestoreStore implements optional Cleaner interface.
var _ Cleaner = (*firestoreStore)(nil)

// NewFirestoreStore constructs a Firestore store for a job, returning the authoritative numTasks.
// It will create the job document if missing and preserve an existing numTasks if already set.
func NewFirestoreStore(ctx context.Context, projectID, jobID string, numTasks int) (Store, int, error) {
	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, 0, fmt.Errorf("new firestore client: %w", err)
	}

	jobDoc := client.Collection(defaultCollectionPrefix).Doc(jobID)
	states := jobDoc.Collection("states")

	resolvedNumTasks := numTasks
	err = client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(jobDoc)
		if err != nil {
			if status.Code(err) != codes.NotFound {
				return err
			}
			return tx.Set(jobDoc, map[string]any{
				"numTasks": numTasks,
				"deleted":  false,
			})
		}
		var job struct {
			NumTasks int `firestore:"numTasks"`
		}
		if err := snap.DataTo(&job); err != nil {
			return err
		}
		if job.NumTasks > 0 {
			resolvedNumTasks = job.NumTasks
		}
		return nil
	})
	if err != nil {
		_ = client.Close()
		return nil, 0, fmt.Errorf("init job doc: %w", err)
	}

	fs := &firestoreStore{
		client:      client,
		jobDoc:      jobDoc,
		states:      states,
		jobID:       jobID,
		numTasks:    resolvedNumTasks,
		subscribers: make(map[chan StateEvent]context.CancelFunc),
	}

	return fs, resolvedNumTasks, nil
}

// SetState writes a state using a transaction to bump version per {tag,task} key.
func (f *firestoreStore) SetState(ctx context.Context, state State) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	key := stateKey(state)
	state.JobID = f.jobID

	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc := f.states.Doc(key)
		prevVer := int64(0)
		snap, err := tx.Get(doc)
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		if err == nil {
			var existing State
			if err := snap.DataTo(&existing); err != nil {
				return err
			}
			prevVer = existing.Version
		}
		state.Version = prevVer + 1
		if state.Timestamp.IsZero() {
			state.Timestamp = time.Now().UTC()
		}
		return tx.Set(doc, state)
	})
}

// GetState reads a single state.
func (f *firestoreStore) GetState(ctx context.Context, taskID int, tag string) (State, error) {
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	default:
	}
	key := tag + ":" + strconv.Itoa(taskID)
	snap, err := f.states.Doc(key).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return State{}, ErrNotFound
		}
		return State{}, err
	}
	var st State
	if err := snap.DataTo(&st); err != nil {
		return State{}, err
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
	iter := f.states.Where("tag", "==", tag).Documents(ctx)
	defer iter.Stop()
	var out []State
	for {
		snap, err := iter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return nil, err
		}
		var st State
		if err := snap.DataTo(&st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sortStates(out)
	return out, nil
}

// ListAllStates lists all states for this job.
func (f *firestoreStore) ListAllStates(ctx context.Context) ([]State, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	iter := f.states.Documents(ctx)
	defer iter.Stop()
	var out []State
	for {
		snap, err := iter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return nil, err
		}
		var st State
		if err := snap.DataTo(&st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sortStates(out)
	return out, nil
}

// Subscribe registers a new subscriber and streams live updates via query snapshots.
func (f *firestoreStore) Subscribe(ctx context.Context) (<-chan StateEvent, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ch := make(chan StateEvent, 16)
	ctxSub, cancel := context.WithCancel(ctx)

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		cancel()
		return nil, ErrNotSupported
	}
	f.subscribers[ch] = cancel
	f.mu.Unlock()

	go func() {
		defer close(ch)
		it := f.states.Snapshots(ctxSub)
		defer it.Stop()
		for {
			snap, err := it.Next()
			if err != nil {
				return
			}
			for _, change := range snap.Changes {
				if change.Kind == firestore.Removed {
					continue
				}
				var st State
				if err := change.Doc.DataTo(&st); err != nil {
					continue
				}
				select {
				case <-ctxSub.Done():
					return
				case ch <- StateEvent{State: st, Replay: false}:
				}
			}
		}
	}()

	return ch, nil
}

// Close closes subscriptions and the underlying Firestore client.
func (f *firestoreStore) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	for _, cancel := range f.subscribers {
		cancel()
	}
	f.subscribers = nil
	f.mu.Unlock()
	return f.client.Close()
}

// DeleteJob removes job metadata and task states.
func (f *firestoreStore) DeleteJob(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	iter := f.states.Documents(ctx)
	defer iter.Stop()
	batch := f.client.Batch()
	for {
		snap, err := iter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return err
		}
		batch.Delete(snap.Ref)
	}
	batch.Delete(f.jobDoc)
	_, err := batch.Commit(ctx)
	return err
}

// MarkJobDeleted tombstones a job for GC.
func (f *firestoreStore) MarkJobDeleted(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err := f.jobDoc.Update(ctx, []firestore.Update{{Path: "deleted", Value: true}})
	return err
}
