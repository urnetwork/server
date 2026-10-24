// Synthetic restart barriers prove one review cannot obtain another build/test
// draw or deadline. No fixture is operational approval or model provenance.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sourceBuildAttemptFixture struct {
	Request       reviewedSourceBuildRequest
	Settings      sourceBuildBrokerSettings
	Owner         string
	Image         string
	PipelineCount int
	QualifyCount  int
	Deadline      time.Time
}

func newSourceBuildAttemptFixture(t *testing.T) *sourceBuildAttemptFixture {
	t.Helper()
	settings := defaultSourceBuildBrokerSettings()
	settings.Sandbox.StateDirectory = filepath.Join(t.TempDir(), "private-build-state")
	if err := os.Mkdir(settings.Sandbox.StateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	return &sourceBuildAttemptFixture{Request: sourceBuildContextFixture(t), Settings: settings, Image: "sha256:" + strings.Repeat("b", 64)}
}

func (self *sourceBuildAttemptFixture) pipeline(_ context.Context, _ reviewedSourceBuildRequest, owner string, settings sourceBuildSandboxSettings, _ sourceBuildDocker) (*sourceBuildArtifacts, error) {
	self.PipelineCount++
	self.Owner = owner
	root := filepath.Join(settings.StateDirectory, owner)
	path := filepath.Join(root, "compile", "output", "sim-latency")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	content := []byte("synthetic-compiled-artifact-not-an-executable")
	if err := writePrivateReviewFile(path, content); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	return &sourceBuildArtifacts{Directory: root, ImageId: self.Image, SimulatorSha256: hex.EncodeToString(digest[:])}, nil
}

func (self *sourceBuildAttemptFixture) docker(_ context.Context, args []string, output, diagnostics io.Writer) error {
	if len(args) != 5 || args[0] != "image" || args[1] != "inspect" || args[4] != self.Image {
		return errors.New("unexpected synthetic Docker command")
	}
	return json.NewEncoder(output).Encode(map[string]any{"Id": self.Image, "Config": map[string]any{"OnBuild": []string{},
		"Labels": map[string]string{"com.urnetwork.review-build.owner": self.Owner, "com.urnetwork.competition.image-kind": "evaluator-base"}}})
}

func (self *sourceBuildAttemptFixture) qualifier(t *testing.T) sourceBuildQualificationFunc {
	t.Helper()
	_, proposal, _ := reviewedProposalFixture(t)
	return func(ctx context.Context, request sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
		self.QualifyCount++
		deadline, _ := ctx.Deadline()
		if self.Deadline.IsZero() {
			self.Deadline = deadline
		} else if !self.Deadline.Equal(deadline) {
			t.Fatal("retry extended the original build deadline")
		}
		receipt := proposal.BuildReceipt
		receipt.Staging, receipt.ReviewId = self.Request.Proposal.Staging, self.Request.Proposal.ReviewId
		receipt.PatchSha256, receipt.SourceLockSha256 = self.Request.Proposal.PatchSha256, self.Request.Proposal.SourceLockSha256
		receipt.EvaluatorImageDigest, receipt.SandboxId = request.Artifacts.ImageId, request.Owner
		return receipt, nil
	}
}

func TestSourceBuildAttemptRetainsImageOwnerAndDeadlineAcrossQualificationRetry(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	passing := fixture.qualifier(t)
	qualify := func(ctx context.Context, request sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
		receipt, err := passing(ctx, request)
		if fixture.QualifyCount == 1 {
			return receipt, errors.New("synthetic incomplete qualifier transport")
		}
		return receipt, err
	}
	invoke := func() (*reviewedSourceBuildReceipt, error) {
		return executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify)
	}
	if receipt, err := invoke(); err == nil || receipt != nil {
		t.Fatal("incomplete qualification produced authority", err)
	}
	owner := fixture.Owner
	// A new materialization path is not a new approved source or build draw.
	fixture.Request.Proposal.PatchPath = filepath.Join(t.TempDir(), "canonical.patch")
	for repository := range fixture.Request.Proposal.StagedRoots {
		fixture.Request.Proposal.StagedRoots[repository] = filepath.Join(t.TempDir(), repository)
	}
	first, err := invoke()
	if err != nil || first == nil || first.SandboxId != owner || fixture.PipelineCount != 1 || fixture.QualifyCount != 2 {
		t.Fatal("retry rebuilt or replaced the original image", fixture.PipelineCount, fixture.QualifyCount, err)
	}
	second, err := invoke()
	if err != nil || second == nil || *second != *first || fixture.PipelineCount != 1 || fixture.QualifyCount != 2 {
		t.Fatal("completed receipt was not replayed", err)
	}
}

