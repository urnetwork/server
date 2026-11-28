//go:build linux

// Coordinator file transport retains the Linux descriptor-owned open boundary.
package competitionagent_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// Bytes, named paths and hashes cross the actual public file transport.
func TestCompetitionProtocolUnixFileRoundTripRetainsBytes(t *testing.T) {
	root := t.TempDir()
	source, err := os.MkdirTemp(root, "source-")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.MkdirTemp(root, "destination-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "reports"), 0700); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("synthetic protocol evidence\n"), 64)
	if err := os.WriteFile(filepath.Join(source, "reports", "result.json"), content, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	files := []competitionagent.File{{Path: "reports/result.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}}
	limits := competitionagent.DefaultLimits()
	var wire bytes.Buffer
	if err := competitionagent.WriteFiles(&wire, source, files, limits.ArtifactBytes, limits); err != nil {
		t.Fatal(err)
	}
	if err := competitionagent.ReadFiles(&wire, destination, files, limits.ArtifactBytes, limits); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(destination, "reports", "result.json"))
	if err != nil || !bytes.Equal(actual, content) || wire.Len() != 0 {
		t.Fatalf("portable file transport changed evidence: %v", err)
	}
}

// A FIFO has no writer: rejection must use nonblocking native open plus fstat.
func TestCompetitionProtocolUnixRejectsSymlinkAndFifoLeaves(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "regular"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "regular"), filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nil)
	limits := competitionagent.DefaultLimits()
	for _, name := range []string{"symlink", "fifo"} {
		var wire bytes.Buffer
		files := []competitionagent.File{{Path: name, Sha256: hex.EncodeToString(digest[:]), Bytes: 0}}
		if err := competitionagent.WriteFiles(&wire, root, files, limits.ArtifactBytes, limits); err == nil || wire.Len() != 0 {
			t.Fatalf("non-regular leaf reached portable wire: %s err=%v", name, err)
		}
	}
}
