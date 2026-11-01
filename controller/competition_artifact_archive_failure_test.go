// Synthetic archive failures exercise the real evaluator discard boundaries
// and private event encoding without a database, network or evaluator image.
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
	"net"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/urnetwork/server"
)

// Wrapped backend types keep useful classifications, never their private text.
func TestCompetitionArtifactArchiveFailureClassifiesWrappedCauses(t *testing.T) {
	checks := []struct {
		cause    error
		category string
		s3Code   string
		status   int
	}{
		{cause: context.DeadlineExceeded, category: "deadline_exceeded"},
		{cause: context.Canceled, category: "canceled"},
		{cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic-private-network")}, category: "network"},
		{cause: &os.PathError{Op: "open", Path: "synthetic-private-path", Err: os.ErrPermission}, category: "filesystem_permission"},
		{cause: &os.PathError{Op: "open", Path: "synthetic-private-path", Err: os.ErrNotExist}, category: "filesystem_not_found"},
		{cause: &os.PathError{Op: "read", Path: "synthetic-private-path", Err: errors.New("synthetic-private-io")}, category: "filesystem"},
		{cause: minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "synthetic-private-message"}, category: "s3", s3Code: "AccessDenied", status: 403},
		{cause: &minio.ErrorResponse{Code: "SlowDown", StatusCode: 503, Message: "synthetic-private-message"}, category: "s3", s3Code: "SlowDown", status: 503},
		{cause: minio.ErrorResponse{Code: "TooManyRequests", StatusCode: 429, Message: "synthetic-private-message"}, category: "s3", s3Code: "TooManyRequests", status: 429},
		{cause: errors.New("synthetic-private-other"), category: "other"},
	}
	for _, check := range checks {
		cause := fmt.Errorf("synthetic-private-context: %w", check.cause)
		detail := competitionArtifactArchiveFailureDetails("archive_attempt", cause)
		digest := sha256.Sum256([]byte(cause.Error()))
		if detail == nil || detail.Phase != "archive_attempt" || detail.Category != check.category ||
			detail.S3Code != check.s3Code || detail.HttpStatus != check.status || detail.DetailSha256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("safe archive classification mismatch: %+v", detail)
		}
		encoded, err := json.Marshal(detail)
		if err != nil || bytes.Contains(encoded, []byte("synthetic-private")) {
			t.Fatal("private cause text reached archive diagnostics")
		}
	}
	if competitionArtifactArchiveFailureDetails("archive_attempt", nil) != nil {
		t.Fatal("absence of a cause created archive failure metadata")
	}
}

// Well-formed but unrecognized codes are not safe merely because they look typed.
func TestCompetitionArtifactArchiveFailureRejectsUntrustedMetadata(t *testing.T) {
	for _, status := range []int{-1, 0, 99, 600, 1000} {
		cause := minio.ErrorResponse{
			Code: "SyntheticPrivateCredential", StatusCode: status,
			Message: "https://synthetic-user:synthetic-password@archive.example/synthetic-private-key",
			Key:     "synthetic-private-key", RequestID: "synthetic-private-request",
			HostID: "synthetic-private-host", BucketName: "synthetic-private-bucket",
		}
		detail := competitionArtifactArchiveFailureDetails("synthetic-private-phase", cause)
		if detail.Phase != "unknown" || detail.Category != "s3" || detail.S3Code != "unknown" || detail.HttpStatus != 0 {
			t.Fatalf("untrusted diagnostic metadata admitted: %+v", detail)
		}
		safe := competitionArtifactArchivePhaseError("retain_object", cause)
		encoded, err := json.Marshal(detail)
		if err != nil || strings.Contains(safe.Error(), "synthetic-") || bytes.Contains(encoded, []byte("SyntheticPrivate")) || bytes.Contains(encoded, []byte("synthetic-")) {
			t.Fatal("backend metadata escaped through formatting or JSON")
		}
		var actual minio.ErrorResponse
		if !errors.As(safe, &actual) || actual != cause {
			t.Fatal("safe archive wrapper changed backend identity")
		}
	}
	if !errors.Is(competitionArtifactArchivePhaseError("retain_object", context.Canceled), context.Canceled) {
		t.Fatal("safe archive wrapper changed cancellation identity")
	}
}

// Each backend boundary can fail independently without real retained writes.
type competitionArchiveFailureTestStore struct {
	server.RetainedBlobStore
	putErr, getErr error
	retention      *server.BlobRetention
	reader         io.ReadCloser
	puts           int
}

// Returns a deterministic upload error or explicit proof selected by the test.
func (self *competitionArchiveFailureTestStore) PutRetained(context.Context, string, string, string, time.Time) (*server.BlobRetention, error) {
	self.puts++
	return self.retention, self.putErr
}

// Separates opening a version from its lazy read and close failures.
func (self *competitionArchiveFailureTestStore) GetVersion(context.Context, string, string) (io.ReadCloser, error) {
	return self.reader, self.getErr
}

// A read can succeed even when the exact-version reader fails to close.
type competitionArchiveFailureTestReader struct {
	io.Reader
	closeErr error
}

