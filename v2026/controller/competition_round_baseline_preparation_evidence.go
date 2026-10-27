// The separately pinned host producer returns only a purpose-specific control
// receipt. Raw native artifacts are authenticated before private retained upload;
// neither this document nor its archive is a candidate completion or score.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/urnetwork/server/v2026"
	"golang.org/x/sys/unix"
)

// Pointer booleans distinguish explicit absence of submission/candidate work
// from a malformed producer receipt that merely omitted those assertions.
type roundBaselinePreparationCompletion struct {
	Schema                 int      `json:"schema"`
	Kind                   string   `json:"kind"`
	Owner                  string   `json:"owner"`
	ContextSha256          string   `json:"context_sha256"`
	JobId                  string   `json:"job_id"`
	RoundId                string   `json:"round_id"`
	SourceEpoch            int      `json:"source_epoch"`
	BaseSha                string   `json:"base_sha"`
	BaseImageId            string   `json:"base_image_id"`
	ProvidersSha256        string   `json:"providers_sha256"`
	ScorerVersion          string   `json:"scorer_version"`
	Replicates             int      `json:"replicates"`
	ControlEvaluationIds   []string `json:"control_evaluation_ids"`
	BaselineSha256         string   `json:"baseline_sha256"`
	EvidenceManifestSha256 string   `json:"evidence_manifest_sha256"`
	SubmissionStarted      *bool    `json:"submission_started"`
	CandidateExecuted      *bool    `json:"candidate_executed"`
	CleanupComplete        *bool    `json:"cleanup_complete"`
}

// A private archive namespace and purpose separate these controls from the
// first real candidate attempt, even though both cite that same real job id.
type roundBaselinePreparationArchiveDocument struct {
	Schema               int                  `json:"schema"`
	Kind                 string               `json:"kind"`
	Outcome              string               `json:"outcome"`
	Owner                string               `json:"owner"`
	ContextSha256        string               `json:"context_sha256"`
	JobId                string               `json:"job_id"`
	RoundId              string               `json:"round_id"`
	BaseSha              string               `json:"base_sha"`
	ImageId              string               `json:"image_id"`
	ProvidersSha256      string               `json:"providers_sha256"`
	WorkerImage          string               `json:"worker_image_digest"`
	BaselineSha256       string               `json:"baseline_sha256"`
	ControlEvaluationIds []string             `json:"control_evaluation_ids"`
	Artifacts            []evaluationArtifact `json:"artifacts"`
	Retention            *artifactRetention   `json:"retention"`
}

// The optional archive capability cannot fall back to a fake successful or
// failed ArchiveAttempt/worker-result. Ordinary attempt object keys stay intact.
type roundBaselinePreparationArchive interface {
	ArchiveRoundBaselinePreparation(context.Context, *Settings, *roundBaselinePreparation, string, roundBaselinePreparationArchiveDocument) (json.RawMessage, error)
}

// Final native segment names are a protocol fact, not operator-selected paths.
var roundBaselinePreparationSegmentPattern = regexp.MustCompile(`^[0-9]+-[1-9][0-9]*\.pb\.zst$`)

