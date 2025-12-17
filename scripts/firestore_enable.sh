#!/usr/bin/env bash
set -euo pipefail

PROJECT_ID=${1:-}
REGION=${2:-}

if [[ -z "$PROJECT_ID" || -z "$REGION" ]]; then
  echo "usage: firestore_enable.sh <project_id> <region>" >&2
  exit 1
fi

echo "Enabling Firestore API for project ${PROJECT_ID}..."
gcloud services enable firestore.googleapis.com --project "${PROJECT_ID}"

echo "Ensuring Firestore database exists (region=${REGION})..."
if gcloud firestore databases describe --project "${PROJECT_ID}" >/dev/null 2>&1; then
  echo "Firestore database already exists for project ${PROJECT_ID}."
else
  gcloud firestore databases create --project "${PROJECT_ID}" --database="(default)" --location="${REGION}" --type=firestore-native
fi

echo "Done."