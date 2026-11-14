//go:build linux

// Tests own only private temporary directories and fake metadata stores. No
// live controller, native mount, candidate, database or retained object changes.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
)

type competitionReceiveCleanupFixture struct {
	root, directory string
	job             *queuedJob
	identity        competitionagent.Identity
	custody         *competitionReceiveCustody
	response        competitionagent.Response
	outcome         EvaluationOutcome
	settings        competitionReceiveCleanupSettings
}

// Creation capture precedes population. Every identity and object version is
// synthetic, and the file inventory is signed-peer-shaped, not a host capture.
func newCompetitionReceiveCleanupFixture(t *testing.T) *competitionReceiveCleanupFixture {
	return newCompetitionReceiveCleanupCycleFixture(t, 0)
}

// A recovery cycle keeps both original submission and authorized cycle clocks.
func newCompetitionReceiveCleanupCycleFixture(t *testing.T, cycle int) *competitionReceiveCleanupFixture {
	return newCompetitionReceiveCleanupInputFixture(t, cycle, nil)
}

// Controller input pins are copied at creation, before any received bytes.
func newCompetitionReceiveCleanupInputFixture(t *testing.T, cycle int, inputs []competitionagent.File) *competitionReceiveCleanupFixture {
	t.Helper()
	now := time.Now().UTC()
	started, lease := now.Add(-time.Minute), now.Add(time.Minute)
	deadline := started.Add(time.Duration(submissionEvaluationTimeoutSeconds) * time.Second)
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), StartedAt: &started},
		AttemptCount: 1, ExecutorHostId: "synthetic-receive-host", SlotId: 1, SlotFencingToken: 4, ExecutionDeadline: &deadline, LeaseExpiresAt: &lease}
	executionStart := started
	if cycle > 0 {
		original := now.Add(-2 * time.Hour)
		job.StartedAt, job.CycleStartedAt, job.EvaluationCycle, job.CycleAttemptOffset, job.AttemptCount = &original, &started, cycle, 1, 2
	}
	identity := competitionagent.Identity{HostId: job.ExecutorHostId, RunId: competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount), JobId: job.JobId.String(), RoundId: job.RoundId.String(),
		RoundEpoch: 2, SourceEpoch: 2, Cycle: job.EvaluationCycle, Attempt: job.AttemptCount, StartedAt: executionStart, DeadlineAt: deadline}
	root := t.TempDir()
	directory, custody, err := createCompetitionReceive(root, job, identity, inputs)
	if err != nil || custody == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { custody.Close() })
	if err := os.MkdirAll(filepath.Join(directory, "raw", "deep"), 0700); err != nil {
		t.Fatal(err)
	}
	content, receipt := []byte("synthetic retained evidence"), []byte("synthetic controller receipt")
	if err := os.WriteFile(filepath.Join(directory, "raw", "deep", "evidence.txt"), content, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "executor-receipt.json"), receipt, 0400); err != nil {
		t.Fatal(err)
	}
	digest, receiptDigest := sha256.Sum256(content), sha256.Sum256(receipt)
	fileSha, receiptSha := hex.EncodeToString(digest[:]), hex.EncodeToString(receiptDigest[:])
	retainedUntil := now.Add(24 * time.Hour)
	manifest := artifactManifest{Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), Attempt: job.AttemptCount,
		Executor:  &competitionExecutorArtifactIdentity{HostId: job.ExecutorHostId, SlotId: job.SlotId, SlotFencingToken: job.SlotFencingToken, RunId: identity.RunId, AgentReceiptSha256: receiptSha},
		Artifacts: []evaluationArtifact{{Path: "raw/deep/evidence.txt", Sha256: fileSha, Bytes: int64(len(content))}, {Path: "executor-receipt.json", Sha256: receiptSha, Bytes: int64(len(receipt))}},
		Retention: &artifactRetention{Backend: "server/blob", Bucket: "synthetic-evidence", ManifestKey: "synthetic/manifest.json", RetainUntil: retainedUntil,
			AuthenticatedAfterUpload: true, ComplianceObjectLock: true, ObjectCount: 2,
			Objects: []retainedArtifact{{Path: "raw/deep/evidence.txt", Sha256: fileSha, Bytes: int64(len(content)), VersionId: "synthetic-version-1", Mode: "COMPLIANCE", RetainUntil: retainedUntil},
				{Path: "executor-receipt.json", Sha256: receiptSha, Bytes: int64(len(receipt)), VersionId: "synthetic-version-2", Mode: "COMPLIANCE", RetainUntil: retainedUntil}}}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	response := competitionagent.Response{Identity: &identity, RunId: identity.RunId, FencingToken: job.SlotFencingToken, Files: []competitionagent.File{{Path: "raw/deep/evidence.txt", Sha256: fileSha, Bytes: int64(len(content))}}}
	outcome := EvaluationOutcome{Score: competitionRankingScore(90, 110), ArtifactManifest: manifestBytes, distributedArchiveVerified: true}
	fixture := &competitionReceiveCleanupFixture{root: root, directory: directory, job: job, identity: identity, custody: custody, response: response, outcome: outcome, settings: defaultCompetitionReceiveCleanupSettings()}
	return fixture
}

