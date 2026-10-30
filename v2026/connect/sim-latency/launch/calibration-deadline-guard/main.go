//go:build linux

// One-shot pre-finalization protection uses an existing scoped evaluation hold.
// It never writes calibration acceptance, retries CAS, or changes product policy.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Facts and budgets belong to this one closed deployment, not shared globals.
type deadlineGuardSettings struct {
	ApiUrl, Environment, CompetitionId, SeasonId, RoundId                              string
	Unit, CredentialPath, BodyPath, MarkerPath, ClaimPath, ProtectedRoot               string
	OwnerEvidenceSha256                                                                string
	OwnerUid                                                                           uint32
	ExpectedRevision                                                                   int
	Deadline                                                                           time.Time
	RequestTimeout, ConnectTimeout, StopTimeout, StopWaitDelay                         time.Duration
	MaximumBodyBytes, MaximumMarkerBytes, MaximumCredentialBytes, MaximumResponseBytes int64
}

// The executable accepts no endpoint, scope, file, clock, revision or unit flags.
func defaultDeadlineGuardSettings() deadlineGuardSettings {
	return deadlineGuardSettings{
		ApiUrl:      "https://api.bringyour.com/competition/v2/holds",
		Environment: "main", CompetitionId: "sim-latency-season-1",
		SeasonId:            "sim-latency-production-20261028-r2",
		RoundId:             "01a1855d-49bd-0250-90db-d0c0885f0e46",
		Unit:                "urnetwork-competition-production-r2.service",
		CredentialPath:      "/etc/urnetwork/competition-production-r2-20261029-v1/scheduler.token",
		BodyPath:            "/etc/urnetwork/competition-production-r2-20261029-v1/pre-finalization-hold-v2.json",
		MarkerPath:          "/var/lib/urnetwork/competition-production-r2-20261029-v1/calibration-accepted-before-finalization.json",
		ClaimPath:           "/var/lib/urnetwork/competition-production-r2-20261029-v1/pre-finalization-guard.claim",
		OwnerEvidenceSha256: "d0c8bcbd5a99eba6b412b9996797d023f02a0615339c46b1e0b448cd5ddc3834",
		ProtectedRoot:       "/", OwnerUid: 0, ExpectedRevision: 4,
		Deadline:       time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC),
		RequestTimeout: 10 * time.Second, ConnectTimeout: 3 * time.Second,
		StopTimeout: 50 * time.Second, StopWaitDelay: 2 * time.Second,
		MaximumBodyBytes: 16 * 1024, MaximumMarkerBytes: 16 * 1024,
		MaximumCredentialBytes: 16 * 1024, MaximumResponseBytes: 64 * 1024,
	}
}

// Field order matches the existing native seven-field canonical request digest.
type deadlineGuardHoldRequest struct {
	Kind              string `json:"kind"`
	Active            bool   `json:"active"`
	ExpectedRevision  int    `json:"expected_revision"`
	IdempotencyKey    string `json:"idempotency_key"`
	ReasonCode        string `json:"reason_code"`
	PublicExplanation string `json:"public_explanation"`
	EvidenceSha256    string `json:"evidence_sha256"`
}

// Root alone installs this marker after reviewing actual accepted calibration.
// Its evidence digest is not evaluated statistically by this operational guard.
type deadlineGuardMarker struct {
	Schema               int       `json:"schema"`
	Kind                 string    `json:"kind"`
	Environment          string    `json:"environment"`
	CompetitionId        string    `json:"competition_id"`
	SeasonId             string    `json:"season_id"`
	RoundId              string    `json:"round_id"`
	Accepted             bool      `json:"accepted"`
	AcceptedAt           time.Time `json:"accepted_at"`
	OwnerAmendmentSha256 string    `json:"owner_amendment_sha256"`
	EvidenceSha256       string    `json:"evidence_sha256"`
}

// This is the actual existing API response shape, not a new endpoint contract.
type deadlineGuardHoldResponse struct {
	HoldId        string                   `json:"hold_id"`
	Environment   string                   `json:"environment"`
	CompetitionId string                   `json:"competition_id"`
	SeasonId      string                   `json:"season_id"`
	ActorId       string                   `json:"actor_id"`
	Revision      int                      `json:"revision"`
	CreatedAt     time.Time                `json:"created_at"`
	Request       deadlineGuardHoldRequest `json:"request"`
	RequestSha256 string                   `json:"request_sha256"`
}

// Tests inject transport, clock and stop ownership without external actions.
type deadlineGuard struct {
	settings deadlineGuardSettings
	client   *http.Client
	now      func() time.Time
	stop     func(context.Context, string) error
}

