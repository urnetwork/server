// Synthetic exported packets exercise the exact materializer/seed boundary.
// These tests neither use a live elected identity nor authenticate a provider.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

func winnerMultipassExportedSeedFixture(t *testing.T) (*controller.CompetitionSourceReviewPacket, []byte) {
	t.Helper()
	request, _ := winnerSnowJudgmentFixture()
	request.Packet.Seal.Entries = []model.CompetitionSealedEntry{request.Packet.Entry, {State: "synthetic-unrelated-private-entry"}}
	directory := filepath.Join(t.TempDir(), "fresh-export")
	if _, err := materializeSourceReviewPacket(request.Packet, directory); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	return request.Packet, content
}

// The export's original full-seal digest remains provenance, not a newly hashed
// partial seal. The original packet and exact export bytes remain unmodified.
func TestWinnerMultipassSeedDecodesActualMaterializedMetadataProjection(t *testing.T) {
	original, content := winnerMultipassExportedSeedFixture(t)
	contentBefore := append([]byte{}, content...)
	seed, err := decodeWinnerMultipassInstrumentSeed(content)
	if err != nil {
		t.Fatal(err)
	}
	want := *original
	want.Patch, want.Score, want.Seal.Entries = nil, nil, nil
	if !reflect.DeepEqual(seed, &want) || seed.Seal.SealSha256 != original.Seal.SealSha256 ||
		len(original.Patch) == 0 || len(original.Score) == 0 || len(original.Seal.Entries) != 2 || !bytes.Equal(contentBefore, content) {
		t.Fatal("metadata seed was reinterpreted as a full seal, normalized, or mutated its authentic input")
	}
}

// An omitted/false/aliased marker, hidden payload or non-null inventory cannot
// silently become a valid metadata-only seed. Unknown fields remain rejected.
func TestWinnerMultipassSeedRejectsAmbiguousOrUnlabeledProjection(t *testing.T) {
	_, content := winnerMultipassExportedSeedFixture(t)
	mutations := []func(map[string]json.RawMessage){
		func(fields map[string]json.RawMessage) { delete(fields, "seal_metadata_only") },
		func(fields map[string]json.RawMessage) { fields["seal_metadata_only"] = json.RawMessage("false") },
		func(fields map[string]json.RawMessage) { fields["seal_metadata_only"] = json.RawMessage("null") },
		func(fields map[string]json.RawMessage) { fields["seal_metadata_only"] = json.RawMessage(`"true"`) },
		func(fields map[string]json.RawMessage) { fields["seal_metadata_omitted"] = json.RawMessage("true") },
		func(fields map[string]json.RawMessage) { fields["SEAL_METADATA_ONLY"] = json.RawMessage("true") },
		func(fields map[string]json.RawMessage) { fields["PATCH"] = json.RawMessage("null") },
		func(fields map[string]json.RawMessage) { delete(fields, "patch") },
		func(fields map[string]json.RawMessage) { fields["patch"] = json.RawMessage(`""`) },
		func(fields map[string]json.RawMessage) { fields["score"] = json.RawMessage(`"e30="`) },
		func(fields map[string]json.RawMessage) {
			var seal map[string]json.RawMessage
			_ = json.Unmarshal(fields["seal"], &seal)
			seal["entries"] = json.RawMessage("[]")
			fields["seal"], _ = json.Marshal(seal)
		},
		func(fields map[string]json.RawMessage) {
			var seal map[string]json.RawMessage
			_ = json.Unmarshal(fields["seal"], &seal)
			delete(seal, "entries")
			fields["seal"], _ = json.Marshal(seal)
		},
		func(fields map[string]json.RawMessage) {
			var seal map[string]json.RawMessage
			_ = json.Unmarshal(fields["seal"], &seal)
			seal["Entries"] = json.RawMessage("null")
			fields["seal"], _ = json.Marshal(seal)
		},
	}
	for index, mutate := range mutations {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(content, &fields); err != nil {
			t.Fatal(err)
		}
		mutate(fields)
		changed, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeWinnerMultipassInstrumentSeed(changed); err == nil {
			t.Fatalf("ambiguous metadata projection %d accepted", index)
		}
	}
	duplicate := bytes.Replace(content, []byte(`"seal_metadata_only": true`), []byte(`"seal_metadata_only": true, "seal_metadata_only": true`), 1)
	if bytes.Equal(content, duplicate) {
		t.Fatal("synthetic duplicate marker mutation did not apply")
	}
	if _, err := decodeWinnerMultipassInstrumentSeed(duplicate); err == nil {
		t.Fatal("duplicate projection marker accepted")
	}
}