func (self *competitionReceiveCleanupFixture) retain(t *testing.T) {
	t.Helper()
	var err error
	self.outcome, err = (&competitionAgentEvaluator{}).retainedReceiveOutcome(self.directory, self.custody, self.response, self.outcome)
	if err != nil || self.outcome.receiveCustody != self.custody {
		t.Fatal("synthetic exact retention did not transfer private custody", err)
	}
}

func (self *competitionReceiveCleanupFixture) assertPreserved(t *testing.T) {
	t.Helper()
	for _, name := range []string{"raw/deep/evidence.txt", "executor-receipt.json"} {
		if _, err := os.Lstat(filepath.Join(self.directory, filepath.FromSlash(name))); err != nil {
			t.Fatal("uncertain cleanup consumed previously retained local evidence", name, err)
		}
	}
}

// A public completion operation acknowledges before housekeeping can start.
type competitionReceiveCleanupStore struct {
	Store
	complete func(context.Context) (bool, error)
}

func (self *competitionReceiveCleanupStore) Complete(ctx context.Context, _ *Settings, _ string, _ server.Id, _ EvaluationOutcome) (bool, error) {
	return self.complete(ctx)
}

func (self *competitionReceiveCleanupFixture) worker(complete func(context.Context) (bool, error)) *Worker {
	settings := defaultCompetitionHeartbeatSettings()
	settings.ReceiveCleanup = self.settings
	return &Worker{store: &competitionReceiveCleanupStore{complete: complete}, workerId: "synthetic-receive-worker", heartbeatSettings: settings}
}

func TestCompetitionReceiveCleanupOnlyAfterTimelyCompletionAck(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.retain(t)
	called := false
	worker := fixture.worker(func(context.Context) (bool, error) { fixture.assertPreserved(t); called = true; return false, nil })
	manifest := string(fixture.outcome.ArtifactManifest)
	if retry, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil || retry || !called {
		t.Fatal("acknowledged completion changed while housekeeping ran", retry, err)
	}
	if _, err := os.Lstat(fixture.directory); !errors.Is(err, os.ErrNotExist) || !fixture.custody.closed || string(fixture.outcome.ArtifactManifest) != manifest {
		t.Fatal("acknowledged exact owned receive was not removed/closed, or manifest changed", err)
	}
}

func TestCompetitionReceiveCleanupRetainedFailureAckDoesNotChangeOutcome(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.outcome.Score, fixture.outcome.Error = nil, submissionError("synthetic_build_failure", "Synthetic retained failure.")
	fixture.retain(t)
	worker := fixture.worker(func(context.Context) (bool, error) { return true, nil })
	if retry, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil || !retry || fixture.outcome.Error.Code != "synthetic_build_failure" {
		t.Fatal("housekeeping changed committed failure/retry semantics", retry, err)
	}
	if _, err := os.Lstat(fixture.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("acknowledged retained original attempt leaked its local copy", err)
	}
}

