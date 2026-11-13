// Prevents source-graph incompatibilities from reaching a published evaluator.
// The base must type-check the same offline package surface as every candidate.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dependency list does not type-check test-only imports. Freeze both vet
// gates so a source pin missing a fixture API fails before a base is released.
func TestEvaluatorBaseVetsCompleteCandidateSurfaceOffline(t *testing.T) {
	for _, fixture := range []struct {
		name string
		gate string
	}{
		{
			name: "Dockerfile.base",
			gate: "\nRUN --network=none GOFLAGS=-mod=readonly GOPROXY=off GOSUMDB=off go vet ./connect/...\n",
		},
		{
			name: "Dockerfile.submission",
			gate: "    GOFLAGS=-mod=readonly GOPROXY=off GOSUMDB=off \\\n    go vet ./connect/...\n",
		},
	} {
		dockerfileBytes, err := os.ReadFile(filepath.Join("evaluator", "container", fixture.name))
		if err != nil {
			t.Fatal(err)
		}
		if count := strings.Count(string(dockerfileBytes), fixture.gate); count != 1 {
			t.Errorf("%s offline candidate vet gate count = %d, want 1", fixture.name, count)
		}
	}
}
