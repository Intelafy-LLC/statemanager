package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Options defines CLI arguments for the migration tool.
type Options struct {
	projectID        string
	collectionPrefix string
	targetSchema     int
	dryRun           bool
	limit            int
	onlyJob          string
}

func parseFlags() Options {
	var opts Options
	flag.StringVar(&opts.projectID, "project", "", "GCP project ID")
	flag.StringVar(&opts.collectionPrefix, "collection-prefix", "jobs", "Collection prefix (e.g., jobs)")
	flag.IntVar(&opts.targetSchema, "target", 0, "Target schema version")
	flag.BoolVar(&opts.dryRun, "dry-run", false, "Validate without writing changes")
	flag.IntVar(&opts.limit, "limit", 0, "Optional limit on documents to migrate")
	flag.StringVar(&opts.onlyJob, "job", "", "Optional jobID to scope migration")
	flag.Parse()
	return opts
}

func main() {
	opts := parseFlags()
	if opts.projectID == "" {
		log.Fatal("--project is required")
	}
	if opts.targetSchema == 0 {
		log.Fatal("--target schema version is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	log.Printf("starting migration: project=%s prefix=%s target=%d dryRun=%t limit=%d job=%s",
		opts.projectID, opts.collectionPrefix, opts.targetSchema, opts.dryRun, opts.limit, opts.onlyJob)

	if err := run(ctx, opts); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
}

func run(ctx context.Context, opts Options) error {
	client, err := firestore.NewClient(ctx, opts.projectID)
	if err != nil {
		return fmt.Errorf("firestore client: %w", err)
	}
	defer client.Close()

	jobs := client.Collection(opts.collectionPrefix)
	if opts.onlyJob != "" {
		return migrateJob(ctx, jobs.Doc(opts.onlyJob), opts)
	}

	iter := jobs.Documents(ctx)
	defer iter.Stop()

	count := 0
	for {
		snap, err := iter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return err
		}
		if err := migrateJob(ctx, snap.Ref, opts); err != nil {
			return err
		}
		count++
		if opts.limit > 0 && count >= opts.limit {
			break
		}
	}
	return nil
}

func migrateJob(ctx context.Context, jobDoc *firestore.DocumentRef, opts Options) error {
	if _, err := jobDoc.Get(ctx); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return err
	}

	states := jobDoc.Collection("states").Documents(ctx)
	defer states.Stop()
	stateCount := 0
	for {
		_, err := states.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return err
		}
		stateCount++
		if opts.limit > 0 && stateCount >= opts.limit {
			break
		}
	}

	log.Printf("job=%s states=%d dryRun=%t target=%d", jobDoc.ID, stateCount, opts.dryRun, opts.targetSchema)
	if opts.dryRun {
		return nil
	}

	_, err := jobDoc.Update(ctx, []firestore.Update{{Path: "schemaVersion", Value: opts.targetSchema}})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}
