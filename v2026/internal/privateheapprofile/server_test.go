package privateheapprofile

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestKernelPeerAuthentication(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Log("real kernel root peer is expected to authenticate")
	}
	var calls atomic.Int32
	s := testServer(t, rootPeer, func(context.Context, func() Companion) (Capture, error) {
		calls.Add(1)
		return Capture{}, errProfileBound
	})
	out, _, err := request(t, s, testIdentity())
	if os.Geteuid() == 0 {
		if err != nil || out.Status != "capture_unavailable" || calls.Load() != 1 {
			t.Fatal("kernel root peer refused")
		}
	} else if err == nil || calls.Load() != 0 || s.used.Load() {
		t.Fatal("kernel non-root peer accepted")
	}
}

func TestMalformedRequestDoesNotConsume(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) {
		calls.Add(1)
		return Capture{}, errProfileBound
	})
	valid, err := json.Marshal(Request{Schema: 1, Expected: testIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{"not-json\n", strings.Repeat("x", maxRequestBytes+1) + "\n", string(valid) + " {}\n", strings.TrimSuffix(string(valid), "}") + ",\"unexpected\":true}\n"}
	for _, input := range cases {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: s.path})
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = conn.Write([]byte(input))
		var one [1]byte
		n, _ := conn.Read(one[:])
		conn.Close()
		if n != 0 || calls.Load() != 0 || s.used.Load() {
			t.Fatal("malformed request captured or consumed")
		}
	}
	out, _, err := request(t, s, testIdentity())
	if err != nil || out.Status != "capture_unavailable" || calls.Load() != 1 {
		t.Fatal("valid request unavailable after malformed request")
	}
}

func TestPrivateDirectoryAndExistingPathRefusal(t *testing.T) {
	for _, kind := range []string{"public_directory", "directory_symlink", "existing_socket_path"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "private")
			switch kind {
			case "public_directory":
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "directory_symlink":
				if err := os.Symlink(base, dir); err != nil {
					t.Fatal(err)
				}
			case "existing_socket_path":
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, SocketName), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, err := start(context.Background(), Config{}, dir, testIdentity(), rootPeer, collect)
			if err == nil || s != nil {
				if s != nil {
					s.Close()
				}
				t.Fatal("unsafe or occupied path accepted")
			}
			if kind == "existing_socket_path" {
				data, err := os.ReadFile(filepath.Join(dir, SocketName))
				if err != nil || string(data) != "existing" {
					t.Fatal("existing path removed")
				}
			}
		})
	}
}

func TestClosePreservesReplacementPath(t *testing.T) {
	s := testServer(t, rootPeer, collect)
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Close()
	data, err := os.ReadFile(s.path)
	if err != nil || string(data) != "replacement" {
		t.Fatal("non-owned replacement removed")
	}
}