func TestSourceBuildAttemptInterruptedPipelineDoesNotBuildAgain(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	qualify := fixture.qualifier(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("synthetic interrupted pipeline did not interrupt")
			}
		}()
		pipeline := func(context.Context, reviewedSourceBuildRequest, string, sourceBuildSandboxSettings, sourceBuildDocker) (*sourceBuildArtifacts, error) {
			panic("synthetic controller crash before pipeline observation")
		}
		executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, pipeline, qualify)
	}()
	if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err == nil || receipt != nil || !strings.Contains(err.Error(), "interrupted") || fixture.PipelineCount != 0 || fixture.QualifyCount != 0 {
		t.Fatal("missing observation silently launched another pipeline", err)
	}
}

func TestSourceBuildAttemptCompletedPipelineFailureDoesNotRetry(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	count := 0
	pipeline := func(context.Context, reviewedSourceBuildRequest, string, sourceBuildSandboxSettings, sourceBuildDocker) (*sourceBuildArtifacts, error) {
		count++
		return nil, errors.New("synthetic definitive build failure")
	}
	for range 2 {
		if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, pipeline, fixture.qualifier(t)); err == nil || receipt != nil {
			t.Fatal("failed pipeline became a qualified receipt", err)
		}
	}
	if count != 1 || fixture.QualifyCount != 0 {
		t.Fatal("failed pipeline was resampled", count, fixture.QualifyCount)
	}
}

func TestSourceBuildAttemptChangedSettingsCannotResetReviewIdentity(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	qualify := fixture.qualifier(t)
	if _, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err != nil {
		t.Fatal(err)
	}
	fixture.Settings.Timeout += time.Hour
	if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err == nil || receipt != nil || !strings.Contains(err.Error(), "original reviewed request") || fixture.PipelineCount != 1 || fixture.QualifyCount != 1 {
		t.Fatal("changed deployment settings opened another draw", err)
	}
}

func TestSourceBuildAttemptRevalidatesRetainedBinaryBeforeReceiptReplay(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	qualify := fixture.qualifier(t)
	if _, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.Settings.Sandbox.StateDirectory, fixture.Owner, "compile", "output", "sim-latency")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic-replaced-binary"), 0o400); err != nil {
		t.Fatal(err)
	}
	if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err == nil || receipt != nil || fixture.PipelineCount != 1 || fixture.QualifyCount != 1 {
		t.Fatal("replaced retained artifact reused passing receipt", err)
	}
}

func TestSourceBuildAttemptConcurrentControllerCannotDispatchAnotherPipeline(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	qualify := fixture.qualifier(t)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	pipeline := func(ctx context.Context, request reviewedSourceBuildRequest, owner string, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (*sourceBuildArtifacts, error) {
		close(entered)
		<-release
		return fixture.pipeline(ctx, request, owner, settings, docker)
	}
	go func() {
		_, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, pipeline, qualify)
		finished <- err
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("controller failed before its explicit pipeline barrier: %v", err)
	}
	receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify)
	close(release)
	firstErr := <-finished
	if err == nil || receipt != nil || !strings.Contains(err.Error(), "active controller") || firstErr != nil || fixture.PipelineCount != 1 || fixture.QualifyCount != 1 {
		t.Fatal("concurrent controller escaped the actual process lock", err, firstErr)
	}
}

func TestSourceBuildAttemptExpiredOriginalDeadlineCannotBeExtended(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	qualify := fixture.qualifier(t)
	if _, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(fixture.Request.Proposal.ReviewId.String()))
	path := filepath.Join(fixture.Settings.Sandbox.StateDirectory, ".attempts", hex.EncodeToString(key[:]), "reservation.json")
	var reservation sourceBuildAttemptReservation
	if err := readSourceTestRecord(path, fixture.Settings.MaximumRequestBytes, &reservation); err != nil {
		t.Fatal(err)
	}
	reservation.StartedAt, reservation.Deadline = time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
	content, err := json.Marshal(reservation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); !errors.Is(err, context.DeadlineExceeded) || receipt != nil || fixture.PipelineCount != 1 || fixture.QualifyCount != 1 {
		t.Fatal("restart reset the expired original deadline", err)
	}
}

func TestSourceBuildAttemptRevalidatesImageOwnerBeforeReceiptReplay(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	qualify := fixture.qualifier(t)
	if _, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err != nil {
		t.Fatal(err)
	}
	fixture.Owner = strings.Repeat("f", 64)
	if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, qualify); err == nil || receipt != nil || !strings.Contains(err.Error(), "exact owner") || fixture.PipelineCount != 1 || fixture.QualifyCount != 1 {
		t.Fatal("different image owner reused a qualified receipt", err)
	}
}

func TestSourceBuildAttemptRejectsExposedStateBeforePipelineDispatch(t *testing.T) {
	fixture := newSourceBuildAttemptFixture(t)
	if err := os.Chmod(fixture.Settings.Sandbox.StateDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if receipt, err := executeJournaledSourceBuildBroker(context.Background(), fixture.Request, fixture.Settings, fixture.docker, fixture.pipeline, fixture.qualifier(t)); err == nil || receipt != nil || !strings.Contains(err.Error(), "private and canonical") || fixture.PipelineCount != 0 || fixture.QualifyCount != 0 {
		t.Fatal("nonprivate build state dispatched candidate work", err)
	}
}