// A typed SQL publication cannot accept a producer's unbound summary or an
// archive from another source/workload/job, even when its SHA syntax is valid.
func validatePreparedRoundBaseline(prepared *preparedRoundBaseline, preparation *roundBaselinePreparation, settings *Settings) error {
	if prepared == nil || preparation == nil || preparation.SourceJob == nil || len(prepared.ArtifactManifest) == 0 {
		return errors.New("prepared round baseline is incomplete")
	}
	digest, err := validateRoundBaselineJson(settings, &preparation.SourceJob.Round, prepared.BaselineJson)
	if err != nil || digest != prepared.BaselineSha256 {
		return errors.New("prepared baseline differs from the exact frozen round")
	}
	hash := sha256.Sum256(prepared.ArtifactManifest)
	if hex.EncodeToString(hash[:]) != prepared.ArtifactManifestSha256 {
		return errors.New("prepared baseline archive digest mismatch")
	}
	var archive roundBaselinePreparationArchiveDocument
	if err := decodeCompetitionPreparationJson(bytes.NewReader(prepared.ArtifactManifest), settings.roundBaselinePreparation.Settings.MaximumRecordBytes, &archive); err != nil {
		return err
	}
	if archive.Schema != 1 || archive.Kind != "sim-latency-round-baseline-preparation-artifacts" || archive.Outcome != "prepared" || archive.Owner != preparation.Owner || archive.ContextSha256 != preparation.ContextSha256 ||
		archive.JobId != preparation.JobId.String() || archive.RoundId != preparation.RoundId.String() || archive.BaseSha != settings.BaseSha || archive.ImageId != settings.EvaluatorImageDigest ||
		archive.ProvidersSha256 != preparation.SourceJob.Round.ProvidersSha256 || archive.WorkerImage != preparation.WorkerImage || archive.BaselineSha256 != prepared.BaselineSha256 || len(archive.ControlEvaluationIds) != 9 ||
		archive.Retention == nil || archive.Retention.Backend != "server/blob" || !archive.Retention.AuthenticatedAfterUpload || !archive.Retention.ComplianceObjectLock || archive.Retention.RetainUntil.Before(settings.RetainUntil) {
		return errors.New("baseline archive lacks its exact owned retained provenance")
	}
	expectedKey := filepath.ToSlash(filepath.Join(archive.Retention.Prefix, "competition", "v1", settings.CompetitionId, "rounds", preparation.RoundId.String(), "baseline-preparation", preparation.Owner, "baseline-preparation-manifest.json"))
	if archive.Retention.Bucket != settings.ArtifactBucket || archive.Retention.ManifestKey != expectedKey ||
		len(archive.Artifacts) == 0 || len(archive.Artifacts) > settings.roundBaselinePreparation.Settings.MaximumArtifacts+6 || archive.Retention.ObjectCount != len(archive.Artifacts) || len(archive.Retention.Objects) != len(archive.Artifacts) {
		return errors.New("baseline archive used an ordinary attempt namespace or incomplete retention inventory")
	}
	artifactKVs := map[string]evaluationArtifact{}
	var total int64
	for _, artifact := range archive.Artifacts {
		if artifact.Path == "" || filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "..") ||
			!sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes < 0 || artifact.Bytes > settings.roundBaselinePreparation.Settings.MaximumEvidenceBytes-total {
			return errors.New("baseline archive artifact identity is invalid")
		}
		if _, exists := artifactKVs[artifact.Path]; exists {
			return errors.New("baseline archive repeats an artifact")
		}
		artifactKVs[artifact.Path], total = artifact, total+artifact.Bytes
	}
	seenRetained := map[string]bool{}
	objectPrefix := strings.TrimSuffix(archive.Retention.ManifestKey, "/baseline-preparation-manifest.json") + "/objects/"
	for _, retained := range archive.Retention.Objects {
		artifact, exists := artifactKVs[retained.Path]
		if !exists || seenRetained[retained.Path] || retained.Sha256 != artifact.Sha256 || retained.Bytes != artifact.Bytes || retained.Key != objectPrefix+artifact.Sha256+"/"+artifact.Path ||
			retained.Mode != "COMPLIANCE" || retained.RetainUntil.Before(settings.RetainUntil) || settings.AutomationPolicy.Enabled && retained.VersionId == "" {
			return errors.New("baseline archive is not fully authenticated immutable retained data")
		}
		seenRetained[retained.Path] = true
	}
	if archive.Retention.Bytes != total {
		return errors.New("baseline archive byte accounting differs")
	}
	seen := map[string]bool{}
	for index, id := range archive.ControlEvaluationIds {
		if id != fmt.Sprintf("baseline-%02d-%s", index+1, preparation.JobId.String()[:8]) || seen[id] {
			return errors.New("baseline archive substituted or duplicated a control")
		}
		seen[id] = true
	}
	return nil
}

