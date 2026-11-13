// Separate distributed terminal-infrastructure authority never borrows a
// singleton incident, slot or runtime qualification. Observation sends only
// authenticated availability and exact old-run status, never start or cancel.
package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// The production caller always supplies the pinned fixed-command transport.
// Synthetic signed transports exercise the same validators without host RPCs.
type competitionDistributedRecoveryTransportFactory func(*Settings, CompetitionExecutorHostPolicy) (competitionAgentTransport, error)

// A native observation may not survive an unbounded final database wait even
// with unchanged fences. Nonpositive lifetime refuses new grants.
type competitionDistributedReevaluationSettings struct {
	NativeProofLifetime time.Duration
}

// Each recovery owner receives its own operational proof-age budget.
func defaultCompetitionDistributedReevaluationSettings() competitionDistributedReevaluationSettings {
	return competitionDistributedReevaluationSettings{NativeProofLifetime: time.Minute}
}

// Both boundaries come from actual database clock reads, not an operator's
// timestamps or the process clock. Backward/future cuts fail closed.
func competitionDistributedRecoveryProofTimely(started, completed time.Time, lifetime time.Duration) bool {
	return lifetime > 0 && !started.IsZero() && !completed.Before(started) && completed.Before(started.Add(lifetime))
}

// Native inventories stay in the private immutable audit. Only its digest is
// copied into the authenticated owner projection of the paid submission.
type competitionDistributedRecoveryProof struct {
	Schema                   int                                                `json:"schema"`
	ObservedAt               time.Time                                          `json:"observed_at"`
	DatabaseBracketStartedAt time.Time                                          `json:"database_bracket_started_at"`
	DatabaseBracketExpiresAt time.Time                                          `json:"database_bracket_expires_at"`
	AuthorizationSha256      string                                             `json:"authorization_sha256"`
	OriginalTerminalSha256   string                                             `json:"original_terminal_sha256"`
	OriginalStatus           competitionagent.Response                          `json:"original_status"`
	Availability             []competitionagent.Response                        `json:"availability"`
	IdleSlots                []model.CompetitionDistributedReevaluationIdleSlot `json:"idle_slots"`
}

// A different capability and exact incident class are mandatory even when the
// same compiled program also understands historical singleton recoveries.
func bindCompetitionDistributedReevaluationRuntime(settings *Settings, proof model.CompetitionJobReevaluationRuntime, digest string) error {
	if settings == nil || proof.Schema != 2 || proof.Kind != model.CompetitionDistributedJobReevaluationRuntimeKind ||
		proof.Capability != model.CompetitionDistributedJobReevaluationCapability || !proof.Accepted || !proof.ApiCompatible || !proof.WorkerCompatible ||
		len(proof.IncidentClasses) != 1 || proof.IncidentClasses[0] != model.CompetitionJobReevaluationDistributedInfrastructure ||
		!gitShaPattern.MatchString(proof.SourceCommit) || !imageDigestPattern.MatchString(proof.ApiImageDigest) ||
		!imageDigestPattern.MatchString(proof.WorkerImageDigest) || !sha256Pattern.MatchString(proof.WorkerSha256) ||
		!sha256Pattern.MatchString(proof.EvidenceSha256) || !sha256Pattern.MatchString(digest) {
		return ErrConflict
	}
	actual, err := model.CompetitionAutomationDigest(proof)
	if err != nil || actual != digest {
		return ErrConflict
	}
	copy := proof
	copy.IncidentClasses = append([]string(nil), proof.IncidentClasses...)
	settings.reevaluationRuntime, settings.reevaluationRuntimeSha256 = &copy, digest
	return nil
}

