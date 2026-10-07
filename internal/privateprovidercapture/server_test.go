package privateprovidercapture

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/internal/privateheapprofile"
)

func syntheticIdentity() privateheapprofile.Identity {
	return privateheapprofile.Identity{PID: 42, StartTicks: 12345, BootID: "synthetic-boot.example", Revision: strings.Repeat("a", 40), ExecutableSHA256: strings.Repeat("b", 64), Host: "synthetic-host.example", Block: "synthetic-block"}
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func privateTestServer(t *testing.T, peer func(*net.UnixConn) bool) *Server {
	t.Helper()
	s, err := start(context.Background(), filepath.Join(privateTestDir(t), "private"), syntheticIdentity(), peer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func socketExchange(t *testing.T, s *Server, wire []byte) []byte {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: s.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(io.LimitReader(conn, MaxResponseBytes+1))
	if len(result) > MaxResponseBytes {
		t.Fatal("response exceeded fixed cap")
	}
	return result
}

func socketRequest(t *testing.T, s *Server, request Request) Response {
	t.Helper()
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result := socketExchange(t, s, append(wire, '\n'))
	var response Response
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal("private response unavailable")
	}
	return response
}

func TestProviderCaptureSocketIdentityAndOneUse(t *testing.T) {
	s := privateTestServer(t, func(*net.UnixConn) bool { return true })
	req := Request{Schema: 1, Expected: s.identity, Operation: "arm", CaptureID: strings.Repeat("c", 32)}
	req.Expected.StartTicks++
	if got := socketRequest(t, s, req); got.Snapshot.State != "identity_mismatch" || s.Recorder.Enabled() {
		t.Fatal("stale process identity armed")
	}
	req.Expected = s.identity
	if got := socketRequest(t, s, req); got.Snapshot.State != "armed" {
		t.Fatal("matching process failed to arm")
	}
	if !s.Recorder.Offer(syntheticRecord(time.Now())) {
		t.Fatal("actual sample refused")
	}
	req.Operation = "status"
	if got := socketRequest(t, s, req); got.Snapshot.Records != nil || got.Snapshot.Retained != 1 {
		t.Fatal("status exposed a tuple")
	}
	req.Operation = "read"
	if got := socketRequest(t, s, req); got.Snapshot.State != "complete" || len(got.Snapshot.Records) != 1 || got.Identity != s.identity {
		t.Fatal("private read lost sample provenance")
	}
	if got := socketRequest(t, s, req); got.Snapshot.State != "consumed" || got.Snapshot.Records != nil {
		t.Fatal("repeated export accepted")
	}
}

func TestProviderCaptureSocketMalformedRequestsDoNotArm(t *testing.T) {
	s := privateTestServer(t, func(*net.UnixConn) bool { return true })
	req := Request{Schema: 1, Expected: s.identity, Operation: "arm", CaptureID: strings.Repeat("c", 32)}
	wire, _ := json.Marshal(req)
	for _, bad := range []string{"invalid\n", strings.Repeat("x", MaxRequestBytes+1) + "\n", string(wire) + " {}\n", strings.TrimSuffix(string(wire), "}") + ",\"extra\":true}\n", strings.Replace(string(wire), "\"arm\"", "\"heap\"", 1) + "\n"} {
		if got := socketExchange(t, s, []byte(bad)); len(got) != 0 || s.Recorder.Enabled() {
			t.Fatal("malformed command armed or produced data")
		}
	}
	if got := socketRequest(t, s, req); got.Snapshot.State != "armed" {
		t.Fatal("malformed command consumed the arm")
	}
}

func TestProviderCaptureSocketRequiresKernelRootPeer(t *testing.T) {
	s := privateTestServer(t, privateheapprofile.RootPeer)
	wire, _ := json.Marshal(Request{Schema: 1, Expected: s.identity, Operation: "arm", CaptureID: strings.Repeat("c", 32)})
	out := socketExchange(t, s, append(wire, '\n'))
	if os.Geteuid() == 0 {
		if len(out) == 0 || !s.Recorder.Enabled() {
			t.Fatal("kernel root peer refused")
		}
	} else if len(out) != 0 || s.Recorder.Enabled() {
		t.Fatal("non-root peer accepted")
	}
}

func TestProviderCaptureSocketPrivatePathsAndIndependentProcesses(t *testing.T) {
	dir := filepath.Join(privateTestDir(t), "private")
	identity := syntheticIdentity()
	first, err := start(context.Background(), dir, identity, func(*net.UnixConn) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	identity.PID++
	second, err := start(context.Background(), dir, identity, func(*net.UnixConn) bool { return true })
	if err != nil {
		t.Fatal("second process collided with first socket")
	}
	defer second.Close()
	if first.path == second.path {
		t.Fatal("two processes shared a socket path")
	}
	if info, err := os.Lstat(first.path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket is not private")
	}
	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("socket directory is not private")
	}
	if duplicate, err := start(context.Background(), dir, identity, func(*net.UnixConn) bool { return true }); err == nil || duplicate != nil {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatal("occupied process socket was replaced")
	}
	first.Close()
	if _, err := os.Lstat(second.path); err != nil {
		t.Fatal("closing first process removed second socket")
	}
}

func TestProviderCaptureSocketRefusesUnsafeAndCrashStalePaths(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "stale"} {
		base := privateTestDir(t)
		dir := filepath.Join(base, "private")
		if kind == "symlink" {
			if err := os.Symlink(base, dir); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "public" {
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "stale" {
				if err := os.WriteFile(SocketPath(dir, syntheticIdentity()), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		s, err := start(context.Background(), dir, syntheticIdentity(), func(*net.UnixConn) bool { return true })
		if err == nil || s != nil {
			if s != nil {
				s.Close()
			}
			t.Fatal("unsafe path accepted")
		}
		if kind == "stale" {
			if data, err := os.ReadFile(SocketPath(dir, syntheticIdentity())); err != nil || string(data) != "preserve" {
				t.Fatal("stale evidence path modified")
			}
		}
	}
}

func TestProviderCaptureSocketInactiveOutsideMain(t *testing.T) {
	s, err := Start(context.Background(), "test", "synthetic-host.example", "synthetic-block")
	if err != nil || s != nil {
		t.Fatal("non-Main process started a diagnostic")
	}
	s, err = Start(context.Background(), "main", "synthetic-host.example", "synthetic-block")
	if err != nil || s != nil {
		t.Fatal("unqualified placement started a diagnostic")
	}
}
