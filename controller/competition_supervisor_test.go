// Explicit fake-clock transitions prove progress without review, safe retries,
// and exact runtime acknowledgement without live epochs or external services.
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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Owns authoritative fake state independently of each restarted reconciler.
type competitionSupervisorFixture struct {
	now                   time.Time
	snapshot              CompetitionSupervisorSnapshot
	lease                 model.CompetitionSupervisorLease
	choices               int
	publications          int
	preparations          int
	activations           int
	creations             int
	failPrepare           bool
	failCreatedCheckpoint bool
}

// Models only the independently proven unpublished/publication boundary. Git
// probe tests exercise the exact eleven-ref evidence required in production.
type competitionSupervisorFallbackFixture struct {
	*competitionSupervisorFixture
	unpublished bool
	abandoned   bool
}

// A promotion is ready initially; the explicitly recorded fallback selects
// only incumbent source and consumes a new activation attempt identity.
func (self *competitionSupervisorFallbackFixture) ChooseSource(ctx context.Context, lease model.CompetitionSupervisorLease, snapshot *CompetitionSupervisorSnapshot, transition model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	intent, err := self.competitionSupervisorFixture.ChooseSource(ctx, lease, snapshot, transition)
	if transition.Reason == "safe_carry_forward_after_unpublished_promotion" {
		intent.Generation, intent.Reason = 3, "readiness_fallback"
	} else {
		intent.Kind, intent.SourceGeneration = "reviewed_source_promotion", 1
	}
	return intent, err
}

// The synthetic publisher fails without assuming whether a ref advanced.
func (self *competitionSupervisorFallbackFixture) PublishSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	self.publications++
	return errors.New("synthetic publication failure")
}

// Any known external advancement prevents abandonment and preserves retries.
func (self *competitionSupervisorFallbackFixture) FallbackSource(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, _ string) (bool, error) {
	if !self.unpublished {
		return false, nil
	}
	self.abandoned, intent.State = true, "abandoned"
	return true, nil
}

