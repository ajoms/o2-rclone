#!/bin/bash
set -e

RCLONE_VERSION="v1.72.1"
BUILD_DIR="$(dirname "$0")/build"
BACKEND_SRC="$(dirname "$0")/backend/o2"

echo "=== o2-rclone build script ==="
echo "Building rclone $RCLONE_VERSION with O2 Cloud backend..."

# Create temp working directory
WORK_DIR=$(mktemp -d)
trap "rm -rf $WORK_DIR" EXIT

# Clone rclone (shallow)
echo "Cloning rclone $RCLONE_VERSION..."
git clone --depth 1 --branch $RCLONE_VERSION https://github.com/rclone/rclone.git "$WORK_DIR/rclone" 2>/dev/null

# Copy backend
echo "Installing O2 backend..."
mkdir -p "$WORK_DIR/rclone/backend/o2"
cp "$BACKEND_SRC"/*.go "$WORK_DIR/rclone/backend/o2/"

# Register backend in all.go
sed -i.bak '/"_github.com\/rclone\/rclone\/backend\/netstorage"/a\\t_ "github.com/rclone/rclone/backend/o2"' "$WORK_DIR/rclone/backend/all/all.go"

# Build
cd "$WORK_DIR/rclone"
mkdir -p "$BUILD_DIR"

echo "Building for current platform..."
go build -o "$BUILD_DIR/rclone" .
echo "Built: $BUILD_DIR/rclone"

# Cross-compile for Linux
if [ -t 0 ]; then
    read -p "Build Linux binaries too? (y/N) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        GOOS=linux GOARCH=amd64 go build -o "$BUILD_DIR/rclone-linux-amd64" .
        echo "Built: $BUILD_DIR/rclone-linux-amd64"
        GOOS=linux GOARCH=arm64 go build -o "$BUILD_DIR/rclone-linux-arm64" .
        echo "Built: $BUILD_DIR/rclone-linux-arm64"
    fi
fi

echo "=== Done ==="
echo "Test: $BUILD_DIR/rclone help backends | grep o2"
