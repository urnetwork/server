package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The release runner builds Alt through this directory. Keep the multi-arch
// binary paths and the image entrypoint joined so a release cannot publish an
// empty or differently named container.
func TestAltContainerBuildContract(t *testing.T) {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate alt container test")
	}
	directory := filepath.Dir(filename)

	makefileBytes, err := os.ReadFile(filepath.Join(directory, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(makefileBytes)
	for _, required := range []string{
		"GOOS=linux GOARCH=arm64 ${GOBUILD} -o build/linux/arm64/alt",
		"GOOS=linux GOARCH=amd64 ${GOBUILD} -o build/linux/amd64/alt",
		"--platform linux/arm64/v8,linux/amd64",
		"-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION}",
		"--push",
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("Alt Makefile is missing %q", required)
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
		"COPY build/$TARGETPLATFORM/alt /usr/local/sbin/bringyour-alt",
		`CMD ["/usr/local/sbin/bringyour-alt", "-p", "80"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Alt Dockerfile is missing %q", required)
		}
	}
}
