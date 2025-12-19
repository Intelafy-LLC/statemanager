PROJECT_ID ?= $(shell gcloud config get-value project 2>/dev/null)
REGION ?= us-central1
COLLECTION_PREFIX ?= jobs
GO ?= go
GOFLAGS ?=

.PHONY: firestore-init firestore-indexes firestore-emulator migrate guard-PROJECT_ID
.PHONY: test-firestore gc

guard-%:
	@if [ -z "$($*)" ]; then \
		echo "$* is required"; exit 1; \
	fi

firestore-init: guard-PROJECT_ID
	./scripts/firestore_enable.sh $(PROJECT_ID) $(REGION)
	./scripts/firestore_apply_indexes.sh $(PROJECT_ID)

firestore-indexes: guard-PROJECT_ID
	./scripts/firestore_apply_indexes.sh $(PROJECT_ID)

HOST_PORT ?= localhost:8787

firestore-emulator:
	gcloud beta emulators firestore start --host-port=$(HOST_PORT)

# Run Firestore-tagged tests against a running emulator (requires FIRESTORE_EMULATOR_HOST to be set, e.g., localhost:8787).
test-firestore:
	@if [ -z "$$FIRESTORE_EMULATOR_HOST" ]; then echo "FIRESTORE_EMULATOR_HOST must be set (e.g., localhost:8787)"; exit 1; fi
	GOFLAGS="$(GOFLAGS) -tags=firestore" $(GO) test ./...

migrate: guard-PROJECT_ID
	$(GO) run ./cmd/migrate --project=$(PROJECT_ID) --collection-prefix=$(COLLECTION_PREFIX) $(ARGS)

TOMBSTONE_AGE ?= 30d
DELETE_AGE ?= 90d

gc: guard-PROJECT_ID
	$(GO) run ./cmd/gc --project=$(PROJECT_ID) --collection-prefix=$(COLLECTION_PREFIX) --tombstone-age=$(TOMBSTONE_AGE) --delete-age=$(DELETE_AGE) $(ARGS)
