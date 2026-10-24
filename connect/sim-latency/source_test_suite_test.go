// Explicit Go-event streams prove package success, printed PASS and skipped
// mandatory tests cannot manufacture a complete correctness result.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceCorrectnessEventFixture(t *testing.T, mutation string) []byte {
	t.Helper()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	emit := func(action, pkg, test string) {
		if err := encoder.Encode(map[string]string{"Action": action, "Package": pkg, "Test": test}); err != nil {
			t.Fatal(err)
		}
	}
	changed := false
	for pkg, tests := range sourceCorrectnessRequiredTests() {
		for _, name := range tests {
			selected := !changed
			changed = true
			if selected && mutation == "missing" {
				continue
			}
			if !(selected && mutation == "no-run") {
				emit("run", pkg, name)
			}
			if selected && mutation == "repeat" {
				emit("run", pkg, name)
			}
			action := "pass"
			if selected && mutation == "skip" {
				action = "skip"
			}
			if selected && mutation == "fail" {
				action = "fail"
			}
			emit(action, pkg, name)
		}
		if mutation == "unfinished" {
			emit("run", pkg, "TestSyntheticUnfinished")
		}
		if mutation != "package-incomplete" {
			emit("pass", pkg, "")
		}
	}
	if mutation == "truncated" {
		return output.Bytes()[:output.Len()-3]
	}
	return output.Bytes()
}

func TestSourceCorrectnessSuiteRequiresEveryObservedMandatoryTest(t *testing.T) {
	expected := 0
	for _, names := range sourceCorrectnessRequiredTests() {
		expected += len(names)
	}
	if count, err := countSourceCorrectnessTests(sourceCorrectnessEventFixture(t, "")); err != nil || count != expected {
		t.Fatal("complete fixed test event census failed", count, expected, err)
	}
	for _, mutation := range []string{"missing", "no-run", "repeat", "skip", "fail", "package-incomplete", "truncated", "unfinished"} {
		if _, err := countSourceCorrectnessTests(sourceCorrectnessEventFixture(t, mutation)); err == nil {
			t.Fatal("incomplete/forged suite passed", mutation)
		}
	}
	if _, err := countSourceCorrectnessTests([]byte("PASS\n")); err == nil {
		t.Fatal("printed PASS was accepted as observed test events")
	}
}

func TestSourceCorrectnessSuiteUsesContainedImageAndRetainsExactLogs(t *testing.T) {
	request := sourceBuildContextFixture(t)
	settings := sourceSandboxTestSettings()
	settings.StateDirectory = t.TempDir()
	owner := strings.Repeat("a", 64)
	root := filepath.Join(settings.StateDirectory, owner)
	if err := os.MkdirAll(filepath.Join(root, "metadata", "output"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata", "output", "metadata.tar"), sourceBuildMetadataArchive(t, []*tar.Header{{Name: "version.mod", Typeflag: tar.TypeReg, Size: 4}}), 0o400); err != nil {
		t.Fatal(err)
	}
	// Test-owned sealed seed directories must be made removable in cleanup.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "correctness", "metadata"), 0o700) })
	image := "sha256:" + strings.Repeat("b", 64)
	daemon := &sourceSandboxDaemonFixture{input: sourceBuildSandboxInput{Owner: owner, Phase: "correctness"}, startOutput: string(sourceCorrectnessEventFixture(t, ""))}
	result, err := executeSourceCorrectnessSuite(context.Background(), request, sourceBuildArtifacts{Directory: root, ImageId: image}, owner, settings, daemon.run)
	if err != nil || result == nil || result.TestCount <= 0 || result.ExitCode != 0 || result.ImageId != image || result.SuiteSha256 != sourceCorrectnessSuiteDigest() || !daemon.removed {
		t.Fatal("fixed suite lost containment, observation or cleanup", result, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "correctness", "test-logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if result.TestLogsSha256 != hex.EncodeToString(digest[:]) {
		t.Fatal("test evidence hash does not bind retained exact logs")
	}
}

func TestSourceCorrectnessSandboxCannotSelectAnArbitraryCommand(t *testing.T) {
	input := sourceSandboxFixture()
	input.Phase = "correctness"
	input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "metadata")
	input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "output")
	args, err := sourceBuildSandboxArguments(input, sourceSandboxTestSettings())
	if err != nil || !strings.HasSuffix(args[len(args)-1], sourceCorrectnessSuiteScript()) {
		t.Fatal("correctness phase did not select the fixed suite", err)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--mount=") && !strings.HasSuffix(arg, ",readonly") {
			t.Fatal("test agent gained writable host access")
		}
	}
}
