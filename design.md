# State Manager Module Design

## 1. Overview

This document outlines the design for a Go module, `statemanager`, intended to manage and synchronize state across distributed processes. The primary use case is for multi-step, partitioned jobs where each partition (task) needs to report its state and be aware of the overall job progress.

The manager provides a consistent view of state for a given "job", which is composed of one or more "tasks". It uses a pluggable `Store` interface to abstract the backend data source, with the initial implementation targeting Google Firestore.

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

// Store is the interface for backend data storage. A Store instance is scoped
// to a single job.
type Store interface {
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
type Manager struct {
    // ... private fields
    taskIndex int
}

// configured with the job's authoritative number of tasks and the caller's task index.
func NewManager(jobID string, taskIndex int, numTasks int, store Store) *Manager {
    // ... implementation
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

// SetError is a convenience method that sets a state with an error status and formatted message.
// It automatically sets Status to a negative error code and formats the Message with "🔴 " prefix.
// The provided state should have JobID, TaskID, Tag, and optionally Payload populated.
// Example: manager.SetError(ctx, fmt.Errorf("connection timeout"), State{JobID: "job-1", TaskID: 0, Tag: "ingest"})
func (m *Manager) SetError(ctx context.Context, err error, state State) error {
    state.Status = -1  // or derive from error type/code if structured
    state.Message = fmt.Sprintf("🔴 %v", err)
    return m.SetState(ctx, state)
}

// SetStarted is a convenience method that marks a task/tag as started (Status = 1).
// Example: manager.SetStarted(ctx, State{JobID: "job-1", TaskID: 0, Tag: "ingest", Message: "Beginning ingestion"})
func (m *Manager) SetStarted(ctx context.Context, state State) error {
    state.Status = 1
    if state.Message == "" {
        state.Message = "🟢 Started"
    }
    return m.SetState(ctx, state)
}

// SetFinished is a convenience method that marks a task/tag as finished (Status = math.MaxInt32).
// Example: manager.SetFinished(ctx, State{JobID: "job-1", TaskID: 0, Tag: "ingest", Message: "Completed successfully"})
func (m *Manager) SetFinished(ctx context.Context, state State) error {
    state.Status = math.MaxInt32
    if state.Message == "" {
        state.Message = "⚫ Finished"
    }
    return m.SetState(ctx, state)
}

// SetWarning is a convenience method that sets a state with a warning status and formatted message.
// It sets Status to -2 (warning code) and formats the Message with "🟡 " prefix.
// Example: manager.SetWarning(ctx, "retrying connection", State{JobID: "job-1", TaskID: 0, Tag: "ingest"})
func (m *Manager) SetWarning(ctx context.Context, warning string, state State) error {
    state.Status = -2  // distinct warning code
    state.Message = fmt.Sprintf("🟡 %s", warning)
    return m.SetState(ctx, state)
}

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

### Constructor

The constructor will be a standalone function that handles the "get-or-create" logic for the job.

`func NewFirestoreStore(ctx context.Context, projectID, jobID string, numTasks int) (Store, int, error)`

**Logic:**
1.  Initialize a new Firestore client using the `projectID`.
2.  Reference the job document path: `jobs/{jobID}`.
3.  In a Firestore transaction, attempt to read the document.
    *   If the document exists, read the `numTasks` field from it. This value becomes the authoritative `numTasks`.
    *   If the document does not exist, create it, setting the `numTasks` field with the value passed into the constructor.
4.  Return a new `firestoreStore` instance containing the client, `jobID`, and the authoritative `numTasks`. The constructor also returns this authoritative `numTasks` value to the caller.

The `firestoreStore` struct will hold the firestore client, the `jobID`, and the authoritative `numTasks`.

### Data Model

The Firestore data model will consist of two main document types: a single Job Document to store metadata about the job, and a collection of State Documents.

#### Job Document

To ensure all managers for a given job have a consistent `numTasks` count, we'll persist it in a central document.

*   **Document Path:** `jobs/{jobID}`
*   **Document Fields:**
    *   `numTasks`: `number` (The total number of tasks/partitions for this job)
    *   `createdAt`: `timestamp`
    *   `deleted`: `bool` (optional tombstone for cleanup/GC)
*   **Global writes:** Only task `0` is permitted to mutate the job document; other tasks treat global writes as no-ops.

#### State Documents

We will use a single collection for each job to hold the states. Each document in this collection will represent the state of a single task for a single tag. This model allows for efficient querying and atomic updates.

*   **Collection:** `jobs/{jobID}/states`
*   **Document ID:** Use `{jobID}-{tag}-{taskID}` so keys remain unique even when different jobs share identical tags and task indices. This still allows simple upserts via `Set`.
*   **Document ID:** Use `{taskID}-{tag}`. The document is already unique within the `jobs/{jobID}/states` collection path. This allows for simple upserts via `Set`.
*   **Document Fields:**
    *   `taskID`: `number`
    *   `tag`: `string`
    *   `status`: `number`
    *   `message`: `string`
    *   `timestamp`: `timestamp` (Firestore server time; ordering/debugging)
    *   `version`: `number` (monotonic per `{taskID, tag}`; incremented on each write)
    *   `eventID`: `string` (optional, MVP+; unique per write for idempotency/dedup)
    *   `payload`: `map` (optional tag-specific data)
    *   `schemaVersion`: `number` (default 1; supports additive schema changes)

Example Document Path: `jobs/job-123/states/0-ingest` (with Document ID `0-ingest`)

### Caching Strategy

To minimize Firestore reads, the `firestoreStore` will maintain an in-memory cache of the job's states.

*   **Structure:** The cache will be a `map[string]State`, where the key is a composite of `taskID` and `tag`.
*   **Initialization:** When the `firestoreStore` is created, it will perform an initial read of the entire `jobs/{jobID}/states` collection to populate the cache.
*   **Updates:** The store will use Firestore's snapshot listeners (`Query.Snapshots`) to receive real-time updates. When a document is added, modified, or removed in the Firestore collection, the listener will receive the change and update the in-memory cache. This makes `GetState` calls extremely fast as they will primarily read from memory.
*   **Writes:** `SetState` will write directly to Firestore. The snapshot listener will then pick up this change and update the local cache, ensuring consistency.

### Listener Lifecycle and Fan-Out

*   Startup: each task's store instance starts one Firestore listener when constructed. That listener covers the job and keeps the cache warm for that task process (one listener per job per task process).
*   Fan-out: client subscriptions are served from the listener's stream; filtering is applied in the manager. This avoids multiple listeners per process while supporting many subscribers.
*   Reconnect: on disconnect, the listener retries with exponential backoff, then refreshes the cache by re-listing states and reconciling by `Version`.

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

**Use `log/slog`** for all structured logging:
- Include fields: `jobID`, `tag`, `taskID`, `version`, `eventID`, `error`.
- Prefix all error messages with "🔴 " for visibility in logs.
- Example: `slog.Error("🔴 failed to write state", "jobID", jobID, "taskID", taskID, "error", err)`

### Observability

**Metrics (exportable via API, not persisted in Firestore):**
- Cache hit rate, listener reconnect count, replay duration (ms), live delivery latency (ms).
- Write latency/error rate, subscription channel drops (buffered messages lost on cancel).
- Track `version` drift to detect missed updates.

**No Cloud Monitoring Dependency:** Emit metrics via a lightweight HTTP endpoint (e.g., `/metrics`) or push to a pluggable backend interface. Keep implementation cloud-agnostic so the store can be swapped (e.g., AWS DynamoDB) without refactoring telemetry.

### Security & Tenancy

- **One Firestore per cloud project:** strict project isolation; no cross-project data sharing.
- **IAM:** service account scoped to the job's project with minimal permissions (read/write `jobs/{jobID}` collection).
- `projectID` is required per `NewFirestoreStore` call; never default or infer.

### Cleanup and Lifecycle

- Add utility functions to delete or tombstone a job (`DeleteJob` or `MarkJobDeleted`), removing the job document and associated task states or marking them for GC.
- Define retention policy (e.g., delete jobs after configurable TTL once finished) and require appropriate IAM for cleanup operations.
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

## 7. Implementation Plan

This design will be broken down into the following implementation steps:

1.  **Define Interfaces and Structs:** Create the `statemanager.go` file with the `State` struct, `Store` interface, `Manager` struct, and `Option` types.
2.  **Implement `Manager` shell:** Implement the `NewManager`, `SetState`, `GetState`, and `Subscribe` methods with placeholder or `panic` logic.
3.  **Implement `GetState` Logic:** Code the full aggregation logic within the `Manager.GetState` method based on the option parameters. This logic will operate on data returned from the store.
4.  **Implement Firestore Store:**
    *   Create a `firestore.go` file.
    *   Implement the `firestoreStore` struct.
    *   Implement the `NewFirestoreStore` constructor, which will initialize the Firestore client and the snapshot listener for caching.
    *   Implement the `SetState`, `GetState`, `ListStates...` methods with `Version`/`EventID` support.
5.  **Add Deduplication Logic:** Implement manager-level `seenVersions` tracking in `Subscribe` for replay and live phases.
6.  **Logging Integration:** Replace placeholder logging with `log/slog`; prefix errors with "🔴 " and warnings with "🟡 ".
7.  **Develop Unit Tests:** Create parallel tests for each component.
    *   Test the `Manager`'s aggregation logic using a mock `Store`.
    *   Test the `firestoreStore` against the Firestore emulator.
    *   Test deduplication, replay ordering, reconnect/resync scenarios.
8.  **Add `Subscribe` functionality:** Implement the channel-based subscription logic in both the store and the manager with replay/live phase separation.
9.  **Observability:** Add metrics endpoint and structured logging with job/tag/task context.
10. **Refine and Document:** Add comments, examples, and finalize the public documentation.
