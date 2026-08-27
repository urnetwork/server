GOBUILD=go build -trimpath

.PHONY: all clean build test

all: clean build

clean:
	rm -rf build

build:
	mkdir -p build/linux/amd64
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/competitionrebaseline

test:
	go test ../../competition ./...