func TestCompetitionReceiveCleanupCompletionErrorPreservesAndCloses(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.retain(t)
	worker := fixture.worker(func(context.Context) (bool, error) { return false, ErrLeaseLost })
	if _, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("completion failure was hidden by housekeeping", err)
	}
	fixture.assertPreserved(t)
	if !fixture.custody.closed {
		t.Fatal("failed completion leaked held receive descriptors")
	}
}

func TestCompetitionReceiveCleanupLateCompletionAckPreservesAndCloses(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.retain(t)
	now := time.Now()
	worker := fixture.worker(func(context.Context) (bool, error) { now = now.Add(time.Minute); return false, nil })
	worker.heartbeatSettings.CompletionRetry.Now = func() time.Time { return now }
	if _, err := worker.completeWithRetry(t.Context(), fixture.job, now.Add(time.Hour), fixture.outcome); err == nil {
		t.Fatal("late metadata acknowledgement became cleanup authority")
	}
	fixture.assertPreserved(t)
	if !fixture.custody.closed {
		t.Fatal("late completion leaked held receive descriptors")
	}
}

func TestCompetitionReceiveCleanupShutdownAfterAckPreserves(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.retain(t)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker := fixture.worker(func(context.Context) (bool, error) { cancel(); return false, nil })
	if retry, err := worker.completeWithRetry(parent, fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil || retry {
		t.Fatal("post-commit shutdown altered acknowledged completion", err)
	}
	fixture.assertPreserved(t)
	if !fixture.custody.closed {
		t.Fatal("shutdown completion leaked held receive descriptors")
	}
}

func TestCompetitionReceiveCleanupQualificationConsumesBeforeTransferAndClose(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	consumed := false
	evaluator := &competitionAgentEvaluator{afterRetained: func(directory string, _ competitionagent.Response, outcome EvaluationOutcome) error {
		if fixture.custody.closed || outcome.receiveCustody != nil {
			t.Fatal("qualification saw closed or already transferred cleanup authority")
		}
		if _, err := os.ReadFile(filepath.Join(directory, "raw", "deep", "evidence.txt")); err != nil {
			return err
		}
		consumed = true
		return nil
	}}
	outcome, err := evaluator.retainedReceiveOutcome(fixture.directory, fixture.custody, fixture.response, fixture.outcome)
	if err != nil || !consumed || outcome.receiveCustody != fixture.custody {
		t.Fatal("retained qualification did not consume before capability transfer", err)
	}
	outcome.closeReceiveCustody()
	fixture.assertPreserved(t)
}

func TestCompetitionReceiveCleanupQualificationOrArchiveErrorNeverTransfers(t *testing.T) {
	for _, mode := range []string{"qualification", "unverified", "noncompliant", "changed-manifest"} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		evaluator := &competitionAgentEvaluator{}
		if mode == "qualification" {
			evaluator.afterRetained = func(string, competitionagent.Response, EvaluationOutcome) error { return ErrLeaseLost }
		}
		if mode == "unverified" {
			fixture.outcome.distributedArchiveVerified = false
		}
		if mode == "noncompliant" {
			fixture.outcome.ArtifactManifest = json.RawMessage(`{"schema":1}`)
		}
		if mode == "changed-manifest" {
			fixture.outcome.ArtifactManifest = json.RawMessage(`{"schema":2}`)
		}
		outcome, err := evaluator.retainedReceiveOutcome(fixture.directory, fixture.custody, fixture.response, fixture.outcome)
		if outcome.receiveCustody != nil || mode == "qualification" && !errors.Is(err, ErrLeaseLost) {
			t.Fatal("failed qualification/retention transferred housekeeping permission", mode, err)
		}
		fixture.custody.Close()
		fixture.assertPreserved(t)
	}
}

