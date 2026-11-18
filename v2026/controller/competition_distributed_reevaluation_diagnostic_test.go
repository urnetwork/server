// Semantic grant refusals retain safe native transport facts but never acquire
// authority from a failed exchange or expose its private underlying error text.
package controller

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// Only one closed read-only operation fails; all others use the existing real
// synthetic-signature validator fixture. Call counts prove immediate refusal.
type competitionRecoveryDiagnosticTransport struct {
	base      competitionDistributedRecoveryTestTransport
	operation string
	failure   error
	calls     *int
}

// Failure never supplies a response; healthy calls retain signed validation.
func (self competitionRecoveryDiagnosticTransport) Exchange(ctx context.Context, request competitionagent.Request, input, output string) (competitionagent.Response, error) {
	*self.calls++
	if request.Operation == self.operation {
		return competitionagent.Response{}, self.failure
	}
	return self.base.Exchange(ctx, request, input, output)
}

// Original-status transport loss remains a conflict with its exact unwrap
// causes; the joined error's description contains no private native strings.
func TestCompetitionRecoveryStatusPreservesTransportDiagnostic(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	private := errors.New("synthetic-private-stderr-path-command-sentinel")
	failure := &CompetitionAgentTransportError{stage: "read", firstErr: io.EOF, contextAfter: context.Canceled, ReadFailed: true, cause: errors.Join(private, io.EOF, context.Canceled)}
	calls := 0
	factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionRecoveryDiagnosticTransport{base: competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round}, operation: "status", failure: failure, calls: &calls}, nil
	}
	proof, err := observeCompetitionDistributedRecovery(t.Context(), settings, round, request, factory)
	var got *CompetitionAgentTransportError
	if proof != nil || calls != 1 || !errors.Is(err, ErrConflict) || errors.Is(err, ErrLeaseLost) || !errors.As(err, &got) || got != failure || !errors.Is(err, private) || !errors.Is(err, io.EOF) || !errors.Is(err, context.Canceled) {
		t.Fatal("status refusal lost semantic or transport authority boundaries")
	}
	if strings.Contains(err.Error(), "sentinel") || !strings.Contains(err.Error(), "stage=read") {
		t.Fatal("status refusal erased safe metadata or exposed private data")
	}
}

// Availability transport loss still means lease lost, not a reusable partial
// proof or a newly authoritative status/availability response.
func TestCompetitionRecoveryAvailabilityPreservesTransportDiagnostic(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	private := errors.New("synthetic-private-availability-sentinel")
	failure := &CompetitionAgentTransportError{stage: "wait", firstErr: private, WaitFailed: true, ChildExitKnown: true, ChildExitCode: 7, cause: private}
	calls := 0
	factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionRecoveryDiagnosticTransport{base: competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round}, operation: "availability", failure: failure, calls: &calls}, nil
	}
	proof, err := observeCompetitionDistributedRecovery(t.Context(), settings, round, request, factory)
	var got *CompetitionAgentTransportError
	if proof != nil || calls != 2 || !errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrConflict) || !errors.As(err, &got) || got != failure || !errors.Is(err, private) {
		t.Fatal("availability refusal lost semantic or transport authority boundaries")
	}
	if strings.Contains(err.Error(), "sentinel") || !strings.Contains(err.Error(), "child_exit=7") {
		t.Fatal("availability refusal erased safe metadata or exposed private data")
	}
}

// An invalid authentically signed envelope with no transport failure keeps the
// old semantic sentinel; no fabricated process diagnostic is attached to it.
func TestCompetitionRecoveryInvalidEnvelopeKeepsSemanticRefusal(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round, mutate: func(response *competitionagent.Response) {
			if response.Identity != nil {
				response.FencingToken++
			}
		}}, nil
	}
	proof, err := observeCompetitionDistributedRecovery(t.Context(), settings, round, request, factory)
	var failure *CompetitionAgentTransportError
	if proof != nil || !errors.Is(err, ErrConflict) || errors.As(err, &failure) {
		t.Fatal("invalid envelope borrowed transport authority")
	}
}

// Joining private native causes cannot change the existing fixed HTTP error
// class/message selected by either semantic recovery refusal sentinel.
func TestCompetitionRecoveryTransportKeepsPublicErrorMapping(t *testing.T) {
	private := errors.New("synthetic-private-http-diagnostic-sentinel")
	failure := &CompetitionAgentTransportError{stage: "read", firstErr: io.EOF, ReadFailed: true, cause: private}
	for _, semantic := range []error{ErrConflict, ErrLeaseLost} {
		expectedStatus, expected := competitionAutomationError(semantic)
		status, public := competitionAutomationError(errors.Join(semantic, failure))
		if status != expectedStatus || !reflect.DeepEqual(public, expected) {
			t.Fatal("safe diagnostic altered the public semantic refusal")
		}
	}
}
