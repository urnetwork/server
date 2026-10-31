// Exact initial or separately approved calibration-maintenance authority may
// queue authenticated work while grading stays held. Neither fabricates host
// heartbeats or changes frozen policy, activation, evaluation or publication.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// These are content-addressed authority facts, not a general hold feature.
// A per-service value also permits entirely synthetic test authorities.
type competitionIntakeAuthority struct {
	Environment, CompetitionId, SeasonId, RoundId string
	PolicySha256, OwnerSha256                     string
	BaseSha, ImageDigest, SourceLockSha256        string
	HardwareId, HostQualificationSha256           string
	EvaluationHoldId, EvaluationRequestSha256     string
	RewardHoldId, RewardRequestSha256             string
}

// The 2026-10-29 replacement approval already requires these exact initial
// holds and forbids grading until deferred gates pass. No new policy is made.
func approvedCompetitionIntakeAuthority() competitionIntakeAuthority {
	return competitionIntakeAuthority{
		Environment: "main", CompetitionId: "sim-latency-season-1", SeasonId: "sim-latency-production-20261028-r2",
		RoundId:          "01a1855d-49bd-0250-90db-d0c0885f0e46",
		PolicySha256:     "0a094d2b799f5cc546b28de9ee7fe24db88cc0d70a68a189d2404807f0d0d5c6",
		OwnerSha256:      "2a5e8fcf301b12cd6f9983e8ffb753804d31a9e863097c7368a2f85d236ef3ad",
		BaseSha:          "a9e8e1a710b24c749e6499a0606938252c83e136",
		ImageDigest:      "sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2",
		SourceLockSha256: "ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf",
		HardwareId:       "sille-macpro6-1-single-host", HostQualificationSha256: "08ee3a5a4acf0f4b8fabf6279d4257bd9fdc3314ffb9f59e87f2981556cb300d",
		EvaluationHoldId: "01a187b3-a624-1bb6-f139-ed91584cc93c", EvaluationRequestSha256: "f69c1cb7492d4a5dfa169af0d510cb6b0320d891d240917dc4ae63da86e05961",
		RewardHoldId: "01a187b3-a6fb-0166-40f4-d4604e3f92f2", RewardRequestSha256: "a1d1b91d62bfa16bda8a2d37890e3417645866a33ea17524669378ceb765be33",
	}
}

// This unexported per-request permit is never serialized into policy or config.
type competitionIntakeAdmission struct {
	Authority            competitionIntakeAuthority
	MaintenanceAuthority *competitionCalibrationMaintenanceAuthority
	SourceIntentSha256   string
}

// Existing pooled connections and caller-owned transactions share only this
// read capability; no nested connection or transaction is acquired.
type competitionIntakeReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Reads current revisions, not idempotent historical responses or cached booleans.
func readCompetitionIntakeHolds(ctx context.Context, reader competitionIntakeReader, settings *Settings) (competitionAutomationHoldStatus, error) {
	var content []byte
	err := reader.QueryRow(ctx, `SELECT COALESCE(jsonb_object_agg(kind,jsonb_build_object('active',active,'revision',revision,'hold_id',hold_id,
 'created_at',created_at AT TIME ZONE 'UTC','idempotency_key',idempotency_key,'request_sha256',request_sha256,'hold',hold_json)),'{}'::jsonb)
 FROM (SELECT DISTINCT ON (kind) kind,active,revision,hold_id,created_at,idempotency_key,request_sha256,hold_json FROM competition_automation_hold
 WHERE environment=$1 AND competition_id=$2 AND season_id=$3
 AND kind IN ('admission','evaluation','reward_delivery')
 ORDER BY kind,revision DESC) AS latest`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId).Scan(&content)
	if err != nil {
		return competitionAutomationHoldStatus{}, err
	}
	var records map[string]struct {
		Active         *bool           `json:"active"`
		Revision       *int            `json:"revision"`
		HoldId         string          `json:"hold_id"`
		CreatedAt      *time.Time      `json:"created_at"`
		IdempotencyKey string          `json:"idempotency_key"`
		RequestSha256  string          `json:"request_sha256"`
		Hold           json.RawMessage `json:"hold"`
	}
	if err := json.Unmarshal(content, &records); err != nil {
		return competitionAutomationHoldStatus{}, err
	}
	status := competitionAutomationHoldStatus{}
	for kind, record := range records {
		var hold model.CompetitionAutomationHold
		var required struct {
			Request struct {
				Active *bool `json:"active"`
			} `json:"request"`
		}
		if record.Active == nil || record.Revision == nil || *record.Revision < 1 || record.CreatedAt == nil ||
			json.Unmarshal(record.Hold, &hold) != nil || json.Unmarshal(record.Hold, &required) != nil || required.Request.Active == nil ||
			hold.Revision != *record.Revision || hold.Request.Active != *record.Active ||
			hold.HoldId.String() != record.HoldId || !hold.CreatedAt.Truncate(time.Microsecond).Equal(*record.CreatedAt) ||
			hold.Request.IdempotencyKey != record.IdempotencyKey || hold.RequestSha256 != record.RequestSha256 ||
			hold.Environment != settings.AutomationPolicy.Environment || hold.CompetitionId != settings.CompetitionId || hold.SeasonId != settings.AutomationPolicy.SeasonId || hold.Request.Kind != kind {
			return status, errors.New("competition hold scope does not match its durable row")
		}
		requestSha256, err := model.CompetitionAutomationDigest(hold.Request)
		if err != nil || requestSha256 != record.RequestSha256 {
			return status, errors.New("competition hold request does not match its durable digest")
		}
		// PostgreSQL owns the maintenance deadline; JSON cannot move it. Its
		// microsecond precision may truncate the original JSON nanoseconds.
		hold.CreatedAt = *record.CreatedAt
		switch kind {
		case "admission":
			status.Admission = *record.Active
		case "evaluation":
			status.Evaluation, status.EvaluationHold = *record.Active, &hold
		case "reward_delivery":
			status.RewardDelivery, status.RewardDeliveryHold = *record.Active, &hold
		default:
			return status, errors.New("unexpected competition hold capability")
		}
	}
	return status, nil
}

