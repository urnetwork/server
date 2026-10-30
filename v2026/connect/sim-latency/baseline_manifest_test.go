// Exercises the unchanged retained-tree verifier on isolated synthetic snapshots,
// keeping manifest coverage failures distinct from authenticated payload drift.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Owns only a fresh synthetic tree and empty process-home/temp directories.
type baselineManifestFixture struct {
	root               string
	homeDirectory      string
	tempDirectory      string
	commandTimeout     time.Duration
	waitDelay          time.Duration
	maximumOutputBytes int
}

// Drains joined child output without retaining more than the fixture's budget.
// os/exec serializes writes when stdout and stderr share this comparable writer.
type baselineManifestCapture struct {
	content      bytes.Buffer
	maximumBytes int
	exceeded     bool
}

// Continues draining after the retained-output bound instead of blocking a child.
func (self *baselineManifestCapture) Write(content []byte) (int, error) {
	remaining := self.maximumBytes - self.content.Len()
	retained := min(len(content), remaining)
	if retained < len(content) {
		self.exceeded = true
	}
	if _, err := self.content.Write(content[:retained]); err != nil {
		return 0, err
	}
	return len(content), nil
}

// Copies the actual verifier unchanged and seals a complete tiny snapshot.
func newBaselineManifestFixture(t *testing.T) *baselineManifestFixture {
	t.Helper()
	verifier, err := os.ReadFile(filepath.Join("baseline", "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	fixture := &baselineManifestFixture{
		root: filepath.Join(parent, "snapshot"), homeDirectory: filepath.Join(parent, "home"),
		tempDirectory: filepath.Join(parent, "tmp"), commandTimeout: 15 * time.Second,
		waitDelay: time.Second, maximumOutputBytes: 64 * 1024,
	}
	for _, directory := range []string{fixture.root, fixture.homeDirectory, fixture.tempDirectory} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte("{\"synthetic_value\":17}\n")
	files := map[string][]byte{
		"README.md":                    []byte("Synthetic retained snapshot.\n"),
		"verify.sh":                    verifier,
		"historical/results.csv":       []byte("value\n11\n"),
		"addition/README.md":           []byte("Synthetic new evidence.\n"),
		"addition/nested/results.json": payload,
		"addition/MANIFEST.sha256":     []byte(fmt.Sprintf("%x  nested/results.json\n", sha256.Sum256(payload))),
	}
	paths := make([]string, 0, len(files))
	for path, content := range files {
		fixture.write(t, path, content)
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var manifest strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(files[path]), path)
	}
	fixture.write(t, "MANIFEST.sha256", []byte(manifest.String()))
	return fixture
}

// Changes only a named synthetic leaf beneath the fixture's owned root.
func (self *baselineManifestFixture) write(t *testing.T, path string, content []byte) {
	t.Helper()
	if !filepath.IsLocal(path) {
		t.Fatalf("non-local synthetic path: %q", path)
	}
	absolute := filepath.Join(self.root, path)
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, content, 0600); err != nil {
		t.Fatal(err)
	}
}

// Uses trusted Bash, a clean environment and a joined, bounded process group.
func (self *baselineManifestFixture) verify(t *testing.T) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), self.commandTimeout)
	defer cancel()
	capture := &baselineManifestCapture{maximumBytes: self.maximumOutputBytes}
	command := exec.CommandContext(ctx, "/bin/bash", filepath.Join(self.root, "verify.sh"))
	command.Dir = self.root
	command.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + self.homeDirectory, "TMPDIR=" + self.tempDirectory,
		"TMP=" + self.tempDirectory, "LANG=C", "LC_ALL=C",
	}
	command.Stdout, command.Stderr = capture, capture
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = self.waitDelay
	err := command.Run()
	if ctx.Err() != nil || capture.exceeded {
		t.Fatalf("synthetic verifier exceeded its bound: context=%v output_exceeded=%t", ctx.Err(), capture.exceeded)
	}
	entries, readErr := os.ReadDir(self.tempDirectory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("verifier did not clean its owned temporary files: entries=%d error=%v", len(entries), readErr)
	}
	return capture.content.String(), err
}

// A complete manifest includes nested evidence, its metadata and the verifier.
func TestBaselineManifestAcceptsExactTree(t *testing.T) {
	fixture := newBaselineManifestFixture(t)
	output, err := fixture.verify(t)
	if err != nil || !strings.Contains(output, "sim-latency baseline snapshot: verified") {
		t.Fatalf("complete synthetic snapshot rejected: %v\n%s", err, output)
	}
}

// A newly retained nested file must fail coverage before checksum acceptance.
func TestBaselineManifestRejectsOmittedNestedFile(t *testing.T) {
	fixture := newBaselineManifestFixture(t)
	fixture.write(t, "addition/nested/omitted.json", []byte("{\"synthetic_value\":23}\n"))
	output, err := fixture.verify(t)
	if err == nil || !strings.Contains(output, "baseline manifest coverage differs from the working tree") {
		t.Fatalf("omitted nested evidence accepted or misclassified: %v\n%s", err, output)
	}
}

// Historical payload drift is not blessed by merely preserving tree coverage.
func TestBaselineManifestRejectsPayloadChecksumDrift(t *testing.T) {
	fixture := newBaselineManifestFixture(t)
	fixture.write(t, "historical/results.csv", []byte("value\n29\n"))
	output, err := fixture.verify(t)
	if err == nil || !strings.Contains(output, "historical/results.csv: FAILED") {
		t.Fatalf("changed payload accepted or misclassified: %v\n%s", err, output)
	}
}

// README updates need explicit checksum maintenance just like measured files.
func TestBaselineManifestRejectsReadmeChecksumDrift(t *testing.T) {
	fixture := newBaselineManifestFixture(t)
	fixture.write(t, "README.md", []byte("Synthetic revised snapshot description.\n"))
	output, err := fixture.verify(t)
	if err == nil || !strings.Contains(output, "README.md: FAILED") {
		t.Fatalf("changed README accepted or misclassified: %v\n%s", err, output)
	}
}

// Duplicating a correct digest cannot stand in for an independently covered file.
func TestBaselineManifestRejectsDuplicateEntries(t *testing.T) {
	fixture := newBaselineManifestFixture(t)
	manifest, err := os.ReadFile(filepath.Join(fixture.root, "MANIFEST.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, found := strings.Cut(string(manifest), "\n")
	if !found {
		t.Fatal("synthetic manifest has no complete entry")
	}
	fixture.write(t, "MANIFEST.sha256", append(manifest, []byte(first+"\n")...))
	output, err := fixture.verify(t)
	if err == nil || !strings.Contains(output, "baseline manifest coverage differs from the working tree") {
		t.Fatalf("duplicate entry accepted or misclassified: %v\n%s", err, output)
	}
}

// Malformed records cannot silently disappear from the parsed coverage census.
func TestBaselineManifestRejectsMalformedEntries(t *testing.T) {
	fixture := newBaselineManifestFixture(t)
	manifest, err := os.ReadFile(filepath.Join(fixture.root, "MANIFEST.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(t, "MANIFEST.sha256", append(manifest, []byte("invalid-digest  addition/unlisted.txt\n")...))
	output, err := fixture.verify(t)
	if err == nil || !strings.Contains(output, "baseline manifest contains a malformed entry") {
		t.Fatalf("malformed entry accepted or misclassified: %v\n%s", err, output)
	}
}
