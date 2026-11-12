// Historical canonical bytes are checked against frozen pre-extension structs,
// not a new-type round trip that could conceal wire-tag or field-order drift.
package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Frozen pre-extension field order, types and wire tags; never regenerate from current types.
type historicalCompetitionRecoveryAuthorization struct {
	Schema                       int                                                           `json:"schema"`
	Kind                         string                                                        `json:"kind"`
	IncidentClass                string                                                        `json:"incident_class"`
	Environment                  string                                                        `json:"environment"`
	CompetitionId                string                                                        `json:"competition_id"`
	SeasonId                     string                                                        `json:"season_id"`
	JobId                        server.Id                                                     `json:"job_id"`
	RoundId                      server.Id                                                     `json:"round_id"`
	ExpectedCycle                int                                                           `json:"expected_cycle"`
	ExpectedAttemptCount         int                                                           `json:"expected_attempt_count"`
	ExpectedTerminalSnapshot     json.RawMessage                                               `json:"expected_terminal_snapshot"`
	ExpectedTerminalSha256       string                                                        `json:"expected_terminal_sha256"`
	EvidenceSha256               string                                                        `json:"evidence_sha256"`
	IncidentEventId              int64                                                         `json:"incident_event_id"`
	IncidentEventSha256          string                                                        `json:"incident_event_sha256"`
	IncidentAttempt              int                                                           `json:"incident_attempt"`
	FailureManifestSha256        string                                                        `json:"failure_manifest_sha256"`
	RejectedArtifactPath         string                                                        `json:"rejected_artifact_path"`
	RoundPolicySha256            string                                                        `json:"round_policy_sha256"`
	BaselineSha256               string                                                        `json:"baseline_sha256"`
	RuntimeQualificationSha256   string                                                        `json:"runtime_qualification_sha256"`
	FixedSourceCommit            string                                                        `json:"fixed_source_commit"`
	FixedWorkerSha256            string                                                        `json:"fixed_worker_sha256"`
	FixedWorkerImageDigest       string                                                        `json:"fixed_worker_image_digest"`
	ApiImageDigest               string                                                        `json:"api_image_digest"`
	AuthorizedAt                 time.Time                                                     `json:"authorized_at"`
	ExpiresAt                    time.Time                                                     `json:"expires_at"`
	SingleUse                    bool                                                          `json:"single_use"`
	PostCloseApproval            *model.CompetitionJobReevaluationPostCloseApproval            `json:"post_close_approval,omitempty"`
	BackingServiceEvidence       *model.CompetitionJobReevaluationBackingServiceEvidence       `json:"backing_service_evidence,omitempty"`
	OperatorInterruptionEvidence *model.CompetitionJobReevaluationOperatorInterruptionEvidence `json:"operator_interruption_evidence,omitempty"`
}

// Frozen pre-extension field order, types and wire tags; never regenerate from current types.
type historicalCompetitionRecoveryCycle struct {
	Schema                     int                                `json:"schema"`
	Environment                string                             `json:"environment"`
	CompetitionId              string                             `json:"competition_id"`
	SeasonId                   string                             `json:"season_id"`
	JobId                      server.Id                          `json:"job_id"`
	RoundId                    server.Id                          `json:"round_id"`
	PatchSha256                string                             `json:"patch_sha256"`
	RoundPolicySha256          string                             `json:"round_policy_sha256"`
	BaselineSha256             string                             `json:"baseline_sha256"`
	Cycle                      int                                `json:"cycle"`
	ReceiptSha256              string                             `json:"receipt_sha256"`
	AuthorizationSha256        string                             `json:"authorization_sha256"`
	EvidenceSha256             string                             `json:"evidence_sha256"`
	PreviousTerminalSha256     string                             `json:"previous_terminal_sha256"`
	PreviousFailure            model.CompetitionEvaluationFailure `json:"previous_failure"`
	IncidentClass              string                             `json:"incident_class,omitempty"`
	RuntimeQualificationSha256 string                             `json:"runtime_qualification_sha256"`
	FixedSourceCommit          string                             `json:"fixed_source_commit"`
	FixedWorkerSha256          string                             `json:"fixed_worker_sha256"`
	FixedWorkerImageDigest     string                             `json:"fixed_worker_image_digest"`
	ApiImageDigest             string                             `json:"api_image_digest"`
	AuthorizedAt               time.Time                          `json:"authorized_at"`
	StartedAt                  *time.Time                         `json:"started_at,omitempty"`
	ExecutionTimeoutSeconds    int                                `json:"execution_timeout_seconds"`
}

// Frozen pre-extension field order, types and wire tags; never regenerate from current types.
type historicalCompetitionRecoveryReceipt struct {
	historicalCompetitionRecoveryCycle
	RequestSha256 string `json:"request_sha256"`
	AttemptOffset int    `json:"attempt_offset"`
}

// All values come from synthetic fixtures. A new tag, reordered field, omitted
// old field or unexpectedly serialized extension changes these exact bytes.
func TestCompetitionDistributedRecoveryPreservesHistoricalCanonicalBytes(t *testing.T) {
	_, _, request := newCompetitionLegacyArchiveRequestFixture(t)
	cycle := newCompetitionReevaluationCycleTestFixture(t)
	receipt := model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: cycle, RequestSha256: strings.Repeat("c", 64), AttemptOffset: 2}
	for _, entry := range []struct {
		name       string
		current    any
		historical any
	}{
		{name: "authorization", current: request.Authorization, historical: &historicalCompetitionRecoveryAuthorization{}},
		{name: "cycle", current: cycle, historical: &historicalCompetitionRecoveryCycle{}},
		{name: "receipt", current: receipt, historical: &historicalCompetitionRecoveryReceipt{}},
	} {
		current, err := json.Marshal(entry.current)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(current, entry.historical); err != nil {
			t.Fatal(err)
		}
		historical, err := json.Marshal(entry.historical)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(current, historical) {
			t.Fatalf("%s canonical bytes differ from frozen predecessor", entry.name)
		}
	}
}
