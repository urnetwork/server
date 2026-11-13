//go:build linux

// Synthetic clock, owned files and injected transport exercise every external
// decision without a real API, credential, service, database or model call.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A deterministic transport cannot fall through to a real network.
type deadlineGuardTestTransport func(*http.Request) (*http.Response, error)

// The callback owns the complete one-request observation.
func (self deadlineGuardTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// Each fixture owns synthetic identity, files, clock and exact action counts.
type deadlineGuardFixture struct {
	guard                     *deadlineGuard
	request                   deadlineGuardHoldRequest
	response                  deadlineGuardHoldResponse
	now                       time.Time
	token                     string
	calls, stops              int
	status                    int
	responseBytes             []byte
	transportError, stopError error
}

// No fixture imports production identities or reads default production paths.
func newDeadlineGuardFixture(t *testing.T) *deadlineGuardFixture {
	t.Helper()
	return newDeadlineGuardRevisionFixture(t, t.TempDir(), 7, "hold.json", "synthetic-deadline-hold")
}

// Rearm fixtures retain their own claim scope while varying sealed body identity.
func newDeadlineGuardRevisionFixture(t *testing.T, root string, expectedRevision int, bodyName, idempotencyKey string) *deadlineGuardFixture {
	t.Helper()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2034, time.April, 7, 11, 0, 0, 0, time.UTC)
	settings := deadlineGuardSettings{
		ApiUrl:      "https://control.example/competition/v2/holds",
		Environment: "test-environment", CompetitionId: "test-competition", SeasonId: "test-season",
		RoundId: "test-round", Unit: "synthetic-worker.service",
		CredentialPath: filepath.Join(root, "secrets", "scheduler.token"),
		BodyPath:       filepath.Join(root, bodyName), MarkerPath: filepath.Join(root, "accepted.json"),
		ClaimPath: filepath.Join(root, "attempt.claim"), OwnerEvidenceSha256: strings.Repeat("a", 64),
		ProtectedRoot: root, OwnerUid: uint32(os.Getuid()), ExpectedRevision: expectedRevision,
		Deadline: now, RequestTimeout: time.Second, ConnectTimeout: time.Second,
		StopTimeout: time.Second, StopWaitDelay: time.Second,
		MaximumBodyBytes: 16 * 1024, MaximumMarkerBytes: 16 * 1024,
		MaximumCredentialBytes: 16 * 1024, MaximumResponseBytes: 64 * 1024,
	}
	fixture := &deadlineGuardFixture{
		now: now, token: "synthetic-scheduler-token", status: http.StatusOK,
		request: deadlineGuardHoldRequest{Kind: "evaluation", Active: true, ExpectedRevision: settings.ExpectedRevision,
			IdempotencyKey: idempotencyKey, ReasonCode: "OPERATOR_HOLD",
			PublicExplanation: "Synthetic pending calibration.", EvidenceSha256: strings.Repeat("a", 64)},
	}
	body, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.response = deadlineGuardHoldResponse{
		HoldId: "11111111-2222-4333-8444-555555555555", Environment: settings.Environment,
		CompetitionId: settings.CompetitionId, SeasonId: settings.SeasonId, ActorId: "synthetic-scheduler",
		Revision: settings.ExpectedRevision + 1, CreatedAt: now,
		Request: fixture.request, RequestSha256: fmt.Sprintf("%x", sha256.Sum256(body)),
	}
	fixture.guard = &deadlineGuard{settings: settings, now: func() time.Time { return fixture.now },
		stop: func(ctx context.Context, unit string) error {
			fixture.stops++
			if unit != settings.Unit || ctx.Err() != nil {
				t.Error("stop lost fixed unit or independent context")
			}
			return fixture.stopError
		}}
	fixture.guard.client = newDeadlineGuardClient(settings, deadlineGuardTestTransport(func(call *http.Request) (*http.Response, error) {
		fixture.calls++
		if call.Method != http.MethodPost || call.URL.String() != settings.ApiUrl ||
			call.Header.Get("Authorization") != "Bearer "+fixture.token || call.Header.Get("Content-Type") != "application/json" {
			t.Error("request changed its fixed method, identity or authorization")
		}
		content, err := io.ReadAll(call.Body)
		if err != nil || string(content) != string(body) {
			t.Error("request changed sealed bytes")
		}
		if fixture.transportError != nil {
			return nil, fixture.transportError
		}
		responseBytes := fixture.responseBytes
		if responseBytes == nil {
			responseBytes, err = json.Marshal(fixture.response)
			if err != nil {
				t.Fatal(err)
			}
		}
		return &http.Response{StatusCode: fixture.status, Body: io.NopCloser(strings.NewReader(string(responseBytes))),
			Header: make(http.Header), Request: call}, nil
	}))
	writeDeadlineGuardFixture(t, settings.BodyPath, body)
	writeDeadlineGuardFixture(t, settings.CredentialPath, []byte(fixture.token+"\n"))
	return fixture
}

