package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Intelafy-LLC/statemanager"
)

func main() {
	var (
		projectID = flag.String("project", "", "GCP project ID")
		jobID     = flag.String("job", "", "Job ID to monitor")
		tag       = flag.String("tag", "", "Optional tag filter")
		taskID    = flag.Int("task", -1, "Optional task filter (0-based)")
	)
	flag.Parse()

	if *projectID == "" || *jobID == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false}))

	backend := statemanager.NewFirestoreStore(*projectID)
	jobStoreIface, _, err := backend.OpenJob(ctx, *jobID)
	if err != nil {
		logger.Error("open_job", slog.String("jobID", *jobID), slog.Any("err", err))
		os.Exit(1)
	}
	jobStore := jobStoreIface.(statemanager.JobScopedStore)
	if inserted, err := statemanager.InsertStore(ctx, jobStore); err != nil || !inserted {
		logger.Error("insert_store", slog.String("jobID", *jobID), slog.Any("err", err), slog.Bool("inserted", inserted))
		os.Exit(1)
	}
	mgr, err := statemanager.NewManager(*jobID)
	if err != nil {
		logger.Error("new_manager", slog.String("jobID", *jobID), slog.Any("err", err))
		os.Exit(1)
	}
	defer mgr.Close()

	var opts []statemanager.Option
	if *tag != "" {
		opts = append(opts, statemanager.WithTag(*tag))
	}
	if *taskID >= 0 {
		opts = append(opts, statemanager.WithTaskID(*taskID))
	}

	ch, err := mgr.Subscribe(ctx, opts...)
	if err != nil {
		logger.Error("subscribe", slog.Any("err", err))
		os.Exit(1)
	}

	for evt := range ch {
		s := evt.State
		logger.Info("state_change",
			slog.Any("state", s),
			slog.Bool("replay", evt.Replay),
		)
	}

	_ = mgr.Close()
}