// Returns only the explicit synthetic close failure.
func (self *competitionArchiveFailureTestReader) Close() error { return self.closeErr }

// Lazy MinIO read failures must keep their typed cause instead of collapsing
// into a hash mismatch, and a completed PUT never bypasses authentication.
func TestCompetitionArtifactArchiveFailureIdentifiesRetentionPhases(t *testing.T) {
	settings := validSettings()
	content := []byte("synthetic artifact bytes\n")
	digest := sha256.Sum256(content)
	cause := minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "synthetic-private-backend"}
	checks := []struct {
		phase  string
		change func(*competitionArchiveFailureTestStore)
	}{
		{phase: "retain_object", change: func(store *competitionArchiveFailureTestStore) { store.putErr = cause }},
		{phase: "retention_proof", change: func(store *competitionArchiveFailureTestStore) { store.retention.Size++ }},
		{phase: "open_retained_version", change: func(store *competitionArchiveFailureTestStore) { store.getErr = cause }},
		{phase: "read_retained_version", change: func(store *competitionArchiveFailureTestStore) { store.reader = io.NopCloser(iotest.ErrReader(cause)) }},
		{phase: "close_retained_version", change: func(store *competitionArchiveFailureTestStore) {
			store.reader = &competitionArchiveFailureTestReader{Reader: bytes.NewReader(content), closeErr: cause}
		}},
		{phase: "authenticate_retained_version", change: func(store *competitionArchiveFailureTestStore) {
			store.reader = io.NopCloser(strings.NewReader("changed synthetic artifact\n"))
		}},
	}
	for _, check := range checks {
		store := &competitionArchiveFailureTestStore{
			retention: &server.BlobRetention{Key: "synthetic-key", Size: int64(len(content)), Mode: "COMPLIANCE", RetainUntil: settings.RetainUntil},
			reader:    io.NopCloser(bytes.NewReader(content)),
		}
		check.change(store)
		retained, err := (&blobArtifactArchive{store: store}).retainFile(
			context.Background(), settings, "synthetic.json", "synthetic-key", "unused-synthetic-path",
			hex.EncodeToString(digest[:]), int64(len(content)), "application/json",
		)
		detail := competitionArtifactArchiveFailureDetails("archive_attempt", err)
		if retained != nil || err == nil || detail == nil || detail.Phase != check.phase || store.puts != 1 {
			t.Fatalf("retention failure phase lost: wanted=%s detail=%+v", check.phase, detail)
		}
		if check.phase != "retention_proof" && check.phase != "authenticate_retained_version" &&
			(detail.Category != "s3" || detail.S3Code != "AccessDenied" || detail.HttpStatus != 403 || !errors.Is(err, cause)) {
			t.Fatalf("typed retention failure lost: %+v", detail)
		}
		if strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("retention error formatting exposed backend detail")
		}
	}
}

// Builds a real authenticated score handoff; only durable retention is failed.
func competitionArchiveFailureScoredScript(t *testing.T, settings *Settings, job *queuedJob) string {
	t.Helper()
	rawScores := make([]float64, settings.EvaluationPolicy.Replicates)
	for index := range rawScores {
		rawScores[index] = 100 + float64(index%3) - 1
	}
	baseline := testRoundBaseline(t, settings, &job.Round, rawScores)
	baselineDigest := sha256.Sum256(baseline)
	raw, normalized := 100.0, 100.0
	score := &ScoreResult{
		ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized,
		Placeable: true, Gates: map[string]Gate{}, Significance: testScoreSignificance(false),
		Diagnostics: map[string]any{"baseline_sha256": hex.EncodeToString(baselineDigest[:])},
	}
	score.Significance.TakeoverMarginPercent = settings.EvaluationPolicy.TakeoverMargin * 100
	if err := validateScore(score); err != nil {
		t.Fatal(err)
	}
	result := evaluatorResult{
		Schema: 1, JobId: job.JobId.String(), Score: score,
		Security: evaluationSecurity{
			TemplateDatabaseReset: true, RedisReset: true, CgroupContained: true, ResourceLimits: true,
			ManagementCpuReserved: true, ManagementMemoryReserved: true, DefaultDenyNetwork: true,
			OfflineBuild: true, OfflineBuildResourceLimits: true, NoProductionSecrets: true,
			StructuralPatchCheck: true, AccountingComplete: true, ResourceReportComplete: true,
			CleanupComplete: true, ImmutableReports: true, CgroupId: "synthetic-cgroup",
			TemplateDatabaseId: "synthetic-database", RedisGenerationId: "synthetic-redis",
		},
	}
	scoreBytes, err := json.Marshal(score)
	if err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, artifact := range []struct {
		path  string
		value []byte
	}{
		{path: "accounting.json", value: []byte("{}\n")},
		{path: "baseline.json", value: baseline},
		{path: "resources.json", value: []byte("{}\n")},
		{path: "score.json", value: scoreBytes},
		{path: "evaluation.complete.json", value: []byte("{}\n")},
	} {
		digest := sha256.Sum256(artifact.value)
		result.Artifacts = append(result.Artifacts, evaluationArtifact{
			Path: artifact.path, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(artifact.value)),
		})
		body += "printf '%s' " + failureArchiveShellQuote(string(artifact.value)) + " > " + failureArchiveShellQuote(artifact.path) + "\n"
	}
	value, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return body + "printf '%s' " + failureArchiveShellQuote(string(value)) + " > worker-result.json\nexit 0\n"
}