// Only fresh owned leaves are created; production files are unreachable here.
func writeDeadlineGuardFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

// A Root acceptance stand-in binds the exact synthetic amendment and round.
func (self *deadlineGuardFixture) marker() deadlineGuardMarker {
	return deadlineGuardMarker{Schema: 1, Kind: "competition-calibration-accepted-before-finalization",
		Environment: self.guard.settings.Environment, CompetitionId: self.guard.settings.CompetitionId,
		SeasonId: self.guard.settings.SeasonId, RoundId: self.guard.settings.RoundId,
		Accepted: true, AcceptedAt: self.now.Add(-time.Hour),
		OwnerAmendmentSha256: self.request.EvidenceSha256, EvidenceSha256: strings.Repeat("b", 64)}
}

// Encoding fixture metadata never touches a real marker or accepts calibration.
func (self *deadlineGuardFixture) writeMarker(t *testing.T, marker deadlineGuardMarker) {
	t.Helper()
	content, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDeadlineGuardFixture(t, self.guard.settings.MarkerPath, content)
}

// Pending calibration re-holds once and confirms the exact resulting revision.
func TestDeadlineGuardPendingCalibrationAppliesOneHold(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	outcome, err := fixture.guard.Run(t.Context())
	if err != nil || outcome != "evaluation_hold_confirmed" || fixture.calls != 1 || fixture.stops != 0 {
		t.Fatalf("pending hold not confirmed: outcome=%s error=%v calls=%d stops=%d", outcome, err, fixture.calls, fixture.stops)
	}
}

// The native API replays historical rows, so a second invocation must not POST.
func TestDeadlineGuardDurableClaimStopsReplayWithoutNetwork(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	if _, err := fixture.guard.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(fixture.guard.settings.ClaimPath)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("one-use claim was not retained privately")
	}
	outcome, err := fixture.guard.Run(t.Context())
	if err == nil || outcome != "worker_stopped" || fixture.calls != 1 || fixture.stops != 1 {
		t.Fatal("repeat invocation treated a historical POST replay as current authority")
	}
	fixture.writeMarker(t, fixture.marker())
	outcome, err = fixture.guard.Run(t.Context())
	if err != nil || outcome != "calibration_marker_accepted" || fixture.calls != 1 || fixture.stops != 1 {
		t.Fatal("subsequent Root acceptance did not skip without recycling the claim")
	}
}

// No early invocation can claim the deadline or issue a service/API action.
func TestDeadlineGuardBeforeDeadlineIsInert(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	fixture.now = fixture.now.Add(-time.Second)
	outcome, err := fixture.guard.Run(t.Context())
	if err != nil || outcome != "not_due" || fixture.calls != 0 || fixture.stops != 0 {
		t.Fatal("early invocation was not inert")
	}
}

// Accepted exact-scope Root evidence skips without opening a missing credential.
func TestDeadlineGuardTrustedMarkerSkipsNetworkAndStop(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	fixture.writeMarker(t, fixture.marker())
	if err := os.Remove(fixture.guard.settings.CredentialPath); err != nil {
		t.Fatal(err)
	}
	outcome, err := fixture.guard.Run(t.Context())
	if err != nil || outcome != "calibration_marker_accepted" || fixture.calls != 0 || fixture.stops != 0 {
		t.Fatal("trusted marker did not skip safely")
	}
}

// Scope, lineage, acceptance and time omissions never become authority by default.
func TestDeadlineGuardInvalidMarkersStillApplyHold(t *testing.T) {
	cases := []func(*deadlineGuardMarker){
		func(marker *deadlineGuardMarker) { marker.Accepted = false },
		func(marker *deadlineGuardMarker) { marker.Schema = 0 },
		func(marker *deadlineGuardMarker) { marker.Kind = "synthetic-wrong-kind" },
		func(marker *deadlineGuardMarker) { marker.Environment = "another-test-environment" },
		func(marker *deadlineGuardMarker) { marker.CompetitionId = "another-test-competition" },
		func(marker *deadlineGuardMarker) { marker.SeasonId = "another-test-season" },
		func(marker *deadlineGuardMarker) { marker.RoundId = "another-test-round" },
		func(marker *deadlineGuardMarker) { marker.EvidenceSha256 = "" },
		func(marker *deadlineGuardMarker) { marker.OwnerAmendmentSha256 = "" },
		func(marker *deadlineGuardMarker) { marker.OwnerAmendmentSha256 = strings.Repeat("c", 64) },
		func(marker *deadlineGuardMarker) { marker.AcceptedAt = time.Time{} },
		func(marker *deadlineGuardMarker) { marker.AcceptedAt = marker.AcceptedAt.Add(48 * time.Hour) },
	}
	for index, mutate := range cases {
		fixture := newDeadlineGuardFixture(t)
		marker := fixture.marker()
		mutate(&marker)
		fixture.writeMarker(t, marker)
		outcome, err := fixture.guard.Run(t.Context())
		if err != nil || outcome != "evaluation_hold_confirmed" || fixture.calls != 1 || fixture.stops != 0 {
			t.Fatalf("invalid marker case%d granted skip or lost hold", index)
		}
	}
}