// Shape validation does not depend on mutable deployment state, so exact
// committed replay remains possible after approval expiration or rotation.
func validateCompetitionDistributedRecoveryRequest(settings *Settings, principal *Principal, request model.CompetitionJobReevaluationRequest) error {
	a := request.Authorization
	d := a.Distributed
	if settings == nil || !settings.AutomationPolicy.Enabled ||
		(!competitionPrincipalAllows(principal, competitionSchedulerRole, settings.CompetitionId, settings.AutomationPolicy) &&
			!competitionPrincipalAllows(principal, "operator", settings.CompetitionId, settings.AutomationPolicy)) {
		return ErrAutomationDisabled
	}
	if a.Schema != 2 || a.Kind != model.CompetitionDistributedJobReevaluationAuthorizationKind || a.IncidentClass != model.CompetitionJobReevaluationDistributedInfrastructure || d == nil || d.Schema != 1 ||
		!tokenNamePattern.MatchString(request.IdempotencyKey) || !sha256Pattern.MatchString(request.AuthorizationSha256) ||
		a.Environment != settings.AutomationPolicy.Environment || a.CompetitionId != settings.CompetitionId || a.SeasonId != settings.AutomationPolicy.SeasonId ||
		a.JobId == (server.Id{}) || a.RoundId == (server.Id{}) || a.ExpectedCycle != 0 || a.ExpectedAttemptCount < 1 || !a.SingleUse ||
		a.AuthorizedAt.IsZero() || !a.AuthorizedAt.Before(a.ExpiresAt) || !sha256Pattern.MatchString(a.EvidenceSha256) ||
		!sha256Pattern.MatchString(a.ExpectedTerminalSha256) || a.IncidentEventId < 1 || !sha256Pattern.MatchString(a.IncidentEventSha256) ||
		a.IncidentAttempt != a.ExpectedAttemptCount || a.FailureManifestSha256 != "" || a.RejectedArtifactPath != "" ||
		a.BackingServiceEvidence != nil || a.OperatorInterruptionEvidence != nil || !sha256Pattern.MatchString(a.RoundPolicySha256) || !sha256Pattern.MatchString(a.BaselineSha256) ||
		(d.Reason != model.CompetitionDistributedReevaluationRetentionBudget && d.Reason != model.CompetitionDistributedReevaluationLegacyPrelaunch) ||
		!tokenNamePattern.MatchString(d.OriginalHostId) || d.OriginalSlotId < 1 || d.OriginalFencingToken < 1 || d.NativeFencingToken < 1 ||
		d.NativeFencingToken > d.OriginalFencingToken || d.OriginalRunId != competitionagent.RunId(a.JobId.String(), 0, a.ExpectedAttemptCount) ||
		!sha256Pattern.MatchString(d.OriginalManifestSha256) || !sha256Pattern.MatchString(d.OriginalRequestSha256) ||
		d.OriginalStartedAt.IsZero() || !d.OriginalDeadlineAt.Equal(d.OriginalStartedAt.Add(3*time.Hour)) ||
		!sha256Pattern.MatchString(d.SourceLockSha256) || !imageDigestPattern.MatchString(d.EvaluatorImageDigest) ||
		!sha256Pattern.MatchString(d.PolicyApprovalSha256) || !sha256Pattern.MatchString(d.ClosureEvidenceSha256) ||
		!tokenNamePattern.MatchString(d.SupervisorOwnerId) || d.SupervisorFencingToken < 1 || len(d.IdleSlots) < 2 || 16 < len(d.IdleSlots) ||
		validateCompetitionReevaluationPostCloseApproval(a) != nil {
		return ErrConflict
	}
	seenHosts, seenSlots := map[string]bool{}, map[int]bool{}
	for index, slot := range d.IdleSlots {
		if !tokenNamePattern.MatchString(slot.HostId) || slot.SlotId < 1 || slot.FencingToken < 0 || slot.FencingToken == int64(^uint64(0)>>1) ||
			seenHosts[slot.HostId] || seenSlots[slot.SlotId] || index > 0 && slot.SlotId <= d.IdleSlots[index-1].SlotId {
			return ErrConflict
		}
		seenHosts[slot.HostId], seenSlots[slot.SlotId] = true, true
	}
	digest, err := model.CompetitionAutomationDigest(a)
	if err != nil || digest != request.AuthorizationSha256 {
		return ErrConflict
	}
	terminalDigest, err := CompetitionJobReevaluationSnapshotDigest(a.ExpectedTerminalSnapshot)
	if err != nil || terminalDigest != a.ExpectedTerminalSha256 {
		return ErrConflict
	}
	return nil
}

// Every envelope is verified again here even though production transport also
// authenticates it. Neither unsigned fixtures nor another frozen host suffice.
func validateCompetitionDistributedRecoveryEnvelope(response competitionagent.Response, host CompetitionExecutorHostPolicy) error {
	key, err := base64.StdEncoding.DecodeString(host.AgentPublicKeyBase64)
	if err != nil || len(key) != ed25519.PublicKeySize || competitionagent.VerifyResponse(response, key) != nil || response.Schema != competitionagent.Schema || response.HostId != host.HostId {
		return ErrConflict
	}
	return nil
}

