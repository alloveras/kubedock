#!/bin/bash
set -euo pipefail

TAG="latest"
IMAGE="docker.io/alloveras/kubedock:${TAG}"
BUILD=$(git rev-parse --short HEAD)
DATE=$(date +%Y%m%d-%H%M%S)

echo "==> Ensuring multi-platform builder exists"
if ! docker buildx inspect multiarch &>/dev/null; then
  docker buildx create --name multiarch --driver docker-container --use
else
  docker buildx use multiarch
fi

echo "==> Building and pushing multi-arch image: ${IMAGE}"
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  --build-arg VERSION="${TAG}" \
  --build-arg BUILD="${BUILD}" \
  --build-arg DATE="${DATE}" \
  --build-arg IMAGE="${IMAGE}" \
  -t "${IMAGE}" \
  .

echo "==> Done: ${IMAGE} (build=${BUILD})"