// Redirects and proxy fallback are refused; each request has an independent bound.
func newDeadlineGuardClient(settings deadlineGuardSettings, transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = &http.Transport{
			Proxy: nil, DialContext: (&net.Dialer{Timeout: settings.ConnectTimeout}).DialContext,
			TLSHandshakeTimeout: settings.ConnectTimeout, DisableKeepAlives: true,
		}
	}
	return &http.Client{Transport: transport, Timeout: settings.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Owned evidence uses lowercase SHA-256, never null/empty identity equality.
func deadlineGuardSha256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Exactly one complete object is accepted; unknown fields cannot grant authority.
func decodeDeadlineGuardJson(content []byte, target any) error {
	if !utf8.Valid(content) {
		return errors.New("invalid protected JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid protected JSON")
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return errors.New("extra protected JSON")
	}
	return nil
}

// Only private regular single-link leaves under a protected ownership chain open.
// Descriptor-scoped resolution and nonblocking/no-follow opens exclude escapes.
func readDeadlineGuardFile(settings deadlineGuardSettings, path string, maximum int64) ([]byte, error) {
	relative, err := filepath.Rel(settings.ProtectedRoot, path)
	if err != nil || maximum <= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		!filepath.IsAbs(settings.ProtectedRoot) || filepath.Clean(settings.ProtectedRoot) != settings.ProtectedRoot ||
		!filepath.IsLocal(relative) || relative == "." {
		return nil, errors.New("protected file unavailable")
	}
	var leaf os.FileInfo
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, errors.New("protected file unavailable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != settings.OwnerUid || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("unprotected file path")
		}
		if current == path {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 ||
				info.Mode().Perm()&0400 == 0 || stat.Nlink != 1 || info.Size() > maximum {
				return nil, errors.New("unprotected file leaf")
			}
			leaf = info
		} else if !info.IsDir() {
			return nil, errors.New("unprotected file path")
		}
		if current == settings.ProtectedRoot {
			break
		}
	}
	root, err := os.OpenRoot(settings.ProtectedRoot)
	if err != nil {
		return nil, errors.New("protected file unavailable")
	}
	defer root.Close()
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("protected file unavailable")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !os.SameFile(leaf, before) {
		return nil, errors.New("protected file changed")
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if readErr != nil || statErr != nil || pathErr != nil || int64(len(content)) > maximum ||
		!os.SameFile(before, current) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		clear(content)
		return nil, errors.New("protected file changed")
	}
	return content, nil
}

// A durable one-use claim fences historical idempotency replays across restarts.
// Every failure retains any created leaf; the tool never removes or retries it.
func claimDeadlineGuardAttempt(settings deadlineGuardSettings) error {
	relative, err := filepath.Rel(settings.ProtectedRoot, settings.ClaimPath)
	if err != nil || !filepath.IsAbs(settings.ClaimPath) || filepath.Clean(settings.ClaimPath) != settings.ClaimPath ||
		!filepath.IsAbs(settings.ProtectedRoot) || filepath.Clean(settings.ProtectedRoot) != settings.ProtectedRoot ||
		!filepath.IsLocal(relative) || relative == "." {
		return errors.New("one-use claim unavailable")
	}
	for current := filepath.Dir(settings.ClaimPath); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return errors.New("one-use claim unavailable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != settings.OwnerUid || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return errors.New("one-use claim ancestry unprotected")
		}
		if current == settings.ProtectedRoot {
			break
		}
	}
	root, err := os.OpenRoot(settings.ProtectedRoot)
	if err != nil {
		return errors.New("one-use claim unavailable")
	}
	defer root.Close()
	file, err := root.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return errors.New("one-use claim already exists or is unavailable")
	}
	_, writeErr := io.WriteString(file, "one-use evaluation hold attempt; preserve on every outcome\n")
	modeErr := file.Chmod(0400)
	syncErr := file.Sync()
	closeErr := file.Close()
	if errors.Join(writeErr, modeErr, syncErr, closeErr) != nil {
		return errors.New("one-use claim not durable")
	}
	parent, err := root.Open(filepath.Dir(relative))
	if err != nil {
		return errors.New("one-use claim parent unavailable")
	}
	syncErr, closeErr = parent.Sync(), parent.Close()
	if errors.Join(syncErr, closeErr) != nil {
		return errors.New("one-use claim parent not durable")
	}
	return nil
}

// Invalid/missing markers authorize no skip and are never repaired by this tool.
func (self *deadlineGuard) calibrationAccepted(request deadlineGuardHoldRequest) bool {
	content, err := readDeadlineGuardFile(self.settings, self.settings.MarkerPath, self.settings.MaximumMarkerBytes)
	if err != nil {
		return false
	}
	defer clear(content)
	var marker deadlineGuardMarker
	if decodeDeadlineGuardJson(content, &marker) != nil {
		return false
	}
	return marker.Schema == 1 && marker.Kind == "competition-calibration-accepted-before-finalization" &&
		marker.Environment == self.settings.Environment && marker.CompetitionId == self.settings.CompetitionId &&
		marker.SeasonId == self.settings.SeasonId && marker.RoundId == self.settings.RoundId &&
		marker.Accepted && !marker.AcceptedAt.IsZero() && !marker.AcceptedAt.After(self.now().UTC()) &&
		marker.OwnerAmendmentSha256 == request.EvidenceSha256 && deadlineGuardSha256(marker.EvidenceSha256)
}

