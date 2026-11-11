// An append-only operational amendment changes executor control identities,
// never the frozen round, product source, scoring policy or submission clocks.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Previous and replacement identities remain explicit; an alias cannot relabel
// a corrected executable as the old qualification or reuse old admission proof.
type CompetitionExecutorControlHostAmendment struct {
	HostId                      string `json:"host_id"`
	PreviousQualificationSha256 string `json:"previous_qualification_sha256"`
	QualificationSha256         string `json:"qualification_sha256"`
	PreviousAgentSha256         string `json:"previous_agent_sha256"`
	AgentSha256                 string `json:"agent_sha256"`
	PreviousBuilderSha256       string `json:"previous_builder_sha256"`
	BuilderSha256               string `json:"builder_sha256"`
}

// The deliberately closed shape has no configurable gates, host membership,
// response keys, equivalence tolerance, resources, deadlines or source commits.
type CompetitionExecutorControlAmendment struct {
	Schema               int                                       `json:"schema"`
	Kind                 string                                    `json:"kind"`
	RoundId              server.Id                                 `json:"round_id"`
	Environment          string                                    `json:"environment"`
	CompetitionId        string                                    `json:"competition_id"`
	SeasonId             string                                    `json:"season_id"`
	PolicyApprovalSha256 string                                    `json:"policy_approval_sha256"`
	SourceLockSha256     string                                    `json:"source_lock_sha256"`
	EvaluatorImageDigest string                                    `json:"evaluator_image_digest"`
	ConfigLocalSha256    string                                    `json:"config_local_sha256"`
	VaultLocalSha256     string                                    `json:"vault_local_sha256"`
	EvidenceSha256       string                                    `json:"evidence_sha256"`
	Hosts                []CompetitionExecutorControlHostAmendment `json:"hosts"`
}

// Validated runtime authority is loaded from the append-only round record, not
// inherited by another round or accepted from an agent's self-reported header.
type competitionExecutorControlAmendmentRecord struct {
	Amendment CompetitionExecutorControlAmendment
	Sha256    string
}

// Canonical request bytes are retained beside JSONB; DB formatting is not a hash.
func competitionExecutorControlAmendmentBytes(value CompetitionExecutorControlAmendment) ([]byte, string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(content)
	return content, hex.EncodeToString(digest[:]), nil
}

// Validation preserves the exact original ordered host set and response keys.
// Both the former and actual replacement fingerprints remain independently bound.
func validateCompetitionExecutorControlAmendment(settings *Settings, round *roundRecord, value CompetitionExecutorControlAmendment) error {
	if settings == nil || round == nil || round.Epoch < 2 || round.Staging || settings.DistributedEvaluationPolicy == nil ||
		value.Schema != 1 || value.Kind != "executor-control-correctness/v1" || value.RoundId != round.RoundId ||
		value.Environment != settings.AutomationPolicy.Environment || value.CompetitionId != settings.CompetitionId || value.SeasonId != settings.AutomationPolicy.SeasonId ||
		value.PolicyApprovalSha256 != settings.DistributedEvaluationPolicy.ApprovalSha256 || value.SourceLockSha256 != settings.AutomationPolicy.SourceLockSha256 ||
		value.EvaluatorImageDigest != settings.EvaluatorImageDigest || value.ConfigLocalSha256 != settings.EvaluationPolicy.ConfigLocalSha256 || value.VaultLocalSha256 != settings.EvaluationPolicy.VaultLocalSha256 ||
		!sha256Pattern.MatchString(value.EvidenceSha256) || len(value.Hosts) != len(settings.DistributedEvaluationPolicy.Hosts) {
		return errors.New("executor control amendment differs from its exact frozen scope")
	}
	for index, host := range value.Hosts {
		previous := settings.DistributedEvaluationPolicy.Hosts[index]
		if host.HostId != previous.HostId || host.PreviousQualificationSha256 != previous.QualificationSha256 || host.QualificationSha256 == previous.QualificationSha256 || host.PreviousAgentSha256 == host.AgentSha256 {
			return errors.New("executor control amendment changed host membership or omitted a real replacement")
		}
		for _, digest := range []string{host.PreviousQualificationSha256, host.QualificationSha256, host.PreviousAgentSha256, host.AgentSha256, host.PreviousBuilderSha256, host.BuilderSha256} {
			if !sha256Pattern.MatchString(digest) {
				return errors.New("executor control amendment omitted a bound release identity")
			}
		}
	}
	return nil
}

