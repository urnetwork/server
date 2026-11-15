package privateheapprofile

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

const testRequestID = "00112233445566778899aabbccddeeff"

func replayFixture() Capture {
	profile := []byte("heap profile: 1: 64 [1: 65] @ heap/1048576\n1: 64 [1: 64] @ 0x1234\n\n")
	digest := sha256.Sum256(profile)
	before := RuntimeContext{ObservedUTC: time.Date(2026, 10, 6, 0, 34, 50, 123456789, time.UTC)}
	after := before
	after.ObservedUTC = before.ObservedUTC.Add(3 * time.Second)
	return Capture{Profile: profile, Records: 1, Rate: 524288, StackPCBound: 32, ProfileSHA256: hex.EncodeToString(digest[:]), ProfileBytes: len(profile), Before: before, After: after, IntervalNS: int64(3 * time.Second)}
}

func v2Request(t *testing.T, s *Server, operation, id string, identity Identity) []byte {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: s.path})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(Request{Schema: 2, Operation: operation, RequestID: id, Expected: identity}); err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(io.LimitReader(conn, maxRetainedResponseBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > maxRetainedResponseBytes {
		t.Fatal("response exceeded bound")
	}
	return wire
}

func statusV2(t *testing.T, s *Server, id string) AttemptStatus {
	t.Helper()
	var out AttemptStatus
	if err := json.Unmarshal(v2Request(t, s, "status", id, testIdentity()), &out); err != nil {
		t.Fatal(err)
	}
	if out.Schema != 2 || out.Status != "status" || out.RequestID != id || out.Identity != testIdentity() {
		t.Fatal("status binding")
	}
	return out
}

func stateV2(t *testing.T, wire []byte) string {
	t.Helper()
	var out Response
	line, _, _ := bytes.Cut(wire, []byte{'\n'})
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatal(err)
	}
	return out.Status
}

func TestRetainedStatusDoesNotCaptureAndReplayIsExactOnce(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) { calls.Add(1); return replayFixture(), nil })
	for i := 0; i < 3; i++ {
		if got := statusV2(t, s, testRequestID); got.AttemptState != "unused" || got.RetainedBytes != 0 || calls.Load() != 0 || s.used.Load() {
			t.Fatal("status consumed sampler")
		}
	}
	original := v2Request(t, s, "capture", testRequestID, testIdentity())
	if stateV2(t, original) != "complete" || calls.Load() != 1 {
		t.Fatal("capture failed")
	}
	status := statusV2(t, s, testRequestID)
	if status.AttemptState != "available" || status.RetainedBytes != len(original) || status.ExpiresInMillis <= 0 || status.ExpiresInMillis > responseTTL.Milliseconds() {
		t.Fatal("retention accounting")
	}
	if stateV2(t, v2Request(t, s, "capture", testRequestID, testIdentity())) != "already_consumed" || calls.Load() != 1 {
		t.Fatal("duplicate capture")
	}
	if statusV2(t, s, "ffeeddccbbaa99887766554433221100").AttemptState != "request_mismatch" {
		t.Fatal("wrong request accepted")
	}
	if stateV2(t, v2Request(t, s, "retrieve", "ffeeddccbbaa99887766554433221100", testIdentity())) != "request_mismatch" {
		t.Fatal("wrong request retrieved")
	}
	replayed := v2Request(t, s, "retrieve", testRequestID, testIdentity())
	if !bytes.Equal(original, replayed) || calls.Load() != 1 {
		t.Fatal("retrieval changed bytes or captured")
	}
	if statusV2(t, s, testRequestID).AttemptState != "retrieved" {
		t.Fatal("missing consumed retrieval")
	}
	if stateV2(t, v2Request(t, s, "retrieve", testRequestID, testIdentity())) != "retrieved" || calls.Load() != 1 {
		t.Fatal("retrieval repeated")
	}
}

func TestRetainedCaptureSurvivesOriginalReaderDisconnect(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) { calls.Add(1); return replayFixture(), nil })
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: s.path})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(Request{Schema: 2, Operation: "capture", RequestID: testRequestID, Expected: testIdentity()}); err != nil {
		t.Fatal(err)
	}
	// Model a reader that rejects a complete header and closes before the body.
	header, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !bytes.Contains(header, []byte("123456789Z")) {
		t.Fatal("missing actual Go nanosecond clock")
	}
	if statusV2(t, s, testRequestID).AttemptState != "available" {
		t.Fatal("failed reader destroyed capture")
	}
	wire := v2Request(t, s, "retrieve", testRequestID, testIdentity())
	want, err := responseWire(testIdentity(), testRequestID, replayFixture())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire, want) || calls.Load() != 1 {
		t.Fatal("one-shot retained result lost")
	}
}