// Cancellation or uncertain network state still requests bounded worker shutdown.
// External error strings, bodies and credentials never enter returned diagnostics.
func (self *deadlineGuard) failClosed(ctx context.Context, message string) (string, error) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), self.settings.StopTimeout)
	defer cancel()
	if self.stop(stopCtx, self.settings.Unit) != nil {
		return "worker_stop_failed", errors.New(message + "; fixed worker stop failed")
	}
	return "worker_stopped", errors.New(message + "; fixed worker stopped")
}

// At/after the deadline there is one POST at most, with no latest-row guessing.
func (self *deadlineGuard) Run(ctx context.Context) (string, error) {
	if self.now().UTC().Before(self.settings.Deadline) {
		return "not_due", nil
	}
	body, err := readDeadlineGuardFile(self.settings, self.settings.BodyPath, self.settings.MaximumBodyBytes)
	if err != nil {
		return self.failClosed(ctx, "protected hold body unavailable")
	}
	defer clear(body)
	var request deadlineGuardHoldRequest
	if decodeDeadlineGuardJson(body, &request) != nil || request.Kind != "evaluation" || !request.Active ||
		request.ExpectedRevision != self.settings.ExpectedRevision || request.ReasonCode != "OPERATOR_HOLD" ||
		request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 ||
		strings.TrimSpace(request.PublicExplanation) == "" || len(request.PublicExplanation) > 4096 ||
		!deadlineGuardSha256(request.EvidenceSha256) || request.EvidenceSha256 != self.settings.OwnerEvidenceSha256 {
		return self.failClosed(ctx, "protected hold body invalid")
	}
	if self.calibrationAccepted(request) {
		return "calibration_marker_accepted", nil
	}
	credential, err := readDeadlineGuardFile(self.settings, self.settings.CredentialPath, self.settings.MaximumCredentialBytes)
	if err != nil {
		return self.failClosed(ctx, "protected scheduler credential unavailable")
	}
	defer clear(credential)
	token := strings.TrimSpace(string(credential))
	if token == "" {
		return self.failClosed(ctx, "protected scheduler credential invalid")
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return self.failClosed(ctx, "protected scheduler credential invalid")
		}
	}
	postCtx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	call, err := http.NewRequestWithContext(postCtx, http.MethodPost, self.settings.ApiUrl, bytes.NewReader(body))
	if err != nil {
		return self.failClosed(ctx, "fixed hold request unavailable")
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Authorization", "Bearer "+token)
	if claimDeadlineGuardAttempt(self.settings) != nil {
		return self.failClosed(ctx, "one-use hold attempt unavailable")
	}
	response, err := self.client.Do(call)
	if err != nil {
		return self.failClosed(ctx, "hold response unavailable")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaximumResponseBytes+1))
	if err != nil || int64(len(content)) > self.settings.MaximumResponseBytes || response.StatusCode != http.StatusOK {
		return self.failClosed(ctx, "hold response not confirmed")
	}
	var result deadlineGuardHoldResponse
	canonical, _ := json.Marshal(request)
	expectedDigest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	if decodeDeadlineGuardJson(content, &result) != nil || result.Environment != self.settings.Environment ||
		result.CompetitionId != self.settings.CompetitionId || result.SeasonId != self.settings.SeasonId ||
		result.HoldId == "" || result.ActorId == "" || result.CreatedAt.IsZero() ||
		result.Revision != request.ExpectedRevision+1 || result.Request != request ||
		result.RequestSha256 != expectedDigest {
		return self.failClosed(ctx, "hold response identity invalid")
	}
	return "evaluation_hold_confirmed", nil
}

// The fixed unit already owns bounded control-group cleanup of its own children.
func stopDeadlineGuardUnit(settings deadlineGuardSettings, ctx context.Context, unit string) error {
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "stop", unit)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = settings.StopWaitDelay
	if command.Run() != nil {
		return errors.New("fixed worker stop failed")
	}
	return nil
}

// Root/timer invocation emits only a fixed outcome, never remote or secret bytes.
func main() {
	if os.Geteuid() != 0 || len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "deadline guard requires its fixed Root invocation")
		os.Exit(1)
	}
	settings := defaultDeadlineGuardSettings()
	guard := &deadlineGuard{settings: settings, client: newDeadlineGuardClient(settings, nil), now: time.Now,
		stop: func(ctx context.Context, unit string) error { return stopDeadlineGuardUnit(settings, ctx, unit) }}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	outcome, err := guard.Run(ctx)
	fmt.Fprintln(os.Stdout, outcome)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deadline guard failed closed")
		os.Exit(1)
	}
}
