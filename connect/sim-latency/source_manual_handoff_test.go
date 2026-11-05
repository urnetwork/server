// Packet manifests identify prepared bytes without inventing an election,
// model review, manual release disposition or runtime qualification.
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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Includes all exact exported bytes, including newlines and the deliberately
// unfinished worksheets. An unrelated sealed submission is never copied.
func TestSourceReviewPacketManifestBindsCompletePreparedProjection(t *testing.T) {
	patch, score := []byte("synthetic canonical patch\n"), []byte("{\n  \"synthetic_score\": 80\n}\n\n")
	patchDigest, scoreDigest := sha256.Sum256(patch), sha256.Sum256(score)
	packet := &controller.CompetitionSourceReviewPacket{Patch: patch, Score: score,
		Entry: model.CompetitionSealedEntry{JobId: server.NewId(), State: "succeeded", PatchSha256: hex.EncodeToString(patchDigest[:]), ScoreSha256: hex.EncodeToString(scoreDigest[:])},
		Seal:  model.CompetitionEvaluationSeal{RoundId: server.NewId(), SealSha256: strings.Repeat("a", 64)}}
	directory := filepath.Join(t.TempDir(), "packet")
	if _, err := materializeSourceReviewPacket(packet, directory); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "packet-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sourceReviewPacketManifest
	if err := json.Unmarshal(content, &manifest); err != nil || manifest.Schema != 1 || manifest.Status != "PREPARED" || manifest.PromotionAuthorized || manifest.SealSha256 != packet.Seal.SealSha256 || len(manifest.Files) != 5 {
		t.Fatalf("invalid prepared packet manifest: %s, %v", content, err)
	}
	expectedNames := []string{"candidate.json", "canonical.patch", "manual-handoff-template.json", "review-template.json", "score.json"}
	fileContents := map[string][]byte{}
	for index, entry := range manifest.Files {
		if entry.Path != expectedNames[index] {
			t.Fatalf("manifest file census/order changed: %s", entry.Path)
		}
		value, err := os.ReadFile(filepath.Join(directory, entry.Path))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(value)
		if hex.EncodeToString(digest[:]) != entry.Sha256 || int64(len(value)) != entry.Bytes {
			t.Fatalf("manifest lost exact file identity: %s", entry.Path)
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Path))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			t.Fatalf("prepared evidence is not private/read-only: %s, %v", entry.Path, err)
		}
		fileContents[entry.Path] = value
	}
	reencoded, err := encodeSourceReviewPacketManifest(packet, fileContents)
	if err != nil || !bytes.Equal(reencoded, content) {
		t.Fatal("same prepared bytes changed packet identity", err)
	}
	fileContents["score.json"] = bytes.TrimSpace(fileContents["score.json"])
	changed, err := encodeSourceReviewPacketManifest(packet, fileContents)
	if err != nil || bytes.Equal(changed, content) {
		t.Fatal("score formatting mutation retained the exact packet version", err)
	}
}

// Published rules and actual frozen provider identities are mandatory context;
// no unchecked template can be interpreted as a candidate approval.
func TestSourceManualHandoffTemplateRequiresBothClarificationsAndManualGate(t *testing.T) {
	jobId := server.NewId()
	packet := &controller.CompetitionSourceReviewPacket{WinnerReviewPolicy: model.CompetitionSnowWinnerReviewPolicy, RewardRevision: 4,
		Seal:           model.CompetitionEvaluationSeal{RoundId: server.NewId(), SealSha256: strings.Repeat("a", 64)},
		Entry:          model.CompetitionSealedEntry{JobId: jobId, PatchSha256: strings.Repeat("b", 64), ScoreSha256: strings.Repeat("c", 64), BaseSourceLockSha256: strings.Repeat("d", 64)},
		RewardDecision: &model.CompetitionRewardDecision{Revision: 4, DecisionSha256: strings.Repeat("e", 64), SelectedJobId: &jobId}}
	value := newSourceManualHandoffTemplate(packet)
	if value.Status != "PREPARED" || value.PromotionAuthorized || value.Decision != "" || value.ReviewerId != "" || value.PrivateEvidenceSha256 != "" ||
		value.PersistedOperatorApproval != "" || value.SelectedProductSourceLockSha256 != "" || !value.CurrentElectedCandidate || value.RewardDecisionSha256 != packet.RewardDecision.DecisionSha256 ||
		strings.Join(value.RequiredProviderModels, ",") != strings.Join(model.CompetitionSnowWinnerReviewModelsForPolicy(packet.WinnerReviewPolicy), ",") {
		t.Fatal("manual worksheet invented selection, provider review or release authority")
	}
	if value.WorkloadPreservation.Version != winnerWorkloadReviewVersion || value.WorkloadAddendumSha256 != winnerWorkloadReviewAddendumSha256() || value.WorkloadPreservation.Disposition != "" ||
		value.ProductionBenefit.Version != server.CompetitionProductionBenefitVersion || value.ProductionBenefit.AddendumSha256 != server.CompetitionProductionBenefitAddendumSha256() || value.ProductionBenefit.Disposition != "" ||
		value.ProductionBenefit.ProductionPath != "" || value.ProductionBenefit.Mechanism != "" || value.ProductionBenefit.ClientBenefit != "" {
		t.Fatal("manual worksheet lost the qualified addenda or invented a subcheck")
	}
	seen := map[string]bool{}
	for _, check := range value.RequiredChecks {
		if seen[check.Check] || check.Requirement == "" || check.Disposition != "" || check.Explanation != "" || check.EvidenceSha256 != "" {
			t.Fatal("required check is missing, duplicated or preapproved")
		}
		seen[check.Check] = true
	}
	for _, required := range []string{"final_seal_and_current_election", "intentional_traffic_suppression", "credible_production_client_benefit", "honesty_safety_and_frozen_provider", "exact_ten_repository_product", "persisted_manual_publication_gate", "restart_safe_publication_and_index"} {
		if !seen[required] {
			t.Fatalf("manual handoff omitted %s", required)
		}
	}
	otherJobId := server.NewId()
	packet.RewardDecision.SelectedJobId = &otherJobId
	if newSourceManualHandoffTemplate(packet).CurrentElectedCandidate {
		t.Fatal("older candidate remained the elected winner after ranked replacement")
	}
	packet.RewardDecision.SelectedJobId = nil
	if newSourceManualHandoffTemplate(packet).CurrentElectedCandidate {
		t.Fatal("no-winner election fabricated a selected candidate")
	}
}