// A failed unpublished promotion is retained as abandoned history; its safe
// successor carries the incumbent with a fresh full preparation interval.
func TestCompetitionSupervisorFallsBackOnlyBeforePublication(t *testing.T) {
	for _, unpublished := range []bool{true, false} {
		fixture := &competitionSupervisorFallbackFixture{competitionSupervisorFixture: newCompetitionSupervisorFixture(), unpublished: unpublished}
		step := func() (string, error) {
			return (&CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-owner", Driver: fixture, Now: func() time.Time { return fixture.now }}).Reconcile(context.Background())
		}
		for _, want := range []string{"source_choice_pending", "source_publication_pending"} {
			if state, err := step(); err != nil || state != want {
				t.Fatalf("state %s, want %s: %v", state, want, err)
			}
		}
		original := fixture.snapshot.Intent
		state, err := step()
		if !unpublished {
			if err == nil || state != "publication_blocked" || fixture.abandoned || fixture.snapshot.Intent != original {
				t.Fatalf("partial publication was abandoned: %s %v", state, err)
			}
			continue
		}
		if err != nil || state != "source_choice_pending" || !fixture.abandoned || original.State != "abandoned" || fixture.snapshot.Transition.SourceActivationGeneration != 0 {
			t.Fatalf("unpublished fallback checkpoint: %s %v", state, err)
		}
		fixture.now = fixture.now.Add(time.Hour)
		if state, err := step(); err != nil || state != "source_publication_pending" {
			t.Fatalf("fallback restaging: %s %v", state, err)
		}
		intent := fixture.snapshot.Intent
		if intent.Generation != 3 || intent.SourceGeneration != 0 || intent.Kind != "reviewed_source_carry_forward" || intent.Threshold != 0.161 || !intent.PreparedAt.Equal(fixture.now) {
			t.Fatalf("fallback changed incumbent or reused preparation: %+v", intent)
		}
	}
}

// Genuine review cannot manufacture eligibility, and an archive byte digest
// must not be silently replaced with canonical parsed-score JSON.
func TestReviewedSourceRequiresSealedEligibleArchiveIdentity(t *testing.T) {
	patch := []byte("synthetic exact approved patch")
	patchDigest := sha256.Sum256(patch)
	score := competitionRankingScore(80, 125)
	score.Significance.ReplicateCount = 9
	score.Gates = map[string]Gate{}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		score.Gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	archive, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	archive = append(archive, '\n')
	archiveDigest := sha256.Sum256(archive)
	entry := model.CompetitionSealedEntry{JobId: server.NewId(), State: "succeeded", Eligible: true, Score: score, PatchSha256: hex.EncodeToString(patchDigest[:]), ScoreSha256: hex.EncodeToString(archiveDigest[:]), BaseSourceLockSha256: strings.Repeat("a", 64), ArtifactManifestSha256: strings.Repeat("b", 64)}
	seal := &model.CompetitionEvaluationSeal{RoundId: server.NewId(), BaselineSha256: strings.Repeat("c", 64), BaselineRawScore: 100, TakeoverMargin: 0.12, Entries: []model.CompetitionSealedEntry{entry}}
	review := model.CompetitionIntegrityReviewResult{RoundId: seal.RoundId, Request: model.CompetitionIntegrityReviewRequest{JobId: entry.JobId, PatchSha256: entry.PatchSha256, ScoreSha256: entry.ScoreSha256, BaseSourceLockSha256: entry.BaseSourceLockSha256, ArtifactManifestSha256: entry.ArtifactManifestSha256, BaselineSha256: seal.BaselineSha256}}
	if _, err := reviewedSourceSealedEntry(review, seal, patch); err != nil {
		t.Fatal(err)
	}
	canonical, _ := model.CompetitionAutomationDigest(score)
	review.Request.ScoreSha256 = canonical
	if _, err := reviewedSourceSealedEntry(review, seal, patch); err == nil {
		t.Fatal("reserialized score replaced archived bytes")
	}
	review.Request.ScoreSha256 = entry.ScoreSha256
	seal.Entries[0].Eligible = false
	if _, err := reviewedSourceSealedEntry(review, seal, patch); err == nil {
		t.Fatal("source review made an originally ineligible candidate promotable")
	}
	seal.Entries[0].Eligible = true
	score.Gates["G6_resources"] = Gate{Passed: false, Details: map[string]any{}}
	if _, err := reviewedSourceSealedEntry(review, seal, patch); err == nil {
		t.Fatal("source review bypassed an evaluator gate")
	}
}

// Starts after a sealed epoch with an unreviewed reward winner and clean source.
func newCompetitionSupervisorFixture() *competitionSupervisorFixture {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &competitionSupervisorFixture{now: now, snapshot: CompetitionSupervisorSnapshot{
		Round:         &model.CompetitionRoundResult{RoundId: server.NewId(), Epoch: 1, Status: "sealed", DecisionMode: model.CompetitionDecisionModeV2, ClosesAt: now},
		Seal:          &model.CompetitionEvaluationSeal{Epoch: 1, SealedAt: now, SourceChoiceAt: now},
		CurrentSource: &model.CompetitionSourceActivation{Generation: 1, SourceGeneration: 0, TargetEpoch: 1, Threshold: 0.161, State: "active"},
	}}
}

// Competing owners cannot change a live fake fence.
func (self *competitionSupervisorFixture) Acquire(_ context.Context, owner string, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	if self.lease.OwnerId != "" && self.lease.OwnerId != owner && self.now.Before(self.lease.LeaseExpiresAt) {
		return nil, errors.New("lease unavailable")
	}
	if self.lease.OwnerId == "" || !self.now.Before(self.lease.LeaseExpiresAt) {
		self.lease.FencingToken++
	}
	self.lease.OwnerId, self.lease.LeaseExpiresAt = owner, self.now.Add(ttl)
	copy := self.lease
	return &copy, nil
}

// Rejects stale tokens, independently of owner-provided timestamps.
func (self *competitionSupervisorFixture) Renew(_ context.Context, lease model.CompetitionSupervisorLease, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	if lease.OwnerId != self.lease.OwnerId || lease.FencingToken != self.lease.FencingToken {
		return nil, errors.New("lease lost")
	}
	self.lease.LeaseExpiresAt = self.now.Add(ttl)
	copy := self.lease
	return &copy, nil
}

// Returns the durable snapshot reconstructed after restart.
func (self *competitionSupervisorFixture) Snapshot(context.Context, model.CompetitionSupervisorLease) (*CompetitionSupervisorSnapshot, error) {
	copy := self.snapshot
	return &copy, nil
}

// Emulates expected-revision checkpoint writes and lost final acknowledgments.
func (self *competitionSupervisorFixture) SaveTransition(_ context.Context, _ model.CompetitionSupervisorLease, transition model.CompetitionSupervisorTransition) (*model.CompetitionSupervisorTransition, error) {
	if self.failCreatedCheckpoint && transition.State == "round_created" {
		return nil, errors.New("crash before checkpoint")
	}
	revision := 0
	if self.snapshot.Transition != nil {
		revision = self.snapshot.Transition.Revision
	}
	if transition.ExpectedRevision != revision {
		return nil, errors.New("checkpoint conflict")
	}
	transition.Revision = revision + 1
	self.snapshot.Transition = &transition
	copy := transition
	return &copy, nil
}

// The existing singleton worker remains the only evaluation owner.
func (self *competitionSupervisorFixture) RunWorker(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error {
	return nil
}

// Sealing is irrelevant until the newly opened fake round closes.
func (self *competitionSupervisorFixture) Seal(context.Context, model.CompetitionSupervisorLease, server.Id) error {
	return nil
}

// Deliberately exposes no review API: progress must not depend on one.
func (self *competitionSupervisorFixture) ChooseSource(_ context.Context, _ model.CompetitionSupervisorLease, _ *CompetitionSupervisorSnapshot, transition model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	self.choices++
	self.snapshot.Intent = &model.CompetitionSourceActivation{Generation: 2, ExpectedGeneration: 1, SourceGeneration: 0, TargetEpoch: transition.TargetEpoch, Kind: "reviewed_source_carry_forward", Reason: "review_pending", Threshold: 0.161, PreparedAt: self.now, State: "prepared"}
	return self.snapshot.Intent, nil
}

// Counts source publication independently of runtime readiness.
func (self *competitionSupervisorFixture) PublishSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	self.publications++
	return nil
}

// Injects readiness failure at an explicit state boundary.
func (self *competitionSupervisorFixture) PrepareSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	self.preparations++
	if self.failPrepare {
		return errors.New("readiness failed")
	}
	return nil
}

