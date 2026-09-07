GO ?= go
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

BUILD_DIR := build/$(GOOS)/$(GOARCH)
BINARY := $(BUILD_DIR)/sim-latency

.PHONY: all clean build test

all: clean build

clean:
	rm -rf build

build:
	mkdir -p $(BUILD_DIR)
	env GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		$(GO) build -trimpath -buildvcs=true -o $(BINARY) .

test:
	./tests.sh
