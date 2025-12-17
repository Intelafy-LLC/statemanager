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

echo "Applying Firestore indexes from ${INDEX_FILE} to project ${PROJECT_ID}..."
gcloud firestore indexes composite create --project "${PROJECT_ID}" --file="${INDEX_FILE}"

echo "Done."