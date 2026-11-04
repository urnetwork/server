// Protocol regressions exercise the actual framing and confined filesystem path.
package competitionagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Root confinement must hold before mkdir, even with a reused private root.
func TestReadFilesNeverCreatesThroughSymlinkAncestor(t *testing.T) {
	destination := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(destination, "redirect")); err != nil {
		t.Fatal(err)
	}
	content := []byte("synthetic report")
	digest := sha256.Sum256(content)
	files := []File{{Path: "redirect/created/report.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}}
	if err := ReadFiles(bytes.NewReader(content), destination, files, DefaultLimits().ArtifactBytes, DefaultLimits()); err == nil {
		t.Fatal("symlink ancestor was accepted")
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory was mutated before rejection: entries=%v err=%v", entries, err)
	}
}

// A writer may return n<len(p),nil; the frame must never report that as success.
func TestWriteDocumentRejectsShortWrites(t *testing.T) {
	for _, failCall := range []int{1, 2} {
		writer := &protocolShortWriter{failCall: failCall}
		if err := WriteDocument(writer, Request{Schema: Schema, Operation: "check", HostId: "executor.example"}, 4096); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write on call %d was not rejected: %v", failCall, err)
		}
	}
}

// A deterministic full writer otherwise preserves both frame components.
type protocolShortWriter struct {
	call     int
	failCall int
}

// Deliberately violates the full-write convention to exercise the boundary.
func (self *protocolShortWriter) Write(content []byte) (int, error) {
	self.call++
	if self.call == self.failCall {
		return len(content) - 1, nil
	}
	return len(content), nil
}

// Typed JSON's duplicate/case folding must not silently rewrite authority.
func TestReadDocumentRejectsDuplicateAndCaseAliasedFields(t *testing.T) {
	for _, content := range []string{
		`{"schema":1,"schema":2,"operation":"check","host_id":"executor.example","signature":""}`,
		`{"Schema":1,"operation":"check","host_id":"executor.example","signature":""}`,
		`{"schema":1,"operation":"check","host_id":"executor.example","signature":"","self_check":{"checks":{"ready":true,"ready":false}}}`,
	} {
		var input bytes.Buffer
		header := make([]byte, 8)
		binary.BigEndian.PutUint64(header, uint64(len(content)))
		input.Write(header)
		input.WriteString(content)
		var request Request
		if err := ReadDocument(&input, &request, 4096); err == nil {
			t.Fatalf("ambiguous protocol fields were accepted: %s", content)
		}
	}
}

// The signed clock and seed-bearing request cannot be replaced on a reconnect.
func TestRequestAuthenticationBindsClockInputsAndOwnerFence(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	request := Request{Schema: Schema, Operation: "start", HostId: "executor.example", RunId: RunId("synthetic-job", 0, 1), FencingToken: 1,
		Start: &StartManifest{Identity: Identity{HostId: "executor.example", RunId: RunId("synthetic-job", 0, 1), JobId: "synthetic-job", RoundId: "synthetic-round", SourceEpoch: 2,
			Attempt: 1, StartedAt: startedAt, DeadlineAt: startedAt.Add(3 * time.Hour)}}}
	if err := SignRequest(&request, privateKey); err != nil || VerifyRequest(request, publicKey) != nil {
		t.Fatalf("valid request did not authenticate: %v", err)
	}
	request.Start.Identity.DeadlineAt = request.Start.Identity.DeadlineAt.Add(time.Second)
	if err := VerifyRequest(request, publicKey); err == nil {
		t.Fatal("changed absolute deadline authenticated")
	}
	request.Start.Identity.DeadlineAt = request.Start.Identity.DeadlineAt.Add(-time.Second)
	request.FencingToken++
	if err := VerifyRequest(request, publicKey); err == nil {
		t.Fatal("changed owner fence authenticated")
	}
}

// Declared limits and regular-file hashes are checked before/during body writes.
func TestReadFilesRejectsTamperingAndOversizedDeclarations(t *testing.T) {
	limits := DefaultLimits()
	content := []byte("synthetic report")
	digest := sha256.Sum256(content)
	files := []File{{Path: "reports/result.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}}
	if err := ReadFiles(bytes.NewReader([]byte("changed-content!")), t.TempDir(), files, limits.ArtifactBytes, limits); err == nil {
		t.Fatal("tampered file body was accepted")
	}
	files[0].Bytes = limits.ArtifactBytes + 1
	if err := ReadFiles(bytes.NewReader(nil), t.TempDir(), files, limits.ArtifactBytes, limits); err == nil {
		t.Fatal("oversized declaration was accepted")
	}
}
