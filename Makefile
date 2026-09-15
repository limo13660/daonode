SHELL := /bin/sh

GO ?= go
REQUIRED_GO_VERSION := go1.26.1
REQUIRED_GOEXPERIMENT := jsonv2
BUILD_TAGS := with_quic
VERSION ?= dev
OUTPUT ?= daonode

.PHONY: check-go download test test-sudoku-interop build

check-go:
	@actual="$$(unset GOROOT; GOTOOLCHAIN=local $(GO) env GOVERSION)"; \
	if [ "$$actual" != "$(REQUIRED_GO_VERSION)" ]; then \
		echo "daonode requires $(REQUIRED_GO_VERSION); found $$actual" >&2; \
		exit 1; \
	fi

download: check-go
	unset GOROOT; GOTOOLCHAIN=local GOEXPERIMENT=$(REQUIRED_GOEXPERIMENT) $(GO) mod download

test: check-go
	unset GOROOT; GOTOOLCHAIN=local GOEXPERIMENT=$(REQUIRED_GOEXPERIMENT) $(GO) test -tags $(BUILD_TAGS) ./... -count=1

test-sudoku-interop: check-go
	GO="$(GO)" YSBLCORE_DIR="$(YSBLCORE_DIR)" ./script/test-sudoku-interop.sh

build: check-go
	unset GOROOT; GOTOOLCHAIN=local GOEXPERIMENT=$(REQUIRED_GOEXPERIMENT) $(GO) build \
		-tags $(BUILD_TAGS) \
		-trimpath \
		-ldflags "-X 'github.com/limo13660/daonode/cmd.version=$(VERSION)' -s -w -buildid=" \
		-o $(OUTPUT) .
