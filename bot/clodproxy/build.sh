#!/bin/bash
# Build clodproxy as a static linux/amd64 binary that will be embedded
# into the bot via //go:embed and written into the container's runtime
# directory. Same recipe as permbridge/schedbridge — CGO_ENABLED=0 +
# netgo so the binary works in minimal base images without depending
# on a specific libc.
set -euo pipefail

cd "$(dirname "$0")"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -tags netgo \
  -o clodproxy.linux-amd64 \
  .

printf '%s\n' "Built $(pwd)/clodproxy.linux-amd64 ($(stat -c%s clodproxy.linux-amd64) bytes)"
