GO ?= go
GOOS ?= $(shell GOTOOLCHAIN=local $(GO) env GOOS)
GOARCH ?= $(shell GOTOOLCHAIN=local $(GO) env GOARCH)

BUILD_DIR := build/$(GOOS)/$(GOARCH)
BINARY := $(BUILD_DIR)/sim-latency

.PHONY: all clean build test

all: clean build

clean:
	rm -rf build

build:
	@test "$$(GOTOOLCHAIN=local $(GO) env GOVERSION)" = go1.27.3 || { echo "sim-latency requires Go 1.27.3" >&2; exit 1; }
	mkdir -p $(BUILD_DIR)
	env GOTOOLCHAIN=local GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		$(GO) build -trimpath -buildvcs=true -o $(BINARY) .

test:
	./tests.sh
