// File and document controls are deterministic and never access configuration,
// production credentials or a database.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	limits := defaultRecoverySettings()
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
		if err := decodeRecoveryDocument([]byte(content), &value, limits.DocumentBytes); err == nil {
			t.Fatalf("ambiguous document admitted: %s", content)
		}
	}
	var accepted struct {
		Schema int `json:"schema"`
	}
	if err := decodeRecoveryDocument([]byte("{\"schema\":1}\n"), &accepted, limits.DocumentBytes); err != nil || accepted.Schema != 1 {
		t.Fatalf("strict document refused: %+v error=%v", accepted, err)
	}
}

// Case aliases cannot name the same typed authority field twice, including
// nested typed objects and Unicode spellings folded by encoding/json.
func TestRecoveryDocumentRejectsCaseAliasedProtocolKeys(t *testing.T) {
	limits := defaultRecoverySettings()
	for _, content := range []string{
		`{"Schema":1}`,
		`{"schema":1,"Schema":2}`,
		`{"SCHEMA":1,"schema":2}`,
		`{"ſchema":1}`,
		`{"expected_terminal_Snapshot":{"G1":1}}`,
		`{"expected_terminal_snapshot":{},"Expected_terminal_snapshot":{}}`,
	} {
		var auth model.CompetitionJobReevaluationAuthorization
		if err := decodeRecoveryDocument([]byte(content), &auth, limits.DocumentBytes); err == nil {
			t.Fatalf("case-aliased authority admitted: %s", content)
		}
	}
	for _, content := range []string{
		`{"runtime":{"Schema":1}}`,
		`{"runtime":{"schema":1,"Schema":2}}`,
		`{"runtime":{"expected_terminal_snapshot":{"G1":1}}}`,
	} {
		var envelope struct {
			Runtime model.CompetitionJobReevaluationRuntime `json:"runtime"`
		}
		if err := decodeRecoveryDocument([]byte(content), &envelope, limits.DocumentBytes); err == nil {
			t.Fatalf("nested typed authority admitted a case alias or data-map escape: %s", content)
		}
	}
	var accepted struct {
		Runtime model.CompetitionJobReevaluationRuntime `json:"runtime"`
	}
	if err := decodeRecoveryDocument([]byte(`{"runtime":{"schema":1}}`), &accepted, limits.DocumentBytes); err != nil || accepted.Runtime.Schema != 1 {
		t.Fatalf("exact nested protocol field refused: %+v error=%v", accepted, err)
	}
}

// The exact root raw snapshot is data, not a typed protocol. Preserve both
// case-distinct keys and exact numeric tokens without weakening duplicates.
func TestRecoveryDocumentPreservesCaseSensitiveSnapshotData(t *testing.T) {
	limits := defaultRecoverySettings()
	snapshot := `{"score_json":{"gates":{"G1":{"passed":true},"g1":{"passed":false}},"diagnostics":{"Schema":1,"schema":2,"samples":9007199254740993,"magnitude":1e400}}}`
	content := `{"schema":1,"expected_terminal_snapshot":` + snapshot + `}`
	var auth model.CompetitionJobReevaluationAuthorization
	if err := decodeRecoveryDocument([]byte(content), &auth, limits.DocumentBytes); err != nil || auth.Schema != 1 || !bytes.Equal(auth.ExpectedTerminalSnapshot, []byte(snapshot)) {
		t.Fatalf("case-sensitive raw snapshot changed or was refused: snapshot=%s error=%v", auth.ExpectedTerminalSnapshot, err)
	}
	var stored struct {
		ScoreJson struct {
			Gates map[string]struct {
				Passed bool `json:"passed"`
			} `json:"gates"`
			Diagnostics map[string]json.Number `json:"diagnostics"`
		} `json:"score_json"`
	}
	if err := json.Unmarshal(auth.ExpectedTerminalSnapshot, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.ScoreJson.Gates) != 2 || !stored.ScoreJson.Gates["G1"].Passed || stored.ScoreJson.Gates["g1"].Passed ||
		stored.ScoreJson.Diagnostics["Schema"] != json.Number("1") || stored.ScoreJson.Diagnostics["schema"] != json.Number("2") ||
		stored.ScoreJson.Diagnostics["samples"] != json.Number("9007199254740993") || stored.ScoreJson.Diagnostics["magnitude"] != json.Number("1e400") {
		t.Fatalf("snapshot data lost distinct keys or numeric precision: %+v", stored.ScoreJson)
	}
	for _, content := range []string{
		`{"expected_terminal_snapshot":{"G1":1,"G1":2}}`,
		`{"expected_terminal_snapshot":{"G1":1,"\u00471":2}}`,
	} {
		var auth model.CompetitionJobReevaluationAuthorization
		if err := decodeRecoveryDocument([]byte(content), &auth, limits.DocumentBytes); err == nil {
			t.Fatalf("raw snapshot duplicate was admitted: %s", content)
		}
	}
	var runtime model.CompetitionJobReevaluationRuntime
	if err := decodeRecoveryDocument([]byte(content), &runtime, limits.DocumentBytes); err == nil {
		t.Fatal("snapshot data exception admitted an unknown runtime protocol field")
	}
}