// Authenticates one bounded sealed leaf without following any parent/leaf link.
func readRoundBaselinePreparationArtifact(root, relative string, maximum int64) ([]byte, evaluationArtifact, error) {
	var artifact evaluationArtifact
	file, err := openFailureArtifactPath(root, relative, false)
	if err != nil {
		return nil, artifact, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Size() < 0 || info.Size() > maximum {
		return nil, artifact, errors.New("baseline artifact is not sealed, regular or bounded")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) != info.Size() {
		return nil, artifact, errors.New("baseline artifact changed during its bounded read")
	}
	hash := sha256.Sum256(content)
	return content, evaluationArtifact{Path: relative, Sha256: hex.EncodeToString(hash[:]), Bytes: int64(len(content))}, nil
}

// Known producer output is the only retained corpus. Inputs, runtime credentials,
// source checkouts, candidate output and arbitrary extra paths are never allowed.
func roundBaselinePreparationArtifactAllowed(relative string, preparation *roundBaselinePreparation) bool {
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.Contains(relative, "\\") || strings.IndexFunc(relative, unicode.IsControl) >= 0 {
		return false
	}
	for _, name := range []string{"resource-boundary.json", "evidence-quota.json", "local-mounts.json", "docker-id-map.json", "policy.json", "evaluation-sources.json", "candidate-original-source.json", "builder-identity.json", "baseline-scorer.log", "baseline-score-output/baseline.json"} {
		if relative == "evidence/"+name {
			return true
		}
	}
	for index := 1; index <= 9; index++ {
		stage := fmt.Sprintf("baseline-%02d", index)
		prefix := "evidence/runs/" + stage + "/"
		for _, name := range []string{"migration.json", "containers.json", "network-boundary.json", "network-boundary.post.json", "container-resource-state.json"} {
			if relative == prefix+name {
				return true
			}
		}
		run := prefix + "output/" + stage + "-" + preparation.JobId.String()[:8] + "/"
		for _, name := range []string{"run.json", "run.complete.json", "results.csv", "stderr.log", "accounting.json", "accounting.source.json", "resources.json", "site/settings.yml"} {
			if relative == run+name {
				return true
			}
		}
		if strings.HasPrefix(relative, run+"site/stats/local/") && strings.HasSuffix(relative, ".pb.zst") {
			components := strings.Split(strings.TrimPrefix(relative, run+"site/stats/local/"), "/")
			if len(components) == 3 && workerIdPattern.MatchString(components[0]) && components[1] == "findproviders2" &&
				roundBaselinePreparationSegmentPattern.MatchString(components[2]) {
				return true
			}
		}
	}
	return false
}

// Parses native sidecars only after their complete bytes are manifest-bound.
// The pinned native baseline scorer has already decoded/gated the sample corpus.
func authenticateRoundBaselinePreparation(directory string, settings *Settings, preparation *roundBaselinePreparation) ([]byte, roundBaselinePreparationArchiveDocument, error) {
	var document roundBaselinePreparationArchiveDocument
	limit := settings.roundBaselinePreparation.Settings.MaximumRecordBytes
	content, receiptArtifact, err := readRoundBaselinePreparationArtifact(directory, "round-baseline-preparation.complete.json", limit)
	if err != nil {
		return nil, document, err
	}
	var receipt roundBaselinePreparationCompletion
	if err := decodeCompetitionPreparationJson(bytes.NewReader(content), limit, &receipt); err != nil {
		return nil, document, err
	}
	if receipt.Schema != 1 || receipt.Kind != "sim-latency-round-baseline-preparation-complete" || receipt.Owner != preparation.Owner || receipt.ContextSha256 != preparation.ContextSha256 ||
		receipt.JobId != preparation.JobId.String() || receipt.RoundId != preparation.RoundId.String() || receipt.SourceEpoch != evaluationSourceEpoch(&preparation.SourceJob.Round) ||
		receipt.BaseSha != settings.BaseSha || receipt.BaseImageId != settings.EvaluatorImageDigest || receipt.ProvidersSha256 != preparation.SourceJob.Round.ProvidersSha256 || receipt.ScorerVersion != ScorerVersion ||
		receipt.Replicates != 9 || len(receipt.ControlEvaluationIds) != 9 || receipt.SubmissionStarted == nil || *receipt.SubmissionStarted || receipt.CandidateExecuted == nil || *receipt.CandidateExecuted || receipt.CleanupComplete == nil || !*receipt.CleanupComplete {
		return nil, document, errors.New("baseline preparation receipt changed purpose or frozen identity")
	}
	for _, forbidden := range []string{"worker-result.json", "evaluation.complete.json", "score.json"} {
		if _, err := os.Lstat(filepath.Join(directory, forbidden)); !errors.Is(err, os.ErrNotExist) {
			return nil, document, errors.New("baseline preparation published submission authority")
		}
	}
	baseline, baselineArtifact, err := readRoundBaselinePreparationArtifact(directory, "baseline.json", limit)
	if err != nil || baselineArtifact.Sha256 != receipt.BaselineSha256 {
		return nil, document, errors.New("baseline preparation omitted its exact baseline")
	}
	if digest, err := validateRoundBaselineJson(settings, &preparation.SourceJob.Round, baseline); err != nil || digest != receipt.BaselineSha256 {
		return nil, document, errors.New("baseline preparation native R9 contract failed")
	}
	evidenceBytes, evidenceArtifact, err := readRoundBaselinePreparationArtifact(directory, "evidence-manifest.json", settings.roundBaselinePreparation.Settings.MaximumManifestBytes)
	if err != nil || evidenceArtifact.Sha256 != receipt.EvidenceManifestSha256 {
		return nil, document, errors.New("baseline evidence manifest identity failed")
	}
	var evidence competitionQualificationEvidence
	if err := decodeCompetitionPreparationJson(bytes.NewReader(evidenceBytes), settings.roundBaselinePreparation.Settings.MaximumManifestBytes, &evidence); err != nil {
		return nil, document, err
	}
	if evidence.Schema != 1 || evidence.Kind != "sim-latency-evidence-manifest" || evidence.JobId != receipt.JobId || evidence.RoundId != receipt.RoundId || len(evidence.Artifacts) == 0 || len(evidence.Artifacts) > settings.roundBaselinePreparation.Settings.MaximumArtifacts {
		return nil, document, errors.New("baseline evidence inventory is invalid")
	}
	bound := map[string]evaluationArtifact{}
	var total int64
	for _, artifact := range evidence.Artifacts {
		if !roundBaselinePreparationArtifactAllowed(artifact.Path, preparation) || !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes < 0 || artifact.Bytes > settings.roundBaselinePreparation.Settings.MaximumRawFileBytes || artifact.Bytes > settings.roundBaselinePreparation.Settings.MaximumEvidenceBytes-total {
			return nil, document, errors.New("baseline evidence contains unsafe or oversized output")
		}
		if _, exists := bound[artifact.Path]; exists {
			return nil, document, errors.New("baseline evidence repeats a path")
		}
		file, err := openFailureArtifactPath(directory, artifact.Path, false)
		if err != nil {
			return nil, document, err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Size() != artifact.Bytes {
			file.Close()
			return nil, document, errors.New("baseline raw artifact is not a bounded sealed regular leaf")
		}
		hash := sha256.New()
		count, readErr := io.Copy(hash, io.LimitReader(file, artifact.Bytes+1))
		closeErr := file.Close()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Size() != artifact.Bytes || count != artifact.Bytes || errors.Join(readErr, closeErr) != nil || hex.EncodeToString(hash.Sum(nil)) != artifact.Sha256 {
			return nil, document, errors.New("baseline raw artifact hash/size/seal failed")
		}
		bound[artifact.Path], total = artifact, total+artifact.Bytes
	}
	for index, id := range receipt.ControlEvaluationIds {
		expected := fmt.Sprintf("baseline-%02d-%s", index+1, preparation.JobId.String()[:8])
		if id != expected {
			return nil, document, errors.New("baseline receipt changed or repeated a real control id")
		}
		root := fmt.Sprintf("evidence/runs/baseline-%02d/output/%s/", index+1, id)
		for _, name := range []string{"run.json", "run.complete.json", "results.csv", "stderr.log", "accounting.json", "accounting.source.json", "resources.json"} {
			if _, ok := bound[root+name]; !ok {
				return nil, document, errors.New("baseline evidence omitted a required native raw file")
			}
		}
		if err := authenticateRoundBaselineNativeRun(directory, root, id, bound, settings, preparation); err != nil {
			return nil, document, err
		}
	}
	document = roundBaselinePreparationArchiveDocument{Schema: 1, Kind: "sim-latency-round-baseline-preparation-artifacts", Outcome: "prepared", Owner: preparation.Owner,
		ContextSha256: preparation.ContextSha256, JobId: receipt.JobId, RoundId: receipt.RoundId, BaseSha: settings.BaseSha, ImageId: settings.EvaluatorImageDigest,
		ProvidersSha256: receipt.ProvidersSha256, WorkerImage: preparation.WorkerImage, BaselineSha256: receipt.BaselineSha256, ControlEvaluationIds: receipt.ControlEvaluationIds,
		Artifacts: append(append([]evaluationArtifact{}, evidence.Artifacts...), receiptArtifact, baselineArtifact, evidenceArtifact)}
	for _, relative := range []string{"baseline-preparation.stderr.log", "baseline-preparation.stderr-summary.json"} {
		_, artifact, err := readRoundBaselinePreparationArtifact(directory, relative, settings.roundBaselinePreparation.Settings.MaximumDiagnosticBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, document, err
		}
		document.Artifacts = append(document.Artifacts, artifact)
	}
	return baseline, document, nil
}

// Use the observed marker->run->CSV/accounting chain, never invented counts.
func authenticateRoundBaselineNativeRun(directory, root, id string, bound map[string]evaluationArtifact, settings *Settings, preparation *roundBaselinePreparation) error {
	read := func(name string, target any) error {
		content, _, err := readRoundBaselinePreparationArtifact(directory, root+name, settings.roundBaselinePreparation.Settings.MaximumRecordBytes)
		if err != nil {
			return err
		}
		var unique map[string]json.RawMessage
		if err := decodeCompetitionEvidenceJson(content, &unique, nil); err != nil {
			return err
		}
		return json.Unmarshal(content, target)
	}
	var run struct {
		Schema                 int    `json:"schema"`
		Kind                   string `json:"kind"`
		EvaluationId           string `json:"evaluation_id"`
		CompletionState        string `json:"completion_state"`
		Official               bool   `json:"official"`
		BuildRevision          string `json:"build_revision"`
		BuildModified          bool   `json:"build_modified"`
		ConfigSha256           string `json:"config_sha256"`
		SourceEpoch            int    `json:"source_epoch"`
		ScoreSchema            int    `json:"score_schema"`
		ScorerVersion          string `json:"scorer_version"`
		RequestTimeoutMs       int64  `json:"request_timeout_ms"`
		ResultsCsvSha256       string `json:"results_csv_sha256"`
		ResultsCsvBytes        int64  `json:"results_csv_bytes"`
		AccountingSourceSha256 string `json:"accounting_source_sha256"`
		AccountingSourceBytes  int64  `json:"accounting_source_bytes"`
		StatsRoot              string `json:"stats_root"`
	}
	if err := read("run.json", &run); err != nil {
		return err
	}
	var marker struct {
		Schema        int    `json:"schema"`
		Kind          string `json:"kind"`
		EvaluationId  string `json:"evaluation_id"`
		RunSha256     string `json:"run_manifest_sha256"`
		RunBytes      int64  `json:"run_manifest_bytes"`
		ScoreSchema   int    `json:"score_schema"`
		ScorerVersion string `json:"scorer_version"`
	}
	if err := read("run.complete.json", &marker); err != nil {
		return err
	}
	var resources struct {
		Schema             int    `json:"schema"`
		Kind               string `json:"kind"`
		Complete           bool   `json:"complete"`
		EvaluationId       string `json:"evaluation_id"`
		ExitCode           *int   `json:"exit_code"`
		OomKilled          *bool  `json:"oom_killed"`
		HardKilled         *bool  `json:"hard_killed"`
		LimitEscape        *bool  `json:"limit_escape"`
		MeasurementMissing *bool  `json:"measurement_missing"`
	}
	if err := read("resources.json", &resources); err != nil {
		return err
	}
	var accounting struct {
		Schema        int    `json:"schema"`
		Kind          string `json:"kind"`
		Complete      bool   `json:"complete"`
		EvaluationId  string `json:"evaluation_id"`
		ProviderBytes int64  `json:"provider_egress_bytes"`
	}
	if err := read("accounting.json", &accounting); err != nil {
		return err
	}
	var accountingSource struct {
		Schema        int    `json:"schema"`
		Kind          string `json:"kind"`
		Complete      bool   `json:"complete"`
		EvaluationId  string `json:"evaluation_id"`
		ProviderBytes int64  `json:"provider_egress_bytes"`
	}
	if err := read("accounting.source.json", &accountingSource); err != nil {
		return err
	}
	if run.Schema != 2 || run.Kind != "sim-latency-run" || run.EvaluationId != id || run.CompletionState != "complete" || !run.Official || run.BuildModified || run.BuildRevision != settings.BaseSha || run.ConfigSha256 != preparation.SourceJob.Round.ProvidersSha256 || run.SourceEpoch != evaluationSourceEpoch(&preparation.SourceJob.Round) || run.ScoreSchema != ScoreSchema || run.ScorerVersion != ScorerVersion || run.RequestTimeoutMs != settings.EvaluationPolicy.RequestTimeoutMs ||
		run.ResultsCsvSha256 != bound[root+"results.csv"].Sha256 || run.ResultsCsvBytes != bound[root+"results.csv"].Bytes || run.AccountingSourceSha256 != bound[root+"accounting.source.json"].Sha256 || run.AccountingSourceBytes != bound[root+"accounting.source.json"].Bytes ||
		marker.Schema != 1 || marker.Kind != "sim-latency-complete" || marker.EvaluationId != id || marker.RunSha256 != bound[root+"run.json"].Sha256 || marker.RunBytes != bound[root+"run.json"].Bytes || marker.ScoreSchema != ScoreSchema || marker.ScorerVersion != ScorerVersion ||
		resources.Schema != 1 || resources.Kind != "sim-latency-resource-report" || !resources.Complete || resources.EvaluationId != id || resources.ExitCode == nil || *resources.ExitCode != 0 || resources.OomKilled == nil || *resources.OomKilled || resources.HardKilled == nil || *resources.HardKilled || resources.LimitEscape == nil || *resources.LimitEscape || resources.MeasurementMissing == nil || *resources.MeasurementMissing || accounting.Schema != 1 || accounting.Kind != "sim-latency-accounting" || !accounting.Complete || accounting.EvaluationId != id || accounting.ProviderBytes <= 0 ||
		accountingSource.Schema != 1 || accountingSource.Kind != "sim-latency-provider-accounting-source" || !accountingSource.Complete || accountingSource.EvaluationId != id || accountingSource.ProviderBytes != accounting.ProviderBytes {
		return errors.New("baseline native marker/CSV/accounting/resource chain disagrees")
	}
	statsPrefix := "/artifacts/" + id + "/site/stats/"
	if !strings.HasPrefix(run.StatsRoot, statsPrefix) || filepath.Clean(run.StatsRoot) != run.StatsRoot {
		return errors.New("baseline native stats path escaped its run")
	}
	stats := root + strings.TrimPrefix(run.StatsRoot, "/artifacts/"+id+"/") + "/findproviders2/"
	segments := 0
	for path, artifact := range bound {
		if strings.HasPrefix(path, stats) && strings.HasSuffix(path, ".pb.zst") && artifact.Bytes > 0 {
			segments++
		}
	}
	if segments == 0 {
		return errors.New("baseline native finalized sample segments are absent")
	}
	statsDirectory, err := openFailureArtifactPath(directory, strings.TrimSuffix(stats, "/"), true)
	if err != nil {
		return err
	}
	entries, readErr := statsDirectory.ReadDir(settings.roundBaselinePreparation.Settings.MaximumArtifacts + 1)
	closeErr := statsDirectory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) != segments || len(entries) > settings.roundBaselinePreparation.Settings.MaximumArtifacts {
		return errors.New("baseline sample inventory differs from its bounded complete manifest")
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".pb.zst") || bound[stats+entry.Name()].Bytes <= 0 {
			return errors.New("baseline sample inventory contains partial, missing or unsafe data")
		}
	}
	return nil
}

