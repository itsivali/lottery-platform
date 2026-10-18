SHELL := /bin/bash
GO_UNIT_FLAGS := -race -shuffle=on -count=1
COVER_FILE := coverage.out

.PHONY: setup dev down gen gen-check lint test test-int test-all e2e cover ci skips openapi-lint

setup:  ## install git hooks
	lefthook install

dev:  ## bring up the full local stack
	docker compose up -d --build --wait
	@echo "api: http://localhost:8080/readyz"

down:
	docker compose down -v

gen:  ## regenerate code (sqlc / oapi-codegen) -- nothing to generate yet in Phase 0
	@if [ -f sqlc.yaml ]; then sqlc generate; fi
	@if [ -f api/oapi-codegen.yaml ]; then oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml; fi

gen-check: gen  ## fail if generation changes anything
	git diff --exit-code -- . ':!go.sum'

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	golangci-lint run ./...
	actionlint

test:  ## unit tests
	go test $(GO_UNIT_FLAGS) ./...

test-int:  ## integration tests (need Docker)
	go test -tags integration $(GO_UNIT_FLAGS) ./...

test-all: test test-int

cover:
	go test -tags integration $(GO_UNIT_FLAGS) -coverprofile=$(COVER_FILE) ./...
	scripts/check-coverage.sh $(COVER_FILE)

skips:
	scripts/check-skips.sh

openapi-lint:
	npx --yes @redocly/cli@1.25.15 lint api/openapi.yaml

e2e:  ## journey against the compose stack
	scripts/e2e.sh

ci: lint skips test-all cover  ## exactly what CI runs for Go
