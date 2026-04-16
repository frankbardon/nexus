.PHONY: build run clean test fmt vet docs docs-serve docs-clean

BINARY_NAME=nexus
BUILD_DIR=bin
GO=go

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/nexus

run: build
	$(BUILD_DIR)/$(BINARY_NAME) -config configs/default.yaml

clean:
	rm -rf $(BUILD_DIR)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint: vet
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

docs:
	mdbook build docs

docs-serve:
	mdbook serve docs --open

docs-clean:
	rm -rf docs/book

.DEFAULT_GOAL := build
