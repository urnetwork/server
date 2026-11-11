// The real context writer and image planner must authenticate identical locks.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both instrument adoption and inherited product generations retain coverage.
func TestSourceBuildContextMetadataMatchesCoverageImagePlan(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, inherited)
		assertSourceBuildContextMetadataMatchesPlan(t, request)
	}
}

// Historical sources keep their original coverage-free lock bytes.
func TestSourceBuildContextMetadataMatchesLegacyImagePlan(t *testing.T) {
	assertSourceBuildContextMetadataMatchesPlan(t, sourceBuildContextFixture(t))
}

// Exercise the actual metadata producer, not a second hand-written codec.
func assertSourceBuildContextMetadataMatchesPlan(t *testing.T, request reviewedSourceBuildRequest) {
	t.Helper()
	root := t.TempDir()
	context := &sourceBuildContext{Directory: root, ManifestPath: filepath.Join(root, "sim-latency.yml"), LockPath: filepath.Join(root, "source-lock.json")}
	if err := writeSourceBuildContextMetadata(context, request); err != nil {
		t.Fatal(err)
	}
	plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile(context.LockPath)
	if err != nil || !bytes.Equal(lock, plan.SourceLock) {
		t.Fatal("build context and image planner disagree on authenticated source lock", err)
	}
	manifest, err := os.ReadFile(context.ManifestPath)
	if err != nil || !bytes.Equal(manifest, request.SourceManifest) {
		t.Fatal("build context changed approved source manifest", err)
	}
}
