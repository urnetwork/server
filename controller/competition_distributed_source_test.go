// Synthetic fixtures use the actual trusted preparer/source-builder wire shapes.
// No checkout, Docker daemon, production identity or mutable source tree is used.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestCompetitionDistributedMeasuredGraphUsesCanonicalTenRepositoryContract(t *testing.T) {
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = strings.Repeat("a", 40)
	}
	if len(commits) != 10 || validateDistributedMeasuredCommits(commits) != nil {
		t.Fatal("the canonical ten-repository graph was reduced by a second obsolete-repository subtraction")
	}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		delete(commits, repository)
		if validateDistributedMeasuredCommits(commits) == nil {
			t.Fatal("an absent measured product repository was accepted", repository)
		}
		commits[repository] = strings.Repeat("a", 40)
	}
	commits["operator-proxy"] = strings.Repeat("b", 40)
	if validateDistributedMeasuredCommits(commits) == nil {
		t.Fatal("an operator control repository entered the measured product graph")
	}
}

// Rehashing malicious inventories proves semantic source checks, not merely
// rejection caused by a changed outer file digest or an invented protocol.
func TestCompetitionDistributedSourcesVerifyAllTenFrozenHeadsAndLifecycle(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*competitionDistributedSourceEvidence)
	}{
		{name: "valid", mutate: func(*competitionDistributedSourceEvidence) {}},
		{name: "wrong baseline head", mutate: func(e *competitionDistributedSourceEvidence) {
			e.Baseline.Repositories["connect"] = strings.Repeat("e", 40)
		}},
		{name: "operator repository", mutate: func(e *competitionDistributedSourceEvidence) {
			e.Baseline.Repositories["operator-proxy"] = strings.Repeat("a", 40)
		}},
		{name: "source generation conflation", mutate: func(e *competitionDistributedSourceEvidence) { e.Baseline.SourceEpoch = 2 }},
		{name: "mutable host checkout", mutate: func(e *competitionDistributedSourceEvidence) { e.HostRepositoriesUsed = true }},
		{name: "writable source mount", mutate: func(e *competitionDistributedSourceEvidence) { e.ReadOnlyMount = false }},
		{name: "missing cleanup", mutate: func(e *competitionDistributedSourceEvidence) { e.CleanupComplete = false }},
		{name: "different patch", mutate: func(e *competitionDistributedSourceEvidence) {
			patch := strings.Repeat("f", 64)
			e.Candidate.CandidatePatchSha256 = &patch
		}},
		{name: "candidate build-map mismatch", mutate: func(e *competitionDistributedSourceEvidence) {
			e.Candidate.Repositories["sdk"] = strings.Repeat("e", 40)
		}},
	} {
		settings := validSettings()
		settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings()}}
		settings.distributedSourceCommits = map[string]string{}
		for _, repository := range model.CompetitionSourceRepositoryNames() {
			settings.distributedSourceCommits[repository] = settings.BaseSha
		}
		copyCommits := func(commits map[string]string) map[string]string {
			copy := map[string]string{}
			for repository, commit := range commits {
				copy[repository] = commit
			}
			return copy
		}
		patch := strings.Repeat("c", 64)
		job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), EvaluatorImageDigest: settings.EvaluatorImageDigest, PatchSha256: patch},
			Round: roundRecord{RoundResult: RoundResult{Epoch: 2}}}
		baseline := competitionDistributedSourceIdentity{Schema: 1, Kind: "sim-latency-evaluation-source", Temporary: true, BaseImageId: settings.EvaluatorImageDigest,
			BaseSha: settings.BaseSha, SourceEpoch: 1, Branch: "sim-latency", SourceLockSha256: strings.Repeat("b", 64), Repositories: copyCommits(settings.distributedSourceCommits)}
		candidate := baseline
		candidate.Repositories = copyCommits(baseline.Repositories)
		candidate.Repositories["server"] = strings.Repeat("d", 40)
		candidate.CandidatePatchSha256 = &patch
		buildCommits := copyCommits(candidate.Repositories)
		originalBytes, err := json.MarshalIndent(baseline, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		originalBytes = append(originalBytes, '\n')
		originalDigest := sha256.Sum256(originalBytes)
		commits, err := json.MarshalIndent(buildCommits, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		commitsDigest := sha256.Sum256(append(commits, '\n'))
		evidence := competitionDistributedSourceEvidence{Schema: 1, Kind: "sim-latency-temporary-evaluation-sources", Origin: "authenticated_evaluator_image",
			RuntimeTarget: "/workspace", ReadOnlyMount: true, Baseline: baseline, Candidate: candidate, CleanupComplete: true}
		change.mutate(&evidence)
		evidenceBytes, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		buildBytes, err := json.Marshal(map[string]any{"schema": 1, "base_image_id": settings.EvaluatorImageDigest, "base_sha": settings.BaseSha,
			"builder_sha256": strings.Repeat("a", 64), "candidate_commits": buildCommits, "candidate_commits_sha256": hex.EncodeToString(commitsDigest[:]), "candidate_sha": buildCommits["server"],
			"image": "synthetic/submission:immutable", "image_id": "sha256:" + strings.Repeat("d", 64), "image_key": strings.Repeat("e", 64), "original_source_identity_sha256": hex.EncodeToString(originalDigest[:]),
			"patch_sha256": patch, "patch_validator_sha256": strings.Repeat("f", 64), "policy_sha256": strings.Repeat("a", 64)})
		if err != nil {
			t.Fatal(err)
		}
		directory := t.TempDir()
		artifacts := []evaluationArtifact{}
		for _, item := range []struct {
			name    string
			content []byte
		}{
			{name: "evidence/evaluation-sources.json", content: evidenceBytes},
			{name: "evidence/candidate-original-source.json", content: originalBytes},
			{name: "evidence/candidate-build.json", content: buildBytes},
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, item.name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeExclusiveFile(filepath.Join(directory, item.name), item.content, 0400); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(item.content)
			artifacts = append(artifacts, evaluationArtifact{Path: item.name, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(item.content))})
		}
		err = authenticateDistributedCompetitionSources(settings, job, directory, artifacts, true)
		if change.name == "valid" && err != nil {
			t.Fatal("actual trusted source schema was rejected", err)
		}
		if change.name != "valid" && err == nil {
			t.Errorf("semantically altered source proof was accepted: %s", change.name)
		}
	}
}
