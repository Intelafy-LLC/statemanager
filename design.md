# State Manager Module Design

## 1. Overview

This document outlines the design for a Go module, `statemanager`, intended to manage and synchronize state across distributed processes. The primary use case is for multi-step, partitioned jobs where each partition (task) needs to report its state and be aware of the overall job progress.

The manager provides a consistent view of state for a given "job", which is composed of one or more "tasks". It uses a pluggable `Store` interface to abstract the backend data source. A `Store` starts as an unbound backend; jobs are created or opened explicitly via lifecycle methods before any state operations. The current code ships with:

* An in-memory store (default for tests).
* A Firestore-backed store.
* Helper tooling: `Makefile` targets for Firestore enable/index/apply, emulator, and a migration stub (`cmd/migrate`).

A key feature is the "lowest status wins" principle for state aggregation. When the state of a job or a workstream is requested, the task with the minimum status value determines the overall status, providing a clear and pessimistic view of job progress.

## 2. Core Concepts

*   **Job:** A unit of work identified by a unique `JobID`. A job consists of a fixed number of tasks.
*   **Task:** A partition of a job, identified by a zero-based `TaskIndex`.
*   **Manager Identity:** Each `Manager` instance is bound to a single task via `taskIndex` at construction time. A manager may only write states for its own task.
*   **State:** A snapshot of a task's progress at a point in time. It is defined by the `State` struct.
*   **Tag:** A user-defined string used to segregate different work streams or steps within a job. For example, a job might have tasks reporting status for tags like "ingest", "process", and "export".
*   **Status:** An `int32` value representing the progress of a task for a given tag.
    *   `0`: Not Started (default)
    *   `math.MaxInt32`: Finished
    *   `< 0`: Represents an error code.
    *   Other positive values are defined by the client application.
*   **State Aggregation:** The process of combining states from multiple tasks to determine a single job-level state. The rule is that the lowest numerical `Status` value is the one that prevails.

### 2.1 Task Isolation & Global Writes

*   Managers enforce task isolation: `SetState` rejects writes where `state.TaskID` differs from the manager's `taskIndex`.
*   Global job metadata lives in a single job document. Only task `0` performs global job writes; calls from other tasks are accepted but treated as no-ops for global updates.
*   Cross-task writes are not allowed. Tasks communicate only by emitting their own task-tag states for others to consume.

## 3. API Definition

The public API of the `statemanager` module.

