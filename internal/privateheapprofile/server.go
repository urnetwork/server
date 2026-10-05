package privateheapprofile

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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const SocketDirectory = "/run/urnetwork-private-heap"
const SocketName = "profile.sock"
const maxRequestBytes = 2048

type Config struct {
	TargetHost  string
	TargetBlock string
	Host        string
	Block       string
	Companion   func() Companion
}

type Request struct {
	Schema   int      `json:"schema"`
	Expected Identity `json:"expected"`
}

type Response struct {
	Schema   int      `json:"schema"`
	Status   string   `json:"status"`
	Identity Identity `json:"identity"`
	Capture  *Capture `json:"capture,omitempty"`
}

type Server struct {
	listener   *net.UnixListener
	identity   Identity
	companion  func() Companion
	ctx        context.Context
	cancel     context.CancelFunc
	used       atomic.Bool
	done       chan struct{}
	mu         sync.Mutex
	active     *net.UnixConn
	path       string
	socketInfo os.FileInfo
	peer       func(*net.UnixConn) bool
	capture    func(context.Context, func() Companion) (Capture, error)
}

func ValidTarget(target string) bool {
	if target == "" {
		return true
	}
	p := strings.Split(target, "/")
	if len(p) != 2 {
		return false
	}
	validHost := p[0] == "by-us-fmt-5-edge-0" || p[0] == "by-us-fmt-5-edge-1" || p[0] == "by-us-fmt-5-edge-3" || p[0] == "by-us-fmt-5-edge-4"
	return validHost && (p[1] == "g1" || p[1] == "g2" || p[1] == "g3" || p[1] == "g4")
}

// Start is inert without an exact opt-in host/block match. The Unix socket is
// root-authenticated and never exposes a public service route.
func Start(ctx context.Context, c Config) (*Server, error) {
	if c.TargetHost == "" && c.TargetBlock == "" {
		return nil, nil
	}
	if !ValidTarget(c.TargetHost + "/" + c.TargetBlock) {
		return nil, errors.New("private_heap_target_invalid")
	}
	if c.TargetHost != c.Host || c.TargetBlock != c.Block {
		return nil, nil
	}
	identity, err := currentIdentity(c.Host, c.Block)
	if err != nil {
		return nil, errors.New("private_heap_identity_unavailable")
	}
	return start(ctx, c, SocketDirectory, identity, rootPeer, collect)
}

func start(ctx context.Context, c Config, dir string, identity Identity, peer func(*net.UnixConn) bool, capture func(context.Context, func() Companion) (Capture, error)) (*Server, error) {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !directoryOwnedBySelf(info) {
		return nil, errors.New("private_heap_directory_invalid")
	}
	path := filepath.Join(dir, SocketName)
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
	ownedCtx, cancel := context.WithCancel(ctx)
	s := &Server{listener: listener, identity: identity, companion: c.Companion, ctx: ownedCtx, cancel: cancel, done: make(chan struct{}), path: path, socketInfo: socketInfo, peer: peer, capture: capture}
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
		s.active = conn
		s.mu.Unlock()
		s.handle(conn)
		conn.Close()
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) handle(conn *net.UnixConn) {
	conn.SetDeadline(time.Now().Add(CaptureBudget + 3*time.Second))
	if !s.peer(conn) {
		return
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestBytes+1)).ReadBytes('\n')
	if err != nil || len(line) > maxRequestBytes {
		return
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return
	}
	if request.Schema != 1 || request.Expected != s.identity {
		s.respond(conn, "identity_mismatch", nil)
		return
	}
	if !s.used.CompareAndSwap(false, true) {
		s.respond(conn, "already_consumed", nil)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, CaptureBudget)
	defer cancel()
	capture, err := s.capture(ctx, s.companion)
	if err != nil || ctx.Err() != nil || len(capture.Profile) > MaxProfileBytes {
		s.respond(conn, "capture_unavailable", nil)
		return
	}
	if s.respond(conn, "complete", &capture) == nil {
		_, _ = conn.Write(capture.Profile)
	}
}

func (s *Server) respond(w io.Writer, status string, capture *Capture) error {
	return json.NewEncoder(w).Encode(Response{Schema: 1, Status: status, Identity: s.identity, Capture: capture})
}

// Close cancels and joins the listener's sole owner, including an accepted
// diagnostic. It never signals or terminates the serving process.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.listener.Close()
	s.mu.Lock()
	if s.active != nil {
		s.active.Close()
	}
	s.mu.Unlock()
	<-s.done
	if info, err := os.Lstat(s.path); err == nil && os.SameFile(info, s.socketInfo) {
		_ = os.Remove(s.path)
	}
}