// The host operation and archive retries share the reservation's original
// deadline. A retry reuploads these same authenticated bytes, never reruns Go.
func (self CommandEvaluator) PrepareRoundBaseline(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation) (result *preparedRoundBaseline, resultErr error) {
	if preparation == nil || preparation.SourceJob == nil {
		return nil, errors.New("baseline producer source job is missing")
	}
	frozen, frozenErr := settingsForFrozenRound(settings, &preparation.SourceJob.Round)
	if frozenErr != nil {
		return nil, frozenErr
	}
	settings = frozen
	profile := settings.roundBaselinePreparation
	archive, ok := settings.artifactArchive.(roundBaselinePreparationArchive)
	if profile == nil || !ok || preparation.SourceJob == nil {
		return nil, errors.New("baseline producer/retained archive capability is unavailable")
	}
	if err := settings.artifactArchive.Check(ctx); err != nil {
		return nil, err
	}
	if err := competitionQualificationRootPath(profile.Config.Program.Path, false); err != nil {
		return nil, err
	}
	if err := verifyPinnedExecutable(profile.Config.Program.Path, profile.Config.Program.Sha256); err != nil {
		return nil, err
	}
	for _, pin := range profile.Config.Dependencies {
		if err := competitionQualificationRootPath(pin.Path, false); err != nil {
			return nil, err
		}
		actual, _, err := hashRegularFile(pin.Path)
		if err != nil || actual != pin.Sha256 {
			return nil, errors.New("baseline producer dependency changed")
		}
	}
	job := preparation.SourceJob
	providers, err := readRoundWorkload(ctx, settings, &job.Round)
	if err != nil {
		return nil, err
	}
	defer clear(providers)
	seed, err := revealRoundSecret(settings, &job.Round)
	if err != nil {
		return nil, err
	}
	directory, err := createRoundBaselinePreparationDirectory(settings.ArtifactRoot, job.RoundId.String(), preparation.Owner)
	if err != nil {
		return nil, errors.Join(errRoundBaselinePreparationHeld, err)
	}
	defer func() { _ = sealArtifactDirectory(directory) }()
	defer func() {
		if resultErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), profile.Settings.CleanupTimeout)
		defer cancel()
		manifest, archiveErr := retainFailedRoundBaselinePreparation(cleanupCtx, settings, preparation, directory, archive)
		failure := &roundBaselinePreparationFailure{Cause: errors.Join(resultErr, archiveErr)}
		if archiveErr == nil {
			digest := sha256.Sum256(manifest)
			failure.ManifestSha256 = hex.EncodeToString(digest[:])
		}
		resultErr = failure
	}()
	providersPath, err := materializeRoundWorkload(directory, providers)
	if err != nil {
		return nil, err
	}
	patchPath := filepath.Join(directory, "canonical.patch")
	if err := writeExclusiveFile(patchPath, job.Patch, 0o400); err != nil {
		return nil, err
	}
	copy := *job
	copy.AttemptCount = 1
	request := evaluatorRequestForJob(settings, &copy, seed, directory, patchPath, providersPath)
	request.RoundBaselinePath, request.RoundBaselineSha256 = "", ""
	requestBytes, err := json.Marshal(request)
	request.RoundSeedHex, seed = "", ""
	if err != nil {
		return nil, err
	}
	defer clear(requestBytes)
	requestPath := filepath.Join(directory, "worker-request.json")
	if err := writeExclusiveFile(requestPath, requestBytes, 0o400); err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(directory, "baseline-preparation.stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return nil, err
	}
	stdout := &boundedBuffer{limit: maxSelfCheckBytes}
	stderrWriter := &roundBaselinePreparationLogWriter{Writer: stderr, Limit: profile.Settings.MaximumDiagnosticBytes}
	exitCode, runErr := runContainedCommand(ctx, directory, profile.Config.Program.Path, []string{"--request", requestPath, "--receipt", filepath.Join(directory, "round-baseline-preparation.complete.json"), "--owner", preparation.Owner, "--context-sha256", preparation.ContextSha256}, stdout, stderrWriter)
	closeErr := stderr.Close()
	logSummary, summaryErr := json.Marshal(struct {
		Schema    int    `json:"schema"`
		Kind      string `json:"kind"`
		Limit     int64  `json:"limit_bytes"`
		Observed  int64  `json:"observed_bytes"`
		Retained  int64  `json:"retained_bytes"`
		Truncated bool   `json:"truncated"`
	}{Schema: 1, Kind: "sim-latency-round-baseline-preparation-log", Limit: stderrWriter.Limit, Observed: stderrWriter.Observed, Retained: stderrWriter.Retained, Truncated: stderrWriter.Observed > stderrWriter.Retained})
	if summaryErr == nil {
		summaryErr = writeExclusiveFile(filepath.Join(directory, "baseline-preparation.stderr-summary.json"), logSummary, 0o400)
	}
	closeErr = errors.Join(closeErr, summaryErr)
	if runErr != nil || closeErr != nil || exitCode != 0 || ctx.Err() != nil {
		return nil, errors.Join(errors.New("owned baseline producer failed or was canceled; raw failure evidence retained"), runErr, closeErr, ctx.Err())
	}
	baseline, document, err := authenticateRoundBaselinePreparation(directory, settings, preparation)
	if err != nil {
		return nil, err
	}
	if err := sealArtifactDirectory(directory); err != nil {
		return nil, err
	}
	var archived json.RawMessage
	for attempt := 0; attempt < profile.Settings.ArchiveAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		archived, err = archive.ArchiveRoundBaselinePreparation(ctx, settings, preparation, directory, document)
		if err == nil {
			break
		}
		if attempt+1 < profile.Settings.ArchiveAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(profile.Settings.ArchiveBackoff):
			}
		}
	}
	if err != nil {
		return nil, errors.Join(errRoundBaselinePreparationHeld, err)
	}
	hash := sha256.Sum256(archived)
	prepared := &preparedRoundBaseline{BaselineJson: baseline, BaselineSha256: document.BaselineSha256, ArtifactManifest: archived, ArtifactManifestSha256: hex.EncodeToString(hash[:])}
	if err := validatePreparedRoundBaseline(prepared, preparation, settings); err != nil {
		return nil, err
	}
	return prepared, nil
}