```go
package statemanager

import "context"

// State represents the status of a single task for a specific tag.
type State struct {
    JobID          string
    TaskID         int
    Tag            string
    Status         int32
    Message        string
    Timestamp      time.Time
    Version        int64             // monotonic per {JobID, Tag, TaskID}; used for dedup and ordering
    EventID        string            // unique per write; idempotency token
    Payload        map[string]any    // optional tag-specific data
    SchemaVersion  int               // schema version of the payload/state
}

// Timestamp source: all persisted timestamps are Firestore server timestamps (UTC) set on write.
// Clients should treat them as authoritative and not adjust for local clock skew.
// Version/EventID are used to deduplicate replays and reconcile after reconnects.

// Store is an unbound backend that can create or open jobs, returning job-scoped stores.
// Job-scoped instances implement the same interface; job operations on an unbound instance
// must return ErrNotSupported.
type Store interface {
    // Lifecycle
    NewJob(ctx context.Context, jobID string, numTasks int) (Store, int, error) // create; fail if exists or numTasks <= 0
    OpenJob(ctx context.Context, jobID string) (Store, int, error)              // open; fail if missing

    // Job-scoped operations (valid only on bound instances)
    SetState(ctx context.Context, state State) error
    GetState(ctx context.Context, taskID int, tag string) (State, error)
    ListStatesForTag(ctx context.Context, tag string) ([]State, error)
    ListAllStates(ctx context.Context) ([]State, error)
    Subscribe(ctx context.Context) (<-chan StateEvent, error)
    Close() error
}

// StateEvent wraps a State with delivery metadata (e.g., replay indicator).
type StateEvent struct {
    State  State
    Replay bool
}

// Manager is the main entrypoint for interacting with the state management system.
// Each Manager instance is bound to a single task via taskIndex.
// Managers drive job lifecycle by invoking Store.NewJob or Store.OpenJob.

func NewManagerForNewJob(ctx context.Context, store Store, jobID string, taskIndex int, numTasks int) (*Manager, error) {
    // create job; fail if it already exists; returns a manager bound to the job-scoped store
}

func NewManagerForExistingJob(ctx context.Context, store Store, jobID string, taskIndex int) (*Manager, error) {
    // open existing job; fail if missing; returns a manager bound to the job-scoped store
}

// Close gracefully shuts down the manager. It closes all active subscription
// channels and closes the underlying store connection.
func (m *Manager) Close() error {
    // ... implementation
}
    // ... implementation
}

// SetState persists the state for a specific task and tag.
// It rejects writes where state.TaskID != manager.taskIndex.
func (m *Manager) SetState(ctx context.Context, state State) error {
    // ... implementation
}

// Convenience helpers such as SetError/SetStarted/SetFinished/SetWarning are intentionally omitted;
// callers set Status/Message directly before invoking SetState.

// Option defines the signature for functions that modify GetState queries.
type Option func(*queryOptions)

type queryOptions struct {
    taskID *int
    tag    *string
}

func WithTaskID(id int) Option {
    return func(o *queryOptions) {
        o.taskID = &id
    }
}

func WithTag(tag string) Option {
    return func(o *queryOptions) {
        o.tag = &tag
    }
}

// GetState retrieves state based on the provided options.
// It aggregates status according to the "lowest status wins" rule.
func (m *Manager) GetState(ctx context.Context, opts ...Option) (State, error) {
    // ... implementation
}

// Synchronize waits until every task reaches at least targetStatus for the given tag,
// or the context expires. It returns a slice of length numTasks containing the latest
// observed state per task (missing tasks are defaulted) plus any error (including
// context cancellation/timeout).
func (m *Manager) Synchronize(ctx context.Context, tag string, targetStatus int32) ([]State, error) {
    // ... implementation
}

// ManagerRegistry caches Managers per jobID for reuse across goroutines.
// It provides GetOrCreate(ctx, jobID, factory) and CloseAll().
type ManagerRegistry struct {
    // ... fields
}

// Subscribe returns a channel that emits state change events.
// The lifetime of the subscription is tied to the provided context.
// To unsubscribe, the client must cancel the context. This will signal the
// manager to close the channel and clean up the subscription resources.
//
// Client Usage Example:
//
//  ctx, cancel := context.WithCancel(context.Background())
//  defer cancel() // Best practice to ensure cleanup
//
//  stateCh, err := manager.Subscribe(ctx, WithTag("process"))
//  if err != nil {
//      // handle error
//  }
//
//  for evt := range stateCh {
//      fmt.Println("Received state update:", evt.State, "replay?", evt.Replay)
//      if evt.State.Status == StatusFinished {
//          cancel() // Unsubscribe and exit loop
//      }
//  }
//  // The loop terminates because cancel() causes the channel to close.
//
func (m *Manager) Subscribe(ctx context.Context, opts ...Option) (<-chan StateEvent, error) {
    // ... implementation
}
```

### `GetState` Behavior Details

The `GetState` function's behavior changes based on the supplied `Option`s:

1.  **`GetState(ctx, WithTaskID(i), WithTag(t))`**:
    *   **Action:** Retrieves the state for the specific task `i` and tag `t`.
    *   **Logic:** A direct call to `store.GetState(ctx, i, t)`. If no state is found, it returns a default "Not Started" state (`Status: 0`).

2.  **`GetState(ctx, WithTag(t))`**:
    *   **Action:** Retrieves the aggregate state for the entire job for tag `t`.
    *   **Logic:**
        1.  Calls `store.ListStatesForTag(ctx, t)`.
        2.  Iterates through all `numTasks` from `0` to `n-1`.
        3.  For each task, if a state is present in the list, its status is used. If not, its status defaults to `0`.
        4.  The function returns the `State` object corresponding to the task with the lowest numerical status.

3.  **`GetState(ctx, WithTaskID(i))`**:
    *   **Action:** Retrieves the aggregate state for task `i` across all its tags.
    *   **Logic:**
        1.  Calls `store.ListAllStates(ctx)` and filters the results in-memory for task `i`.
        2.  It finds the state with the lowest status among all tags for that task.
        3.  If no states exist for the task, it returns a default "Not Started" state (`Status: 0`).

4.  **`GetState(ctx)`**:
    *   **Action:** Retrieves the global aggregate state for the entire job across all tasks and tags.
    *   **Logic:**
        1.  Calls `store.ListAllStates(ctx)`.
        2.  It finds the state with the absolute lowest status value.
        3.  The logic must account for tasks that may not have reported any status yet (defaulting to `0`).

### `Subscribe` Behavior Details

While `GetState` queries the **aggregated state** (answering, "What is the overall status right now?"), `Subscribe` is for observing the stream of **atomic changes** (answering, "Tell me about every single state change as it occurs?").

The `Subscribe` method provides a two-phase "replay then live" model:

