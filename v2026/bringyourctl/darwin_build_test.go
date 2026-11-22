// The owner runs bringyourctl from a Mac, so the command must build for darwin
// from any host. A linux-only symbol reached from a cross-platform file breaks
// that build while every linux service image still builds.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Cross-compiles this command for darwin/arm64 without cgo, so a linux test
// host catches the break before the owner's `go run` does.
func TestBuildsForDarwin(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the whole command")
	}
	binary := filepath.Join(t.TempDir(), "bringyourctl")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("GOOS=darwin GOARCH=arm64 go build: %v\n%s", err, output)
	}
}
