package privateprovidercapture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026/internal/privateheapprofile"
)

const (
	SocketDirectory  = "/run/urnetwork-private-provider"
	MaxRequestBytes  = 2048
	MaxResponseBytes = 16 * 1024
	SocketBudget     = 2 * time.Second
)

type Request struct {
	Schema    int                         `json:"schema"`
	Expected  privateheapprofile.Identity `json:"expected"`
	Operation string                      `json:"operation"`
	CaptureID string                      `json:"capture_id"`
}

type Response struct {
	Schema     int                         `json:"schema"`
	Identity   privateheapprofile.Identity `json:"identity"`
	Provenance string                      `json:"provenance"`
	Snapshot   Snapshot                    `json:"snapshot"`
}

// SocketPath keeps independent API runners separate even when /run is shared.
// The caller derives it from the same native identity used by the wire check.
func SocketPath(dir string, identity privateheapprofile.Identity) string {
	return filepath.Join(dir, "capture-"+strconv.Itoa(identity.PID)+"-"+strconv.FormatUint(identity.StartTicks, 10)+".sock")
}

// Server owns one sequential, root-authenticated local listener and one bounded
// recorder. Close is concurrent-safe and joins both owners before returning.
type Server struct {
	Recorder   *Recorder
	listener   *net.UnixListener
	identity   privateheapprofile.Identity
	peer       func(*net.UnixConn) bool
	path       string
	socketInfo os.FileInfo
	mu         sync.Mutex
	active     *net.UnixConn
	closed     bool
	closeOnce  sync.Once
	done       chan struct{}
}

// Start exposes an unarmed socket only in the known Main API scope. Other
// environments/placements are inert. The socket cannot perform heap capture,
// read arbitrary files, or query a model; arming requires exact process identity.
func Start(ctx context.Context, env, host, block string) (*Server, error) {
	if env != "main" || !privateheapprofile.ValidTarget(host+"/"+block) {
		return nil, nil
	}
	identity, err := privateheapprofile.LocalIdentity(host, block)
	if err != nil {
		return nil, errors.New("private_provider_identity_unavailable")
	}
	return start(ctx, SocketDirectory, identity, privateheapprofile.RootPeer)
}

func start(ctx context.Context, dir string, identity privateheapprofile.Identity, peer func(*net.UnixConn) bool) (*Server, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("private_provider_context_unavailable")
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !privateheapprofile.OwnsPrivateDirectory(info) {
		return nil, errors.New("private_provider_directory_invalid")
	}
	if identity.PID <= 0 || identity.StartTicks == 0 {
		return nil, errors.New("private_provider_process_invalid")
	}
	path := SocketPath(dir, identity)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	socketInfo, err := os.Lstat(path)
	if err != nil {
		listener.Close()
		return nil, err
	}
	s := &Server{Recorder: NewRecorder(ctx), listener: listener, identity: identity, peer: peer, path: path, socketInfo: socketInfo, done: make(chan struct{})}
	go s.run()
	return s, nil
}

func (s *Server) run() {
	defer close(s.done)
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.active = conn
		s.mu.Unlock()
		s.handle(conn)
		conn.Close()
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
	}
}

func (s *Server) handle(conn *net.UnixConn) {
	_ = conn.SetDeadline(time.Now().Add(SocketBudget))
	if !s.peer(conn) {
		return
	}
	line, err := bufio.NewReader(io.LimitReader(conn, MaxRequestBytes+1)).ReadBytes('\n')
	if err != nil || len(line) > MaxRequestBytes {
		return
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || request.Schema != 1 || !validCaptureID(request.CaptureID) {
		return
	}
	if request.Operation != "arm" && request.Operation != "status" && request.Operation != "read" {
		return
	}
	var snapshot Snapshot
	if request.Expected != s.identity {
		snapshot.State = "identity_mismatch"
	} else if request.Operation == "arm" {
		state := s.Recorder.Arm(request.CaptureID)
		snapshot = s.Recorder.Inspect(request.CaptureID, false)
		snapshot.State = state
	} else {
		snapshot = s.Recorder.Inspect(request.CaptureID, request.Operation == "read")
	}
	wire, err := json.Marshal(Response{Schema: 1, Identity: s.identity, Provenance: "POST /network/find-providers2; ordinary group; completed model zero/cache_missing; first observed missing legacy group only; no HTTP delivery claim", Snapshot: snapshot})
	if err != nil || len(wire)+1 > MaxResponseBytes {
		return
	}
	_, _ = conn.Write(append(wire, '\n'))
}

func (s *Server) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.listener.Close()
		if s.active != nil {
			s.active.Close()
		}
		s.mu.Unlock()
		<-s.done
		s.Recorder.Close()
		if info, err := os.Lstat(s.path); err == nil && os.SameFile(info, s.socketInfo) {
			_ = os.Remove(s.path)
		}
	})
}
