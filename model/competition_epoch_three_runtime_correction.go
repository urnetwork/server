// Defines the exact-round runtime identity transition. It does not change
// the frozen product source, scoring policy or the historical API receipt.
package model

import (
	"errors"

	"github.com/urnetwork/server"
)

// Independent approval pins the entire canonical document, not only the
// replacement fingerprint. The store must also enforce pre-attempt state.
type CompetitionEpochThreeRuntimeCorrection struct {
	Schema                       int       `json:"schema"`
	Kind                         string    `json:"kind"`
	RoundId                      server.Id `json:"round_id"`
	SourceGeneration             int64     `json:"source_activation_generation"`
	SourceIntentSha256           string    `json:"source_intent_sha256"`
	SourceLockSha256             string    `json:"source_lock_sha256"`
	EvaluatorImageDigest         string    `json:"evaluator_image_digest"`
	PreviousQualificationSha256  string    `json:"previous_qualification_sha256"`
	WorkerQualificationSha256    string    `json:"worker_qualification_sha256"`
	WorkerImageDigest            string    `json:"worker_image_digest"`
	HostContainmentReceiptSha256 string    `json:"host_containment_receipt_sha256"`
}

// A live replacement binds the exact original intent and genuine replacement
// evidence. The caller must obtain the approval digest independently.
func ValidateCompetitionEpochThreeRuntimeCorrection(value CompetitionEpochThreeRuntimeCorrection, intent *CompetitionSourceActivation, approvedSha256 string) error {
	if intent == nil || intent.Staging || intent.Environment != "main" || intent.TargetEpoch != 3 || intent.Generation != 3 ||
		intent.AbandonedAt != nil || value.Schema != 1 || value.Kind != "epoch-three-runtime-identity-correction/v1" ||
		value.RoundId.String() != "01a1f4d0-47c9-ac2d-ce45-55ef159a9b41" || value.RoundId != intent.TargetRoundId ||
		value.SourceGeneration != intent.Generation || value.SourceIntentSha256 != intent.IntentSha256 ||
		value.SourceLockSha256 != intent.SourceLockSha256 || value.EvaluatorImageDigest != intent.EvaluatorImageDigest ||
		value.PreviousQualificationSha256 != intent.HostQualificationSha256 || value.WorkerQualificationSha256 == value.PreviousQualificationSha256 {
		return errors.New("runtime correction does not bind the exact epoch-three source")
	}
	for _, digest := range []string{value.SourceIntentSha256, value.SourceLockSha256, value.PreviousQualificationSha256, value.WorkerQualificationSha256, value.HostContainmentReceiptSha256, approvedSha256} {
		if !competitionAutomationSha256.MatchString(digest) {
			return errors.New("runtime correction has an invalid digest")
		}
	}
	for _, digest := range []string{value.EvaluatorImageDigest, value.WorkerImageDigest} {
		if len(digest) != 71 || digest[:7] != "sha256:" || !competitionAutomationSha256.MatchString(digest[7:]) {
			return errors.New("runtime correction has an invalid image digest")
		}
	}
	actual, err := CompetitionAutomationDigest(value)
	if err != nil || actual != approvedSha256 {
		return errors.New("runtime correction differs from the independent approval")
	}
	return nil
}
