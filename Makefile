PROJECT_ID ?= $(shell gcloud config get-value project 2>/dev/null)
REGION ?= us-central1
COLLECTION_PREFIX ?= jobs
GO ?= go
GOFLAGS ?=

.PHONY: firestore-init firestore-indexes firestore-emulator migrate guard-PROJECT_ID

guard-%:
	@if [ -z "$($*)" ]; then \
		echo "$* is required"; exit 1; \
	fi

firestore-init: guard-PROJECT_ID
	./scripts/firestore_enable.sh $(PROJECT_ID) $(REGION)
	./scripts/firestore_apply_indexes.sh $(PROJECT_ID)

firestore-indexes: guard-PROJECT_ID
	./scripts/firestore_apply_indexes.sh $(PROJECT_ID)

firestore-emulator:
	gcloud beta emulators firestore start --host-port=localhost:8080

migrate: guard-PROJECT_ID
	GOFLAGS="$(GOFLAGS) -tags=firestore" $(GO) run ./cmd/migrate --project=$(PROJECT_ID) --collection-prefix=$(COLLECTION_PREFIX) $(ARGS)