func TestCompetitionReceiveCleanupPreflightRefusesSymlinkAndHardlink(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "extra-directory", "extra-file"} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		fixture.retain(t)
		outside := filepath.Join(t.TempDir(), "synthetic-unrelated")
		if err := os.WriteFile(outside, []byte("synthetic foreign data"), 0400); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "symlink":
			if err := os.Symlink(outside, filepath.Join(fixture.directory, "extra")); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(filepath.Join(fixture.directory, "raw", "deep", "evidence.txt"), filepath.Join(t.TempDir(), "linked")); err != nil {
				t.Fatal(err)
			}
		case "extra-directory":
			if err := os.Mkdir(filepath.Join(fixture.directory, "unexpected"), 0700); err != nil {
				t.Fatal(err)
			}
		case "extra-file":
			if err := os.WriteFile(filepath.Join(fixture.directory, "unexpected.txt"), []byte("synthetic unsigned data"), 0400); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.custody.cleanup(t.Context(), fixture.settings, fixture.job, fixture.outcome); err == nil {
			t.Fatal("unsafe tree acquired deletion authority", mode)
		}
		fixture.assertPreserved(t)
		if _, err := os.Stat(outside); err != nil {
			t.Fatal("cleanup touched data outside its owned child", err)
		}
	}
}

func TestCompetitionReceiveCleanupPreflightRefusesDifferentNativeMount(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.retain(t)
	native := fixture.custody.native.(*competitionReceiveNative)
	actual := native.stat
	native.stat = func(file *os.File) (competitionReceiveFileIdentity, error) {
		identity, err := actual(file)
		if file.Name() == "evidence.txt" {
			identity.mountId++
		}
		return identity, err
	}
	if err := fixture.custody.cleanup(t.Context(), fixture.settings, fixture.job, fixture.outcome); err == nil {
		t.Fatal("different mount was mistaken for an owned regular file")
	}
	fixture.assertPreserved(t)
}

func TestCompetitionReceiveCleanupBoundsAndCancellationPreserveBeforeUnlink(t *testing.T) {
	for _, mode := range []string{"entries", "depth", "path", "bytes", "cancel-before", "cancel-after-preflight"} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		fixture.retain(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		switch mode {
		case "entries":
			fixture.settings.MaximumEntries = 1
		case "depth":
			fixture.settings.MaximumDepth = 1
		case "path":
			fixture.settings.MaximumPathBytes = 1
		case "bytes":
			fixture.settings.MaximumBytes = 1
		case "cancel-before":
			cancel()
		case "cancel-after-preflight":
			fixture.custody.native.(*competitionReceiveNative).beforeDelete = cancel
		}
		if err := fixture.custody.cleanup(ctx, fixture.settings, fixture.job, fixture.outcome); err == nil {
			t.Fatal("cleanup ignored a bound or canceled preflight", mode)
		}
		fixture.assertPreserved(t)
	}
}

func TestCompetitionReceiveCleanupChildAndRootSwapsPreserveOriginalAndReplacement(t *testing.T) {
	for _, mode := range []string{"child", "root"} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		fixture.retain(t)
		native := fixture.custody.native.(*competitionReceiveNative)
		moved := ""
		native.beforeDelete = func() {
			path := fixture.directory
			if mode == "root" {
				path = fixture.root
			}
			moved = path + "-synthetic-moved"
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(moved) })
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "unrelated"), []byte("synthetic unrelated replacement"), 0400); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.custody.cleanup(t.Context(), fixture.settings, fixture.job, fixture.outcome); err == nil {
			t.Fatal("replaced root/child generation authorized cleanup", mode)
		}
		preserved := moved
		if mode == "root" {
			preserved = filepath.Join(moved, filepath.Base(fixture.directory))
		}
		if _, err := os.Stat(filepath.Join(preserved, "raw", "deep", "evidence.txt")); err != nil {
			t.Fatal("path swap consumed original owned data", err)
		}
		path := fixture.directory
		if mode == "root" {
			path = fixture.root
		}
		if _, err := os.Stat(filepath.Join(path, "unrelated")); err != nil {
			t.Fatal("path swap consumed unrelated replacement data", err)
		}
	}
}

