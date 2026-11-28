// Synthetic baseline/private script identities exercise the actual dispatch
// manifest boundary without a database, agent process or production fixtures.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
)

// The pre-fix manifest selected the producer digest for both private hosts.
func TestCompetitionPrivateEvaluatorManifestKeepsProducerIndependent(t *testing.T) {
	settings := validSettings()
	settings.EvaluatorCommandSha256 = strings.Repeat("a", 64)
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings(), Hosts: []competitionExecutorConnection{
		{HostId: "local.example", EvaluatorCommandSha256: strings.Repeat("b", 64), AgentConfigSnapshotPath: "/protected/local-role.json"},
		{HostId: "remote.example", EvaluatorCommandSha256: strings.Repeat("c", 64), AgentConfigSnapshotPath: "/protected/remote-role.json"},
	}}}
	started := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	deadline := started.Add(3 * time.Hour)
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), StartedAt: &started},
		Round: roundRecord{RoundResult: RoundResult{Epoch: 2}}, AttemptCount: 1, ExecutionDeadline: &deadline, RoundBaseline: &roundBaselineRecord{Sha256: strings.Repeat("d", 64)}}
	for _, connection := range settings.distributedCoordinator.Config.Hosts {
		manifest, err := competitionAgentManifestForJob(settings, job, CompetitionExecutorHostPolicy{HostId: connection.HostId})
		if err != nil || manifest.EvaluatorCommandSha256 != connection.EvaluatorCommandSha256 || manifest.EvaluatorCommandSha256 == settings.EvaluatorCommandSha256 {
			t.Fatal("private dispatch reused the baseline producer's script", err)
		}
		if !manifest.Identity.StartedAt.Equal(started) || !manifest.Identity.DeadlineAt.Equal(deadline) || manifest.RoundBaselineSha256 != job.RoundBaseline.Sha256 {
			t.Fatal("script selection changed an immutable clock or baseline")
		}
	}
	if settings.EvaluatorCommandSha256 != strings.Repeat("a", 64) {
		t.Fatal("private dispatch rewrote the baseline producer")
	}
}

// The whole snapshot, not a self-reported new digest, authenticates selection.
func TestCompetitionPrivateEvaluatorSnapshotAuthenticatesWholeRole(t *testing.T) {
	content, err := json.Marshal(map[string]any{"host_id": "remote.example", "evaluator": map[string]string{"path": "/protected/evaluator", "sha256": strings.Repeat("b", 64)}, "state_directory": "/protected/state"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	connection := competitionExecutorConnection{HostId: "remote.example", AgentConfigSha256: hex.EncodeToString(digest[:]), EvaluatorCommandSha256: strings.Repeat("b", 64)}
	if err := validateCompetitionExecutorEvaluatorSnapshot(connection, content); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*competitionExecutorConnection){
		func(value *competitionExecutorConnection) { value.HostId = "another.example" },
		func(value *competitionExecutorConnection) { value.AgentConfigSha256 = strings.Repeat("c", 64) },
		func(value *competitionExecutorConnection) { value.EvaluatorCommandSha256 = strings.Repeat("a", 64) },
	} {
		changed := connection
		change(&changed)
		if validateCompetitionExecutorEvaluatorSnapshot(changed, content) == nil {
			t.Fatal("unbound host, snapshot or evaluator accepted")
		}
	}
	if validateCompetitionExecutorEvaluatorSnapshot(connection, append(content, '\n', '{')) == nil {
		t.Fatal("changed snapshot bytes accepted")
	}
}

// Legacy same-script deployments retain their former manifest, not a newly
// invented private pin. Partially configured roles must fail before seed I/O.
func TestCompetitionPrivateEvaluatorMissingPinCannotReachWorkload(t *testing.T) {
	settings := validSettings()
	settings.EvaluatorCommandSha256 = strings.Repeat("a", 64)
	settings.ArtifactRoot = t.TempDir()
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Hosts: []competitionExecutorConnection{{HostId: "local.example"}}}}
	value, err := competitionExecutorEvaluatorCommandSha256(settings, "local.example")
	if err != nil || value != settings.EvaluatorCommandSha256 {
		t.Fatal("historical same-script role changed", err)
	}
	for _, connection := range []competitionExecutorConnection{
		{HostId: "local.example", EvaluatorCommandSha256: strings.Repeat("b", 64)},
		{HostId: "local.example", AgentConfigSnapshotPath: "/protected/role.json"},
		{HostId: "local.example", EvaluatorCommandSha256: "invalid", AgentConfigSnapshotPath: "/protected/role.json"},
	} {
		settings.distributedCoordinator.Config.Hosts = []competitionExecutorConnection{connection}
		if _, _, err := prepareCompetitionAgentBundle(context.Background(), settings, nil, CompetitionExecutorHostPolicy{HostId: "local.example"}); err == nil {
			t.Fatal("incomplete private pin reached workload I/O")
		}
	}
	if _, err := competitionExecutorEvaluatorCommandSha256(settings, "unknown.example"); err == nil {
		t.Fatal("unconfigured host received a producer fallback")
	}
	entries, err := os.ReadDir(settings.ArtifactRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid role created dispatch state", err)
	}
}

// Canonical request signing authenticates the selected private script too.
func TestCompetitionPrivateEvaluatorPinIsPartOfManifestIdentity(t *testing.T) {
	manifest := competitionagent.StartManifest{EvaluatorCommandSha256: strings.Repeat("a", 64)}
	before, err := competitionagent.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluatorCommandSha256 = strings.Repeat("b", 64)
	after, err := competitionagent.ManifestDigest(manifest)
	if err != nil || before == after {
		t.Fatal("script substitution did not change authenticated identity", err)
	}
}
