// File and document controls are deterministic and never access configuration,
// production credentials or a database.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A descriptor opened by the test isolates leaf controls from root ancestry.
func recoveryTestDirectory(t *testing.T) (*os.File, string) {
	t.Helper()
	path := t.TempDir()
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	return directory, path
}

// Private regular files admit exact bytes under the explicit owner/size bound.
func TestRecoveryPrivateLeafReadsExactBoundedBytes(t *testing.T) {
	directory, path := recoveryTestDirectory(t)
	content := []byte("synthetic private proof\n")
	if err := os.WriteFile(filepath.Join(path, "proof.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRecoveryLeaf(directory, "proof.json", uint32(os.Geteuid()), int64(len(content)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("exact private read: bytes=%d error=%v", len(got), err)
	}
	if got, err := readRecoveryLeaf(directory, "proof.json", uint32(os.Geteuid()), int64(len(content)-1)); err == nil || got != nil {
		t.Fatal("oversized private file was admitted")
	}
	if got, err := readRecoveryLeaf(directory, "proof.json", uint32(os.Geteuid())+1, int64(len(content))); err == nil || got != nil {
		t.Fatal("another owner's private file was admitted")
	}
}

// Symlinks, hard links, public files and path escapes never acquire authority.
func TestRecoveryPrivateLeafRejectsAliasesAndUnprotectedFiles(t *testing.T) {
	directory, path := recoveryTestDirectory(t)
	private := filepath.Join(path, "private")
	if err := os.WriteFile(private, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private", filepath.Join(path, "symbolic")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(private, filepath.Join(path, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "public"), []byte("synthetic"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The command's private umask must not turn this negative control private.
	if err := os.Chmod(filepath.Join(path, "public"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symbolic", "hard", "private", "public", "../escape", ".", "..", ""} {
		if got, err := readRecoveryLeaf(directory, name, uint32(os.Geteuid()), 1024); err == nil || got != nil {
			t.Fatalf("unsafe leaf %q was admitted", name)
		}
	}
}

// Opening a directory as if it were a file must fail before a blocking read.
func TestRecoveryPrivateLeafRejectsDirectoryAndInvalidBounds(t *testing.T) {
	directory, path := recoveryTestDirectory(t)
	if err := os.Mkdir(filepath.Join(path, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := readRecoveryLeaf(directory, "nested", uint32(os.Geteuid()), 1024); err == nil || got != nil {
		t.Fatal("directory was admitted as evidence")
	}
	for _, limit := range []int64{0, -1} {
		if got, err := readRecoveryLeaf(directory, "nested", uint32(os.Geteuid()), limit); err == nil || got != nil {
			t.Fatal("non-positive evidence bound was admitted")
		}
	}
}

// Unknown fields and duplicate keys, including nested ones inside a raw JSON
// snapshot, cannot be normalized away into a seemingly authenticated request.
func TestRecoveryDocumentRejectsAmbiguousAuthority(t *testing.T) {
	for _, content := range []string{
		`{"schema":1,"schema":2}`,
		`{"schema":1,"unknown":true}`,
		`{"schema":1} {"schema":2}`,
		`{"schema":1`,
		`{"schema":1,"snapshot":{"a":1,"a":2}}`,
	} {
		var value struct {
			Schema   int            `json:"schema"`
			Snapshot map[string]int `json:"snapshot,omitempty"`
		}
		if err := decodeRecoveryDocument([]byte(content), &value); err == nil {
			t.Fatalf("ambiguous document admitted: %s", content)
		}
	}
	var accepted struct {
		Schema int `json:"schema"`
	}
	if err := decodeRecoveryDocument([]byte("{\"schema\":1}\n"), &accepted); err != nil || accepted.Schema != 1 {
		t.Fatalf("strict document refused: %+v error=%v", accepted, err)
	}
}

// Relative or noncanonical authority names are rejected independently of uid.
func TestRecoveryFileRequiresCanonicalAbsolutePaths(t *testing.T) {
	for _, path := range []string{"", "proof.json", "/", "/private/../proof.json", "/private//proof.json"} {
		if content, err := readRecoveryFile(path, strings.Repeat("a", 64), 1024); err == nil || content != nil {
			t.Fatalf("unsafe authority path %q was admitted", path)
		}
	}
}

// Raw file identity must not be confused with canonical document identity.
func TestRecoveryRawDocumentPinDiffersFromSemanticIdentity(t *testing.T) {
	first := []byte(`{"schema":1}`)
	second := []byte("{ \"schema\" : 1 }\n")
	firstSha, secondSha := sha256.Sum256(first), sha256.Sum256(second)
	firstPin, secondPin := hex.EncodeToString(firstSha[:]), hex.EncodeToString(secondSha[:])
	if !recoveryFileDigestMatches(first, firstPin) || !recoveryFileDigestMatches(second, secondPin) ||
		recoveryFileDigestMatches(first, secondPin) || recoveryFileDigestMatches(second, firstPin) ||
		recoveryFileDigestMatches(first, "") || recoveryFileDigestMatches(first, strings.ToUpper(firstPin)) {
		t.Fatal("raw-file pin accepted different bytes, a semantic equivalent or a noncanonical digest")
	}
	var a, b struct {
		Schema int `json:"schema"`
	}
	if decodeRecoveryDocument(first, &a) != nil || decodeRecoveryDocument(second, &b) != nil || a != b {
		t.Fatal("valid semantic documents did not retain identical meaning")
	}
}

// The CLI must reach the store's immutable receipt lookup after runtime
// rotation. New-grant equality remains the store's separately tested gate.
func TestRecoveryRuntimeBindingDoesNotPreemptHistoricalReplay(t *testing.T) {
	proof := model.CompetitionJobReevaluationRuntime{
		Schema: 1, Kind: model.CompetitionJobReevaluationRuntimeKind, Accepted: true,
		Capability:   model.CompetitionJobReevaluationCapability,
		SourceCommit: strings.Repeat("1", 40), ApiImageDigest: "sha256:" + strings.Repeat("2", 64),
		WorkerImageDigest: "sha256:" + strings.Repeat("3", 64), WorkerSha256: strings.Repeat("4", 64),
		EvidenceSha256: strings.Repeat("5", 64), ApiCompatible: true, WorkerCompatible: true,
	}
	historical := model.CompetitionJobReevaluationAuthorization{RuntimeQualificationSha256: strings.Repeat("6", 64)}
	currentDigest, err := model.CompetitionAutomationDigest(proof)
	if err != nil || currentDigest == historical.RuntimeQualificationSha256 {
		t.Fatal("synthetic runtime rotation did not change qualification identity")
	}
	settings := &controller.Settings{}
	if err := bindRecoveryRuntime(settings, proof); err != nil {
		t.Fatalf("current qualified runtime blocked historical store lookup: %v", err)
	}
	proof.WorkerCompatible = false
	if err := bindRecoveryRuntime(settings, proof); err == nil {
		t.Fatal("historical replay bypassed independent current runtime qualification")
	}
}
