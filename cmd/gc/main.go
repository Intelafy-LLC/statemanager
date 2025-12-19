package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// options holds CLI configuration.
type options struct {
	projectID        string
	collectionPrefix string
	tombstoneSpec    string
	deleteSpec       string
	dryRun           bool
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.projectID, "project", "", "GCP project ID")
	flag.StringVar(&opts.collectionPrefix, "collection-prefix", "jobs", "Collection prefix (e.g., jobs)")
	flag.StringVar(&opts.tombstoneSpec, "tombstone-age", "", "Age before tombstoning (e.g., 72h, 7d, 4w, 1mo)")
	flag.StringVar(&opts.deleteSpec, "delete-age", "", "Age before deleting (must exceed tombstone-age)")
	flag.BoolVar(&opts.dryRun, "dry-run", false, "Log actions without writing")
	flag.Parse()
	return opts
}

func main() {
	opts := parseFlags()
	if opts.projectID == "" {
		log.Fatal("--project is required")
	}
	tombstoneAge, err := parseAge(opts.tombstoneSpec)
	if err != nil {
		log.Fatalf("invalid --tombstone-age: %v", err)
	}
	deleteAge, err := parseAge(opts.deleteSpec)
	if err != nil {
		log.Fatalf("invalid --delete-age: %v", err)
	}
	if deleteAge <= tombstoneAge {
		log.Fatal("delete-age must be greater than tombstone-age in elapsed time")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	client, err := firestore.NewClient(ctx, opts.projectID)
	if err != nil {
		log.Fatalf("firestore client: %v", err)
	}
	defer client.Close()

	now := time.Now().UTC()
	jobs := client.Collection(opts.collectionPrefix)
	iter := jobs.Documents(ctx)
	defer iter.Stop()

	var scanned, tombstoned, deleted int
	for {
		snap, err := iter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			log.Fatalf("iterate jobs: %v", err)
		}
		scanned++
		jobInfo := struct {
			Deleted bool `firestore:"deleted"`
		}{}
		_ = snap.DataTo(&jobInfo)

		lastActive, err := latestActivity(ctx, snap)
		if err != nil {
			log.Printf("warn: job=%s last-activity: %v", snap.Ref.ID, err)
			continue
		}
		age := now.Sub(lastActive)

		if jobInfo.Deleted {
			if age >= deleteAge {
				log.Printf("delete: job=%s age=%s deleted_at=%s", snap.Ref.ID, age, lastActive.Format(time.RFC3339))
				if !opts.dryRun {
					if err := deleteJob(ctx, client, snap.Ref); err != nil {
						log.Printf("error deleting job=%s: %v", snap.Ref.ID, err)
					} else {
						deleted++
					}
				}
			}
			continue
		}

		if age >= tombstoneAge {
			log.Printf("tombstone: job=%s age=%s last_active=%s", snap.Ref.ID, age, lastActive.Format(time.RFC3339))
			if !opts.dryRun {
				if err := markDeleted(ctx, snap.Ref); err != nil {
					log.Printf("error tombstoning job=%s: %v", snap.Ref.ID, err)
				} else {
					tombstoned++
				}
			}
		}
	}

	log.Printf("done: scanned=%d tombstoned=%d deleted=%d dryRun=%t", scanned, tombstoned, deleted, opts.dryRun)
}

// latestActivity returns the most recent activity timestamp for the job.
// Preference order: latest state timestamp, then job UpdateTime, then CreateTime.
func latestActivity(ctx context.Context, snap *firestore.DocumentSnapshot) (time.Time, error) {
	states := snap.Ref.Collection("states").OrderBy("timestamp", firestore.Desc).Limit(1).Documents(ctx)
	defer states.Stop()
	stateSnap, err := states.Next()
	if err != nil && err != iterator.Done {
		return time.Time{}, fmt.Errorf("scan states: %w", err)
	}
	if err == nil {
		var st struct {
			Timestamp time.Time `firestore:"timestamp"`
		}
		if err := stateSnap.DataTo(&st); err == nil && !st.Timestamp.IsZero() {
			return st.Timestamp, nil
		}
		if !stateSnap.UpdateTime.IsZero() {
			return stateSnap.UpdateTime, nil
		}
	}

	if !snap.UpdateTime.IsZero() {
		return snap.UpdateTime, nil
	}
	if !snap.CreateTime.IsZero() {
		return snap.CreateTime, nil
	}
	return time.Time{}, errors.New("no activity timestamps")
}

func markDeleted(ctx context.Context, jobDoc *firestore.DocumentRef) error {
	_, err := jobDoc.Update(ctx, []firestore.Update{{Path: "deleted", Value: true}})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

func deleteJob(ctx context.Context, client *firestore.Client, jobDoc *firestore.DocumentRef) error {
	statesIter := jobDoc.Collection("states").Documents(ctx)
	defer statesIter.Stop()

	batch := client.Batch()
	count := 0
	flush := func() error {
		if count == 0 {
			return nil
		}
		_, err := batch.Commit(ctx)
		batch = client.Batch()
		count = 0
		return err
	}

	for {
		snap, err := statesIter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return err
		}
		batch.Delete(snap.Ref)
		count++
		if count >= 450 {
			if err := flush(); err != nil {
				return err
			}
		}
	}

	batch.Delete(jobDoc)
	count++
	return flush()
}

var agePattern = regexp.MustCompile(`^(?i)(\d+)(h|d|w|mo)$`)

func parseAge(spec string) (time.Duration, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, errors.New("value is required")
	}
	if d, err := time.ParseDuration(spec); err == nil {
		if d <= 0 {
			return 0, errors.New("duration must be positive")
		}
		return d, nil
	}
	m := agePattern.FindStringSubmatch(spec)
	if len(m) != 3 {
		return 0, fmt.Errorf("invalid format: %s", spec)
	}
	value := m[1]
	unit := strings.ToLower(m[2])

	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("value must be positive")
	}

	switch unit {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "w":
		return time.Duration(n) * 24 * 7 * time.Hour, nil
	case "mo":
		return time.Duration(n) * 24 * 30 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported unit: %s", unit)
	}
}
