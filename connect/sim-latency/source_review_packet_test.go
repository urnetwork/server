// Evidence materialization is available before approval and preserves exact
// archived bytes, including insignificant JSON whitespace and final newlines.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Authentication precedes directory creation; neither a reused destination nor
// a reserialized score can be presented as original immutable review evidence.
func TestSourceReviewPacketPreservesAuthenticatedBytesBeforeApproval(t *testing.T) {
	patch, score := []byte("synthetic canonical patch\n"), []byte("{\n  \"synthetic_score\": 80\n}\n\n")
	patchDigest, scoreDigest := sha256.Sum256(patch), sha256.Sum256(score)
	packet := &controller.CompetitionSourceReviewPacket{Patch: patch, Score: score, Entry: model.CompetitionSealedEntry{State: "succeeded", PatchSha256: hex.EncodeToString(patchDigest[:]), ScoreSha256: hex.EncodeToString(scoreDigest[:])},
		Seal: model.CompetitionEvaluationSeal{Entries: []model.CompetitionSealedEntry{{State: "unrelated-private-candidate"}}}}
	root := t.TempDir()
	directory := filepath.Join(root, "new-review")
	if _, err := materializeSourceReviewPacket(packet, directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("review directory is not private: %v", err)
	}
	for name, want := range map[string][]byte{"canonical.patch": patch, "score.json": score} {
		got, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s bytes changed: %q %v", name, got, err)
		}
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("%s is not private/read-only: %v", name, err)
		}
	}
	metadataBytes, err := os.ReadFile(filepath.Join(directory, "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		controller.CompetitionSourceReviewPacket
		SealMetadataOnly bool `json:"seal_metadata_only"`
	}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil || !metadata.SealMetadataOnly || len(metadata.Seal.Entries) != 0 ||
		bytes.Contains(metadataBytes, []byte("unrelated-private-candidate")) || len(packet.Seal.Entries) != 1 {
		t.Fatalf("review packet copied unrelated candidates or mutated its authoritative input: %s, %v", metadataBytes, err)
	}
	if _, err := materializeSourceReviewPacket(packet, directory); !os.IsExist(err) {
		t.Fatalf("existing evidence was overwritten: %v", err)
	}
	packet.Score = bytes.TrimSpace(packet.Score)
	badDirectory := filepath.Join(root, "unauthenticated-review")
	if _, err := materializeSourceReviewPacket(packet, badDirectory); err == nil {
		t.Fatal("reserialized archive bytes accepted")
	}
	if _, err := os.Lstat(badDirectory); !os.IsNotExist(err) {
		t.Fatal("unauthenticated evidence created a packet directory")
	}
}
