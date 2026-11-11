// A root-pinned security exception separates execution identity from the
// historical control. It never edits round policy, baseline bytes or samples.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/urnetwork/server"
)

// Only host release fingerprints change; slots, keys, resources and gates stay
// in the frozen policy. The previous fingerprint is checked after amendments.
type competitionSecurityToolchainHost struct {
	HostId                      string `json:"host_id"`
	PreviousQualificationSha256 string `json:"previous_qualification_sha256"`
	QualificationSha256         string `json:"qualification_sha256"`
}

// The closed shape authorizes only the explicitly approved epoch-two compiler
// hotfix. Configuration is authenticated by the root-pinned coordinator file;
// each archived attempt retains this complete public decision and its digest.
type competitionSecurityToolchainException struct {
	Schema                       int                                `json:"schema"`
	Kind                         string                             `json:"kind"`
	RoundId                      server.Id                          `json:"round_id"`
	Epoch                        int                                `json:"epoch"`
	BaselineSha256               string                             `json:"baseline_sha256"`
	SourceLockSha256             string                             `json:"source_lock_sha256"`
	PreviousEvaluatorImageDigest string                             `json:"previous_evaluator_image_digest"`
	EvaluatorImageDigest         string                             `json:"evaluator_image_digest"`
	PreviousInstrumentSha256     string                             `json:"previous_instrument_sha256"`
	InstrumentSha256             string                             `json:"instrument_sha256"`
	PreviousToolchain            string                             `json:"previous_toolchain"`
	Toolchain                    string                             `json:"toolchain"`
	EvidenceSha256               string                             `json:"evidence_sha256"`
	Hosts                        []competitionSecurityToolchainHost `json:"hosts"`
}

// Shape validation happens before the coordinator can load its signing key.
// No general-purpose image substitution or toolchain upgrade is permitted.
func validateCompetitionSecurityToolchainShape(value *competitionSecurityToolchainException) error {
	if value == nil {
		return nil
	}
	if value.Schema != 1 || value.Kind != "epoch2-security-toolchain-exception/v1" || value.RoundId == (server.Id{}) || value.Epoch != 2 ||
		value.PreviousToolchain != "go1.27.3" || value.Toolchain != "go1.27.4" ||
		!imageDigestPattern.MatchString(value.PreviousEvaluatorImageDigest) || !imageDigestPattern.MatchString(value.EvaluatorImageDigest) || value.PreviousEvaluatorImageDigest == value.EvaluatorImageDigest ||
		!sha256Pattern.MatchString(value.BaselineSha256) || !sha256Pattern.MatchString(value.SourceLockSha256) || !sha256Pattern.MatchString(value.EvidenceSha256) ||
		!sha256Pattern.MatchString(value.PreviousInstrumentSha256) || !sha256Pattern.MatchString(value.InstrumentSha256) || value.PreviousInstrumentSha256 == value.InstrumentSha256 || len(value.Hosts) == 0 {
		return errors.New("security toolchain exception is not the approved closed compiler transition")
	}
	seen := map[string]bool{}
	for _, host := range value.Hosts {
		if !workerIdPattern.MatchString(host.HostId) || seen[host.HostId] || !sha256Pattern.MatchString(host.PreviousQualificationSha256) || !sha256Pattern.MatchString(host.QualificationSha256) || host.PreviousQualificationSha256 == host.QualificationSha256 {
			return errors.New("security toolchain exception omitted a unique replaced host identity")
		}
		seen[host.HostId] = true
	}
	return nil
}

