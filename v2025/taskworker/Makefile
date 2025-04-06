
GOBUILD=go build -trimpath

all: init clean build

clean:
	rm -rf build

build:
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/
	env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 ${GOBUILD} -o build/darwin/arm64/
	env CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 ${GOBUILD} -o build/darwin/amd64/

warp_build:
	$(MAKE) all
	docker buildx build --progress=plain \
		--build-arg warp_env=${WARP_ENV} \
		--platform linux/arm64/v8,linux/amd64 \
		-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION} \
		--no-cache \
		--push \
		.

init:
	go clean -cache
	go clean -modcache
	go get -t -u ./...
