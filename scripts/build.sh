#!/bin/bash

# Insight Trace Build Script
# KETI APOLLO System

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
IMAGE_NAME="ketidevit2/insight-trace"
TAG="${1:-latest}"

echo "=============================================="
echo "  Insight Trace - Build Script"
echo "  KETI APOLLO System Component"
echo "=============================================="

cd "$PROJECT_DIR"

# Step 1: Download dependencies
echo ""
echo "[1/4] Downloading Go dependencies..."
go mod tidy
go mod download

# Step 2: Build binary
echo ""
echo "[2/4] Building binary..."
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/insight-trace ./cmd/main.go
echo "Binary built: bin/insight-trace"

# Step 3: Build Docker image
echo ""
echo "[3/4] Building Docker image..."
docker build -t ${IMAGE_NAME}:${TAG} .
echo "Docker image built: ${IMAGE_NAME}:${TAG}"

# Step 4: Push to registry (optional)
echo ""
echo "[4/4] Pushing to registry..."
if docker push ${IMAGE_NAME}:${TAG} 2>/dev/null; then
    echo "Image pushed: ${IMAGE_NAME}:${TAG}"
else
    echo "Push skipped (registry not available or not logged in)"
    echo "To push manually: docker push ${IMAGE_NAME}:${TAG}"
fi

# Import to containerd (for local Kubernetes)
echo ""
echo "Importing to containerd..."
if command -v ctr &> /dev/null; then
    docker save ${IMAGE_NAME}:${TAG} | ctr -n k8s.io images import -
    echo "Image imported to containerd"
else
    echo "containerd not available, skipping import"
fi

echo ""
echo "=============================================="
echo "  Build Complete!"
echo "  Image: ${IMAGE_NAME}:${TAG}"
echo "=============================================="
