package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Keep the release runner's multi-arch Gossip build joined to the binary path
// copied into its image.
func TestGossipContainerBuildContract(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate gossip container test")
	}
	directory := filepath.Dir(filename)

	makefileBytes, err := os.ReadFile(filepath.Join(directory, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(makefileBytes)
	for _, required := range []string{
		"GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/gossip",
		"GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/gossip",
		"--platform linux/arm64/v8,linux/amd64",
		"-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION}",
		"--push",
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("Gossip Makefile is missing %q", required)
		}
	}

	dockerfileBytes, err := os.ReadFile(filepath.Join(directory, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(dockerfileBytes)
	for _, required := range []string{
		"ARG TARGETPLATFORM",
		"ENV WARP_ENV=$warp_env",
		"COPY build/$TARGETPLATFORM/gossip /usr/local/sbin/bringyour-gossip",
		`CMD ["/usr/local/sbin/bringyour-gossip", "-p", "80"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Gossip Dockerfile is missing %q", required)
		}
	}
}