func TestCompetitionReceiveCleanupChangedFenceOrManifestDoesNotChangeDbSuccess(t *testing.T) {
	for _, mode := range []string{"fence", "manifest", "retention-expired"} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		fixture.retain(t)
		if mode == "fence" {
			fixture.job.SlotFencingToken++
		}
		if mode == "manifest" {
			fixture.outcome.ArtifactManifest = append(fixture.outcome.ArtifactManifest, byte(' '))
		}
		if mode == "retention-expired" {
			fixture.settings.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
		}
		worker := fixture.worker(func(context.Context) (bool, error) { return false, nil })
		if retry, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil || retry {
			t.Fatal("optional cleanup refusal changed committed outcome", mode, err)
		}
		fixture.assertPreserved(t)
	}
}

func TestCompetitionReceiveCleanupCannotAdoptPopulatedOrUnrelatedDirectory(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	if owner, err := captureCompetitionReceiveCustody(fixture.root, fixture.directory, fixture.job, fixture.identity); err == nil {
		owner.Close()
		t.Fatal("existing populated directory was adopted as new custody")
	}
	other, err := os.MkdirTemp(fixture.root, "unrelated-")
	if err != nil {
		t.Fatal(err)
	}
	if owner, err := captureCompetitionReceiveCustody(fixture.root, other, fixture.job, fixture.identity); err == nil {
		owner.Close()
		t.Fatal("unrelated directory acquired future receive cleanup permission")
	}
	if strings.HasPrefix(filepath.Base(other), ".executor-receive-") {
		t.Fatal("synthetic unrelated directory overlapped receive namespace")
	}
}

// Authorized reevaluation has two immutable clocks. Successful housekeeping
// uses the captured cycle start without rewriting the submission's start.
func TestCompetitionReceiveCleanupExactCycleAckPreservesBothOriginalClocks(t *testing.T) {
	fixture := newCompetitionReceiveCleanupCycleFixture(t, 1)
	fixture.retain(t)
	original, cycle := *fixture.job.StartedAt, *fixture.job.CycleStartedAt
	worker := fixture.worker(func(context.Context) (bool, error) { return false, nil })
	if _, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(fixture.directory); !errors.Is(err, os.ErrNotExist) || !fixture.job.StartedAt.Equal(original) || !fixture.job.CycleStartedAt.Equal(cycle) {
		t.Fatal("same-cycle cleanup changed a clock or retained its acknowledged copy", err)
	}
}

// A successful metadata call never substitutes a later attempt, cycle,
// original deadline or source identity for the one captured at creation.
func TestCompetitionReceiveCleanupRejectsChangedAttemptCycleAndClock(t *testing.T) {
	for _, mode := range []string{"job", "round", "host", "slot", "attempt", "cycle", "offset", "start", "cycle-start", "deadline"} {
		fixture := newCompetitionReceiveCleanupCycleFixture(t, 1)
		fixture.retain(t)
		switch mode {
		case "job":
			fixture.job.JobId = server.NewId()
		case "round":
			fixture.job.RoundId = server.NewId()
		case "host":
			fixture.job.ExecutorHostId = "synthetic-other-receive-host"
		case "slot":
			fixture.job.SlotId++
		case "attempt":
			fixture.job.AttemptCount++
		case "cycle":
			fixture.job.EvaluationCycle++
		case "offset":
			fixture.job.CycleAttemptOffset++
		case "start":
			changed := fixture.job.StartedAt.Add(time.Second)
			fixture.job.StartedAt = &changed
		case "cycle-start":
			changed := fixture.job.CycleStartedAt.Add(time.Second)
			fixture.job.CycleStartedAt = &changed
		case "deadline":
			changed := fixture.job.ExecutionDeadline.Add(time.Second)
			fixture.job.ExecutionDeadline = &changed
		}
		worker := fixture.worker(func(context.Context) (bool, error) { return false, nil })
		if _, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil {
			t.Fatal("optional refusal changed acknowledged metadata result", err)
		}
		fixture.assertPreserved(t)
	}
}