// Scope is rechecked against authenticated historical control, never inferred
// from a new compiler's self-report. Sampling is not an exception side effect.
func validateCompetitionSecurityToolchainScope(settings *Settings, round *roundRecord, baseline *roundBaselineRecord) error {
	if settings == nil {
		return errors.New("security toolchain scope is missing settings")
	}
	value := settings.securityToolchainException
	if value == nil {
		return nil
	}
	if err := validateCompetitionSecurityToolchainShape(value); err != nil {
		return err
	}
	if round == nil || baseline == nil || round.Staging || round.Epoch != value.Epoch || round.RoundId != value.RoundId || settings.automationRoundId != value.RoundId ||
		settings.DistributedEvaluationPolicy == nil || len(settings.DistributedEvaluationPolicy.Hosts) != len(value.Hosts) ||
		settings.AutomationPolicy.SourceLockSha256 != value.SourceLockSha256 || settings.EvaluatorImageDigest != value.PreviousEvaluatorImageDigest ||
		settings.EvaluationPolicy.SimulatorSha256 != value.PreviousInstrumentSha256 || settings.EvaluationPolicy.ScorerSha256 != value.PreviousInstrumentSha256 ||
		baseline.Sha256 != value.BaselineSha256 || validateRoundBaselineRecord(settings, round, baseline) != nil {
		return errors.New("security toolchain exception differs from its exact frozen round and retained control")
	}
	for index, previous := range settings.DistributedEvaluationPolicy.Hosts {
		previous = competitionExecutorControlHost(settings, previous)
		host := value.Hosts[index]
		if previous.HostId != host.HostId || previous.QualificationSha256 != host.PreviousQualificationSha256 {
			return errors.New("security toolchain exception changed membership or prior host evidence")
		}
	}
	return nil
}

// Only selected private executors use the replacement. The independent
// baseline producer and historical receipt validators retain original pins.
func competitionExecutionSecurityToolchain(settings *Settings) *competitionSecurityToolchainException {
	if settings != nil && settings.securityToolchainException != nil && competitionDistributedEnabled(settings) &&
		settings.securityToolchainException.RoundId == settings.automationRoundId && settings.securityToolchainException.Epoch == settings.distributedRoundEpoch {
		return settings.securityToolchainException
	}
	return nil
}

// Frozen image provenance remains unchanged in policy and baseline records.
func competitionExecutionImageDigest(settings *Settings) string {
	if value := competitionExecutionSecurityToolchain(settings); value != nil {
		return value.EvaluatorImageDigest
	}
	return settings.EvaluatorImageDigest
}

// Runtime containment must authenticate the new release fingerprint rather
// than claiming the old image's host proof was measured on the hotfix image.
func competitionExecutionQualificationSha256(settings *Settings, previous string) string {
	if value := competitionExecutionSecurityToolchain(settings); value != nil {
		for _, host := range value.Hosts {
			if host.HostId == settings.executorHostId && host.PreviousQualificationSha256 == previous {
				return host.QualificationSha256
			}
		}
		return ""
	}
	return previous
}