// Activation is observable only after successful preparation and the interval.
func (self *competitionSupervisorFixture) ActivateSource(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	self.activations++
	intent.State = "active"
	self.snapshot.CurrentSource = intent
	return nil
}

// Creates one immutable full window and makes that durable round visible even
// when the caller crashes before its checkpoint acknowledgment.
func (self *competitionSupervisorFixture) CreateRound(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, opensAt time.Time) (*model.CompetitionRoundResult, error) {
	self.creations++
	round := &model.CompetitionRoundResult{RoundId: server.NewId(), Epoch: intent.TargetEpoch, Status: "open", DecisionMode: model.CompetitionDecisionModeV2, OpensAt: opensAt, ClosesAt: opensAt.Add(7 * 24 * time.Hour)}
	self.snapshot.Round, self.snapshot.Seal = round, nil
	return round, nil
}

// Each call uses a fresh supervisor object, proving no process-local phase is
// needed. Pending review never extends the zero-wait source-choice deadline.
func TestCompetitionSupervisorCarriesReviewedSourceWithoutWaitingForReview(t *testing.T) {
	fixture := newCompetitionSupervisorFixture()
	step := func(want string) {
		t.Helper()
		supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-supervisor", Driver: fixture, Now: func() time.Time { return fixture.now }}
		state, err := supervisor.Reconcile(context.Background())
		if err != nil || state != want {
			t.Fatalf("state=%s want=%s err=%v", state, want, err)
		}
	}
	step("source_choice_pending")
	step("source_publication_pending")
	step("preparation_pending")
	step("preparation_ready")
	if fixture.activations != 0 || fixture.creations != 0 {
		t.Fatal("preparation interval was skipped")
	}
	step("preparation_ready")
	if fixture.preparations != 1 {
		t.Fatal("completed preparation was repeated while waiting")
	}
	fixture.now = fixture.now.Add(16 * time.Hour)
	step("source_active")
	step("round_created")
	if fixture.choices != 1 || fixture.creations != 1 || fixture.snapshot.Intent.SourceGeneration != 0 || fixture.snapshot.Intent.Threshold != 0.161 {
		t.Fatalf("unsafe carry-forward: %+v", fixture)
	}
	if fixture.snapshot.Round.ClosesAt.Sub(fixture.snapshot.Round.OpensAt) != 7*24*time.Hour {
		t.Fatal("next admission window was shortened")
	}
}