// The evaluator retains its already-authenticated outcome until the worker
// explicitly cancels/joins it; no transport or scoring work is performed.
type competitionReceiveHandbackEvaluator struct {
	outcome EvaluationOutcome
	started chan struct{}
}

func (self *competitionReceiveHandbackEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	return HostSelfCheck{}, nil
}

func (self *competitionReceiveHandbackEvaluator) Evaluate(ctx context.Context, _ *Settings, _ *queuedJob) EvaluationOutcome {
	close(self.started)
	<-ctx.Done()
	return self.outcome
}

// Explicit public Store boundaries observe that creation descriptors close
// before custody handback, and that no completion transaction was attempted.
type competitionReceiveHandbackStore struct {
	fakeStore
	custody          *competitionReceiveCustody
	reason           string
	closedAtHandback bool
}

func (self *competitionReceiveHandbackStore) Heartbeat(context.Context, *Settings, string, server.Id) error {
	return ErrLeaseLost
}

func (self *competitionReceiveHandbackStore) HandBack(_ context.Context, _ string, _ server.Id, reason string) error {
	self.reason, self.closedAtHandback = reason, self.custody.closed
	return nil
}

func TestCompetitionReceiveCleanupJoinedWorkerHandbackPreservesAndCloses(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		fixture.retain(t)
		settings := validSettings()
		if fixture.identity.DeadlineAt.Sub(fixture.identity.StartedAt) != time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second {
			t.Fatal("synthetic receive clock differs from the frozen worker evaluation duration")
		}
		evaluator := &competitionReceiveHandbackEvaluator{outcome: fixture.outcome, started: make(chan struct{})}
		store := &competitionReceiveHandbackStore{custody: fixture.custody}
		worker, err := newWorkerWithImageDigest(settings, store, evaluator, "synthetic-receive-handback-worker", testWorkerImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		ticks := make(chan time.Time, 1)
		worker.evaluationTickForTest = ticks
		parent, cancel := context.WithCancel(t.Context())
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 5*time.Second)
		done := make(chan error, 1)
		started := false
		defer func() {
			cancel()
			if started {
				select {
				case <-done:
				case <-watchdog.Done():
				}
			}
			stopWatchdog()
		}()
		host := HostSelfCheck{RebaselinePassed: true, RebaselineRoundId: &fixture.job.RoundId}
		go func() { done <- worker.evaluateOne(parent, fixture.job, host) }()
		select {
		case <-evaluator.started:
			started = true
		case <-watchdog.Done():
			t.Fatal("worker did not dispatch its synthetic evaluator")
		}
		if shutdown {
			cancel()
		} else {
			ticks <- time.Now()
		}
		select {
		case err := <-done:
			started = false
			if err == nil {
				t.Fatal("handed-back worker did not retain its original failure")
			}
		case <-watchdog.Done():
			t.Fatal("worker did not join custody relinquishment")
		}
		if !store.closedAtHandback || !fixture.custody.closed || len(store.completed) != 0 || store.reason == "" {
			t.Fatal("handback leaked creation descriptors or authorized completion/cleanup")
		}
		fixture.assertPreserved(t)
	}
}

