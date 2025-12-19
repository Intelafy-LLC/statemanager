#!/usr/bin/env bash
set -euo pipefail

PROJECT_ID=${1:-}
INDEX_FILE=${INDEX_FILE:-firestore.indexes.json}

if [[ -z "$PROJECT_ID" ]]; then
  echo "usage: firestore_apply_indexes.sh <project_id>" >&2
  exit 1
fi

if [[ ! -f "$INDEX_FILE" ]]; then
  echo "index file not found: $INDEX_FILE" >&2
  exit 1
fi

echo "Applying Firestore indexes to project ${PROJECT_ID}..."

# Current manifest defines a single index on collectionGroup=states with fields:
# jobID ASC, tag ASC, taskID ASC, version DESC. We apply it directly via flags
# because gcloud does not support a --file/--source flag for composite indexes.
gcloud firestore indexes composite create \
  --project "${PROJECT_ID}" \
  --collection-group=states \
  --query-scope=COLLECTION \
  --field-config=field-path=jobID,order=ASCENDING \
  --field-config=field-path=tag,order=ASCENDING \
  --field-config=field-path=taskID,order=ASCENDING \
  --field-config=field-path=version,order=DESCENDING

echo "Done."