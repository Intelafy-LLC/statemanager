# statemanager

A lightweight Go module for tracking per-task state within a job, aggregating by the "lowest status wins" rule, and streaming replay+live updates. It ships with an in-memory store and a Firestore-backed store. Stores are unbound backends; jobs are created/opened explicitly before use.

## Design

See the detailed design at [design.md](design.md).

## Getting started (in-memory)

```sh
go test ./...
```

Managers enforce task isolation: a manager only writes for its bound `TaskIndex`. Aggregations return the lowest status across the scoped tasks/tags. Managers are constructed via lifecycle-aware helpers: `NewManagerForNewJob(ctx, store, jobID, taskIndex, numTasks)` to create, or `NewManagerForExistingJob(ctx, store, jobID, taskIndex)` to open.

## Firestore-backed usage

Makefile targets (require `gcloud`):

- `make firestore-init PROJECT_ID=... REGION=...` — enable Firestore and create the database if missing; apply indexes.
- `make firestore-indexes PROJECT_ID=...` — apply indexes from `firestore.indexes.json`.
- `make firestore-emulator` — start the emulator locally (default `localhost:8787`, override with `HOST_PORT=host:port`).
- `make test-firestore` — run Firestore-tagged tests against a running emulator (requires `FIRESTORE_EMULATOR_HOST`, e.g., `localhost:8787`).
- `make migrate PROJECT_ID=... ARGS="--target=1 --dry-run"` — run the migration stub.
- `make gc PROJECT_ID=... TOMBSTONE_AGE=30d DELETE_AGE=90d` — tombstone and then delete idle jobs.

Monitor:

- `go run ./cmd/monitor --project=$PROJECT --job=$JOB [--tag=TAG] [--task=TASK]`
- Streams state changes (replay then live) to stdout as JSON via `slog` with fields: `jobID`, `tag`, `taskID`, `version`, `status`, `message`, `timestamp`, `eventID`, `replay`.

Helper scripts:

- `scripts/firestore_enable.sh` — enables API and creates the default database.
- `scripts/firestore_apply_indexes.sh` — applies composite indexes from `firestore.indexes.json`.

### Firestore GC tool

Tombstones and deletes inactive jobs using configurable age thresholds:

```sh
go run ./cmd/gc --project=$PROJECT \
	--collection-prefix=jobs --tombstone-age=30d --delete-age=90d
```

Rules:
- Ages accept `h`, `d`, `w`, `mo` (or any `time.ParseDuration`-compatible string).
- `delete-age` must exceed `tombstone-age` in elapsed time.
- Tombstone sets `deleted=true` on the job document; delete removes the job doc and `states` subcollection.

### Firestore runtime notes

- Constructor: `NewFirestoreStore(projectID)` returns an unbound backend. Call `NewJob(ctx, jobID, numTasks)` to create (fails if exists or numTasks <= 0) or `OpenJob(ctx, jobID)` to open (fails if missing).
- States live under `jobs/{jobID}/states` with document IDs `{tag}:{taskID}`. Versions are bumped transactionally per key.
- Subscriptions stream Firestore query snapshots; the manager handles replay ordering and version-based dedup.

## Migrations (stub)

The migration tool in [cmd/migrate](cmd/migrate) currently stamps `schemaVersion` on job docs and can scope to a single job or all jobs:

```sh
go run ./cmd/migrate --project=$PROJECT_ID --target=1 --dry-run
```

Planned extensions: idempotent backfills for schema changes and emulator-backed integration tests.

## Repository hygiene

- Coverage/profile artifacts are ignored via [.gitignore](.gitignore).
- Firestore is built by default; integration tests remain behind the `firestore` tag to avoid hitting Firestore without an emulator.