// A failed CAS never causes a latest-row guess or a second POST.
func TestDeadlineGuardFailedCasStopsWithoutRetry(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	fixture.status = http.StatusConflict
	outcome, err := fixture.guard.Run(t.Context())
	if err == nil || outcome != "worker_stopped" || fixture.calls != 1 || fixture.stops != 1 {
		t.Fatal("failed CAS did not request exactly one fixed stop")
	}
}

// A syntactically valid response must still bind every acknowledged authority.
func TestDeadlineGuardMalformedResponsesStop(t *testing.T) {
	cases := []func(*deadlineGuardHoldResponse){
		func(response *deadlineGuardHoldResponse) { response.Environment = "other-environment" },
		func(response *deadlineGuardHoldResponse) { response.CompetitionId = "other-competition" },
		func(response *deadlineGuardHoldResponse) { response.SeasonId = "other-season" },
		func(response *deadlineGuardHoldResponse) { response.Revision++ },
		func(response *deadlineGuardHoldResponse) { response.Request.Kind = "reward_delivery" },
		func(response *deadlineGuardHoldResponse) { response.Request.Active = false },
		func(response *deadlineGuardHoldResponse) { response.Request.EvidenceSha256 = "" },
		func(response *deadlineGuardHoldResponse) { response.RequestSha256 = strings.Repeat("c", 64) },
		func(response *deadlineGuardHoldResponse) { response.HoldId = "" },
		func(response *deadlineGuardHoldResponse) { response.CreatedAt = time.Time{} },
	}
	for index, mutate := range cases {
		fixture := newDeadlineGuardFixture(t)
		mutate(&fixture.response)
		outcome, err := fixture.guard.Run(t.Context())
		if err == nil || outcome != "worker_stopped" || fixture.calls != 1 || fixture.stops != 1 {
			t.Fatalf("response case%d was accepted", index)
		}
	}
	for _, content := range []string{"null", "{}", "[]", "not-json", "{} {}"} {
		fixture := newDeadlineGuardFixture(t)
		fixture.responseBytes = []byte(content)
		if _, err := fixture.guard.Run(t.Context()); err == nil || fixture.calls != 1 || fixture.stops != 1 {
			t.Fatal("malformed response avoided fixed stop")
		}
	}
}

