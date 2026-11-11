// Private executors and the independently qualified shared-baseline producer
// have distinct script identities. Operational pins never rewrite round policy.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/urnetwork/server/competitionagent"
)

// The local snapshot is byte-identical to the pinned agent role, including for
// remote hosts. A bare new digest is not independent deployment evidence.
func validateCompetitionExecutorEvaluatorConnection(connection competitionExecutorConnection) error {
	if connection.EvaluatorCommandSha256 == "" && connection.AgentConfigSnapshotPath == "" {
		return nil
	}
	if !sha256Pattern.MatchString(connection.EvaluatorCommandSha256) || !sha256Pattern.MatchString(connection.AgentConfigSha256) || connection.AgentConfigSnapshotPath == "" {
		return errors.New("private evaluator requires its exact pinned agent-role snapshot")
	}
	if err := competitionQualificationRootPath(connection.AgentConfigSnapshotPath, false); err != nil {
		return err
	}
	content, err := readCompetitionPreparationFile(connection.AgentConfigSnapshotPath, 1024*1024, false)
	if err != nil {
		return err
	}
	return validateCompetitionExecutorEvaluatorSnapshot(connection, content)
}

// Hash the complete role before extracting its scalar script identity. Remote
// paths are authenticated bytes, not files to open on the coordinator host.
func validateCompetitionExecutorEvaluatorSnapshot(connection competitionExecutorConnection, content []byte) error {
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != connection.AgentConfigSha256 {
		return errors.New("private evaluator agent-role snapshot digest differs")
	}
	var config struct {
		HostId    string `json:"host_id"`
		Evaluator struct {
			Sha256 string `json:"sha256"`
		} `json:"evaluator"`
	}
	if err := json.Unmarshal(content, &config); err != nil {
		return err
	}
	if config.HostId != connection.HostId || !sha256Pattern.MatchString(config.Evaluator.Sha256) || config.Evaluator.Sha256 != connection.EvaluatorCommandSha256 {
		return errors.New("private evaluator differs from its authenticated host role")
	}
	return nil
}

// Identity and script selection happen before workload/seed I/O. The producer's
// settings stay unchanged, including across per-host and recovery manifests.
func competitionAgentManifestForJob(settings *Settings, job *queuedJob, host CompetitionExecutorHostPolicy) (competitionagent.StartManifest, error) {
	digest, err := competitionExecutorEvaluatorCommandSha256(settings, host.HostId)
	if err != nil || job == nil || job.RoundBaseline == nil {
		if err != nil {
			return competitionagent.StartManifest{}, err
		}
		return competitionagent.StartManifest{}, ErrConflict
	}
	startedAt := job.StartedAt
	if job.EvaluationCycle > 0 {
		startedAt = job.CycleStartedAt
	}
	if startedAt == nil || job.ExecutionDeadline == nil {
		return competitionagent.StartManifest{}, ErrConflict
	}
	manifest := competitionagent.StartManifest{Identity: competitionagent.Identity{HostId: host.HostId, RunId: competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount),
		JobId: job.JobId.String(), RoundId: job.RoundId.String(), RoundEpoch: job.Round.Epoch, SourceEpoch: evaluationSourceEpoch(&job.Round), Cycle: job.EvaluationCycle, Attempt: job.AttemptCount,
		StartedAt: startedAt.UTC(), DeadlineAt: job.ExecutionDeadline.UTC()}, EvaluatorImageDigest: job.EvaluatorImageDigest,
		EvaluatorCommandSha256: digest, SourceLockSha256: settings.AutomationPolicy.SourceLockSha256,
		QualificationSha256: host.QualificationSha256, ConfigLocalSha256: settings.EvaluationPolicy.ConfigLocalSha256,
		VaultLocalSha256: settings.EvaluationPolicy.VaultLocalSha256, RoundBaselineSha256: job.RoundBaseline.Sha256}
	return manifest, manifest.Identity.Validate(settings.distributedCoordinator.Config.Settings.Limits)
}

// Historical same-script roles keep their exact old manifest. A configured
// private pin can only come from the independently checked root deployment role.
func competitionExecutorEvaluatorCommandSha256(settings *Settings, hostId string) (string, error) {
	if settings == nil || settings.distributedCoordinator == nil {
		return "", errors.New("private evaluator has no coordinator role")
	}
	for _, connection := range settings.distributedCoordinator.Config.Hosts {
		if connection.HostId != hostId {
			continue
		}
		if connection.EvaluatorCommandSha256 != "" {
			if !sha256Pattern.MatchString(connection.EvaluatorCommandSha256) || connection.AgentConfigSnapshotPath == "" {
				return "", errors.New("private evaluator pin is incomplete")
			}
			return connection.EvaluatorCommandSha256, nil
		}
		if connection.AgentConfigSnapshotPath != "" || !sha256Pattern.MatchString(settings.EvaluatorCommandSha256) {
			return "", errors.New("private evaluator pin is incomplete")
		}
		return settings.EvaluatorCommandSha256, nil
	}
	return "", errors.New("private evaluator host is not configured")
}