// Retained files require correspondence across all three independent shapes:
// signed transport, archived manifest and exact immutable object version.
func TestCompetitionReceiveCleanupRefusesIncompleteRetentionCorrespondence(t *testing.T) {
	for _, mode := range []string{"artifact-hash", "artifact-bytes", "object-hash", "object-bytes", "object-missing", "object-duplicate", "object-unbound", "signed-hash", "signed-bytes", "receipt-hash", "request-hash", "request-self-repeated"} {
		fixture := newCompetitionReceiveCleanupFixture(t)
		manifest := artifactManifest{}
		if err := json.Unmarshal(fixture.outcome.ArtifactManifest, &manifest); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "artifact-hash":
			manifest.Artifacts[0].Sha256 = strings.Repeat("a", 64)
		case "artifact-bytes":
			manifest.Artifacts[0].Bytes++
		case "object-hash":
			manifest.Retention.Objects[0].Sha256 = strings.Repeat("b", 64)
		case "object-bytes":
			manifest.Retention.Objects[0].Bytes++
		case "object-missing":
			manifest.Retention.Objects = manifest.Retention.Objects[1:]
			manifest.Retention.ObjectCount--
		case "object-duplicate":
			manifest.Retention.Objects = append(manifest.Retention.Objects, manifest.Retention.Objects[0])
			manifest.Retention.ObjectCount++
		case "object-unbound":
			unbound := manifest.Retention.Objects[0]
			unbound.Path = "synthetic-unbound.txt"
			manifest.Retention.Objects = append(manifest.Retention.Objects, unbound)
			manifest.Retention.ObjectCount++
		case "signed-hash":
			fixture.response.Files[0].Sha256 = strings.Repeat("c", 64)
		case "signed-bytes":
			fixture.response.Files[0].Bytes++
		case "receipt-hash":
			manifest.Executor.AgentReceiptSha256 = strings.Repeat("d", 64)
		case "request-hash":
			fixture.response.Files = append(fixture.response.Files, competitionagent.File{Path: "worker-request.json", Sha256: strings.Repeat("e", 64), Bytes: 1})
		case "request-self-repeated":
			manifest.RequestSha256 = strings.Repeat("a", 64)
			fixture.response.RequestSha256 = strings.Repeat("b", 64)
			fixture.response.Files = append(fixture.response.Files, competitionagent.File{Path: "worker-request.json", Sha256: fixture.response.RequestSha256, Bytes: 1})
		}
		content, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		fixture.outcome.ArtifactManifest = content
		outcome, err := (&competitionAgentEvaluator{}).retainedReceiveOutcome(fixture.directory, fixture.custody, fixture.response, fixture.outcome)
		if err != nil || outcome.receiveCustody != nil {
			t.Fatal("incomplete retention/transport identity granted cleanup authority", mode, err)
		}
		fixture.custody.Close()
		fixture.assertPreserved(t)
	}
}

// An original controller input and the source-generated private request use
// independent existing hashes, never a filename-only or signed-only exception.
// Stdout has no such archived hash and stays with its parent directory.
func TestCompetitionReceiveCleanupPinnedEphemeralInputsAndUnboundStdout(t *testing.T) {
	for _, stdoutPath := range []string{"", "worker.stdout.log", "control/worker.stdout.log"} {
		provider, request, stdout := []byte("synthetic original providers"), []byte(`{"synthetic_private_request":true}`), []byte("synthetic unretained stdout")
		digest, requestDigest, stdoutDigest := sha256.Sum256(provider), sha256.Sum256(request), sha256.Sum256(stdout)
		input := competitionagent.File{Path: "providers.yml", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(provider))}
		fixture := newCompetitionReceiveCleanupInputFixture(t, 0, []competitionagent.File{input})
		for path, content := range map[string][]byte{"providers.yml": provider, "worker-request.json": request} {
			if err := os.WriteFile(filepath.Join(fixture.directory, path), content, 0400); err != nil {
				t.Fatal(err)
			}
		}
		fixture.response.Files = append(fixture.response.Files, input, competitionagent.File{Path: "worker-request.json", Sha256: hex.EncodeToString(requestDigest[:]), Bytes: int64(len(request))})
		manifest := artifactManifest{}
		if err := json.Unmarshal(fixture.outcome.ArtifactManifest, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.RequestSha256 = hex.EncodeToString(requestDigest[:])
		fixture.response.RequestSha256 = manifest.RequestSha256
		content, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		fixture.outcome.ArtifactManifest = content
		if stdoutPath != "" {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(fixture.directory, stdoutPath)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture.directory, stdoutPath), stdout, 0400); err != nil {
				t.Fatal(err)
			}
			fixture.response.Files = append(fixture.response.Files, competitionagent.File{Path: stdoutPath, Sha256: hex.EncodeToString(stdoutDigest[:]), Bytes: int64(len(stdout))})
		}
		fixture.retain(t)
		worker := fixture.worker(func(context.Context) (bool, error) { return false, nil })
		if _, err := worker.completeWithRetry(t.Context(), fixture.job, *fixture.job.LeaseExpiresAt, fixture.outcome); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"providers.yml", "worker-request.json", "raw/deep/evidence.txt", "executor-receipt.json"} {
			if _, err := os.Lstat(filepath.Join(fixture.directory, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("proved retained/ephemeral file was not reclaimed", path, err)
			}
		}
		if stdoutPath != "" {
			actual, err := os.ReadFile(filepath.Join(fixture.directory, stdoutPath))
			if err != nil || string(actual) != string(stdout) {
				t.Fatal("unbound stdout was deleted or changed", err)
			}
		} else if _, err := os.Lstat(fixture.directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("fully proved future receive directory remained", err)
		}
	}
}

