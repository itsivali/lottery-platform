SHELL := /bin/bash
GO_UNIT_FLAGS := -race -shuffle=on -count=1
COVER_FILE := coverage.out

.PHONY: setup dev down gen gen-check lint test test-int test-all e2e cover ci skips openapi-lint erd

setup:  ## install git hooks
	lefthook install

dev:  ## bring up the full local stack
	docker compose up -d --build --wait
	@echo "api: http://localhost:8080/readyz"

down:
	docker compose down -v

gen:  ## regenerate code (sqlc / oapi-codegen)
	@if [ -f sqlc.yaml ]; then \
		if command -v sqlc >/dev/null; then sqlc generate; else go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate; fi; \
	fi
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
	go test -tags integration $(GO_UNIT_FLAGS) -coverpkg=./... -coverprofile=$(COVER_FILE) ./...
	scripts/check-coverage.sh $(COVER_FILE)

skips:
	scripts/check-skips.sh

openapi-lint:
	npx --yes @redocly/cli@1.25.15 lint api/openapi.yaml

e2e:  ## journey against the compose stack
	scripts/e2e.sh

ci: lint skips test-all cover  ## exactly what CI runs for Go

ERD_DSN := postgres://lottery:lottery@localhost:$${POSTGRES_PORT:-5433}/lottery?sslmode=disable

erd:  ## regenerate docs/erd.svg from a freshly migrated local Postgres (needs Docker; Graphviz runs in a container)
	docker compose up -d --wait postgres
	DATABASE_URL="$(ERD_DSN)" go run ./cmd/lotteryctl migrate up
	go run ./tools/erd "$(ERD_DSN)" | docker run --rm -i alpine:3.20 sh -c 'apk add -q graphviz font-dejavu >/dev/null 2>&1 && dot -Tsvg' > docs/erd.svg
	docker compose stop postgres
