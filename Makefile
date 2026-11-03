# Copyright (c) 2026 Fyde Innovations Limited
# SPDX-License-Identifier: MIT

GO       ?= go
BIN      ?= bin/fydeos-notes-server
VERSION  ?= development
REVISION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
COMPOSE  := docker compose -f test/compose.yaml
IMAGE    ?= fydeos/fydeos-notes-server:$(VERSION)
GO_LICENSES := github.com/google/go-licenses/v2@v2.0.1

.PHONY: build test test-integration vet fmt generate tidy deps-up deps-down clean \
	docker third-party-licenses license-check

build:
	CGO_ENABLED=0 $(GO) build -trimpath \
		-ldflags "-s -w -X main.releaseVersion=$(VERSION) -X main.buildRevision=$(REVISION)" \
		-o $(BIN) ./cmd/fydeos-notes-server

# Tests that need PostgreSQL, Redis or S3 are skipped unless deps-up has run.
test:
	$(GO) test ./... -count=1

test-integration: deps-up
	NOTES_TEST_DATABASE_URL="postgres://notes:notes@$$($(COMPOSE) port postgres 5432)/notes_test?sslmode=disable" \
	NOTES_TEST_REDIS_URL="redis://$$($(COMPOSE) port redis 6379)/0" \
	NOTES_TEST_S3_ENDPOINT="http://$$($(COMPOSE) port s3 9000)" \
	$(GO) test ./... -count=1 -timeout=8m

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w cmd internal

generate:
	$(GO) generate ./internal/graphql

tidy:
	$(GO) mod tidy

deps-up:
	$(COMPOSE) up -d --wait

deps-down:
	$(COMPOSE) down -v

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) -t $(IMAGE) .

# Collect the license text of every dependency linked into the binary.
# Ship dist/third_party_licenses with binaries and images you distribute.
third-party-licenses:
	$(GO) run $(GO_LICENSES) save ./cmd/fydeos-notes-server --save_path=dist/third_party_licenses --force

# Fail if a linked dependency has an unknown or restrictive license.
license-check:
	$(GO) run $(GO_LICENSES) check ./cmd/fydeos-notes-server --disallowed_types=forbidden,restricted,unknown

clean:
	rm -rf bin dist