// Creates only this owner's three fixed components through open directory
// descriptors. A submitted/sibling symlink cannot redirect an archive root.
func createRoundBaselinePreparationDirectory(root, roundId, owner string) (string, error) {
	if !workerIdPattern.MatchString(owner) {
		return "", errors.New("baseline evidence owner identity is invalid")
	}
	if _, err := server.ParseId(roundId); err != nil {
		return "", errors.New("baseline evidence round identity is invalid")
	}
	directory, err := openFailureArtifactPath(root, "", true)
	if err != nil {
		return "", err
	}
	defer func() { _ = directory.Close() }()
	for index, name := range []string{"baseline-preparations", roundId, owner} {
		err := unix.Mkdirat(int(directory.Fd()), name, 0o700)
		if err != nil && (!errors.Is(err, unix.EEXIST) || index == 2) {
			return "", err
		}
		fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return "", err
		}
		next := os.NewFile(uintptr(fd), name)
		info, err := next.Stat()
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			next.Close()
			return "", errors.New("baseline evidence parent has a foreign owner or permissions")
		}
		if err := directory.Sync(); err != nil {
			next.Close()
			return "", err
		}
		directory.Close()
		directory = next
	}
	return filepath.Join(root, "baseline-preparations", roundId, owner), directory.Sync()
}