// Only the authenticated initial frozen round, still inside its real window,
// can use this compatibility interpretation of its existing approval.
func (self competitionIntakeAuthority) matches(settings *Settings, round *roundRecord, now time.Time) bool {
	return settings != nil && round != nil && self.PolicySha256 != "" &&
		settings.Enabled && settings.AutomationPolicy.Enabled && !settings.AutomationPolicy.Staging && !round.Staging &&
		settings.AutomationPolicy.Version == 2 && settings.AutomationPolicy.RequiredApiCapability == model.CompetitionRuntimeBlobCapability &&
		settings.AutomationPolicy.Environment == self.Environment && settings.CompetitionId == self.CompetitionId &&
		settings.AutomationPolicy.SeasonId == self.SeasonId && settings.AutomationPolicy.PolicyApprovalSha256 == self.PolicySha256 &&
		settings.AutomationPolicy.SourceGeneration == 0 && settings.AutomationPolicy.SourceActivationGeneration == 1 &&
		settings.AutomationPolicy.SourceLockSha256 == self.SourceLockSha256 && settings.BaseSha == self.BaseSha && settings.EvaluatorImageDigest == self.ImageDigest &&
		settings.EvaluationPolicy.HardwareId == self.HardwareId && settings.EvaluationPolicy.HostQualificationSha256 == self.HostQualificationSha256 &&
		round.RoundId.String() == self.RoundId && round.CompetitionId == self.CompetitionId && round.Epoch == 1 &&
		competitionRoundUsesV2(round) && storedPolicyMatches(settings, round.PolicyJson) &&
		!round.Canceled && round.FinalizedAt == nil && round.AutomationSealedAt == nil && submissionWithinEpoch(round, now)
}

// A later incident hold or release/rehold cannot resurrect the initial permit.
func (self competitionIntakeAuthority) holdsMatch(status competitionAutomationHoldStatus) bool {
	match := func(hold *model.CompetitionAutomationHold, kind, id, requestSha256 string) bool {
		return hold != nil && hold.Environment == self.Environment && hold.CompetitionId == self.CompetitionId && hold.SeasonId == self.SeasonId &&
			hold.HoldId.String() == id && hold.Revision == 1 && hold.RequestSha256 == requestSha256 &&
			hold.Request.Kind == kind && hold.Request.Active && hold.Request.ReasonCode == "OPERATOR_HOLD" && hold.Request.EvidenceSha256 == self.OwnerSha256
	}
	return !status.Admission && status.Evaluation && status.RewardDelivery &&
		match(status.EvaluationHold, "evaluation", self.EvaluationHoldId, self.EvaluationRequestSha256) &&
		match(status.RewardDeliveryHold, "reward_delivery", self.RewardHoldId, self.RewardRequestSha256)
}

// Every non-host check remains mandatory, including native blob protection.
// Unknown false checks and incomplete maps are never silently promoted.
func competitionIntakeChecksPass(checks map[string]bool) bool {
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "api_image_identity", "artifact_archive"} {
		if !checks[name] {
			return false
		}
	}
	for name, passed := range checks {
		if !passed && name != "authoritative_evaluator_host" && name != "artifact_storage" && name != "host_rebaseline" {
			return false
		}
	}
	return true
}

