// Closed transport diagnostics retain causal ordering without disclosing any
// private bytes. Native fixtures preserve the existing request/Wait ownership.
package controller

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// An actual empty stdout EOF precedes the cancellation used only for joining.
func TestCompetitionAgentTransportEofPrecedesCleanupCancellation(t *testing.T) {
	fixture := newCompetitionAgentPipeFixture(t, buildCompetitionAgentPipeFixture(t), "before-response", "")
	fixture.start(t, "")
	fixture.emitAndExit(t, false)
	fixture.await(t)
	var failure *CompetitionAgentTransportError
	if !errors.As(fixture.err, &failure) || failure.Stage() != "read" || !failure.ReadFailed || failure.contextBefore != nil || !errors.Is(failure.contextAfter, context.Canceled) {
		t.Fatal("primary EOF was masked by cleanup cancellation", fixture.err)
	}
	if !errors.Is(fixture.err, io.EOF) || !errors.Is(fixture.err, context.Canceled) || fixture.response.Schema != 0 {
		t.Fatal("original EOF/cancellation or response authority changed", fixture.err)
	}
	if competitionAgentTransportErrorClass(failure.firstErr) != "eof" {
		t.Fatal("primary EOF classification changed")
	}
}

// The writer is held at an explicit barrier after its valid request. Its own
// failure must be recorded before the reader observes the resulting cancel.
func TestCompetitionAgentTransportWriteCauseSurvivesReaderCancellation(t *testing.T) {
	fixture := newCompetitionAgentPipeFixture(t, buildCompetitionAgentPipeFixture(t), "complete", "")
	written, failWrite := make(chan struct{}), make(chan struct{})
	privateErr := errors.New("synthetic private path/token/command sentinel")
	fixture.startCommand(t, func(writer io.Writer) error {
		if err := competitionagent.WriteDocument(writer, fixture.request, competitionagent.DefaultLimits().DocumentBytes); err != nil {
			return err
		}
		close(written)
		select {
		case <-failWrite:
			return privateErr
		case <-fixture.transportContext.Done():
			return fixture.transportContext.Err()
		}
	}, func(io.Reader) error {
		<-fixture.transportContext.Done()
		return fixture.transportContext.Err()
	})
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, written)
	close(failWrite)
	fixture.await(t)
	var failure *CompetitionAgentTransportError
	if !errors.As(fixture.err, &failure) || failure.Stage() != "write" || !failure.WriteFailed || failure.contextBefore != nil || !errors.Is(failure.contextAfter, context.Canceled) {
		t.Fatal("writer's first failure was replaced by reader cancellation", fixture.err)
	}
	if !errors.Is(fixture.err, privateErr) || !errors.Is(fixture.err, context.Canceled) || strings.Contains(fixture.err.Error(), "sentinel") {
		t.Fatal("original cause was lost or formatted private text")
	}
}

// Complete response I/O/Wait precedes parent cancellation. A diagnostic read
// closed by that cancellation must not become the primary transport failure.
func TestCompetitionAgentTransportParentCancelRemainsPrimary(t *testing.T) {
	fixture := newCompetitionAgentPipeFixture(t, buildCompetitionAgentPipeFixture(t), "complete-request-eof", "")
	readComplete, returnRead := make(chan struct{}), make(chan struct{})
	fixture.startCommand(t, func(writer io.Writer) error {
		return competitionagent.WriteDocument(writer, fixture.request, competitionagent.DefaultLimits().DocumentBytes)
	}, func(reader io.Reader) error {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return err
		}
		close(readComplete)
		select {
		case <-returnRead:
		case <-fixture.watchdog.Done():
			return fixture.watchdog.Err()
		}
		return nil
	})
	fixture.emitAndExit(t, false)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, readComplete)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, fixture.commandWaited)
	fixture.cancel()
	close(returnRead)
	fixture.await(t)
	var failure *CompetitionAgentTransportError
	if !errors.As(fixture.err, &failure) || failure.Stage() != "context" || !errors.Is(failure.contextBefore, context.Canceled) || !errors.Is(fixture.err, context.Canceled) {
		t.Fatal("caller cancellation was misclassified", fixture.err)
	}
	if failure.ReadFailed || failure.WriteFailed || failure.WaitFailed || failure.ChildCloseFailed || !failure.ChildExitKnown || failure.ChildExitCode != 0 {
		t.Fatal("healthy completed I/O was reported as a causal failure", fixture.err)
	}
}

