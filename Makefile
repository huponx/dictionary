GO  ?= go
BIN ?= bin
IMAGE_TAG  ?= dev
IMAGE_REPO ?= ghcr.io/huponx
# database of `make up` in db-migration; the store tests need it migrated (V15, V20 and V21)
TEST_DB ?= postgres://postgres:postgres@localhost:5432/app?sslmode=disable

.PHONY: help tidy upgrade outdated test test-store test-live vet build run run-worker docker docker-worker clean

help:
	@printf '%s\n' \
		'tidy    go mod tidy' \
		'upgrade  upgrade the dependencies (PATCH=1: patch releases only), tidy, build and test' \
		'outdated  list the dependencies that have a newer version' \
		'test    go test -race ./... (the store tests are skipped without a database)' \
		'test-store  the store tests against $$TEST_DB (db-migration migrated to V25 or later)' \
		'test-live   call the real dictionary APIs once (DICTIONARY_LIVE=1), to check the parsers' \
		'vet     go vet ./...' \
		'build   dictionary and dictionary-worker into $(BIN)/' \
		'run     go run ./cmd/dictionary (reads the environment, see .env.example)' \
		'run-worker  go run ./cmd/dictionary-worker (export jobs and cleanup; same environment)' \
		'docker  build the image of the API' \
		'docker-worker  build the image of the worker'

tidy:
	GOWORK=off $(GO) mod tidy

# Upgrade the dependencies to their latest minor/patch releases (PATCH=1: patch releases only), then tidy and
# check that everything still builds and passes. GOWORK=off: resolves against the module proxy, not the sibling
# folders of go.work. Review `git diff go.mod go.sum` before committing.
PATCH ?=
upgrade:
	GOWORK=off $(GO) get -u$(if $(PATCH),=patch) ./...
	GOWORK=off $(GO) mod tidy
	GOWORK=off $(GO) build ./...
	GOWORK=off $(GO) test ./...

# Lists the modules that have a newer version (nothing is changed).
outdated:
	@GOWORK=off $(GO) list -u -m -f '{{if and .Update (not .Indirect)}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all

test:
	$(GO) test -race ./...

# A throwaway database is better than the dev one: the tests create their own users and delete them, but
# they need the identity_users and dictionary_saved_words tables.
test-store:
	DICTIONARY_TEST_POSTGRES_URL='$(TEST_DB)' $(GO) test -race -count=1 ./internal/store/

test-live:
	DICTIONARY_LIVE=1 $(GO) test -count=1 -run Live -v ./internal/lookup/

vet:
	$(GO) vet ./...

# cgo: the Anki library writes the deck with SQLite (go-sqlite3), so the binary needs a C compiler and glibc.
build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/dictionary ./cmd/dictionary
	$(GO) build -o $(BIN)/dictionary-worker ./cmd/dictionary-worker

run:
	$(GO) run ./cmd/dictionary

run-worker:
	$(GO) run ./cmd/dictionary-worker

docker:
	docker build -f build/dictionary/Dockerfile -t $(IMAGE_REPO)/dictionary:$(IMAGE_TAG) .

docker-worker:
	docker build -f build/dictionary-worker/Dockerfile -t $(IMAGE_REPO)/dictionary-worker:$(IMAGE_TAG) .

clean:
	rm -rf $(BIN)