// Readiness failures remain visible and preserve the intent; recovery starts a
// full window at actual readiness rather than advertising an expired schedule.
func TestCompetitionSupervisorReadinessFailureDoesNotOpenRound(t *testing.T) {
	fixture := newCompetitionSupervisorFixture()
	fixture.failPrepare = true
	supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-supervisor", Driver: fixture, Now: func() time.Time { return fixture.now }}
	for range 3 {
		if _, err := supervisor.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	fixture.now = fixture.now.Add(20 * time.Hour)
	state, err := supervisor.Reconcile(context.Background())
	if err == nil || state != "preparation_blocked" || fixture.creations != 0 || fixture.activations != 0 {
		t.Fatalf("readiness failed open: %s %v", state, err)
	}
	fixture.failPrepare = false
	if state, err := supervisor.Reconcile(context.Background()); err != nil || state != "source_active" {
		t.Fatalf("failed to resume same transition: %s %v", state, err)
	}
	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fixture.snapshot.Round.OpensAt.Equal(fixture.now) || fixture.choices != 1 {
		t.Fatal("recovery changed the choice or backdated admission")
	}
}

// A committed round is authoritative even when its final checkpoint was lost.
func TestCompetitionSupervisorCrashAfterRoundCreationDoesNotCreateTwice(t *testing.T) {
	fixture := newCompetitionSupervisorFixture()
	supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-supervisor", Driver: fixture, Now: func() time.Time { return fixture.now }}
	for range 3 {
		if _, err := supervisor.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	fixture.now = fixture.now.Add(16 * time.Hour)
	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.failCreatedCheckpoint = true
	if _, err := supervisor.Reconcile(context.Background()); err == nil {
		t.Fatal("injected checkpoint crash was ignored")
	}
	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.creations != 1 {
		t.Fatal("restart created another successor round")
	}
}

// Only the current durable owner progresses; the seventh epoch never exists.
func TestCompetitionSupervisorFencingAndSixEpochLimit(t *testing.T) {
	fixture := newCompetitionSupervisorFixture()
	one := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-first", Driver: fixture, Now: func() time.Time { return fixture.now }}
	two := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-second", Driver: fixture, Now: func() time.Time { return fixture.now }}
	if _, err := one.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, err := two.Reconcile(context.Background()); err == nil || state != "lease_unavailable" {
		t.Fatal("second supervisor bypassed fence")
	}
	fixture.snapshot.Round.Epoch = 6
	if state, err := one.Reconcile(context.Background()); err != nil || state != "season_complete" {
		t.Fatalf("season did not stop: %s %v", state, err)
	}
	if fixture.choices != 0 || fixture.creations != 0 {
		t.Fatal("seventh epoch source was selected")
	}
}

// The executable's zero value cannot acquire a lease or change any state.
func TestCompetitionSupervisorDisabledByDefault(t *testing.T) {
	fixture := newCompetitionSupervisorFixture()
	supervisor := &CompetitionSupervisor{Driver: fixture, OwnerId: "synthetic-supervisor"}
	if state, err := supervisor.Reconcile(context.Background()); err == nil || state != "disabled" || fixture.lease.OwnerId != "" {
		t.Fatal("automation enabled implicitly")
	}
}

// Installed runtime bytes, scope and source identity must all match the
// immutable intent; an operator-provided readiness boolean is not consulted.
func TestCompetitionRuntimeManifestRequiresExactInstalledContent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIM_LATENCY_RUNTIME_MANIFEST_DIRECTORY", directory)
	settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "synthetic", SeasonId: "synthetic-season"}}
	commits := map[string]string{}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp", "gvisor"} {
		commits[name] = strings.Repeat("a", 40)
	}
	lockDigest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte("schema: 2\n\n")
	digest := sha256.Sum256(manifest)
	intent := &model.CompetitionSourceActivation{Environment: "synthetic", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", Generation: 2, ManifestCommit: strings.Repeat("b", 40), ManifestSha256: hex.EncodeToString(digest[:]), Manifest: manifest, SourceLockSha256: lockDigest, Commits: commits, EvaluatorImageDigest: "sha256:" + strings.Repeat("c", 64), HostQualificationSha256: strings.Repeat("d", 64)}
	intent.TargetRoundId = server.NewId()
	if err := LoadCompetitionRuntimeManifest(settings, intent); err == nil {
		t.Fatal("missing deployment acknowledged")
	}
	installed := CompetitionRuntimeManifest{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: 2, ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256, Manifest: manifest, SourceLockSha256: lockDigest, EvaluatorImageDigest: intent.EvaluatorImageDigest, HostQualificationSha256: intent.HostQualificationSha256}
	installed.TargetRoundId = intent.TargetRoundId
	content, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "generation-2.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadCompetitionRuntimeManifest(settings, intent); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := LoadCompetitionRuntimeManifest(settings, intent); err == nil {
		t.Fatal("world-writable runtime manifest directory accepted")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	installed.Manifest = []byte("schema: 2\n")
	content, err = json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadCompetitionRuntimeManifest(settings, intent); err == nil {
		t.Fatal("wrong runtime manifest acknowledged")
	}
}
