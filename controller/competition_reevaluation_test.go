// Reevaluation authority stays in the control plane, outside the frozen
// evaluator request; retained artifacts keep their global attempt namespace.
package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A recovered attempt must not add control-plane audit or cycle fields to the
// schema consumed by the separately frozen evaluator image.
func TestCompetitionReevaluationRetainsFrozenEvaluatorRequestSchema(t *testing.T) {
	settings := validSettings()
	settings.CompetitionId = "synthetic-reevaluation-schema"
	settings.ConfigLocalDirectory = filepath.Join(t.TempDir(), "synthetic-config")
	settings.VaultLocalDirectory = filepath.Join(t.TempDir(), "synthetic-vault")
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cycleStartedAt := startedAt.Add(24 * time.Hour)
	jobId, roundId := server.NewId(), server.NewId()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: jobId, RoundId: roundId, StartedAt: &startedAt,
			PatchSha256: strings.Repeat("a", 64), EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest: testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
		AttemptCount: 4, EvaluationCycle: 1, CycleAttemptOffset: 3, CycleStartedAt: &cycleStartedAt,
		Round: roundRecord{RoundResult: RoundResult{
			RoundId: roundId, Epoch: 1, ProvidersSha256: strings.Repeat("b", 64),
		}},
		RoundBaseline: &roundBaselineRecord{RoundId: roundId, Sha256: strings.Repeat("c", 64)},
	}
	baseline := *job.RoundBaseline
	attemptDirectory := filepath.Join(t.TempDir(), "synthetic-attempt-04")
	request := evaluatorRequestForJob(settings, job, strings.Repeat("d", 64), attemptDirectory,
		filepath.Join(attemptDirectory, "canonical.patch"), filepath.Join(attemptDirectory, "providers.yml"))
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	actualNames := make([]string, 0, len(fields))
	for name := range fields {
		actualNames = append(actualNames, name)
	}
	expectedNames := []string{
		"api_image_digest", "artifact_directory", "attempt", "base_sha", "competition_id", "config_local_directory",
		"evaluation_policy", "evaluator_image_digest", "job_id", "patch_path", "patch_policy", "patch_sha256",
		"providers_path", "providers_sha256", "round_baseline_path", "round_baseline_sha256", "round_id",
		"round_seed_hex", "schema", "scorer_version", "source_epoch", "vault_local_directory", "worker_image_digest",
	}
	slices.Sort(actualNames)
	slices.Sort(expectedNames)
	if !slices.Equal(actualNames, expectedNames) {
		t.Fatalf("reevaluation changed the frozen evaluator request fields: got %v, want %v", actualNames, expectedNames)
	}
	if request.Schema != 1 || request.Attempt != 4 || request.SourceEpoch != 0 || request.JobId != jobId.String() ||
		request.RoundId != roundId.String() || request.RoundBaselineSha256 != baseline.Sha256 ||
		request.RoundBaselinePath != filepath.Join(attemptDirectory, "round-baseline.json") ||
		job.StartedAt == nil || !job.StartedAt.Equal(startedAt) || !reflect.DeepEqual(*job.RoundBaseline, baseline) {
		t.Fatal("request construction changed retained attempt, original start, round, or baseline identity")
	}
	archive := &blobArtifactArchive{store: server.NewLocalBlobStore(t.TempDir(), "synthetic-reevaluation-artifacts").(server.RetainedBlobStore)}
	if prefix := archive.attemptPrefix(settings, job); filepath.Base(prefix) != "attempt-04" {
		t.Fatalf("recovered artifact namespace stopped using global attempts: %q", prefix)
	}
}

// Produces a wholly synthetic authenticated projection; no real release,
// incident, account, evidence or authorization identifier enters a test.
func newCompetitionReevaluationCycleTestFixture(t testing.TB) model.CompetitionJobReevaluationCycle {
	t.Helper()
	cycle := model.CompetitionJobReevaluationCycle{
		Schema: 1, Environment: "synthetic-stage", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		JobId: server.NewId(), RoundId: server.NewId(), Cycle: 1,
		PatchSha256: strings.Repeat("1", 64), RoundPolicySha256: strings.Repeat("2", 64), BaselineSha256: strings.Repeat("3", 64),
		AuthorizationSha256: strings.Repeat("4", 64), EvidenceSha256: strings.Repeat("5", 64), PreviousTerminalSha256: strings.Repeat("6", 64),
		PreviousFailure:            model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "artifact_archive_failed", Retriable: false},
		RuntimeQualificationSha256: strings.Repeat("7", 64), FixedSourceCommit: strings.Repeat("8", 40),
		FixedWorkerSha256: strings.Repeat("9", 64), FixedWorkerImageDigest: "sha256:" + strings.Repeat("a", 64), ApiImageDigest: "sha256:" + strings.Repeat("b", 64),
		AuthorizedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ExecutionTimeoutSeconds: 3 * 60 * 60,
	}
	var err error
	cycle.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(cycle)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err != nil {
		t.Fatalf("valid synthetic reevaluation cycle: %v", err)
	}
	return cycle
}

