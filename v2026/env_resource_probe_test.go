// Counts full resource resolutions by the resolver's own probe line, the line
// production services logged on every lookup. Only APIs that predate resolution
// caching are used, so a test built on these reproduces the per-request search
// against the unchanged resolver as well.
package server

import (
	"os"
	"strings"
	"testing"
)

// Collects what this process writes to stderr while run executes. The
// descriptor is redirected rather than os.Stderr, so concurrent writers keep
// their file and the race detector sees no shared variable change. glog writes
// synchronously, so every line run produced is captured when it returns. The
// capture is replayed to the original stderr so test output keeps it.
func captureProcessStderr(t testing.TB, run func()) string {
	t.Helper()
	capture, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	original, err := dupFd(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := dup2(int(capture.Fd()), 2); err != nil {
		closeFd(original)
		t.Fatal(err)
	}
	var captured []byte
	func() {
		defer func() {
			// restore first, so a failing run still reports to the real stderr
			if err := dup2(original, 2); err != nil {
				t.Errorf("restore stderr: %v", err)
			}
			closeFd(original)
			captured, _ = os.ReadFile(capture.Name())
			os.Stderr.Write(captured)
		}()
		run()
	}()
	return string(captured)
}

// The probe line one full resolution of a top-level resource writes for its
// literal home.
func resolverProbe(root string, relPath string) string {
	return "[env]try " + root + ", [" + relPath + "]"
}

// Full resolutions of relPath during run.
func countResolutions(t testing.TB, root string, relPath string, run func()) int {
	t.Helper()
	return strings.Count(captureProcessStderr(t, run), resolverProbe(root, relPath))
}