// Checks both the unchanged public failure and the private payload actually
// consumed by the retry and terminal appendEvent call sites.
func requireCompetitionArchiveFailureOutcome(t *testing.T, outcome EvaluationOutcome, phase, message string) {
	t.Helper()
	wanted := infrastructureError("artifact_archive_failed", message)
	if outcome.Error == nil || *outcome.Error != *wanted || outcome.Score != nil || len(outcome.ArtifactManifest) != 0 ||
		len(outcome.RoundBaselineJson) != 0 || outcome.RoundBaselineSha256 != "" {
		t.Fatalf("public fail-closed archive outcome changed: %+v", outcome)
	}
	if outcome.archiveFailure == nil || outcome.archiveFailure.Phase != phase ||
		outcome.archiveFailure.Category != "s3" || outcome.archiveFailure.S3Code != "AccessDenied" || outcome.archiveFailure.HttpStatus != 403 {
		t.Fatalf("archive cause lost at evaluator discard: %+v", outcome.archiveFailure)
	}
	encoded, err := json.Marshal(competitionArtifactArchiveEventData(outcome, map[string]any{"attempt": 1}))
	if err != nil || bytes.Contains(encoded, []byte("synthetic-private")) {
		t.Fatal("archive event contains backend text")
	}
	var event struct {
		Failure *competitionArtifactArchiveFailure `json:"artifact_archive_failure"`
	}
	if json.Unmarshal(encoded, &event) != nil || event.Failure == nil || *event.Failure != *outcome.archiveFailure {
		t.Fatal("private archive cause did not reach completion event encoding")
	}
	public, err := json.Marshal(outcome)
	if err != nil || bytes.Contains(public, []byte("artifact_archive_failure")) || bytes.Contains(public, []byte("detail_sha256")) || bytes.Contains(public, []byte("synthetic-private")) {
		t.Fatal("private archive detail entered the public outcome JSON")
	}
}

// A valid scored attempt reaches the real archive and still fails closed,
// carrying its sanitized cause instead of discarding it with the score.
func TestCommandEvaluatorPreservesScoredArchiveFailureDetails(t *testing.T) {
	settings, job, _ := failureArchiveEvaluatorFixture(t, func(settings *Settings, job *queuedJob) string {
		return competitionArchiveFailureScoredScript(t, settings, job)
	})
	store := &competitionArchiveFailureTestStore{
		RetainedBlobStore: server.NewLocalBlobStore(t.TempDir(), "synthetic-evidence").(server.RetainedBlobStore),
		putErr:            minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "synthetic-private-backend"},
	}
	settings.artifactArchive = &blobArtifactArchive{store: store}
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if store.puts != 1 {
		t.Fatalf("scored attempt did not reach the real archive: puts=%d", store.puts)
	}
	requireCompetitionArchiveFailureOutcome(t, outcome, "retain_object", "durable artifact retention failed")
}

// The adjacent failed-run archive must not discard the same backend cause.
func TestCommandEvaluatorPreservesFailedArchiveFailureDetails(t *testing.T) {
	settings, job, _ := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + "exit 17\n"
	})
	store := &competitionArchiveFailureTestStore{
		RetainedBlobStore: server.NewLocalBlobStore(t.TempDir(), "synthetic-evidence").(server.RetainedBlobStore),
		putErr:            minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "synthetic-private-backend"},
	}
	settings.artifactArchive = &blobArtifactArchive{store: store}
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if store.puts != 1 {
		t.Fatalf("failed attempt did not reach the real archive: puts=%d", store.puts)
	}
	requireCompetitionArchiveFailureOutcome(t, outcome, "retain_object", "durable failure evidence retention failed")
}

// Authentication failures before any remote call keep their own phase too.
func TestCommandEvaluatorPreservesFailureEvidenceAuthenticationDetails(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, func(manifest map[string]any) { manifest["sanitized"] = false }) + "exit 17\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "artifact_archive_failed" || archive.calls != 0 ||
		outcome.archiveFailure == nil || outcome.archiveFailure.Phase != "authenticate_failed_evidence" ||
		outcome.archiveFailure.Category != "other" || !sha256Pattern.MatchString(outcome.archiveFailure.DetailSha256) {
		t.Fatalf("pre-upload authentication cause lost: %+v", outcome.archiveFailure)
	}
}

// Ordinary outcomes keep exactly the historical private event shape.
func TestCompetitionArtifactArchiveFailureLeavesOrdinaryEventsUnchanged(t *testing.T) {
	encoded, err := json.Marshal(competitionArtifactArchiveEventData(EvaluationOutcome{}, map[string]any{"attempt": 1}))
	if err != nil || string(encoded) != `{"attempt":1}` {
		t.Fatal("ordinary completion event changed")
	}
}