// Resolving a host makes a copy; the original immutable policy is never edited.
func competitionExecutorControlHost(settings *Settings, host CompetitionExecutorHostPolicy) CompetitionExecutorHostPolicy {
	if record := settings.executorControlAmendment; record != nil && record.Amendment.RoundId == settings.automationRoundId {
		for _, replacement := range record.Amendment.Hosts {
			if replacement.HostId == host.HostId && replacement.PreviousQualificationSha256 == host.QualificationSha256 {
				host.QualificationSha256 = replacement.QualificationSha256
				return host
			}
		}
	}
	return host
}

// Empty preserves historical receipt bytes and the original non-amended policy.
func competitionExecutorControlAmendmentSha256(settings *Settings) string {
	if settings.executorControlAmendment != nil && settings.executorControlAmendment.Amendment.RoundId == settings.automationRoundId {
		return settings.executorControlAmendment.Sha256
	}
	return ""
}

// One bounded row is fully authenticated before becoming private runtime state.
func (self PostgresStore) loadExecutorControlAmendment(ctx context.Context, settings *Settings, round *roundRecord) error {
	settings.executorControlAmendment = nil
	var canonical []byte
	var expected string
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			err := conn.QueryRow(ctx, `SELECT amendment_canonical,amendment_sha256 FROM competition_executor_control_amendment WHERE round_id=$1`, round.RoundId).Scan(&canonical, &expected)
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			server.Raise(err)
		})
	})
	if err != nil || len(canonical) == 0 {
		return err
	}
	if len(canonical) > 1024*1024 {
		return errors.New("executor control amendment exceeds its document bound")
	}
	var amendment CompetitionExecutorControlAmendment
	if err := decodeFailureEvidenceJson(canonical, &amendment); err != nil {
		return err
	}
	actual, digest, err := competitionExecutorControlAmendmentBytes(amendment)
	if err != nil || string(actual) != string(canonical) || digest != expected || validateCompetitionExecutorControlAmendment(settings, round, amendment) != nil {
		return errors.New("stored executor control amendment failed authentication")
	}
	settings.executorControlAmendment = &competitionExecutorControlAmendmentRecord{Amendment: amendment, Sha256: expected}
	return nil
}

// The operator supplies the independently approved exact request digest. SQL
// rechecks the live lease and rejects any prior customer or R9 admission start.
func (self PostgresStore) RecordExecutorControlAmendment(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, value CompetitionExecutorControlAmendment, expected string) error {
	if settings == nil || value.RoundId == (server.Id{}) || !sha256Pattern.MatchString(expected) {
		return errors.New("operator executor amendment requires exact scope and digest")
	}
	intent, err := self.CurrentSourceActivation(ctx, settings)
	if err != nil {
		return err
	}
	derived, err := sourceSettingsFromIntent(settings, intent)
	if err != nil || intent == nil || intent.TargetRoundId != value.RoundId {
		return ErrConflict
	}
	round, err := self.GetRound(ctx, settings, value.RoundId)
	if err != nil {
		return err
	}
	frozen, err := settingsForFrozenRound(derived, round)
	if err != nil {
		return err
	}
	content, digest, err := competitionExecutorControlAmendmentBytes(value)
	if err != nil || digest != expected || validateCompetitionExecutorControlAmendment(frozen, round, value) != nil {
		return errors.New("operator executor amendment does not match its approved bytes")
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, frozen)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, frozen, lease))
			server.Raise(setAutomationLeaseGuardsInTx(ctx, tx, frozen, lease))
			var existing string
			var existingCanonical []byte
			err := tx.QueryRow(ctx, `SELECT amendment_sha256,amendment_canonical FROM competition_executor_control_amendment WHERE round_id=$1`, value.RoundId).Scan(&existing, &existingCanonical)
			if err == nil {
				if existing != expected || !bytes.Equal(existingCanonical, content) {
					panic(ErrConflict)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
			_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_control_amendment', $1, true)`, expected)
			server.Raise(err)
			_, err = tx.Exec(ctx, `INSERT INTO competition_executor_control_amendment(round_id,amendment_sha256,amendment_json,amendment_canonical,created_at)
			 VALUES($1,$2,$3,$4,$5)`, value.RoundId, expected, json.RawMessage(content), content, self.nowUtc())
			server.Raise(err)
		}, server.OptReadWrite())
	})
}