// Credential escape, sharing and writable ancestry fail before any API request.
func TestDeadlineGuardCredentialProtectionFailsClosed(t *testing.T) {
	cases := []func(*testing.T, *deadlineGuardFixture){
		func(t *testing.T, fixture *deadlineGuardFixture) {
			if err := os.Chmod(fixture.guard.settings.CredentialPath, 0644); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, fixture *deadlineGuardFixture) {
			target := filepath.Join(fixture.guard.settings.ProtectedRoot, "other-token")
			writeDeadlineGuardFixture(t, target, []byte("synthetic-unrelated-token"))
			if err := os.Remove(fixture.guard.settings.CredentialPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, fixture.guard.settings.CredentialPath); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, fixture *deadlineGuardFixture) {
			if err := os.Chmod(filepath.Dir(fixture.guard.settings.CredentialPath), 0777); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, fixture *deadlineGuardFixture) {
			if err := os.Link(fixture.guard.settings.CredentialPath, filepath.Join(fixture.guard.settings.ProtectedRoot, "shared-token")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for index, mutate := range cases {
		fixture := newDeadlineGuardFixture(t)
		mutate(t, fixture)
		if _, err := fixture.guard.Run(t.Context()); err == nil || fixture.calls != 0 || fixture.stops != 1 {
			t.Fatalf("credential protection case%d reached network or skipped stop", index)
		}
	}
}

// A missing/altered sealed body cannot be replaced with invented CAS authority.
func TestDeadlineGuardInvalidBodyStopsBeforeNetwork(t *testing.T) {
	for _, content := range []string{
		"null", "{}", "not-json", "{} {}",
		`{"kind":"evaluation","active":false}`,
	} {
		fixture := newDeadlineGuardFixture(t)
		writeDeadlineGuardFixture(t, fixture.guard.settings.BodyPath, []byte(content))
		if _, err := fixture.guard.Run(t.Context()); err == nil || fixture.calls != 0 || fixture.stops != 1 {
			t.Fatal("invalid body did not stop without a request")
		}
	}
	for _, mutate := range []func(*deadlineGuardHoldRequest){
		func(request *deadlineGuardHoldRequest) { request.ExpectedRevision-- },
		func(request *deadlineGuardHoldRequest) { request.Kind = "reward_delivery" },
		func(request *deadlineGuardHoldRequest) { request.Active = false },
		func(request *deadlineGuardHoldRequest) { request.ReasonCode = "RECOVERY_APPROVED" },
		func(request *deadlineGuardHoldRequest) { request.EvidenceSha256 = strings.Repeat("c", 64) },
	} {
		fixture := newDeadlineGuardFixture(t)
		body := fixture.request
		mutate(&body)
		content, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		writeDeadlineGuardFixture(t, fixture.guard.settings.BodyPath, content)
		if _, err := fixture.guard.Run(t.Context()); err == nil || fixture.calls != 0 || fixture.stops != 1 {
			t.Fatal("changed sealed authority reached network")
		}
	}
}

// Transport/stop diagnostics containing secrets are never returned or logged.
func TestDeadlineGuardUncertainOutcomeRedactsAndStops(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	fixture.transportError = fmt.Errorf("synthetic private transport detail %s", fixture.token)
	fixture.stopError = errors.New("synthetic private stop detail")
	outcome, err := fixture.guard.Run(t.Context())
	if err == nil || outcome != "worker_stop_failed" || fixture.calls != 1 || fixture.stops != 1 ||
		strings.Contains(err.Error(), fixture.token) || strings.Contains(err.Error(), "private") {
		t.Fatal("uncertain outcome leaked detail, retried, or lost stop failure")
	}
}

// A rearmed revision never treats its predecessor's valid sealed body as current
// authority. Refusal must precede both the durable one-use claim and transport.
func TestDeadlineGuardRearmedRevisionRejectsStaleBodyBeforeClaim(t *testing.T) {
	fixture := newDeadlineGuardFixture(t)
	fixture.guard.settings.ExpectedRevision = fixture.request.ExpectedRevision + 2
	outcome, err := fixture.guard.Run(t.Context())
	if err == nil || outcome != "worker_stopped" || fixture.calls != 0 || fixture.stops != 1 {
		t.Fatal("stale body revision reached transport or avoided the fixed stop")
	}
	if _, err := os.Lstat(fixture.guard.settings.ClaimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale body consumed the one-use claim")
	}
}

// A fresh valid revision/body works independently, but changing either cannot
// recycle the original claim even when a historical API response could replay.
func TestDeadlineGuardRearmedBodyCannotReuseConsumedGlobalClaim(t *testing.T) {
	fresh := newDeadlineGuardRevisionFixture(t, t.TempDir(), 9, "hold-v2.json", "synthetic-rearmed-hold")
	if outcome, err := fresh.guard.Run(t.Context()); err != nil || outcome != "evaluation_hold_confirmed" || fresh.calls != 1 || fresh.stops != 0 {
		t.Fatal("independent fresh revision/body was not admissible", err)
	}

	original := newDeadlineGuardFixture(t)
	if outcome, err := original.guard.Run(t.Context()); err != nil || outcome != "evaluation_hold_confirmed" || original.calls != 1 || original.stops != 0 {
		t.Fatal("original one-use hold was not confirmed", err)
	}
	claimPath := original.guard.settings.ClaimPath
	before, err := os.Lstat(claimPath)
	if err != nil || before.Mode().Perm() != 0400 {
		t.Fatal("original claim was not retained privately")
	}
	beforeBytes, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	rearmed := newDeadlineGuardRevisionFixture(t, original.guard.settings.ProtectedRoot, 9, "hold-v2.json", "synthetic-rearmed-hold")
	if rearmed.guard.settings.ClaimPath != claimPath || rearmed.guard.settings.MarkerPath != original.guard.settings.MarkerPath ||
		rearmed.guard.settings.BodyPath == original.guard.settings.BodyPath || rearmed.request.ExpectedRevision == original.request.ExpectedRevision {
		t.Fatal("synthetic rearm did not retain claim/marker scope with a new revision/body")
	}
	outcome, err := rearmed.guard.Run(t.Context())
	if err == nil || outcome != "worker_stopped" || original.calls != 1 || rearmed.calls != 0 || rearmed.stops != 1 {
		t.Fatal("rearmed revision recycled the consumed global claim or posted twice")
	}
	after, err := os.Lstat(claimPath)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0400 || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("rearm replaced or modified the consumed claim")
	}
	afterBytes, err := os.ReadFile(claimPath)
	if err != nil || string(beforeBytes) != string(afterBytes) {
		t.Fatal("rearm changed consumed claim contents")
	}
}