// Synthetic controls reuse the entire frozen instrument/source context but no
// real candidate authority or archive locator, and cannot mutate their seed.
func TestWinnerMultipassQualificationProjectsOnlyFrozenInstrumentProvenance(t *testing.T) {
	request, _ := winnerSnowJudgmentFixture()
	seed := request.Packet
	seed.SourceCommits = map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		seed.SourceCommits[repository] = strings.Repeat("a", 40)
	}
	lock, err := model.CompetitionSourceLockDigest(seed.SourceCommits)
	if err != nil {
		t.Fatal(err)
	}
	seed.Entry.BaseSourceLockSha256 = lock
	seed.SourceGeneration, seed.ActivationGeneration = 0, 1
	seed.RewardDecision = &model.CompetitionRewardDecision{DecisionSha256: strings.Repeat("c", 64)}
	seed.ManifestArtifact = &controller.CompetitionReviewArtifactIdentity{Key: "synthetic-private-candidate-manifest"}
	seed.ScoreArtifact = &controller.CompetitionReviewArtifactIdentity{Key: "synthetic-private-candidate-score"}
	before, _ := json.Marshal(seed)
	projected, err := winnerMultipassQualificationInstrumentSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if projected.RewardDecision != nil || projected.ManifestArtifact != nil || projected.ScoreArtifact != nil || projected.Patch != nil || projected.Score != nil || projected.Seal.SealSha256 != "" ||
		projected.Entry.JobId == seed.Entry.JobId || projected.Entry.BaseSourceLockSha256 != lock || projected.ActivationGeneration != 1 || projected.BaseSha != seed.BaseSha ||
		!reflect.DeepEqual(projected.Instrument, seed.Instrument) || !reflect.DeepEqual(projected.SourceCommits, seed.SourceCommits) || projected.EvaluatedImageDigest != seed.EvaluatedImageDigest {
		t.Fatal("qualification context dropped frozen authority or inherited candidate-specific provenance")
	}
	sources, err := winnerMultipassQualificationSources()
	if err != nil {
		t.Fatal(err)
	}
	requests, err := currentQgQualificationRequests(projected, request.WorkloadReview, sources)
	if err != nil || len(requests) != 19 {
		t.Fatal("projected instrument does not build the complete unchanged corpus", err)
	}
	for _, control := range requests {
		if control.Packet.RewardDecision != nil || control.Packet.ManifestArtifact != nil || control.Packet.ScoreArtifact != nil || control.Packet.Seal.RoundId == seed.Seal.RoundId ||
			control.Packet.BaseSha != seed.BaseSha || !reflect.DeepEqual(control.Packet.Instrument, seed.Instrument) {
			t.Fatal("synthetic control falsely inherited candidate provenance or changed frozen instrument")
		}
	}
	projected.SourceCommits["server"] = strings.Repeat("d", 40)
	projected.Instrument.ImageDigest = "synthetic-mutation"
	after, _ := json.Marshal(seed)
	if !bytes.Equal(before, after) {
		t.Fatal("qualification instrument projection mutated the authenticated seed")
	}
}

// Full independent reconstruction, not a shape-only source check, binds the
// exact instructions and native observations in the frozen qualification plan.
func TestWinnerMultipassQualificationBindsEntireReconstructedOriginalInputs(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	inputs := winnerMultipassQualificationInputs{Requests: []winnerReviewBrokerRequest{}, Expectations: []currentQgQualificationExpectation{}}
	for _, item := range plan.Cases {
		inputs.Requests = append(inputs.Requests, item.Bundle.Request)
		inputs.Expectations = append(inputs.Expectations, item.Expectation)
	}
	inputs.Requests[0].Operation = "qualification_cancel_before_provider"
	if err := validateWinnerMultipassQualificationOriginalInputs(plan, &inputs); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(inputs)
	for index, mutate := range []func(*winnerMultipassQualificationInputs){
		func(value *winnerMultipassQualificationInputs) {
			value.Requests[1].Instructions += " synthetic substituted instruction"
		},
		func(value *winnerMultipassQualificationInputs) {
			value.Requests[1].Packet.Score = []byte(`{"synthetic":true}`)
		},
		func(value *winnerMultipassQualificationInputs) {
			value.Requests[1].WorkloadReview.Addendum += " synthetic substitution"
		},
		func(value *winnerMultipassQualificationInputs) { value.Expectations[1].Decision = "approve" },
		func(value *winnerMultipassQualificationInputs) { value.Requests = value.Requests[:18] },
	} {
		var changed winnerMultipassQualificationInputs
		if err := json.Unmarshal(canonical, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if validateWinnerMultipassQualificationOriginalInputs(plan, &changed) == nil {
			t.Fatalf("substituted qualification original %d accepted", index)
		}
	}
}
