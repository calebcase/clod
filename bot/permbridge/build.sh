#!/bin/bash
# Build permbridge as a static linux/amd64 binary that will be embedded
# into the bot via //go:embed and written into the container's runtime
# directory. CGO_ENABLED=0 + netgo ensures the resulting binary works in
# minimal base images (alpine/musl, distroless, scratch) without depending
# on a particular libc.
set -euo pipefail

cd "$(dirname "$0")"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -tags netgo \
  -o permbridge.linux-amd64 \
  .

printf '%s\n' "Built $(pwd)/permbridge.linux-amd64 ($(stat -c%s permbridge.linux-amd64) bytes)"