// Canonical exception bytes are public evidence, never a request containing
// a seed or key. The digest is stable across private deployment-file layouts.
func competitionSecurityToolchainDigest(value *competitionSecurityToolchainException) (string, error) {
	if err := validateCompetitionSecurityToolchainShape(value); err != nil {
		return "", err
	}
	if value == nil {
		return "", nil
	}
	content, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

// Historical attempts omit the additive execution field entirely.
func competitionSecurityToolchainExecutionDigest(settings *Settings) string {
	if value := competitionExecutionSecurityToolchain(settings); value != nil {
		return value.EvaluatorImageDigest
	}
	return ""
}

// The archive names both the frozen control image and actual execution image.
// A candidate cannot add, omit or substitute a security exception in evidence.
func validateCompetitionSecurityToolchainArchive(settings *Settings, job *queuedJob, manifest artifactManifest) error {
	expected := competitionExecutionSecurityToolchain(settings)
	if expected == nil {
		if manifest.SecurityToolchainException != nil || manifest.ExecutionEvaluatorImageDigest != "" {
			return errors.New("unapproved security toolchain identity in attempt archive")
		}
		return nil
	}
	if job == nil || validateCompetitionSecurityToolchainScope(settings, &job.Round, job.RoundBaseline) != nil || manifest.ExecutionEvaluatorImageDigest != expected.EvaluatorImageDigest {
		return errors.New("security toolchain archive lost its exact execution scope")
	}
	expectedDigest, err := competitionSecurityToolchainDigest(expected)
	actualDigest, actualErr := competitionSecurityToolchainDigest(manifest.SecurityToolchainException)
	if err != nil || actualErr != nil || expectedDigest == "" || expectedDigest != actualDigest {
		return errors.New("security toolchain archive does not retain the approved decision")
	}
	return nil
}

// One append-only existing job-event anchors the public hotfix decision before
// any claim. It is attached to the control's source job, not a new submission.
type competitionSecurityToolchainDecision struct {
	Kind            string                                `json:"kind"`
	ExceptionSha256 string                                `json:"exception_sha256"`
	Exception       competitionSecurityToolchainException `json:"exception"`
}

// A bounded indexed read authenticates canonical typed bytes, not JSONB text.
// Missing authority is distinct from a conflicting or malformed decision.
func competitionSecurityToolchainDecisionInTx(ctx context.Context, tx server.PgTx, baseline *roundBaselineRecord) (*competitionSecurityToolchainDecision, error) {
	rows, err := tx.Query(ctx, `SELECT payload_json,payload_sha256 FROM competition_job_event
	 WHERE job_id=$1 AND event_type='security_toolchain_exception' ORDER BY event_id LIMIT 2`, baseline.SourceJobId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decision *competitionSecurityToolchainDecision
	for rows.Next() {
		if decision != nil {
			return nil, errors.New("conflicting security toolchain decisions")
		}
		var content []byte
		var expected string
		if err := rows.Scan(&content, &expected); err != nil {
			return nil, err
		}
		decision = &competitionSecurityToolchainDecision{}
		if err := decodeFailureEvidenceJson(content, decision); err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(decision)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(canonical)
		exceptionDigest, err := competitionSecurityToolchainDigest(&decision.Exception)
		if err != nil || decision.Kind != "security-toolchain-decision/v1" || decision.ExceptionSha256 != exceptionDigest || expected != hex.EncodeToString(digest[:]) || decision.Exception.BaselineSha256 != baseline.Sha256 || decision.Exception.RoundId != baseline.RoundId {
			return nil, errors.New("security toolchain decision failed authentication")
		}
	}
	return decision, rows.Err()
}

// Restarts must present the same prospective decision; removing the override
// cannot quietly revert a started round to the previous compiler image.
func requireCompetitionSecurityToolchainDecisionInTx(ctx context.Context, tx server.PgTx, settings *Settings, baseline *roundBaselineRecord) error {
	decision, err := competitionSecurityToolchainDecisionInTx(ctx, tx, baseline)
	if err != nil {
		return err
	}
	expected, err := competitionSecurityToolchainDigest(settings.securityToolchainException)
	if err != nil {
		return err
	}
	if decision == nil && expected == "" || decision != nil && decision.ExceptionSha256 == expected {
		return nil
	}
	return errors.New("execution does not match the durable security toolchain decision")
}

// Scope lock and live supervisor fence serialize first registration with job
// claims. Existing identical decisions are idempotent even after jobs start.
func (self PostgresStore) recordCompetitionSecurityToolchainException(ctx context.Context, settings *Settings, round *roundRecord, baseline *roundBaselineRecord) error {
	if err := validateCompetitionSecurityToolchainScope(settings, round, baseline); err != nil {
		return err
	}
	if settings.automationLease == nil {
		return ErrLeaseLost
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			decision, err := competitionSecurityToolchainDecisionInTx(ctx, tx, baseline)
			server.Raise(err)
			if decision != nil || settings.securityToolchainException == nil {
				server.Raise(requireCompetitionSecurityToolchainDecisionInTx(ctx, tx, settings, baseline))
				return
			}
			var started bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job WHERE round_id=$1 AND
			 (started_at IS NOT NULL OR executor_host_id IS NOT NULL OR state='running'))`, round.RoundId).Scan(&started))
			if started {
				panic(errors.New("security toolchain exception must precede the first customer execution"))
			}
			digest, err := competitionSecurityToolchainDigest(settings.securityToolchainException)
			server.Raise(err)
			appendEvent(ctx, tx, baseline.SourceJobId, self.nowUtc(), "security_toolchain_exception", settings.automationLease.OwnerId,
				competitionSecurityToolchainDecision{Kind: "security-toolchain-decision/v1", ExceptionSha256: digest, Exception: *settings.securityToolchainException})
		}, server.OptReadWrite())
	})
}

// The job retains its frozen image field; only signed execution uses the hotfix.
func competitionExecutionImageForJob(settings *Settings, job *queuedJob) string {
	if value := competitionExecutionSecurityToolchain(settings); value != nil {
		return value.EvaluatorImageDigest
	}
	return job.EvaluatorImageDigest
}
