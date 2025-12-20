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
	projectID        string
	collectionPrefix string

	client *firestore.Client
	jobDoc *firestore.DocumentRef
	states *firestore.CollectionRef
	jobID  string

	numTasks int
	bound    bool

	mu           sync.Mutex
	subscribers map[chan StateEvent]context.CancelFunc
	cache       map[string]State
	cacheReady  bool
	closed      bool
}

// Ensure firestoreStore implements optional Cleaner interface.
var _ Cleaner = (*firestoreStore)(nil)

// NewFirestoreStore constructs an unbound Firestore backend.
func NewFirestoreStore(projectID string) Store {
	return &firestoreStore{projectID: projectID, collectionPrefix: defaultCollectionPrefix}
}

// NewJob creates a Firestore job and returns a job-scoped store.
func (f *firestoreStore) NewJob(ctx context.Context, jobID string, numTasks int) (Store, int, error) {
	if numTasks <= 0 {
		return nil, 0, ErrNotSupported
	}
	client, err := firestore.NewClient(ctx, f.projectID)
	if err != nil {
		return nil, 0, fmt.Errorf("new firestore client: %w", err)
	}

	jobDoc := client.Collection(f.collectionPrefix).Doc(jobID)
	states := jobDoc.Collection("states")

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
		if snap.Exists() {
			return ErrNotSupported
		}
		return nil
	})
	if err != nil {
		_ = client.Close()
		return nil, 0, fmt.Errorf("init job doc: %w", err)
	}

	fs := &firestoreStore{
		projectID:        f.projectID,
		collectionPrefix: f.collectionPrefix,
		client:           client,
		jobDoc:           jobDoc,
		states:           states,
		jobID:            jobID,
		numTasks:         numTasks,
		bound:            true,
		subscribers:      make(map[chan StateEvent]context.CancelFunc),
		cache:            make(map[string]State),
	}

	return fs, numTasks, nil
}

// OpenJob opens an existing Firestore job and returns a job-scoped store.
func (f *firestoreStore) OpenJob(ctx context.Context, jobID string) (Store, int, error) {
	client, err := firestore.NewClient(ctx, f.projectID)
	if err != nil {
		return nil, 0, fmt.Errorf("new firestore client: %w", err)
	}

	jobDoc := client.Collection(f.collectionPrefix).Doc(jobID)
	states := jobDoc.Collection("states")

	snap, err := jobDoc.Get(ctx)
	if err != nil {
		_ = client.Close()
		if status.Code(err) == codes.NotFound {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("get job doc: %w", err)
	}
	var job struct {
		NumTasks int `firestore:"numTasks"`
	}
	if err := snap.DataTo(&job); err != nil {
		_ = client.Close()
		return nil, 0, fmt.Errorf("parse job doc: %w", err)
	}
	if job.NumTasks <= 0 {
		_ = client.Close()
		return nil, 0, fmt.Errorf("job %s has invalid numTasks %d", jobID, job.NumTasks)
	}

	fs := &firestoreStore{
		projectID:        f.projectID,
		collectionPrefix: f.collectionPrefix,
		client:           client,
		jobDoc:           jobDoc,
		states:           states,
		jobID:            jobID,
		numTasks:         job.NumTasks,
		bound:            true,
		subscribers:      make(map[chan StateEvent]context.CancelFunc),
	}

	return fs, job.NumTasks, nil
}

// SetState writes a state using a transaction to bump version per {tag,task} key.
func (f *firestoreStore) SetState(ctx context.Context, state State) error {
	if !f.bound {
		return ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	key := stateKey(state)
	state.JobID = f.jobID

	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
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
	return err
}

// GetState reads a single state.
func (f *firestoreStore) GetState(ctx context.Context, taskID int, tag string) (State, error) {
	if !f.bound {
		return State{}, ErrNotSupported
	}
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
	if !f.bound {
		return nil, ErrNotSupported
	}
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
	if !f.bound {
		return nil, ErrNotSupported
	}
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
	if !f.bound {
		return nil, ErrNotSupported
	}
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
				f.removeSubscriber(ch)
				return
			}
			for _, change := range snap.Changes {
				if change.Kind == firestore.DocumentRemoved {
					continue
				}
				var st State
				if err := change.Doc.DataTo(&st); err != nil {
					continue
				}
				select {
				case <-ctxSub.Done():
					f.removeSubscriber(ch)
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
	if !f.bound {
		return nil
	}
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

func (f *firestoreStore) removeSubscriber(ch chan StateEvent) {
	f.mu.Lock()
	delete(f.subscribers, ch)
	f.mu.Unlock()
}

// JobID returns the bound job identifier, or empty string if unbound.
func (f *firestoreStore) JobID() string {
	return f.jobID
}

// NumTasks returns the configured number of tasks for the bound job.
func (f *firestoreStore) NumTasks() int {
	return f.numTasks
}

// DeleteJob removes job metadata and task states.
func (f *firestoreStore) DeleteJob(ctx context.Context) error {
	if !f.bound {
		return ErrNotSupported
	}
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
	if !f.bound {
		return ErrNotSupported
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err := f.jobDoc.Update(ctx, []firestore.Update{{Path: "deleted", Value: true}})
	return err
}