// Prior source-envelope and frozen-policy authentication are never bypassed.
// That immutable activated source required actual initial R1 and two-role ACKs;
// consulting a mutable online host row would recreate the intake coupling.
func (self *Service) approvedIntakeReadiness(ctx context.Context, settings *Settings, round *roundRecord, checks map[string]bool, holds competitionAutomationHoldStatus) (bool, error) {
	return self.approvedIntakeAdmission(settings, round, checks, holds, server.NowUtc()) != nil, nil
}

// Select the actual proved capability, never a boolean later converted into
// an initial-hold permit. Maintenance requires a prior authentic blob envelope.
func (self *Service) approvedIntakeAdmission(settings *Settings, round *roundRecord, checks map[string]bool, holds competitionAutomationHoldStatus, now time.Time) *competitionIntakeAdmission {
	if !competitionIntakeChecksPass(checks) {
		return nil
	}
	if self.intakeAuthority.matches(settings, round, now) && self.intakeAuthority.holdsMatch(holds) {
		return &competitionIntakeAdmission{Authority: self.intakeAuthority}
	}
	maintenance := self.calibrationMaintenanceAuthority
	if maintenance.Scope.matches(settings, round, now) && maintenance.holdsMatch(holds, now) && sha256Pattern.MatchString(settings.runtimeIntentSha256) {
		return &competitionIntakeAdmission{Authority: maintenance.Scope, MaintenanceAuthority: &maintenance, SourceIntentSha256: settings.runtimeIntentSha256}
	}
	return nil
}

// Ordinary admission remains exactly strict; only a proved exception gets a
// request-local permit, which must survive both durable admission passes.
func (self *Service) requireSecureIntake(ctx context.Context, settings *Settings, round *roundRecord) (*Settings, *CompetitionError) {
	checks, err := self.readinessChecks(ctx, settings)
	if err != nil {
		return nil, infrastructureError("not_ready", "competition intake containment is not ready")
	}
	if secureEvaluatorChecksPass(checks) {
		return settings, nil
	}
	store, ok := self.store.(competitionAutomationHoldStatusStore)
	if !ok {
		return nil, infrastructureError("not_ready", "competition intake hold authority is unavailable")
	}
	holds, err := store.AutomationHoldStatus(ctx, settings)
	if err != nil {
		return nil, infrastructureError("not_ready", "competition intake hold authority is unavailable")
	}
	admission := self.approvedIntakeAdmission(settings, round, checks, holds, server.NowUtc())
	if admission == nil {
		return nil, infrastructureError("not_ready", "competition evaluator containment is not ready")
	}
	copySettings := *settings
	copySettings.intakeAdmission = admission
	return &copySettings, nil
}

// Called under the existing automation scope lock on BOTH Enqueue passes.
// Read the latest hold records using only this transaction. The receipt time
// is shared by both passes, preserving on-time submissions across slow uploads.
func checkCompetitionIntakeAdmission(ctx context.Context, reader competitionIntakeReader, settings *Settings, round *roundRecord, receivedAt time.Time) error {
	return checkCompetitionIntakeAdmissionWithClock(ctx, reader, settings, round, receivedAt, server.NowUtc)
}

// Production always supplies the real server clock. A private clock seam lets
// deterministic tests cross maintenance expiry without altering receipt time.
func checkCompetitionIntakeAdmissionWithClock(ctx context.Context, reader competitionIntakeReader, settings *Settings, round *roundRecord, receivedAt time.Time, nowUtc func() time.Time) error {
	if settings.intakeAdmission == nil {
		return nil
	}
	authority := settings.intakeAdmission.Authority
	if !authority.matches(settings, round, receivedAt) {
		return errors.New("approved intake-only source or window changed")
	}
	holds, err := readCompetitionIntakeHolds(ctx, reader, settings)
	if err != nil {
		return errors.New("approved intake hold authority is unavailable")
	}
	if maintenance := settings.intakeAdmission.MaintenanceAuthority; maintenance != nil {
		if maintenance.Scope != authority || checkCompetitionCalibrationMaintenanceSource(ctx, reader, settings, round, settings.intakeAdmission.SourceIntentSha256) != nil ||
			!maintenance.holdsMatch(holds, nowUtc()) {
			return errors.New("approved calibration maintenance holds, source or lifetime changed")
		}
		return nil
	}
	if !authority.holdsMatch(holds) {
		return errors.New("approved initial intake holds changed")
	}
	return nil
}
