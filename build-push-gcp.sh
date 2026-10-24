#!/usr/bin/env bash
#
# Build the custom livekit-server image (v1.13.5 + stats forwarder changes)
# and push it to GCP Artifact Registry.
#
# Usage:
#   ./build-push-gcp.sh                 # tag derived from version/version.go
#   ./build-push-gcp.sh 1.13.5-custom   # explicit tag
#
# Project is resolved from the active gcloud config; region and repo are fixed to
# the LiveKit SFU Artifact Registry target. All are still overridable via env vars:
#   PROJECT=my-proj REGION=asia-south1 REPO=livekit-sfu ./build-push-gcp.sh
#
set -euo pipefail

cd "$(dirname "$0")"

# --- GCP target ---
# Resolve the project from the active gcloud config (same source of truth as
# deploy-livekit-sfu.sh), unless PROJECT is explicitly overridden.
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
# Region is the GKE cluster region (disha-backend runs in asia-south1); the
# Artifact Registry repo lives in the same region so pulls stay in-region.
REGION="${REGION:-asia-south1}"          # hardcoded cluster region
REPO="${REPO:-livekit-sfu}"              # Artifact Registry repo name
IMAGE="${IMAGE:-livekit-server}"
PLATFORM="${PLATFORM:-linux/amd64}"      # match your GKE/GCE node arch
# ------------------

# Fail early if the project could not be resolved.
if [[ -z "$PROJECT" ]]; then
  echo "ERROR: PROJECT is not set and could not be resolved from 'gcloud config get-value project'." >&2
  echo "       Run 'gcloud config set project <id>' or pass PROJECT=... as an env var." >&2
  exit 1
fi

# Tag: explicit arg, else the version constant from the source.
TAG="${1:-$(sed -n 's/.*Version = "\(.*\)"/\1/p' version/version.go)}"
if [[ -z "$TAG" ]]; then
  echo "ERROR: could not determine tag." >&2
  exit 1
fi

AR="${REGION}-docker.pkg.dev/${PROJECT}/${REPO}/${IMAGE}"
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

echo "Building ${AR}:${TAG} (${PLATFORM}, commit ${SHA})"

gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

docker buildx build \
  --platform "${PLATFORM}" \
  -t "${AR}:${TAG}" \
  -t "${AR}:${TAG}-${SHA}" \
  --push .

echo "Pushed:"
echo "  ${AR}:${TAG}"
echo "  ${AR}:${TAG}-${SHA}"
