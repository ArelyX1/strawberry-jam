#!/usr/bin/env bash
## Fixes a linker bug on MacOS, see https://github.com/golang/go/issues/61229#issuecomment-1954706803
## Forces the old Apple linker.
ifeq ($(shell uname),Darwin)
    DARWIN_TEST_GOFLAGS=-ldflags=-extldflags=-Wl,-ld_classic
endif

GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

# Determine the extension for our Rust library (used via FFI)
ifeq ($(GOOS),darwin)
    LIB_EXT = dylib
else ifeq ($(GOOS),linux)
    LIB_EXT = so
else
  $(error Unsupported platform: $(GOOS))
endif

BANDERSNATCH_LIB = libbandersnatch.$(LIB_EXT)
ERASURECODING_LIB = liberasurecoding.$(LIB_EXT)

# Strip debug symbols and DWARF tables from release binaries.
# Override with `make build LDFLAGS=...` if you need symbols for debugging.
LDFLAGS ?= -s -w

all: help

.PHONY: help
help: Makefile
	@echo "Available commands:"
	@echo
	@sed -n 's/^##//p' $< | column -t -s ':' |  sed -e 's/^/ /'
	@echo

.PHONY: fmt
## fmt: Formats the Go code in the node and the service SDK.
fmt:
	go fmt ./... ./sdk/...

.PHONY: lint
## lint: Runs golangci-lint over the node and the service SDK.
lint:
	golangci-lint run --timeout=5m ./... ./sdk/...

.PHONY: build-bandersnatch
## build-bandersnatch: Builds the bandersnatch library
build-bandersnatch:
	cargo build --release --lib --manifest-path=bandersnatch/Cargo.toml
	mkdir -p internal/crypto/bandersnatch/lib
	cp bandersnatch/target/release/$(BANDERSNATCH_LIB) internal/crypto/bandersnatch/lib/$(BANDERSNATCH_LIB)

.PHONY: build-erasurecoding
## build-erasurecoding: Builds the erasure coding library
build-erasurecoding:
	cargo build --release --lib --manifest-path=erasurecoding/Cargo.toml
	mkdir -p internal/erasurecoding/reedsolomon/lib
	cp erasurecoding/target/release/$(ERASURECODING_LIB) internal/erasurecoding/reedsolomon/lib/$(ERASURECODING_LIB)

.PHONY: test
## test: Runs unit tests across every module in the workspace.
# The SDK is a separate module, so ./... alone would skip it and its tests
# would never run.
test: build-bandersnatch build-erasurecoding
	go test ./... ./sdk/... -race -v $(DARWIN_TEST_GOFLAGS)

.PHONY: test-sdk
## test-sdk: Runs only the service SDK tests.
test-sdk:
	go test ./sdk/... -race -v

.PHONY: integration-tiny
## integration: Runs integration tests with tiny configuration.
integration-tiny: build-bandersnatch build-erasurecoding
	go test ./tests/... ./pkg/network -race -v $(DARWIN_TEST_GOFLAGS) --tags=tiny,integration

.PHONY: integration-full
## integration-full: Runs integration tests with full configuration.
integration-full: build-bandersnatch build-erasurecoding
	go test ./tests/... -race -v $(DARWIN_TEST_GOFLAGS) --tags=full,integration

.PHONY: traces-tiny
## traces-tiny: Runs traces tests with tiny configuration.
traces-tiny: build-bandersnatch build-erasurecoding
	go test ./tests/... $(DARWIN_TEST_GOFLAGS) --tags=tiny,traces

## install-hooks: Install git-hooks from .githooks directory.
.PHONY: install-hooks
install-hooks:
	git config core.hooksPath .githooks

.PHONY: build
build: build-bandersnatch build-erasurecoding
	GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="$(LDFLAGS)" -o strawberry ./cmd/strawberry

.PHONY: release
## release: Cross builds the node for another target, native libraries included
## release TARGET=windows/amd64
release:
	scripts/build-release.sh $(TARGET)

.PHONY: build-conformance
## build-conformance: Builds the conformance tool with the tiny spec
build-conformance: build-bandersnatch build-erasurecoding
	mkdir -p pkg/conformance/bin
	go build -tags="tiny" -ldflags="$(LDFLAGS)" -o pkg/conformance/bin/strawberry ./pkg/conformance/cmd/main.go

.PHONY: build-conformance-full
## build-conformance-full: Builds the conformance tool with the full spec
build-conformance-full: build-bandersnatch build-erasurecoding
	mkdir -p pkg/conformance/bin
	go build -ldflags="$(LDFLAGS)" -o pkg/conformance/bin/strawberry-full ./pkg/conformance/cmd/main.go

.PHONY: test-conformance
## test-conformance: Runs conformance tests
test-conformance: build-bandersnatch build-erasurecoding
	go test ./tests/... $(DARWIN_TEST_GOFLAGS) --tags=conformance,traces
	go test ./pkg/conformance/... -v $(DARWIN_TEST_GOFLAGS) --tags=conformance

.PHONY: run-target
## run-target: Runs the conformance target with socket /tmp/jam_target.sock
run-target:
	./pkg/conformance/bin/strawberry --socket /tmp/jam_target.sock --pprof localhost:6060

.PHONY: bench
## bench: Runs a specific benchmark test. 
## Usage: make bench NAME=<BenchTestName> eg.NAME=BenchmarkTraceFallback
## Designed to run for one set of traces, eg fallback, safrole, not mixed together. (or single traces)
bench: build-bandersnatch build-erasurecoding
  ifndef NAME
	  $(error NAME is required. Usage: make bench NAME=<BenchTestName>)
  endif
	go test -bench=^$(NAME)$$ ./tests/integration --tags=traces,tiny | tee benchmark_results.txt
	python3 bench-stats.py benchmark_results.txt

.PHONY: test-guest
## test-guest: Runs the tests of the part of the guest that needs no PVM.
## This is the half of the economy that can be checked on the host: addresses,
## signatures, Keccak and the decoding of a relayed Ethereum transaction. It
## used to be unreachable, and two of its assumptions were wrong.
test-guest:
	cd guests/core && cargo +nightly-2025-05-10 test

.PHONY: build-guest
## build-guest: Rebuilds the guest blob and says whether it matches the one in
## the repository. The target and build-std are on the command line rather than
## in a .cargo/config.toml, because that file is inherited downwards and
## cannot be overridden, which is what stopped the core crate from being
## testable at all.
build-guest:
	cargo +nightly-2025-05-10 build --release \
		--manifest-path guests/Cargo.toml \
		--target riscv64emac-unknown-none-polkavm.json -Zbuild-std=core,alloc
	references/polkavm/target/release/polkatool link -i revive_v1 \
		-o guests/papucoin.pol \
		guests/target/riscv64emac-unknown-none-polkavm/release/papucoin-guest
	@git diff --quiet -- guests/papucoin.pol \
		&& echo "el blob commiteado es el que construye la fuente" \
		|| (echo "el blob commiteado ya no corresponde a su fuente:"; \
		    git --no-pager diff --stat -- guests/papucoin.pol; false)