func TestRetainedTTLAndCloseReleaseOwnedBytes(t *testing.T) {
	for _, closeEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "expires", true: "close"}[closeEarly], func(t *testing.T) {
			var r retainedResponse
			r.begin(testRequestID)
			wire := make([]byte, 4096)
			if !r.keep(context.Background(), wire, 5*time.Millisecond) {
				t.Fatal("retention failed")
			}
			if closeEarly {
				r.close()
			} else {
				r.workers.Wait()
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.wire != nil || r.stop != nil || r.state != "expired" {
				t.Fatal("retained owner outlived expiry/join")
			}
		})
	}
	var r retainedResponse
	r.begin(testRequestID)
	if r.keep(context.Background(), make([]byte, 1, maxRetainedResponseBytes+1), time.Minute) {
		t.Fatal("oversized backing retained")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r.keep(ctx, []byte("private"), time.Minute) {
		t.Fatal("retained after close")
	}
}

func TestRetainedResponseBoundsAndFailedCaptureStayConsumed(t *testing.T) {
	fixture := replayFixture()
	fixture.Profile = make([]byte, MaxProfileBytes)
	if _, err := responseWire(testIdentity(), testRequestID, fixture); err == nil {
		t.Fatal("header bypassed total retained bound")
	}
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) {
		calls.Add(1)
		return Capture{}, errors.New("fixture")
	})
	if stateV2(t, v2Request(t, s, "capture", testRequestID, testIdentity())) != "capture_unavailable" {
		t.Fatal("failure not reported")
	}
	if statusV2(t, s, testRequestID).AttemptState != "capture_unavailable" {
		t.Fatal("failure not retained as state")
	}
	if stateV2(t, v2Request(t, s, "retrieve", testRequestID, testIdentity())) != "capture_unavailable" {
		t.Fatal("failure fetched data")
	}
	if stateV2(t, v2Request(t, s, "capture", testRequestID, testIdentity())) != "already_consumed" || calls.Load() != 1 {
		t.Fatal("failed capture retried")
	}
}

func TestRetainedIdentityAndMalformedV2CannotConsume(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) { calls.Add(1); return replayFixture(), nil })
	for _, op := range []string{"status", "retrieve", "capture"} {
		id := testIdentity()
		id.StartTicks++
		if stateV2(t, v2Request(t, s, op, testRequestID, id)) != "identity_mismatch" {
			t.Fatal("wrong generation accepted")
		}
	}
	for _, item := range [][2]string{{"capture", "short"}, {"capture", "00112233445566778899AABBCCDDEEFF"}, {"reset", testRequestID}} {
		if len(v2Request(t, s, item[0], item[1], testIdentity())) != 0 {
			t.Fatal("malformed operation accepted")
		}
	}
	if calls.Load() != 0 || s.used.Load() {
		t.Fatal("invalid request consumed")
	}
	denied := testServer(t, func(*net.UnixConn) bool { return false }, func(context.Context, func() Companion) (Capture, error) { calls.Add(1); return replayFixture(), nil })
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: denied.path})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	_ = json.NewEncoder(conn).Encode(Request{Schema: 2, Operation: "status", RequestID: testRequestID, Expected: testIdentity()})
	wire, _ := io.ReadAll(conn) // EOF or a kernel reset is expected before any response.
	conn.Close()
	if len(wire) != 0 || denied.used.Load() || calls.Load() != 0 {
		t.Fatal("nonroot status allowed")
	}
}

func TestRetainedLegacyCompatibilityAndRestartForgetState(t *testing.T) {
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) { return replayFixture(), nil })
	if _, _, err := request(t, s, testIdentity()); err != nil {
		t.Fatal(err)
	}
	if statusV2(t, s, testRequestID).AttemptState != "legacy_consumed" {
		t.Fatal("legacy state invented retention")
	}
	s.Close()
	next := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) { return replayFixture(), nil })
	if statusV2(t, next, testRequestID).AttemptState != "unused" {
		t.Fatal("retention crossed process owner")
	}
}

func TestRetainedCloseWaitsForLateCaptureAndDiscards(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) {
		close(entered)
		<-release // Model a nonpreemptible runtime call.
		return replayFixture(), nil
	})
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: s.path})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Schema: 2, Operation: "capture", RequestID: testRequestID, Expected: testIdentity()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("capture not entered")
	}
	joined := make(chan struct{})
	go func() { defer close(joined); s.Close() }()
	select {
	case <-joined:
		t.Fatal("Close abandoned nonpreemptible owner")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("Close did not join")
	}
	s.retained.mu.Lock()
	defer s.retained.mu.Unlock()
	if s.retained.wire != nil || s.retained.stop != nil || s.retained.state != "expired" {
		t.Fatal("late response retained after cancellation")
	}
}
