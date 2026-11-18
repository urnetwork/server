// A qualification seed is an authenticated metadata projection, never a
// reconstructed full seal or permission to reuse candidate-specific judgments.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The private seed pin authenticates the exact exported bytes. This decoder
// recognizes the export's explicit projection contract without stripping its
// marker, treating an absent inventory as a full seal, or recomputing its hash.
func decodeWinnerMultipassInstrumentSeed(content []byte) (*controller.CompetitionSourceReviewPacket, error) {
	var projection struct {
		controller.CompetitionSourceReviewPacket
		SealMetadataOnly bool `json:"seal_metadata_only"`
	}
	if err := decodeStrictJSONBytes(content, &projection, "authenticated metadata-only qualification instrument seed"); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return nil, err
	}
	// These two objects own only static snake_case fields. Dynamic maps such as
	// source policy and score context are not confused with wire field names.
	var sealFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["seal"], &sealFields); err != nil {
		return nil, err
	}
	for _, object := range []map[string]json.RawMessage{fields, sealFields} {
		for key := range object {
			if key != strings.ToLower(key) {
				return nil, errors.New("qualification seed contains a noncanonical projection field alias")
			}
		}
	}
	if !projection.SealMetadataOnly || !bytes.Equal(bytes.TrimSpace(fields["seal_metadata_only"]), []byte("true")) ||
		!bytes.Equal(bytes.TrimSpace(fields["patch"]), []byte("null")) || !bytes.Equal(bytes.TrimSpace(fields["score"]), []byte("null")) ||
		!bytes.Equal(bytes.TrimSpace(sealFields["entries"]), []byte("null")) || projection.Patch != nil || projection.Score != nil || projection.Seal.Entries != nil ||
		!validSha256(projection.Seal.SealSha256) || !validSha256(projection.Entry.PatchSha256) || !validSha256(projection.Entry.ScoreSha256) {
		return nil, errors.New("qualification seed is not the explicitly labeled metadata-only export with original sealed digests")
	}
	return &projection.CompetitionSourceReviewPacket, nil
}

// Frozen source and instrument are the only real candidate fields reused by
// synthetic controls. Archive locators, reward decisions, elected job identity
// and the candidate's patch/score/seal are deliberately not fixture provenance.
// The unchanged corpus builder supplies its own explicit synthetic identities.
func winnerMultipassQualificationInstrumentSeed(seed *controller.CompetitionSourceReviewPacket) (*controller.CompetitionSourceReviewPacket, error) {
	if seed == nil || controller.ValidateCompetitionReviewInstrument(seed) != nil {
		return nil, errors.New("qualification frozen instrument seed is unavailable")
	}
	lock, err := model.CompetitionSourceLockDigestForMode(seed.SourceCommits, seed.Seal.Staging)
	if err != nil || lock != seed.Entry.BaseSourceLockSha256 || seed.SourceCommits["server"] != seed.BaseSha {
		return nil, errors.New("qualification frozen source seed differs from its authenticated lock")
	}
	projection := &controller.CompetitionSourceReviewPacket{
		Seal: model.CompetitionEvaluationSeal{Staging: seed.Seal.Staging}, Entry: model.CompetitionSealedEntry{BaseSourceLockSha256: lock},
		BaseSha: seed.BaseSha, EvaluatedImageDigest: seed.EvaluatedImageDigest, SourceCommits: seed.SourceCommits,
		SourceGeneration: seed.SourceGeneration, ActivationGeneration: seed.ActivationGeneration, Instrument: seed.Instrument,
		WinnerReviewPolicy: model.CompetitionSnowWinnerReviewPolicy,
	}
	// Isolate nested mutable maps/pointers from the authenticated in-memory seed.
	content, err := json.Marshal(projection)
	if err != nil {
		return nil, err
	}
	var isolated controller.CompetitionSourceReviewPacket
	if err := decodeStrictJSONBytes(content, &isolated, "isolated synthetic qualification instrument context"); err != nil {
		return nil, err
	}
	return &isolated, nil
}