// Uses the same private WORM backend and authenticated object retention, with
// a purpose-specific immutable namespace unrelated to attempt-01 publication.
func (self *blobArtifactArchive) ArchiveRoundBaselinePreparation(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation, directory string, document roundBaselinePreparationArchiveDocument) (json.RawMessage, error) {
	prefix := filepath.ToSlash(filepath.Join(self.store.Prefix(), "competition", "v1", settings.CompetitionId, "rounds", preparation.RoundId.String(), "baseline-preparation", preparation.Owner))
	manifestName := "baseline-preparation-manifest.json"
	if document.Outcome == "infrastructure_hold" {
		manifestName = "baseline-preparation-hold-manifest.json"
	} else if document.Outcome != "prepared" {
		return nil, errors.New("baseline archive purpose is invalid")
	}
	if err := authenticateRoundBaselinePreparationArchiveArtifacts(directory, settings, preparation, document.Artifacts); err != nil {
		return nil, err
	}
	retention := &artifactRetention{Backend: "server/blob", Bucket: self.store.Bucket(), Prefix: self.store.Prefix(), ManifestKey: prefix + "/" + manifestName,
		RetainUntil: settings.RetainUntil.UTC(), AuthenticatedAfterUpload: true, ComplianceObjectLock: true}
	sort.Slice(document.Artifacts, func(i, j int) bool { return document.Artifacts[i].Path < document.Artifacts[j].Path })
	for _, artifact := range document.Artifacts {
		key := prefix + "/objects/" + artifact.Sha256 + "/" + artifact.Path
		retained, err := self.retainFile(ctx, settings, artifact.Path, key, filepath.Join(directory, artifact.Path), artifact.Sha256, artifact.Bytes, artifactContentType(artifact.Path))
		if err != nil {
			return nil, err
		}
		retention.Objects = append(retention.Objects, *retained)
		retention.Bytes += retained.Bytes
		retention.ComplianceObjectLock = retention.ComplianceObjectLock && retained.Mode == "COMPLIANCE"
	}
	retention.ObjectCount = len(retention.Objects)
	document.Retention = retention
	content, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "urnetwork-baseline-preparation-manifest-*.json")
	if err != nil {
		return nil, err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	_, writeErr := file.Write(content)
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(content)
	if _, err := self.retainFile(ctx, settings, manifestName, retention.ManifestKey, temporary, hex.EncodeToString(hash[:]), int64(len(content)), "application/json"); err != nil {
		return nil, err
	}
	return content, nil
}
