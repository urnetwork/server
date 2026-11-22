// The frozen source and independently approved runtime bytes must both match.
package model

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Neither replay on another round nor a partially replaced host is valid.
func TestEpochThreeRuntimeCorrectionRequiresExactSourceAndApproval(t *testing.T) {
	digest := func(letter string) string { return strings.Repeat(letter, 64) }
	intent := CompetitionSourceActivation{
		Environment: "main", Generation: 3, TargetEpoch: 3,
		TargetRoundId: server.RequireParseId("01a1f4d0-47c9-ac2d-ce45-55ef159a9b41"),
		IntentSha256:  digest("a"), SourceLockSha256: digest("b"),
		EvaluatorImageDigest: "sha256:" + digest("c"), HostQualificationSha256: digest("d"),
	}
	value := CompetitionEpochThreeRuntimeCorrection{
		Schema: 1, Kind: "epoch-three-runtime-identity-correction/v1", RoundId: intent.TargetRoundId,
		SourceGeneration: intent.Generation, SourceIntentSha256: intent.IntentSha256,
		SourceLockSha256: intent.SourceLockSha256, EvaluatorImageDigest: intent.EvaluatorImageDigest,
		PreviousQualificationSha256: intent.HostQualificationSha256,
		WorkerQualificationSha256:   digest("e"), WorkerImageDigest: "sha256:" + digest("f"),
		HostContainmentReceiptSha256: digest("1"),
	}
	approval, err := CompetitionAutomationDigest(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompetitionEpochThreeRuntimeCorrection(value, &intent, approval); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CompetitionEpochThreeRuntimeCorrection, *CompetitionSourceActivation, *string)
	}{
		{"no approval", func(_ *CompetitionEpochThreeRuntimeCorrection, _ *CompetitionSourceActivation, pin *string) {
			*pin = ""
		}},
		{"staging", func(_ *CompetitionEpochThreeRuntimeCorrection, source *CompetitionSourceActivation, _ *string) {
			source.Staging = true
		}},
		{"different round", func(_ *CompetitionEpochThreeRuntimeCorrection, source *CompetitionSourceActivation, _ *string) {
			source.TargetRoundId = server.Id{}
		}},
		{"different image", func(_ *CompetitionEpochThreeRuntimeCorrection, source *CompetitionSourceActivation, _ *string) {
			source.EvaluatorImageDigest = "sha256:" + digest("0")
		}},
		{"different worker", func(candidate *CompetitionEpochThreeRuntimeCorrection, _ *CompetitionSourceActivation, _ *string) {
			candidate.WorkerImageDigest = "sha256:" + digest("0")
		}},
		{"different containment receipt", func(candidate *CompetitionEpochThreeRuntimeCorrection, _ *CompetitionSourceActivation, _ *string) {
			candidate.HostContainmentReceiptSha256 = digest("0")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, source, pin := value, intent, approval
			tc.mutate(&candidate, &source, &pin)
			if err := ValidateCompetitionEpochThreeRuntimeCorrection(candidate, &source, pin); err == nil {
				t.Fatal("accepted an unapproved runtime correction")
			}
		})
	}
}
