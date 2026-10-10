# Common development tasks. CI runs the same targets.

GO       ?= go
BIN      ?= bin/monitor
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/plusclouds/monitoring.server/internal/buildinfo.Version=$(VERSION)

# Licenses allowed for linked dependencies (ADR-0010).
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,MPL-2.0

.PHONY: build test lint vuln licenses tidy-check generate generate-check check fuzz

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/monitor

test:
	$(GO) test -race ./...
	$(MAKE) fuzz

# Each parser of pushed data runs its fuzz test for a short while (F08).
FUZZTIME ?= 15s
fuzz:
	$(GO) test ./plugins/push -run '^$$' -fuzz '^FuzzParse$$' -fuzztime $(FUZZTIME)
	$(GO) test ./plugins/push -run '^$$' -fuzz '^FuzzDecode$$' -fuzztime $(FUZZTIME)
	$(GO) test ./plugins/vmagent -run '^$$' -fuzz '^FuzzDecode$$' -fuzztime $(FUZZTIME)
	$(GO) test ./plugins/boxagent -run '^$$' -fuzz '^FuzzDecode$$' -fuzztime $(FUZZTIME)

lint:
	golangci-lint run ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

licenses:
	$(GO) run github.com/google/go-licenses/v2@v2.0.1 check ./... \
		--allowed_licenses=$(ALLOWED_LICENSES) \
		--ignore github.com/plusclouds/monitoring.server

tidy-check:
	$(GO) mod tidy -diff

# Regenerate the API server code from api/openapi.yaml.
generate:
	$(GO) generate ./...

# Generated code is committed; CI fails when it is out of date (ADR-0011).
generate-check: generate
	git diff --exit-code -- internal/api/gen

check: tidy-check generate-check lint test vuln licenses