// A receipt binds exact scope, incident evidence and independently qualified
// fixed runtime; changing any one without a new receipt is not exact replay.
func TestCompetitionReevaluationCycleDigestBindsScopeEvidenceAndRuntime(t *testing.T) {
	before := newCompetitionReevaluationCycleTestFixture(t)
	for _, change := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationCycle)
	}{
		{name: "environment", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.Environment = "synthetic-other" }},
		{name: "competition", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.CompetitionId = "synthetic-other" }},
		{name: "season", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.SeasonId = "synthetic-other" }},
		{name: "job", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.JobId = server.NewId() }},
		{name: "round", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.RoundId = server.NewId() }},
		{name: "patch", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.PatchSha256 = strings.Repeat("c", 64) }},
		{name: "round-policy", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.RoundPolicySha256 = strings.Repeat("c", 64) }},
		{name: "baseline", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.BaselineSha256 = strings.Repeat("c", 64) }},
		{name: "authorization", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.AuthorizationSha256 = strings.Repeat("c", 64) }},
		{name: "evidence", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.EvidenceSha256 = strings.Repeat("c", 64) }},
		{name: "terminal-snapshot", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.PreviousTerminalSha256 = strings.Repeat("c", 64) }},
		{name: "qualification", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.RuntimeQualificationSha256 = strings.Repeat("c", 64) }},
		{name: "fixed-source", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.FixedSourceCommit = strings.Repeat("c", 40) }},
		{name: "fixed-worker", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.FixedWorkerSha256 = strings.Repeat("c", 64) }},
		{name: "worker-image", mutate: func(c *model.CompetitionJobReevaluationCycle) {
			c.FixedWorkerImageDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{name: "api-image", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.ApiImageDigest = "sha256:" + strings.Repeat("c", 64) }},
		{name: "previous-failure", mutate: func(c *model.CompetitionJobReevaluationCycle) {
			c.PreviousFailure.Code = "evaluation_time_budget_exhausted"
		}},
		{name: "authorized-at", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.AuthorizedAt = c.AuthorizedAt.Add(time.Second) }},
	} {
		cycle := before
		change.mutate(&cycle)
		if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err == nil {
			t.Fatalf("changed %s reused an immutable cycle receipt", change.name)
		}
	}
}

// Even a self-consistent digest cannot widen the one-cycle, three-hour,
// terminal infrastructure-only projection accepted from the trusted api.
func TestCompetitionReevaluationCycleRejectsExpandedAuthority(t *testing.T) {
	before := newCompetitionReevaluationCycleTestFixture(t)
	for _, change := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationCycle)
	}{
		{name: "schema", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.Schema = 2 }},
		{name: "second-cycle", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.Cycle = 2 }},
		{name: "unscoped", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.SeasonId = "" }},
		{name: "missing-evidence", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.EvidenceSha256 = "" }},
		{name: "missing-qualification", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.RuntimeQualificationSha256 = "" }},
		{name: "mutable-worker-tag", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.FixedWorkerImageDigest = "synthetic-worker:latest" }},
		{name: "longer-budget", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.ExecutionTimeoutSeconds++ }},
		{name: "genuine-submission-failure", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.PreviousFailure.Kind = "submission" }},
		{name: "unknown-cause", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.PreviousFailure.Code = "synthetic_unknown_failure" }},
		{name: "live-retry", mutate: func(c *model.CompetitionJobReevaluationCycle) { c.PreviousFailure.Retriable = true }},
		{name: "predated-start", mutate: func(c *model.CompetitionJobReevaluationCycle) {
			start := c.AuthorizedAt.Add(-time.Second)
			c.StartedAt = &start
		}},
	} {
		cycle := before
		change.mutate(&cycle)
		var err error
		cycle.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(cycle)
		if err != nil {
			t.Fatalf("digest synthetic %s fixture: %v", change.name, err)
		}
		if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err == nil {
			t.Fatalf("self-consistent %s expanded recovery authority", change.name)
		}
	}
}