// A valid frame plus nonzero actual child exit remains a failure, without
// generating context cancellation or formatting exec.ExitError's text.
func TestCompetitionAgentTransportChildExitIsClosedNumericFact(t *testing.T) {
	fixture := newCompetitionAgentPipeFixture(t, buildCompetitionAgentPipeFixture(t), "exit-failure", "")
	fixture.start(t, "")
	fixture.emitAndExit(t, false)
	fixture.await(t)
	var failure *CompetitionAgentTransportError
	var childExit *exec.ExitError
	if !errors.As(fixture.err, &failure) || !errors.As(fixture.err, &childExit) || failure.Stage() != "wait" || !failure.WaitFailed || !failure.ChildExitKnown || failure.ChildExitCode != 7 {
		t.Fatal("actual child exit did not remain a typed numeric fact", fixture.err)
	}
	if failure.contextBefore != nil || failure.contextAfter != nil || errors.Is(fixture.err, context.Canceled) || fixture.response.Schema != 0 {
		t.Fatal("child exit changed cancellation or response authority", fixture.err)
	}
}

// Overflow is observed before its own cancellation; stderr content never
// appears in the new typed description or any outer transport wrapper.
func TestCompetitionAgentTransportDiagnosticOverflowPrecedesCancel(t *testing.T) {
	fixture := newCompetitionAgentPipeFixture(t, buildCompetitionAgentPipeFixture(t), "diagnostic-overflow", "")
	fixture.transport.profile.Config.Settings.MaximumDiagnosticBytes = 16
	fixture.start(t, "")
	if err := fixture.agent.send("proceed"); err != nil {
		t.Fatal(err)
	}
	fixture.agent.send("exit")
	fixture.await(t)
	var failure *CompetitionAgentTransportError
	if !errors.As(fixture.err, &failure) || failure.Stage() != "diagnostic" || !failure.DiagnosticFailed || failure.contextBefore != nil || !errors.Is(failure.contextAfter, context.Canceled) {
		t.Fatal("diagnostic overflow was masked by cleanup", fixture.err)
	}
	if strings.Contains(fixture.err.Error(), "private synthetic diagnostic") || fixture.response.Schema != 0 {
		t.Fatal("private diagnostic bytes escaped or acquired authority")
	}
}

// Formatting the underlying error is a deterministic tripwire, not merely a
// substring check against one diagnostic payload.
type competitionAgentPrivateDiagnosticError struct{}

// Any formatting of private text is a test failure, not a usable error string.
func (self *competitionAgentPrivateDiagnosticError) Error() string {
	panic("private synthetic error text must never be formatted")
}

// Unwrap retains both private error identity and integrity/cancellation while
// even malformed stage/context metadata can emit only the closed vocabulary.
func TestCompetitionAgentTransportDescriptionNeverFormatsPrivateErrors(t *testing.T) {
	privateErr := &competitionAgentPrivateDiagnosticError{}
	failure := &CompetitionAgentTransportError{stage: "synthetic/private-stage-sentinel", firstErr: privateErr, contextBefore: privateErr, contextAfter: privateErr,
		ReadFailed: true, ChildExitKnown: true, ChildExitCode: 7, cause: errors.Join(privateErr, competitionagent.ErrIntegrity, context.Canceled)}
	text := failure.Error()
	if failure.Stage() != "unknown" || strings.Contains(text, "synthetic") || strings.Contains(text, "sentinel") || !strings.Contains(text, "child_exit=7") {
		t.Fatal("transport description is not closed", text)
	}
	var got *competitionAgentPrivateDiagnosticError
	if !errors.As(failure, &got) || got != privateErr || !errors.Is(failure, privateErr) || !errors.Is(failure, competitionagent.ErrIntegrity) || !errors.Is(failure, context.Canceled) {
		t.Fatal("safe formatting changed the original unwrap identities")
	}
}
