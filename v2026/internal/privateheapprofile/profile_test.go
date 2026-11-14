package privateheapprofile

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sampleSite() {}

func profileFixture() runtime.MemProfileRecord {
	return runtime.MemProfileRecord{AllocBytes: 8192, FreeBytes: 4096, AllocObjects: 2, FreeObjects: 1,
		Stack0: [32]uintptr{reflect.ValueOf(sampleSite).Pointer() + 1}}
}

func TestProfileFormatAndNoMutation(t *testing.T) {
	record := profileFixture()
	calls := 0
	data, n, err := heapText(context.Background(), func(out []runtime.MemProfileRecord, zero bool) (int, bool) {
		calls++
		if zero {
			t.Fatal("fully freed historical sites must be excluded")
		}
		if out == nil {
			return 1, false
		}
		if len(out) != MaxRecords {
			t.Fatalf("unbounded record allocation: %d", len(out))
		}
		out[0] = record
		return 1, true
	}, 512*1024)
	if err != nil || n != 1 || calls != 2 {
		t.Fatalf("capture n=%d calls=%d err=%v", n, calls, err)
	}
	if !strings.HasPrefix(string(data), "heap profile: 1: 4096 [2: 8192] @ heap/1048576\n") {
		t.Fatal("profile header")
	}
	if !strings.Contains(string(data), "privateheapprofile.sampleSite") {
		t.Fatal("symbolized allocation site missing")
	}
	file := filepath.Join(t.TempDir(), "heap.pprof")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "tool", "pprof", "-top", "-sample_index=inuse_space", file)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "inuse_space") {
		t.Fatalf("pprof roundtrip: %v: %s", err, output)
	}
}

func TestProfileRefusesBoundsWithoutRetry(t *testing.T) {
	for _, phase := range []string{"initial_bound", "growing", "invalid", "overflow", "bytes", "canceled", "disabled"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "canceled" {
				cancel()
			}
			calls := 0
			rate := 512 * 1024
			if phase == "disabled" {
				rate = 0
			}
			data, _, err := heapText(ctx, func(out []runtime.MemProfileRecord, _ bool) (int, bool) {
				calls++
				if phase == "initial_bound" {
					return MaxRecords + 1, false
				}
				if out == nil {
					return 1, false
				}
				if phase == "growing" {
					return MaxRecords + 1, false
				}
				r := profileFixture()
				if phase == "invalid" {
					r.FreeBytes = r.AllocBytes + 1
				}
				if phase == "overflow" {
					r.AllocBytes = math.MaxInt64
					r.FreeBytes = 0
					out[0], out[1] = r, r
					return 2, true
				}
				if phase == "bytes" {
					for i := range r.Stack0 {
						r.Stack0[i] = reflect.ValueOf(sampleSite).Pointer() + 1
					}
					for i := range out {
						out[i] = r
					}
					return len(out), true
				}
				out[0] = r
				return 1, true
			}, rate)
			if err == nil || data != nil || calls > 2 {
				t.Fatalf("bounded refusal err=%v bytes=%d calls=%d", err, len(data), calls)
			}
		})
	}
}

func testIdentity() Identity {
	return Identity{PID: 7, StartTicks: 123, BootID: "00000000-0000-0000-0000-000000000001", Revision: strings.Repeat("a", 40), ExecutableSHA256: strings.Repeat("b", 64), Host: "by-us-fmt-5-edge-3", Block: "g2"}
}

func testServer(t *testing.T, peer func(*net.UnixConn) bool, capture func(context.Context, func() Companion) (Capture, error)) *Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	s, err := start(context.Background(), Config{}, dir, testIdentity(), peer, capture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket not private")
	}
	return s
}

func request(t *testing.T, s *Server, identity Identity) (Response, []byte, error) {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: s.path})
	if err != nil {
		return Response{}, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(Request{Schema: 1, Expected: identity}); err != nil {
		return Response{}, nil, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return Response{}, nil, err
	}
	var out Response
	if err := json.Unmarshal(line, &out); err != nil {
		return out, nil, err
	}
	if out.Capture == nil {
		return out, nil, nil
	}
	if out.Capture.ProfileBytes > MaxProfileBytes {
		return out, nil, errors.New("profile bound")
	}
	profile := make([]byte, out.Capture.ProfileBytes)
	_, err = io.ReadFull(reader, profile)
	if err != nil {
		return out, nil, err
	}
	digest := sha256.Sum256(profile)
	if hex.EncodeToString(digest[:]) != out.Capture.ProfileSHA256 {
		return out, nil, errors.New("profile hash")
	}
	return out, profile, nil
}