1.  **Replay Phase (ordered snapshot)**: Upon subscription, the manager delivers a snapshot of the current job state that matches the subscription options. Snapshot events are sent as individual `State` objects **in ascending timestamp order (oldest to newest)** so that action replays preserve causality. Snapshots include only states that actually exist; no synthesized `status: 0` entries are emitted. Snapshot events are marked so clients can distinguish replayed history from live updates.

2.  **Live Phase**: After replay completes, the manager streams subsequent state changes that match the subscription options. Live updates do not reorder; they arrive as the store emits them. A sentinel message (or a documented replay-complete signal) marks the transition so clients avoid double-processing.

Channel semantics: channels are closed when the subscription context is cancelled; any buffered messages that the client abandons are dropped. Channels should be small but at least depth 4 to provide modest burst tolerance without hiding backpressure.

This model ensures the subscriber can replay historical states deterministically, then consume live changes without duplicating actions.

#### Subscription Scopes

The behavior of `Subscribe` changes based on the supplied `Option`s:

1.  **`Subscribe(ctx, WithTaskID(i), WithTag(t))`**:
    *   **Scope:** The single state for task `i` and tag `t`.
    *   **Snapshot:** Delivers the current `State` if it exists; otherwise no snapshot event is emitted.
    *   **Updates:** Streams subsequent changes for that specific task and tag combination.

2.  **`Subscribe(ctx, WithTag(t))`**:
    *   **Scope:** All task states for the single tag `t`.
    *   **Snapshot:** Delivers one `State` per task that has written for tag `t`. Tasks with no prior writes are omitted (no synthesized defaults).
    *   **Updates:** Streams any change made to any task, as long as the change is for tag `t`.

3.  **`Subscribe(ctx, WithTaskID(i))`**:
    *   **Scope:** All tag states for the single task `i`.
    *   **Snapshot:** Delivers one `State` object for each unique tag that exists *for that task*. Tags with no prior writes are omitted.
    *   **Updates:** Streams any change made to task `i`, regardless of the tag.

4.  **`Subscribe(ctx)`**:
    *   **Scope:** Every state for every task and every tag in the entire job.
    *   **Snapshot:** This is the most comprehensive snapshot. It determines all unique tags that exist across the entire job. For each of those tags, it sends the states that actually exist; tasks with no writes for a tag are omitted.
    *   **Updates:** Streams every single atomic state change that occurs in the job.

## 4. Firestore Store Implementation

The `firestoreStore` will implement the `Store` interface. A `firestoreStore` instance is scoped to a single job.

### Constructor and lifecycle

`NewFirestoreStore(projectID string) Store` returns an unbound backend. It does not create or open any job.

Job binding is explicit:

* `NewJob(ctx, jobID, numTasks) (Store, int, error)` — creates `jobs/{jobID}` with `numTasks`; fails if it already exists or `numTasks <= 0`; returns a job-scoped store and the authoritative `numTasks`.
* `OpenJob(ctx, jobID) (Store, int, error)` — opens an existing job; fails if missing or if `numTasks` is invalid; returns a job-scoped store and the authoritative `numTasks`.

### Data Model (current implementation)

The Firestore data model consists of a job document and a collection of state documents under that job. Collection prefix defaults to `jobs` (see `defaultCollectionPrefix`).

#### Job Document

* **Path:** `jobs/{jobID}`
* **Fields:**
    * `numTasks` (number, authoritative count persisted at creation)
    * `deleted` (bool tombstone)
    * `schemaVersion` (number, added by migrations when bumping schema)
* **Behavior:** `NewJob` creates and seeds `numTasks`; `OpenJob` reads it and fails if missing or invalid. No implicit creation during open.

#### State Documents

* **Collection:** `jobs/{jobID}/states`
* **Document ID:** `{tag}:{taskID}` (unique per job)
* **Fields:** mirrors `State` struct: `taskID`, `tag`, `status`, `message`, `timestamp`, `version`, `eventID`, `payload`, `schemaVersion`, `jobID`.
* **Writes:** `SetState` uses a transaction to read the prior version and set `version = prev+1`; timestamps default to `UTC` now when not provided.
* **Reads:** Queries are direct against Firestore (no cache). Manager-level replay/live dedup uses `version`.
* **Subscriptions:** Store uses Firestore query snapshots to stream changes; manager applies filtering, ordering, and version-based dedup on top.

### Subscriptions (current behavior)

* Each store instance establishes a Firestore snapshot stream on `states` and fans changes to subscribers.
* The manager performs the replay+live pipeline, filtering and deduping by `{tag, taskID, version}`. No in-process cache is kept today; reads hit Firestore directly.

