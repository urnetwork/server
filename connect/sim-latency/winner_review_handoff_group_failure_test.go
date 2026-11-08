// Failed containment has protected audit custody, never a portable success
// proof or cleanup capability. The root one-shot intent remains permanently.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
)

type winnerHandoffGroupFailedCapture struct {
	Schema     int                                    `json:"schema"`
	Kind       string                                 `json:"kind"`
	PlanSha256 string                                 `json:"plan_sha256"`
	Invocation winnerMultipassQualificationInvocation `json:"invocation"`
	Original   winnerHandoffCustodyOriginal           `json:"original"`
}

// Return the typed terminal audit only after exact protected readback and a
// durable compact index. Any partial write keeps the full local original.
func retainWinnerHandoffGroupFailure(ctx context.Context, directory, planSha256 string, capture winnerMultipassQualificationCapture, minimumUntil time.Time,
	retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error), read winnerHandoffSpoolReader) (winnerHandoffCustodyOriginal, error) {
	cause := errors.New("failed physical capture retained without cleanup or retry")
	content, err := json.Marshal(capture)
	if err != nil || len(content) > winnerMultipassSpoolCaptureBytes || !validSha256(planSha256) || read == nil || capture.Invocation.Position <= 0 {
		return winnerHandoffCustodyOriginal{}, errors.New("failed capture lacks a bounded original and protected reader")
	}
	name := fmt.Sprintf("failed-capture-%06d.json", capture.Invocation.Position)
	if err := writePrivateReviewFile(filepath.Join(directory, name), content); err != nil {
		return winnerHandoffCustodyOriginal{}, errors.Join(cause, err)
	}
	original, err := retainWinnerHandoffDocument(ctx, name, content, winnerMultipassSpoolCaptureBytes, minimumUntil, retain)
	if err != nil {
		return original, errors.Join(cause, err)
	}
	protected, err := read(ctx, original)
	if err != nil || !bytes.Equal(protected, content) {
		return original, errors.Join(cause, errors.New("failed capture protected exact-version readback unavailable or changed"), err)
	}
	commit := winnerHandoffGroupFailedCapture{Schema: 1, Kind: "winner-review-qualification-failed-capture/v1", PlanSha256: planSha256, Invocation: capture.Invocation, Original: original}
	compact, _ := json.Marshal(commit)
	if len(compact) > winnerMultipassSpoolManifestBytes {
		return original, errors.Join(cause, errors.New("failed capture compact index exceeds its admitted bound"))
	}
	if _, err := retainWinnerHandoffDocument(ctx, "failed-capture-commit.json", compact, winnerMultipassSpoolManifestBytes, minimumUntil, retain); err != nil {
		return original, errors.Join(cause, err)
	}
	path := filepath.Join(directory, fmt.Sprintf("failed-capture-%06d-commit.json", capture.Invocation.Position))
	if err := writePrivateReviewFile(path, compact); err != nil {
		return original, errors.Join(cause, err)
	}
	if err := syncWinnerHandoffSpoolDirectory(directory); err != nil {
		return original, errors.Join(cause, err)
	}
	return original, &winnerMultipassQualificationSpoolRetentionError{Failure: currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(compact)}, Cause: cause}
}

func TestWinnerHandoffGroupFailedCaptureHasProtectedTerminalReference(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	capture := winnerMultipassQualificationCapture{Invocation: winnerMultipassQualificationInvocation{Position: 1, CasePosition: 0, Operation: "tokenize"}, Input: []byte("synthetic original input"), AuthenticationError: "synthetic physical refusal"}
	original, err := retainWinnerHandoffGroupFailure(t.Context(), fixture.Directory, fixture.Commit.Binding.PlanSha256, capture, fixture.Commit.MinimumUntil, fixture.retain, fixture.read)
	var terminal *winnerMultipassQualificationSpoolRetentionError
	if !errors.As(err, &terminal) {
		t.Fatal("protected failed capture omitted terminal report reference", err)
	}
	compact, err := os.ReadFile(terminal.Failure.Path)
	var commit winnerHandoffGroupFailedCapture
	if err != nil || winnerMultipassSha256(compact) != terminal.Failure.Sha256 || json.Unmarshal(compact, &commit) != nil || commit.Original != original || commit.Invocation != capture.Invocation {
		t.Fatal("failed original reference changed", err)
	}
	content, _ := json.Marshal(capture)
	actual, err := fixture.read(t.Context(), original)
	if err != nil || !bytes.Equal(content, actual) {
		t.Fatal("failed original exact version was not retained", err)
	}
	if _, err := os.Stat(fixture.ClaimPath); err != nil {
		t.Fatal("failed capture removed its permanent one-shot claim", err)
	}
	if _, err := os.Stat(fixture.Commit.Attempt); err != nil {
		t.Fatal("failed capture removed unauthenticated physical originals", err)
	}
}

func TestWinnerHandoffGroupFailedCaptureReadbackCannotAuthorizeCleanup(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	fixture.Failure = "readback"
	capture := winnerMultipassQualificationCapture{Invocation: winnerMultipassQualificationInvocation{Position: 1}, AuthenticationError: "synthetic physical refusal"}
	_, err := retainWinnerHandoffGroupFailure(t.Context(), fixture.Directory, fixture.Commit.Binding.PlanSha256, capture, fixture.Commit.MinimumUntil, fixture.retain, fixture.read)
	var terminal *winnerMultipassQualificationSpoolRetentionError
	if err == nil || errors.As(err, &terminal) {
		t.Fatal("failed protected readback became a committed failure audit", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.Directory, "failed-capture-000001.json")); err != nil {
		t.Fatal("partial archive lost the local original", err)
	}
	if _, err := os.Stat(fixture.ClaimPath); err != nil {
		t.Fatal("archive failure released one-shot claim", err)
	}
}
