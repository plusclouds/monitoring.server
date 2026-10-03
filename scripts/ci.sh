#!/usr/bin/env bash
# Runs one CI step inside the Go toolchain image, so a runner needs nothing
# but Docker (no Go, no gcc for -race). The same command works locally:
#
#   scripts/ci.sh lint | test | security
#
# Containers run as the calling user, so no root-owned files are left in the
# workspace, and Go's module and build caches persist on the host between
# runs ($CI_CACHE_DIR, default ~/.cache/monitoring-ci).
set -euo pipefail

GO_IMAGE=${GO_IMAGE:-golang:1.27}
LINT_IMAGE=${LINT_IMAGE:-golangci/golangci-lint:v2.14.0}
CACHE=${CI_CACHE_DIR:-$HOME/.cache/monitoring-ci}
mkdir -p "$CACHE/gopath" "$CACHE/gobuild" "$CACHE/lint" "$CACHE/home"

common=(
  --rm
  --user "$(id -u):$(id -g)"
  -v "$PWD:/src" -w /src
  -v "$CACHE:/cache"
  -e HOME=/cache/home
  -e GOPATH=/cache/gopath
  -e GOCACHE=/cache/gobuild
  -e GOLANGCI_LINT_CACHE=/cache/lint
)

in_go() { docker run "${common[@]}" "$GO_IMAGE" bash -euo pipefail -c "$1"; }

case "${1:-}" in
  lint)
    in_go 'go mod tidy -diff && make generate-check'
    docker run "${common[@]}" "$LINT_IMAGE" golangci-lint run ./...
    ;;
  test)
    # Integration tests start PostgreSQL with testcontainers through the
    # host's Docker daemon. Host networking lets them reach the containers'
    # published ports on localhost.
    sock=/var/run/docker.sock
    docker run "${common[@]}" \
      -v "$sock:$sock" --group-add "$(stat -c %g "$sock")" \
      --network host \
      -e TESTCONTAINERS_HOST_OVERRIDE=localhost \
      -e MONITOR_REQUIRE_DB=1 \
      "$GO_IMAGE" bash -euo pipefail -c 'make test && make build'
    ;;
  security)
    in_go 'make vuln && make licenses'
    ;;
  *)
    echo "usage: $0 lint|test|security" >&2
    exit 2
    ;;
esac
