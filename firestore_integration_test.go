//go:build firestore
// +build firestore

package statemanager

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// getEnvOrDefault returns the env var or default if empty.
func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func requireEmulator(t *testing.T) (context.Context, func()) {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST not set; skipping Firestore integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return ctx, cancel
}

// waitForStatePresent polls until a specific task/tag state exists.
func waitForStatePresent(t *testing.T, mgr *Manager, task int, tag string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_, err := mgr.GetState(ctx, WithTaskID(task), WithTag(tag))
		if err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("state for task %d tag %s not visible after polling", task, tag)
}

func TestFirestoreSetGetAndSubscribe(t *testing.T) {
	ctx, cancel := requireEmulator(t)
	defer cancel()

	projectID := getEnvOrDefault("FIRESTORE_PROJECT_ID", "demo-test")
	jobID := fmt.Sprintf("it-%d", time.Now().UnixNano())
	numTasks := 2

	backend := NewFirestoreStore(projectID)
	mgr, err := NewManagerForNewJob(ctx, backend, jobID, 0, numTasks)
	if err != nil {
		t.Fatalf("NewManagerForNewJob: %v", err)
	}
	defer mgr.Close()

	if mgr.NumTasks() != numTasks {
		t.Fatalf("expected numTasks %d, got %d", numTasks, mgr.NumTasks())
	}

	// Write initial state.
	if err := mgr.SetState(ctx, State{JobID: jobID, TaskID: 0, Tag: "ingest", Status: 1, Message: "seed"}); err != nil {
		t.Fatalf("SetState seed: %v", err)
	}

	// Ensure the seed is visible before subscribing to avoid emulator eventual consistency flake.
	waitForStatePresent(t, mgr, 0, "ingest")

	// Subscribe (should replay the seed state).
	subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
	defer subCancel()
	ch, err := mgr.Subscribe(subCtx, WithTag("ingest"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	first := recvEvent(t, ch, 5*time.Second)
	if first.State.Status != 1 || first.State.TaskID != 0 {
		t.Fatalf("unexpected first state: %+v", first.State)
	}

	// Emit a live update and expect it with higher version.
	if err := mgr.SetState(ctx, State{JobID: jobID, TaskID: 0, Tag: "ingest", Status: 2, Message: "live"}); err != nil {
		t.Fatalf("SetState live: %v", err)
	}
	second := recvEvent(t, ch, 5*time.Second)
	if !(second.State.Version > first.State.Version) {
		t.Fatalf("expected version to increase: %d -> %d", first.State.Version, second.State.Version)
	}
	if second.State.Status != 2 {
		t.Fatalf("unexpected live status: %d", second.State.Status)
	}
}