func TestAuthenticatedIdentityAndOneUse(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) {
		calls.Add(1)
		p := []byte("bounded-profile")
		h := sha256.Sum256(p)
		return Capture{Profile: p, ProfileBytes: len(p), ProfileSHA256: hex.EncodeToString(h[:])}, nil
	})
	for _, field := range []string{"pid", "start", "boot", "source", "image", "modified", "host", "block"} {
		id := testIdentity()
		switch field {
		case "pid":
			id.PID++
		case "start":
			id.StartTicks++
		case "boot":
			id.BootID = "different"
		case "source":
			id.Revision = "different"
		case "image":
			id.ExecutableSHA256 = "different"
		case "modified":
			id.Modified = true
		case "host":
			id.Host = "different"
		case "block":
			id.Block = "g1"
		}
		out, _, err := request(t, s, id)
		if err != nil || out.Status != "identity_mismatch" || calls.Load() != 0 {
			t.Fatalf("identity %s: %v %s", field, err, out.Status)
		}
	}
	out, data, err := request(t, s, testIdentity())
	if err != nil || out.Status != "complete" || string(data) != "bounded-profile" || calls.Load() != 1 {
		t.Fatalf("authorized capture %v %s", err, out.Status)
	}
	out, _, err = request(t, s, testIdentity())
	if err != nil || out.Status != "already_consumed" || calls.Load() != 1 {
		t.Fatal("capture repeated")
	}
}

func TestUnauthenticatedDoesNotCapture(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return false }, func(context.Context, func() Companion) (Capture, error) { calls.Add(1); return Capture{}, nil })
	_, _, err := request(t, s, testIdentity())
	if err == nil || calls.Load() != 0 || s.used.Load() {
		t.Fatal("unauthenticated collection")
	}
}

func TestFailedCaptureIsConsumed(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) {
		calls.Add(1)
		return Capture{}, errProfileBound
	})
	out, _, err := request(t, s, testIdentity())
	if err != nil || out.Status != "capture_unavailable" {
		t.Fatal("missing capture failure")
	}
	out, _, err = request(t, s, testIdentity())
	if err != nil || out.Status != "already_consumed" || calls.Load() != 1 {
		t.Fatal("failed capture retried")
	}
}

func TestCloseJoinsAcceptedCapture(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(ctx context.Context, _ func() Companion) (Capture, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return Capture{}, ctx.Err()
	})
	finished := make(chan struct{})
	go func() { defer close(finished); _, _, _ = request(t, s, testIdentity()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("capture not entered")
	}
	s.Close()
	select {
	case <-canceled:
	default:
		t.Fatal("accepted child not joined")
	}
	<-finished
	if _, err := os.Lstat(s.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned socket retained")
	}
}

func TestDisabledAndNonTargetAreInert(t *testing.T) {
	for _, config := range []Config{{}, {TargetHost: "by-us-fmt-5-edge-3", TargetBlock: "g2", Host: "by-us-fmt-5-edge-1", Block: "g1"}} {
		s, err := Start(context.Background(), config)
		if err != nil || s != nil {
			t.Fatal("disabled or non-target was activated")
		}
	}
	for _, target := range []string{"by-us-fmt-5-edge-5/g1", "by-us-fmt-5-edge-3/beta", "../g1", "by-us-fmt-5-edge-3/g2/extra"} {
		if ValidTarget(target) {
			t.Fatal("invalid diagnostic target")
		}
	}
}

func TestNativeSampleDoesNotForceGC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), CaptureBudget)
	defer cancel()
	capture, err := collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capture.Before.ForcedGCCycles != capture.After.ForcedGCCycles {
		t.Fatal("forced GC occurred")
	}
	if capture.IntervalNS < SampleInterval.Nanoseconds() || capture.ProfileBytes <= 0 || capture.ProfileBytes > MaxProfileBytes || capture.Records > MaxRecords || math.IsNaN(capture.CPUCoreMean) {
		t.Fatal("native capture bounds")
	}
	if capture.Before.Companion.Available || capture.After.Companion.Available {
		t.Fatal("missing owner context invented")
	}
}
