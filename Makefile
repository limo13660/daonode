SHELL := /bin/sh

GO ?= go
REQUIRED_GO_VERSION := go1.26.1
REQUIRED_GOEXPERIMENT := jsonv2
BUILD_TAGS := with_quic
VERSION ?= dev
OUTPUT ?= daonode

.PHONY: check-go download test build

check-go:
	@actual="$$(GOTOOLCHAIN=local $(GO) env GOVERSION)"; \
	if [ "$$actual" != "$(REQUIRED_GO_VERSION)" ]; then \
		echo "daonode requires $(REQUIRED_GO_VERSION); found $$actual" >&2; \
		exit 1; \
	fi

download: check-go
	GOTOOLCHAIN=local GOEXPERIMENT=$(REQUIRED_GOEXPERIMENT) $(GO) mod download

test: check-go
	GOTOOLCHAIN=local GOEXPERIMENT=$(REQUIRED_GOEXPERIMENT) $(GO) test -tags $(BUILD_TAGS) ./... -count=1

build: check-go
	GOTOOLCHAIN=local GOEXPERIMENT=$(REQUIRED_GOEXPERIMENT) $(GO) build \
		-tags $(BUILD_TAGS) \
		-trimpath \
		-ldflags "-X 'github.com/limo13660/daonode/cmd.version=$(VERSION)' -s -w -buildid=" \
		-o $(OUTPUT) .