// The data-map exception does not relax document, encoding, depth or byte
// limits; the nesting boundary counts the enclosing authorization object.
func TestRecoveryDocumentRejectsMalformedOrUnboundedObjects(t *testing.T) {
	limits := defaultRecoverySettings()
	largeContent := `{"expected_terminal_snapshot":{"value":"` + strings.Repeat("a", int(limits.DocumentBytes)) + `"}}`
	for _, content := range []string{
		`null`, `[]`, `true`, `3`, `"synthetic"`,
		`{"schema":1} {"schema":2}`, `{"schema":1`,
		"{\"expected_terminal_snapshot\":{\"value\":\"\xff\"}}",
		`{"expected_terminal_snapshot":{"nested":` + strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63) + `}}`,
		largeContent,
	} {
		var auth model.CompetitionJobReevaluationAuthorization
		if err := decodeRecoveryDocument([]byte(content), &auth, limits.DocumentBytes); err == nil {
			t.Fatalf("malformed or unbounded document was admitted: bytes=%d", len(content))
		}
	}
	content := `{"expected_terminal_snapshot":{"nested":` + strings.Repeat("[", 62) + `0` + strings.Repeat("]", 62) + `}}`
	var auth model.CompetitionJobReevaluationAuthorization
	if err := decodeRecoveryDocument([]byte(content), &auth, limits.DocumentBytes); err != nil || len(auth.ExpectedTerminalSnapshot) == 0 {
		t.Fatalf("bounded data-map document was refused: error=%v", err)
	}
	smallContent := []byte(`{"schema":1}`)
	smallLimits := limits
	smallLimits.DocumentBytes = int64(len(smallContent))
	var smallAuth model.CompetitionJobReevaluationAuthorization
	if err := decodeRecoveryDocument(smallContent, &smallAuth, smallLimits.DocumentBytes); err != nil || smallAuth.Schema != 1 {
		t.Fatalf("exact custom small document limit was refused: error=%v", err)
	}
	smallLimits.DocumentBytes--
	if err := decodeRecoveryDocument(smallContent, &smallAuth, smallLimits.DocumentBytes); err == nil {
		t.Fatal("document one byte over its owner's custom limit was admitted")
	}
	for _, limit := range []int64{0, -1} {
		if err := decodeRecoveryDocument(smallContent, &smallAuth, limit); err == nil {
			t.Fatal("non-positive owner document limit was admitted")
		}
	}
	largeLimits := limits
	largeLimits.DocumentBytes = int64(len(largeContent))
	var largeAuth model.CompetitionJobReevaluationAuthorization
	if err := decodeRecoveryDocument([]byte(largeContent), &largeAuth, largeLimits.DocumentBytes); err != nil || int64(len(largeAuth.ExpectedTerminalSnapshot)) <= limits.DocumentBytes {
		t.Fatalf("explicit raised owner limit was replaced with the default: error=%v", err)
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
	limits := defaultRecoverySettings()
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
	if decodeRecoveryDocument(first, &a, limits.DocumentBytes) != nil || decodeRecoveryDocument(second, &b, limits.DocumentBytes) != nil || a != b {
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
