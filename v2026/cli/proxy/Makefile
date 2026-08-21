
GOBUILD=go build -trimpath

.PHONY: all clean build init warp_build warp_build_image

all: init clean build

clean:
	rm -rf build

build:
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/proxy
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/proxy
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 ${GOBUILD} -o build/darwin/arm64/proxy
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 ${GOBUILD} -o build/darwin/amd64/proxy

warp_build:
	@echo "warp_build must be run through warpctl build so govulncheck gates image publication." >&2
	@false

warp_build_image:
	docker buildx build --progress=plain \
		--build-arg warp_env=${WARP_ENV} \
		--platform linux/arm64/v8,linux/amd64 \
		-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION} \
		--push \
		.

init:
	go clean -cache
	go clean -modcache
