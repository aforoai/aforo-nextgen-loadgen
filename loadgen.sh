#!/bin/bash
# Helper script to run aforo-loadgen via Docker (no Go installation needed)

docker run --rm \
  -v "$(pwd):/workspace" \
  -w /workspace \
  -e AFORO_ADMIN_TOKEN="${AFORO_ADMIN_TOKEN}" \
  golang:1.22-alpine \
  ./bin/aforo-loadgen "$@"
