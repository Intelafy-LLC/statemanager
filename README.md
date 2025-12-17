# statemanager

A lightweight Go module for tracking per-task state within a job, aggregating by the "lowest status wins" rule, and streaming replay+live updates. It ships with an in-memory store (default) and a Firestore-backed store behind the `firestore` build tag.

## Design

See the detailed design at [design.md](design.md).

## Getting started (in-memory)

```sh
go test ./...
```

Managers enforce task isolation: a manager only writes for its bound `TaskIndex`. Aggregations return the lowest status across the scoped tasks/tags.

## Firestore-backed usage

Build with the tag:

```sh
go test -tags=firestore ./...
```

Makefile targets (require `gcloud`):

- `make firestore-init PROJECT_ID=... REGION=...` — enable Firestore and create the database if missing; apply indexes.
- `make firestore-indexes PROJECT_ID=...` — apply indexes from `firestore.indexes.json`.
- `make firestore-emulator` — start the emulator locally.
- `make migrate PROJECT_ID=... ARGS="--target=1 --dry-run"` — run the migration stub (builds with `-tags=firestore`).

Helper scripts:

- `scripts/firestore_enable.sh` — enables API and creates the default database.
- `scripts/firestore_apply_indexes.sh` — applies composite indexes from `firestore.indexes.json`.

### Firestore runtime notes

- Constructor: `NewFirestoreStore(ctx, projectID, jobID, numTasks)` get-or-creates the job doc and preserves existing `numTasks` if present.
- States live under `jobs/{jobID}/states` with document IDs `{tag}:{taskID}`. Versions are bumped transactionally per key.
- Subscriptions stream Firestore query snapshots; the manager handles replay ordering and version-based dedup.

## Migrations (stub)

The migration tool in [cmd/migrate](cmd/migrate) currently stamps `schemaVersion` on job docs and can scope to a single job or all jobs:

```sh
go run -tags=firestore ./cmd/migrate --project=$PROJECT_ID --target=1 --dry-run
```

Planned extensions: idempotent backfills for schema changes and emulator-backed integration tests.

## Repository hygiene

- Coverage/profile artifacts are ignored via [.gitignore](.gitignore).
- Firestore code is behind the `firestore` build tag; in-memory remains the default.
