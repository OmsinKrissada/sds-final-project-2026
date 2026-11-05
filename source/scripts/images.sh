#!/usr/bin/env bash
# Builds and pushes all 5 multi-arch images (build context = repo root).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
need REGISTRY
TAG="${TAG:-latest}"
REGISTRY="${REGISTRY%/}"
cd "$ROOT"
for s in gateway frontend account quiz game; do
  echo ">>> $REGISTRY/quizbox-$s:$TAG"
  docker buildx build --platform linux/arm64,linux/amd64 \
    -f "services/$s/Dockerfile" \
    -t "$REGISTRY/quizbox-$s:$TAG" --push .
done