// Successful repeated cleanup cannot adopt a replacement at the old pathname;
// repeated Close releases the two held originals without descriptor reuse.
func TestCompetitionReceiveCleanupRepeatedFinishAndCloseNeverAdoptsReplacement(t *testing.T) {
	fixture := newCompetitionReceiveCleanupFixture(t)
	fixture.retain(t)
	native := fixture.custody.native.(*competitionReceiveNative)
	root, child := native.root, native.child
	if err := fixture.custody.cleanup(t.Context(), fixture.settings, fixture.job, fixture.outcome); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.directory, "synthetic-unrelated-replacement")
	if err := os.WriteFile(path, []byte("synthetic replacement data"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := fixture.custody.cleanup(t.Context(), fixture.settings, fixture.job, fixture.outcome); err != nil {
		t.Fatal("repeated successful housekeeping was not idempotent", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("repeated cleanup adopted unrelated replacement", err)
	}
	if err := fixture.custody.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.custody.Close(); err != nil {
		t.Fatal("repeated preserving Close was not idempotent", err)
	}
	if native.root != nil || native.child != nil || root.Fd() != ^uintptr(0) || child.Fd() != ^uintptr(0) {
		t.Fatal("held receive creation descriptors were not actually closed")
	}
}

// A controller pin mismatch is a whole-preflight refusal, even if the transport
// signed the replacement. Same-sized on-disk changes also fail the held hash.
func TestCompetitionReceiveCleanupRefusesInputPinAndChangedBytes(t *testing.T) {
	for _, mode := range []string{"input-hash", "input-bytes", "file-bytes"} {
		content := []byte("synthetic pinned input")
		digest := sha256.Sum256(content)
		input := competitionagent.File{Path: "providers.yml", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
		fixture := newCompetitionReceiveCleanupInputFixture(t, 0, []competitionagent.File{input})
		if err := os.WriteFile(filepath.Join(fixture.directory, input.Path), content, 0400); err != nil {
			t.Fatal(err)
		}
		if mode == "input-hash" {
			input.Sha256 = strings.Repeat("f", 64)
		}
		if mode == "input-bytes" {
			input.Bytes++
		}
		fixture.response.Files = append(fixture.response.Files, input)
		if mode == "file-bytes" {
			fixture.retain(t)
			path := filepath.Join(fixture.directory, "raw", "deep", "evidence.txt")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original[0] ^= 1
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, original, 0400); err != nil {
				t.Fatal(err)
			}
			if err := fixture.custody.cleanup(t.Context(), fixture.settings, fixture.job, fixture.outcome); err == nil {
				t.Fatal("same-size replacement passed held content hash")
			}
		} else {
			outcome, err := (&competitionAgentEvaluator{}).retainedReceiveOutcome(fixture.directory, fixture.custody, fixture.response, fixture.outcome)
			if err != nil || outcome.receiveCustody != nil {
				t.Fatal("signed replacement bypassed controller creation pin", mode, err)
			}
		}
		fixture.assertPreserved(t)
	}
}
