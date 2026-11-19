// Baseline reservations must distinguish corrected worker authority from the
// original frozen source, without modifying the stored policy or old digests.
package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The image and effective producer identity are bound to the reservation.
func TestEpochThreeRuntimeCorrectionBindsBaselineReservation(t *testing.T) {
	digest := func(c string) string { return strings.Repeat(c, 64) }
	roundId := server.RequireParseId("01a1f4d0-47c9-ac2d-ce45-55ef159a9b41")
	settings := &Settings{EvaluatorImageDigest: "sha256:" + digest("a")}
	settings.AutomationPolicy.SourceActivationGeneration = 3
	settings.AutomationPolicy.SourceLockSha256 = digest("b")
	settings.EvaluationPolicy.HostQualificationSha256 = digest("c")
	job := &queuedJob{}
	job.JobId, job.RoundId, job.Round.RoundId = server.NewId(), roundId, roundId
	job.Round.PolicyJson = json.RawMessage(`{"frozen":true}`)
	job.WorkerImageDigest = "sha256:" + digest("d")
	original, err := roundBaselinePreparationContext(settings, job)
	if err != nil {
		t.Fatal(err)
	}
	settings.epochThreeRuntimeCorrection = &model.CompetitionEpochThreeRuntimeCorrection{
		RoundId: roundId, SourceGeneration: 3, SourceLockSha256: digest("b"), EvaluatorImageDigest: settings.EvaluatorImageDigest,
		PreviousQualificationSha256: digest("c"), WorkerQualificationSha256: digest("e"), WorkerImageDigest: job.WorkerImageDigest,
	}
	corrected, err := roundBaselinePreparationContext(settings, job)
	if err != nil || corrected == original || settings.EvaluationPolicy.HostQualificationSha256 != digest("c") {
		t.Fatalf("runtime correction failed to bind reservation without changing policy: %v", err)
	}
	job.WorkerImageDigest = "sha256:" + digest("f")
	if _, err := roundBaselinePreparationContext(settings, job); err == nil {
		t.Fatal("a different worker image obtained a corrected baseline reservation")
	}
	if value := competitionBaselineQualification(settings, server.NewId()); value != digest("c") {
		t.Fatal("runtime correction escaped its original round")
	}
}