// A terminal native result is insufficient without the independent all-host
// availability observations and the database's held, empty, unchanged fences.
func observeCompetitionDistributedRecovery(ctx context.Context, settings *Settings, round *roundRecord, request model.CompetitionJobReevaluationRequest, transportForHost competitionDistributedRecoveryTransportFactory) (*competitionDistributedRecoveryProof, error) {
	a, d := request.Authorization, request.Authorization.Distributed
	proof := &competitionDistributedRecoveryProof{Schema: 1, AuthorizationSha256: request.AuthorizationSha256, OriginalTerminalSha256: a.ExpectedTerminalSha256,
		IdleSlots: append([]model.CompetitionDistributedReevaluationIdleSlot(nil), d.IdleSlots...)}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		transport, err := transportForHost(settings, host)
		if err != nil {
			return nil, err
		}
		if host.HostId == d.OriginalHostId {
			response, err := transport.Exchange(ctx, competitionagent.Request{Operation: "status", RunId: d.OriginalRunId, FencingToken: d.NativeFencingToken}, "", "")
			if err != nil || validateCompetitionDistributedRecoveryEnvelope(response, host) != nil || response.RunId != d.OriginalRunId || response.FencingToken != d.NativeFencingToken ||
				response.Identity == nil || response.Identity.HostId != d.OriginalHostId || response.Identity.JobId != a.JobId.String() || response.Identity.RoundId != a.RoundId.String() ||
				response.Identity.RunId != d.OriginalRunId || response.Identity.Cycle != 0 || response.Identity.Attempt != a.ExpectedAttemptCount ||
				response.Identity.RoundEpoch != round.Epoch || response.Identity.SourceEpoch != evaluationSourceEpoch(round) ||
				!response.Identity.StartedAt.Equal(d.OriginalStartedAt) || !response.Identity.DeadlineAt.Equal(d.OriginalDeadlineAt) ||
				response.ManifestSha256 != d.OriginalManifestSha256 || response.RequestSha256 != d.OriginalRequestSha256 || len(response.SelfCheck) != 0 {
				return nil, ErrConflict
			}
			if d.Reason == model.CompetitionDistributedReevaluationRetentionBudget && (response.State != "succeeded" || response.ExitCode != 0 || response.ErrorCode != "") ||
				d.Reason == model.CompetitionDistributedReevaluationLegacyPrelaunch && (response.State != "failed" || response.ExitCode != -1 || response.ErrorCode != "legacy_prelaunch_infrastructure_failure") {
				return nil, ErrConflict
			}
			proof.OriginalStatus = response
		}
		response, err := transport.Exchange(ctx, competitionagent.Request{Operation: "availability"}, "", "")
		if err != nil || validateCompetitionDistributedRecoveryEnvelope(response, host) != nil || response.State != "available" || response.RunId != "" || response.FencingToken != 0 ||
			response.Identity != nil || response.ManifestSha256 != "" || response.RequestSha256 != "" || len(response.Files) != 0 || len(response.SelfCheck) != 0 || response.ExitCode != 0 || response.ErrorCode != "" {
			return nil, ErrLeaseLost
		}
		proof.Availability = append(proof.Availability, response)
	}
	if ctx.Err() != nil || proof.OriginalStatus.Identity == nil {
		return nil, ErrLeaseLost
	}
	proof.ObservedAt = server.NowUtc()
	return proof, nil
}

// Stored private proof must remain bound to the public immutable digest on
// receipt replay. It is never exposed by the authenticated owner job projection.
func validateCompetitionDistributedRecoveryReceipt(receipt *model.CompetitionJobReevaluationReceipt) error {
	if receipt.Schema == 1 {
		if len(receipt.DistributedProof) != 0 {
			return ErrConflict
		}
		return nil
	}
	if receipt.Schema != 2 || receipt.Distributed == nil || len(receipt.DistributedProof) == 0 {
		return ErrConflict
	}
	var proof competitionDistributedRecoveryProof
	if decodeFailureEvidenceJson(receipt.DistributedProof, &proof) != nil || proof.Schema != 1 || proof.ObservedAt.IsZero() ||
		proof.DatabaseBracketStartedAt.IsZero() || !proof.DatabaseBracketStartedAt.Before(proof.DatabaseBracketExpiresAt) ||
		receipt.AuthorizedAt.Before(proof.DatabaseBracketStartedAt) || !receipt.AuthorizedAt.Before(proof.DatabaseBracketExpiresAt) ||
		proof.AuthorizationSha256 != receipt.AuthorizationSha256 || proof.OriginalTerminalSha256 != receipt.PreviousTerminalSha256 {
		return ErrConflict
	}
	digest, err := model.CompetitionAutomationDigest(proof)
	if err != nil || digest != receipt.Distributed.NativeProofSha256 {
		return ErrConflict
	}
	return nil
}

// Canonical typed proof bytes, rather than PostgreSQL JSONB textual order,
// authenticate the separately retained native observations.
func competitionDistributedRecoveryProofJson(proof *competitionDistributedRecoveryProof) (json.RawMessage, string, error) {
	digest, err := model.CompetitionAutomationDigest(proof)
	if err != nil {
		return nil, "", err
	}
	content, err := json.Marshal(proof)
	return content, digest, err
}
