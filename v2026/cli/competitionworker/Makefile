GOBUILD=go build -trimpath

.PHONY: all clean build test warp_build warp_build_image

all: clean build

clean:
	rm -rf build

build:
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/competitionworker
	env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/competitionworker

test:
	go test ../../competition ./...

warp_build:
	@echo "warp_build must be run through warpctl build so govulncheck gates image publication." >&2
	@false

warp_build_image:
	docker buildx build --progress=plain \
		--platform linux/arm64/v8,linux/amd64 \
		-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION} \
		--metadata-file build/image-metadata.json \
		--provenance=mode=max \
		--sbom=true \
		--no-cache \
		--push \
		.
