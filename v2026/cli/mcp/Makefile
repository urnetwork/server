
GOBUILD=go build -trimpath

# A release records this input; it never follows the wall clock.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct)

.PHONY: all clean build warp_build warp_build_image

all: clean build

clean:
	rm -rf build

build:
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 ${GOBUILD} -o build/darwin/arm64/
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 ${GOBUILD} -o build/darwin/amd64/

warp_build:
	@echo "warp_build must be run through warpctl build so govulncheck gates image publication." >&2
	@false

warp_build_image:
	@case "${SOURCE_DATE_EPOCH}" in ''|*[!0-9]*) echo "SOURCE_DATE_EPOCH must be a nonnegative Unix timestamp" >&2; exit 1;; esac
	docker buildx build --progress=plain \
		--build-arg warp_env=${WARP_ENV} \
		--build-arg SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
	--platform linux/arm64/v8,linux/amd64 \
	-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION} \
	--no-cache \
	--output type=image,push=true,rewrite-timestamp=true,unpack=false \
	.
