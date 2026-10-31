// Synthetic heartbeat failures exercise safe cause persistence after real
// worker cancellation and joined evaluator cleanup, without a database or clock sleeps.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Classification never changes an error's authority or exposes its message.
func TestCompetitionHeartbeatFailureClassifiesSafeCauses(t *testing.T) {
	checks := []struct {
		cause           error
		category, state string
	}{
		{cause: fmt.Errorf("synthetic private context: %w", ErrLeaseLost), category: "lease_lost"},
		{cause: fmt.Errorf("synthetic private timeout: %w", context.DeadlineExceeded), category: "deadline_exceeded"},
		{cause: context.Canceled, category: "canceled"},
		{cause: &pgconn.PgError{Code: "08006", Message: "synthetic-private-message"}, category: "postgresql", state: "08006"},
		{cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic-private-message")}, category: "network"},
		{cause: errors.New("synthetic-private-message"), category: "other"},
	}
	for _, check := range checks {
		detail := competitionHeartbeatFailureDetails(check.cause)
		digest := sha256.Sum256([]byte(check.cause.Error()))
		if detail.Category != check.category || detail.SqlState != check.state || detail.DetailSha256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("safe classification mismatch: %+v", detail)
		}
		encoded, err := json.Marshal(competitionHandBackEvent{Reason: "heartbeat_failed", competitionHeartbeatFailure: detail})
		if err != nil || strings.Contains(string(encoded), "synthetic-private") || strings.Contains(string(encoded), "synthetic private") {
			t.Fatal("raw cause reached the event")
		}
	}
}

// Invalid SQLSTATE data is omitted rather than treated as diagnostic text.
func TestCompetitionHeartbeatFailureRejectsMalformedSqlState(t *testing.T) {
	for _, state := range []string{"", "lower", "12 34", "08006-secret", "é2345", "12\n34"} {
		detail := competitionHeartbeatFailureDetails(&pgconn.PgError{Code: state, Message: "synthetic-private-message"})
		if detail.Category != "postgresql" || detail.SqlState != "" || !sha256Pattern.MatchString(detail.DetailSha256) {
			t.Fatalf("invalid SQLSTATE admitted: %+v", detail)
		}
	}
}

// Formatting is redacted while programmatic error identity remains available.
func TestCompetitionHeartbeatFailureFormattingPreservesCause(t *testing.T) {
	pgError := &pgconn.PgError{Code: "40001", Message: "postgres://synthetic-user:synthetic-password@database.example/synthetic"}
	safe := fmt.Errorf("synthetic heartbeat: %w", safeCompetitionHeartbeatError(pgError))
	var actual *pgconn.PgError
	if !errors.As(safe, &actual) || actual != pgError || strings.Contains(safe.Error(), "synthetic-password") || strings.Contains(safe.Error(), "database.example") {
		t.Fatal("safe formatting lost identity or exposed detail")
	}
	if !errors.Is(safeCompetitionHeartbeatError(ErrLeaseLost), ErrLeaseLost) || !errors.Is(safeCompetitionHeartbeatError(context.Canceled), context.Canceled) {
		t.Fatal("safe formatting changed cancellation/fencing identity")
	}
}

// Non-heartbeat handbacks preserve the exact historical reason-only encoding.
func TestCompetitionHeartbeatFailureLeavesOrdinaryEventsUnchanged(t *testing.T) {
	encoded, err := json.Marshal(competitionHandBackEvent{Reason: "worker_shutdown", competitionHeartbeatFailure: competitionHeartbeatFailureDetails(nil)})
	if err != nil || string(encoded) != `{"reason":"worker_shutdown"}` {
		t.Fatal("ordinary handback encoding changed")
	}
	if _, ok := any(PostgresStore{}).(competitionHeartbeatFailureStore); !ok {
		t.Fatal("concrete production store cannot persist safe details")
	}
	if _, ok := any(competitionSupervisorWorkerStore{}).(competitionHeartbeatFailureStore); !ok {
		t.Fatal("supervised production store lost diagnostic capability")
	}
}

// The evaluator holds cleanup until an explicit release, not a timer race.
type heartbeatFailureEvaluator struct {
	started, canceled, release chan struct{}
	joined                     atomic.Bool
}

// This test never performs a host probe or invokes a native evaluator.
func (self *heartbeatFailureEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	return HostSelfCheck{}, nil
}

// Returning only after release makes the handback's join observation explicit.
func (self *heartbeatFailureEvaluator) Evaluate(ctx context.Context, _ *Settings, _ *queuedJob) EvaluationOutcome {
	close(self.started)
	<-ctx.Done()
	close(self.canceled)
	<-self.release
	self.joined.Store(true)
	return EvaluationOutcome{}
}

// The real Store boundary receives the same safe payload used by PostgresStore.
type heartbeatFailureStore struct {
	fakeStore
	cause                      error
	failHost                   bool
	evaluator                  *heartbeatFailureEvaluator
	jobId                      server.Id
	reason                     string
	detail                     competitionHeartbeatFailure
	joined, cleanupContextLive bool
	diagnosticCalls            int
}

// Job heartbeat success is retained when the adjacent host registration fails.
func (self *heartbeatFailureStore) Heartbeat(context.Context, *Settings, string, server.Id) error {
	if self.failHost {
		return nil
	}
	return self.cause
}