### `Subscribe` Implementation

The `firestoreStore`'s `Subscribe` method will leverage the same snapshot listener used for the internal cache. When a state change is received from Firestore, it will be pushed not only to the cache but also to any active subscriber channels.

The `Manager`'s `Subscribe` method will apply the `WithTaskID` and `WithTag` filtering *after* receiving the update from the store's channel, before sending it to the client.

## 5. Reliability & Operations

### Ordering, Deduplication, and Concurrency

**Write Serialization:** Per `{jobID, taskID, tag}` mutexes in the store serialize writes for that key. Parallel writes to different tags are allowed.

**Version Management:** `Version` is incremented transactionally in Firestore per `{jobID, tag, taskID}` to guarantee monotonic ordering. Clients do not set `Version`.

**Idempotency (MVP vs MVP+):** For MVP, we rely on per-key serialization and versions. `EventID` support is deferred to MVP+; if added later, it will act as an optional write-level idempotency token.

**Manager-Level Deduplication:** The manager keeps a per-subscription `seenVersions` map keyed by `{taskID, tag}` → `version`. During replay (sorted oldest first) and live streaming, it emits only when `state.Version` increases. This shields subscribers from duplicates.

**Cache Reconcile:** On reconnect, the cache keeps the highest `Version` per key; lower versions are dropped.

**Thread Safety:** In-memory cache and subscription fan-out are guarded by `sync.RWMutex` and per-subscription locks. Subscription channels are bounded (depth ≥ 4); if a subscriber cancels, pending messages may be dropped.

### Failure & Retry Semantics

**Writes (`SetState`):**
- Use Firestore's built-in retries (exponential backoff).
- Idempotency: clients can safely retry with the same `EventID`; Firestore upsert by document ID ensures no duplicates.
- On persistent failure after retries: return error prefixed with "🔴 " and let the caller decide (log/abort/retry).

**Reads & Listener Failures:**
- On transient listener disconnect: log warning with "🟡 ", back off exponentially (start at 1s, cap at 60s), and re-establish.
- On reconnect: perform full `ListAllStates` refresh and reconcile cache by `Version`.
- If reconnect fails after N attempts (e.g., 5): log fatal error "🔴 " and close all subscriptions.

**Concurrency:**
- Use `errgroup.Group` for parallel work (e.g., fanning out cache writes, subscription delivery).
- No hard concurrency cap by default; optionally allow a configurable semaphore for Firestore call rate limiting (disabled by default).

### Logging

Logging/metrics remain future work; current code does not emit structured logs or metrics.

### Security & Tenancy

- **One Firestore per cloud project:** strict project isolation; no cross-project data sharing.
- **IAM:** service account scoped to the job's project with minimal permissions (read/write `jobs/{jobID}` collection).
- `projectID` is required per `NewFirestoreStore` call; never default or infer.

### Cleanup and Lifecycle

- Add utility functions to delete or tombstone a job (`DeleteJob` or `MarkJobDeleted`), removing the job document and associated task states or marking them for GC.
- Define retention policy (e.g., delete jobs after configurable TTL once finished) and require appropriate IAM for cleanup operations.
- A Firestore-only GC CLI (`cmd/gc`, build-tagged) tombstones jobs after a configurable age, then deletes them after a later age; `delete-age` must exceed `tombstone-age`.
- Dashboard/monitoring consumers can subscribe in a "monitor" mode (subscribe to all tags/tasks) to display job lifecycle and status changes.

## 6. Testing Strategy

**Dedicated Test Jobs:** Create test-specific jobs and tags to exercise:
- Normal flow: multiple tasks, tags; verify aggregation and replay ordering.
- Missing states: verify defaults (`status: 0`) and synthetic states in snapshots.
- Error injection: simulate write failures, listener disconnects, cache inconsistencies.
- Deduplication: inject duplicate `Version` during replay; verify client sees each state once.
- Performance baselines: measure replay time, live delivery latency under load; detect regressions.

**Test Infrastructure:**
- Use Firestore emulator for CI to avoid cloud dependencies.
- Include `-race` detector runs for cache/subscription concurrency.
- Mock `Store` for unit testing `Manager` aggregation logic.

## 7. Current status and gaps

* Implemented: Manager aggregation, replay+live subscribe with dedup; explicit Store lifecycle (NewJob/OpenJob); in-memory store; Firestore store; Firestore emulator integration test; helper scripts and Make targets; migration stub.
* Not yet implemented: structured logging/metrics; advanced schema migrations beyond stamping `schemaVersion` on job docs.
