SHELL := /bin/sh
.DEFAULT_GOAL := build

GO ?= go
GOFMT ?= gofmt
BINARY ?= bin/script-agent
TEST_TIMEOUT ?= 90s

.PHONY: build test cicd

build:
	mkdir -p "$(dir $(BINARY))"
	$(GO) build -o "$(BINARY)" ./cmd/script-agent

test:
	$(GO) test -race -count=1 -timeout=$(TEST_TIMEOUT) ./...

# Ordered and fail-fast, including when invoked with make -j.
# This validates/builds locally; it does not deploy, publish, or push.
cicd:
	@unformatted="$$($(GOFMT) -l .)" || exit $$?; \
	if [ -n "$$unformatted" ]; then \
		printf 'Go files need formatting:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi
	$(MAKE) test
	$(GO) vet ./...
	$(MAKE) build
