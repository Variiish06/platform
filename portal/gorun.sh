#!/bin/sh
# Run a Go command for the portal module inside a container (Go is not
# installed on the host). Usage: ./gorun.sh go test ./...
cd "$(dirname "$0")" || exit 1
exec docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp \
    -e GOCACHE=/go/cache/build -e GOMODCACHE=/go/cache/mod -e GOFLAGS=-buildvcs=false \
    -v platform_gocache:/go/cache -v "$PWD":/src -w /src golang:1.26-alpine "$@"