// Host failure has its own unchanged reason and uses the same safe cause path.
func (self *heartbeatFailureStore) RegisterHost(context.Context, *Settings, HostSelfCheck) error {
	if self.failHost {
		return self.cause
	}
	return nil
}

// Only the already-redacted DTO can cross this optional persistence boundary.
func (self *heartbeatFailureStore) handBackWithFailure(ctx context.Context, _ string, jobId server.Id, reason string, detail competitionHeartbeatFailure) error {
	self.diagnosticCalls++
	self.jobId, self.reason, self.detail = jobId, reason, detail
	self.joined, self.cleanupContextLive = self.evaluator.joined.Load(), ctx.Err() == nil
	return nil
}

// Executes evaluateOne with one forced heartbeat edge and explicit cleanup
// barriers. The supplied cause never reaches a real provider, DB or logger.
func exerciseCompetitionHeartbeatFailure(t *testing.T, cause error, failHost bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	settings := validSettings()
	settings.CompetitionId = "synthetic-heartbeat-competition"
	evaluator := &heartbeatFailureEvaluator{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { cancel(); release.Do(func() { close(evaluator.release) }) })
	store := &heartbeatFailureStore{cause: cause, failHost: failHost, evaluator: evaluator}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "synthetic-heartbeat-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	worker.evaluationTickForTest = ticks
	started := server.NowUtc()
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), StartedAt: &started}, AttemptCount: 1}
	host := passingHostCheck(settings)
	host.RebaselinePassed = true
	host.RebaselineRoundId = &job.RoundId
	done := make(chan error, 1)
	go func() { done <- worker.evaluateOne(ctx, job, host) }()
	select {
	case <-evaluator.started:
	case <-ctx.Done():
		t.Fatal("evaluation did not reach the explicit start barrier")
	}
	ticks <- started
	select {
	case <-evaluator.canceled:
	case <-ctx.Done():
		t.Fatal("failed heartbeat did not cancel the evaluator")
	}
	release.Do(func() { close(evaluator.release) })
	var returned error
	select {
	case returned = <-done:
	case <-ctx.Done():
		t.Fatal("worker did not finish joined cleanup")
	}
	wantReason := "heartbeat_failed"
	if failHost {
		wantReason = "host_heartbeat_failed"
	}
	if store.diagnosticCalls != 1 || store.handbacks != 0 || store.jobId != job.JobId || store.reason != wantReason || store.detail != competitionHeartbeatFailureDetails(cause) {
		t.Fatalf("safe heartbeat handback evidence missing or changed: calls=%d legacy=%d reason=%s detail=%+v", store.diagnosticCalls, store.handbacks, store.reason, store.detail)
	}
	if !store.joined || !store.cleanupContextLive || store.completedCount() != 0 {
		t.Fatal("handback preceded joined cleanup, reused canceled context or completed the job")
	}
	if returned == nil || !errors.Is(returned, cause) {
		t.Fatal("heartbeat error identity was suppressed")
	}
	if strings.Contains(returned.Error(), "synthetic-private") {
		t.Fatal("returned heartbeat error exposed private detail")
	}
	encoded, err := json.Marshal(competitionHandBackEvent{Reason: store.reason, competitionHeartbeatFailure: store.detail})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if json.Unmarshal(encoded, &wire) != nil || len(wire) < 3 || wire["error_category"] != store.detail.Category || wire["error_sha256"] != store.detail.DetailSha256 {
		t.Fatal("safe detail did not reach the existing handback event encoding")
	}
}

// A real lease-loss cause remains fail-closed and diagnosable.
func TestCompetitionHeartbeatFailurePersistsLeaseLossAfterJoin(t *testing.T) {
	exerciseCompetitionHeartbeatFailure(t, fmt.Errorf("synthetic-private context: %w", ErrLeaseLost), false)
}

// PostgreSQL causes retain only SQLSTATE and hash, not their message.
func TestCompetitionHeartbeatFailurePersistsPgCauseAfterJoin(t *testing.T) {
	exerciseCompetitionHeartbeatFailure(t, &pgconn.PgError{Code: "08006", Message: "synthetic-private-message"}, false)
}

// Timeouts still cancel the evaluator and preserve their errors.Is identity.
func TestCompetitionHeartbeatFailurePersistsTimeoutAfterJoin(t *testing.T) {
	exerciseCompetitionHeartbeatFailure(t, context.DeadlineExceeded, false)
}

// The adjacent host-heartbeat path must not silently drop its failure detail.
func TestCompetitionHostHeartbeatFailurePersistsCauseAfterJoin(t *testing.T) {
	exerciseCompetitionHeartbeatFailure(t, &pgconn.PgError{Code: "40001", Message: "synthetic-private-message"}, true)
}

// Legacy Store implementations still receive their original method and reason.
func TestCompetitionHeartbeatFailureKeepsLegacyStoreFallback(t *testing.T) {
	store := &fakeStore{}
	worker := &Worker{store: store, workerId: "synthetic-heartbeat-worker"}
	if err := worker.handBack(server.NewId(), "heartbeat_failed", ErrLeaseLost); err != nil || store.handbacks != 1 {
		t.Fatal("legacy store fallback changed")
	}
	if defaultCompetitionHeartbeatSettings().HandBackTimeout != 15*time.Second {
		t.Fatal("historical cleanup budget changed")
	}
}