// Authorization never invents an execution start. The first claim adds its
// start without rewriting the immutable receipt or borrowing the old start.
func TestCompetitionReevaluationCycleFirstClaimPreservesReceipt(t *testing.T) {
	cycle := newCompetitionReevaluationCycleTestFixture(t)
	receiptSha256 := cycle.ReceiptSha256
	startedAt := cycle.AuthorizedAt.Add(time.Hour)
	cycle.StartedAt = &startedAt
	digest, err := model.CompetitionJobReevaluationCycleDigest(cycle)
	if err != nil || digest != receiptSha256 || model.ValidateCompetitionJobReevaluationCycle(&cycle) != nil {
		t.Fatalf("first claim changed immutable cycle receipt: %q, %v", digest, err)
	}
}

// Equivalent jsonb objects have one identity; duplicate keys, arrays, scalars
// and trailing documents must not manufacture ambiguous terminal snapshots.
func TestCompetitionReevaluationSnapshotDigestRejectsAmbiguousRows(t *testing.T) {
	first, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(`{"attempt_count":3,"state":"failed","nested":{"a":1,"b":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage("{\n \"nested\": {\"b\":2,\"a\":1}, \"state\":\"failed\", \"attempt_count\":3\n}"))
	if err != nil || second != first {
		t.Fatalf("equivalent jsonb snapshots had different identities: %q, %q, %v", first, second, err)
	}
	for _, value := range []string{
		`null`, `[]`, `"synthetic"`, `{"state":"failed","state":"queued"}`,
		`{"nested":{"attempt":3,"attempt":4}}`, `{"state":"failed"}{"state":"queued"}`,
	} {
		if digest, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(value)); err == nil || digest != "" {
			t.Fatalf("ambiguous terminal snapshot was authenticated: value=%q digest=%q error=%v", value, digest, err)
		}
	}
}

// Qualification binds exact independently approved bytes, not a format-valid
// caller-chosen hash or an otherwise compatible but changed runtime receipt.
func TestCompetitionReevaluationRuntimeBindingRequiresExactQualifiedDigest(t *testing.T) {
	settings := validSettings()
	proof := model.CompetitionJobReevaluationRuntime{
		Schema: 1, Kind: model.CompetitionJobReevaluationRuntimeKind, Accepted: true, Capability: model.CompetitionJobReevaluationCapability,
		SourceCommit: strings.Repeat("1", 40), ApiImageDigest: "sha256:" + strings.Repeat("2", 64), WorkerImageDigest: "sha256:" + strings.Repeat("3", 64),
		WorkerSha256: strings.Repeat("4", 64), EvidenceSha256: strings.Repeat("5", 64), ApiCompatible: true, WorkerCompatible: true,
	}
	digest, err := model.CompetitionAutomationDigest(proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindCompetitionJobReevaluationRuntime(settings, proof, digest); err != nil {
		t.Fatalf("bind exact synthetic qualification: %v", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationRuntime)
	}{
		{name: "acceptance", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.Accepted = false }},
		{name: "api-compatibility", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.ApiCompatible = false }},
		{name: "worker-compatibility", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.WorkerCompatible = false }},
		{name: "capability", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.Capability = "synthetic-other" }},
		{name: "source", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.SourceCommit = strings.Repeat("6", 40) }},
		{name: "api-image", mutate: func(p *model.CompetitionJobReevaluationRuntime) {
			p.ApiImageDigest = "sha256:" + strings.Repeat("6", 64)
		}},
		{name: "worker-image", mutate: func(p *model.CompetitionJobReevaluationRuntime) {
			p.WorkerImageDigest = "sha256:" + strings.Repeat("6", 64)
		}},
		{name: "worker-elf", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.WorkerSha256 = strings.Repeat("6", 64) }},
		{name: "evidence", mutate: func(p *model.CompetitionJobReevaluationRuntime) { p.EvidenceSha256 = strings.Repeat("6", 64) }},
	} {
		changed := proof
		change.mutate(&changed)
		if err := BindCompetitionJobReevaluationRuntime(settings, changed, digest); err == nil {
			t.Fatalf("changed %s reused another runtime's qualification digest", change.name)
		}
		if settings.reevaluationRuntime == nil || !reflect.DeepEqual(*settings.reevaluationRuntime, proof) || settings.reevaluationRuntimeSha256 != digest {
			t.Fatalf("rejected %s changed the previously bound fixed runtime", change.name)
		}
	}
	if err := BindCompetitionJobReevaluationRuntime(settings, proof, strings.Repeat("e", 64)); err == nil {
		t.Fatal("format-valid wrong qualification digest authenticated a runtime")
	}
}

// A typed claim fence snapshots values rather than a mutable job pointer, and
// cannot leak into its unfenced parent or borrow a later claim's authority.
func TestCompetitionReevaluationClaimContextSnapshotsCycleAndAttempt(t *testing.T) {
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId()}, EvaluationCycle: 1, AttemptCount: 4}
	parent := t.Context()
	ctx := withCompetitionJobAuthority(parent, job)
	job.EvaluationCycle, job.AttemptCount = 2, 5
	authority, present := ctx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
	if !present || authority.JobId != job.JobId || authority.Cycle != 1 || authority.Attempt != 4 || parent.Value(competitionJobAuthorityKey{}) != nil {
		t.Fatal("claim context followed a mutated job or changed its unfenced parent")
	}
	detached := context.WithoutCancel(ctx)
	if actual, present := detached.Value(competitionJobAuthorityKey{}).(competitionJobAuthority); !present || actual != authority {
		t.Fatal("detached bounded terminal persistence lost the exact original claim fence")
	}
}

// The actual worker uses one fresh cycle deadline, not the old terminal start,
// while preserving that original start and the global attempt counter.
func TestCompetitionReevaluationWorkerUsesOneFreshCycleBudget(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	startedAt := fixture.clock.Now().Add(-24 * time.Hour)
	cycleStartedAt := fixture.clock.Now().Add(-time.Minute)
	fixture.job.StartedAt = &startedAt
	fixture.job.EvaluationCycle, fixture.job.CycleAttemptOffset = 1, 3
	fixture.job.CycleStartedAt, fixture.job.AttemptCount = &cycleStartedAt, 4
	evaluated := false
	var observedDeadline time.Time
	var deadlinePresent bool
	fixture.evaluator.evaluate = func(ctx context.Context) EvaluationOutcome {
		observedDeadline, deadlinePresent = ctx.Deadline()
		evaluated = true
		return fixture.evaluator.outcome
	}
	if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); err != nil {
		t.Fatalf("old terminal start suppressed explicitly recovered evaluation: %v", err)
	}
	if !evaluated || fixture.evaluator.calls != 1 || fixture.store.calls != 1 || fixture.store.completedCount() != 1 ||
		!deadlinePresent || !observedDeadline.Equal(cycleStartedAt.Add(3*time.Hour)) ||
		fixture.job.AttemptCount != 4 || fixture.job.CycleAttemptOffset != 3 || !fixture.job.StartedAt.Equal(startedAt) ||
		!fixture.job.CycleStartedAt.Equal(cycleStartedAt) {
		t.Fatal("recovery refreshed an attempt, deadline, outcome, or original execution start")
	}
}

// A later recovered attempt cannot borrow another execution budget after the
// cycle's three-hour boundary, even when its worker lease remains live.
func TestCompetitionReevaluationWorkerDoesNotRefreshExpiredCycleBudget(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	startedAt := fixture.clock.Now().Add(-48 * time.Hour)
	cycleStartedAt := fixture.clock.Now().Add(-3*time.Hour - time.Minute)
	fixture.job.StartedAt = &startedAt
	fixture.job.EvaluationCycle, fixture.job.CycleAttemptOffset = 1, 3
	fixture.job.CycleStartedAt, fixture.job.AttemptCount = &cycleStartedAt, 5
	fixture.store.complete = func(ctx context.Context, _ int, outcome EvaluationOutcome) (bool, error) {
		if ctx.Err() != nil || outcome.Score != nil || outcome.Error == nil || outcome.Error.Retriable ||
			outcome.Error.Code != "evaluation_time_budget_exhausted" {
			t.Fatal("expired recovered cycle gained evaluator authority or lost bounded terminal persistence")
		}
		return false, nil
	}
	if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); err != nil || fixture.evaluator.calls != 0 ||
		fixture.store.calls != 1 || fixture.job.AttemptCount != 5 || !fixture.job.StartedAt.Equal(startedAt) ||
		!fixture.job.CycleStartedAt.Equal(cycleStartedAt) {
		t.Fatalf("expired recovered attempt was granted a new cycle or lost its original identity: %v", err)
	}
}