// Conflicting locators are rejected before a destination is created. Version
// and original-byte length cannot be silently dropped from authenticated data.
func TestSourceReviewPacketRejectsConflictingArchiveProvenanceBeforeWriting(t *testing.T) {
	patch, score := []byte("synthetic canonical patch\n"), []byte("{\"synthetic_score\":80}\n")
	patchDigest, scoreDigest := sha256.Sum256(patch), sha256.Sum256(score)
	packet := controller.CompetitionSourceReviewPacket{Patch: patch, Score: score,
		Entry:            model.CompetitionSealedEntry{PatchSha256: hex.EncodeToString(patchDigest[:]), ScoreSha256: hex.EncodeToString(scoreDigest[:]), ArtifactManifestSha256: strings.Repeat("a", 64)},
		ManifestArtifact: &controller.CompetitionReviewArtifactIdentity{Path: "artifact-manifest.json", Key: "synthetic/manifest", Sha256: strings.Repeat("a", 64), Bytes: 123},
		ScoreArtifact:    &controller.CompetitionReviewArtifactIdentity{Path: "score.json", Key: "synthetic/score", VersionId: "synthetic-version", Sha256: hex.EncodeToString(scoreDigest[:]), Bytes: int64(len(score))}}
	if err := validateSourceReviewPacketArchiveIdentity(&packet); err != nil {
		t.Fatal(err)
	}
	for index, mutate := range []func(*controller.CompetitionSourceReviewPacket){
		func(value *controller.CompetitionSourceReviewPacket) { value.ScoreArtifact = nil },
		func(value *controller.CompetitionSourceReviewPacket) { value.ManifestArtifact = nil },
		func(value *controller.CompetitionSourceReviewPacket) { value.ScoreArtifact.VersionId = "" },
		func(value *controller.CompetitionSourceReviewPacket) { value.ScoreArtifact.Bytes-- },
		func(value *controller.CompetitionSourceReviewPacket) {
			value.ScoreArtifact.Sha256 = strings.Repeat("b", 64)
		},
		func(value *controller.CompetitionSourceReviewPacket) {
			value.ManifestArtifact.Sha256 = strings.Repeat("b", 64)
		},
		func(value *controller.CompetitionSourceReviewPacket) { value.ManifestArtifact.Key = "" },
		func(value *controller.CompetitionSourceReviewPacket) {
			value.ScoreArtifact.Path = "synthetic-other.json"
		},
	} {
		changed, manifest, artifact := packet, *packet.ManifestArtifact, *packet.ScoreArtifact
		changed.ManifestArtifact, changed.ScoreArtifact = &manifest, &artifact
		mutate(&changed)
		directory := filepath.Join(t.TempDir(), "bad-packet")
		if _, err := materializeSourceReviewPacket(&changed, directory); err == nil {
			t.Fatalf("conflicting archive locator %d accepted", index)
		}
		if _, err := os.Lstat(directory); !os.IsNotExist(err) {
			t.Fatalf("conflicting archive locator created evidence: %v", err)
		}
	}
}
