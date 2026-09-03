package server

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Test directory discovery must retain local unit and integration packages
// while leaving every acceptance-owned subtree to its separate harness.
func TestLocalTestDirectoryDiscoveryExcludesAcceptance(t *testing.T) {
	output, err := exec.Command("./test-dirs.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("discover local test directories: %v\n%s", err, output)
	}
	directories := strings.Fields(string(output))
	for _, directory := range directories {
		if strings.Contains(strings.ToLower(directory), "acceptance") {
			t.Errorf("local test discovery included acceptance-owned directory %q", directory)
		}
	}
	for _, requiredDirectory := range []string{".", "./connect/perfvar", "./grafana", "./proxy"} {
		if !slices.Contains(directories, requiredDirectory) {
			t.Errorf("local test discovery omitted %q", requiredDirectory)
		}
	}
}
