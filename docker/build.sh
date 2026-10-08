#!/bin/bash
set -e

# Example usage:
# ./build-images.sh /opt/niova/bin /local/shirisha/nb-bin paro1618/niova-csi v8 true/false

CSI_BIN_DIR="$1"
NIOVA_BIN_DIR="$2"
DOCKERHUB="$3"
VERSION="$4"
D_PUSH="$5"
if [ -z "$CSI_BIN_DIR" ] || [ -z "$NIOVA_BIN_DIR" ] || \
   [ -z "$DOCKERHUB" ] || [ -z "$VERSION" ]; then
    echo "Usage: $0 <csi-bin-dir> <niova-bin-dir> <dockerhub> <version>"
    exit 1
fi

CONTROLLER_BIN="${CSI_BIN_DIR}/niova-block-csi-controller"
NODE_BIN="${CSI_BIN_DIR}/niova-block-csi-node"

if [ ! -f "$CONTROLLER_BIN" ]; then
    echo "Missing controller binary: $CONTROLLER_BIN"
    exit 1
fi

if [ ! -f "$NODE_BIN" ]; then
    echo "Missing node binary: $NODE_BIN"
    exit 1
fi

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
cd "$DIR"

echo "Preparing build context in $DIR/.build_context..."
rm -rf .build_context
mkdir -p .build_context/bin
mkdir -p .build_context/usr/local/bin .build_context/usr/local/lib .build_context/usr/local/include .build_context/usr/local/sbin .build_context/usr/local/share

# Copy niova-block-csi tools from the system installation directory
if [ -f "$CONTROLLER_BIN" ]; then
    cp "$CONTROLLER_BIN" .build_context/bin/
else
    echo "Error: $CONTROLLER_BIN not found. Please run 'make build' first."
    exit 1
fi

if [ -f "$NODE_BIN" ]; then
    cp "$NODE_BIN" .build_context/bin/
else
    echo "Error: $NODE_BIN not found. Please run 'make build' first."
    exit 1
fi

# Copy dependencies from system
echo "Copying dependencies from /usr/local..."
cp -R "$NIOVA_BIN_DIR"/bin/* .build_context/usr/local/bin/ 2>/dev/null || true
cp -R "$NIOVA_BIN_DIR"/lib/* .build_context/usr/local/lib/ 2>/dev/null || true
cp -R "$NIOVA_BIN_DIR"/include/* .build_context/usr/local/include/ 2>/dev/null || true
cp -R "$NIOVA_BIN_DIR"/sbin/* .build_context/usr/local/sbin/ 2>/dev/null || true
cp -R "$NIOVA_BIN_DIR"/share/* .build_context/usr/local/share/ 2>/dev/null || true

echo "Building Docker image for controller..."
CONTROLLER_IMAGE="${DOCKERHUB}:controller-${VERSION}"
NODE_IMAGE="${DOCKERHUB}:node-${VERSION}"
docker build -f Dockerfile --build-arg BINARY=controller -t "$CONTROLLER_IMAGE" .build_context
echo "Building Docker image for node..."
docker build -f Dockerfile --build-arg BINARY=node -t "$NODE_IMAGE" .build_context
if [ "$D_PUSH" = "true" ]; then
    docker push "$CONTROLLER_IMAGE"
    docker push "$NODE_IMAGE"
else 
    echo "Skipping Docker push (D_PUSH=$D_PUSH)"
fi
echo "Building Docker image for node..."

echo "Docker build completed successfully."
echo "$CONTROLLER_IMAGE"
echo "$NODE_IMAGE"
