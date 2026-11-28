//go:build linux

// Native subprocesses acknowledge signal receipt and physical child Wait on a
// separate Unix control socket; no installed agent/configuration is involved.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A separate finite watchdog owns fixture joining after request cancellation.
type competitionAgentCancelFixture struct {
	listener   *net.UnixListener
	address    string
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	err        error
	owner      net.Conn
	leaf       net.Conn
	ownerRead  *json.Decoder
	leafRead   *json.Decoder
	forceArmed chan struct{}
	armOnce    sync.Once
}

// Only closed event names and native exit facts cross the private socket.
type competitionAgentCancelEvent struct {
	Event         string `json:"event"`
	ChildExited   bool   `json:"child_exited,omitempty"`
	ChildExitCode int    `json:"child_exit_code,omitempty"`
}

// The transport entrypoint is real; only its command is this test binary.
func newCompetitionAgentCancelFixture(t *testing.T, mode string, joinTimeout time.Duration) *competitionAgentCancelFixture {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	address := "@competition-control-cancel-" + hex.EncodeToString(id[:])
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	watchdog, finish := context.WithTimeout(t.Context(), 10*time.Second)
	deadline, _ := watchdog.Deadline()
	if err := listener.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(watchdog)
	fixture := &competitionAgentCancelFixture{listener: listener, address: address, ctx: ctx, cancel: cancel, done: make(chan struct{}), forceArmed: make(chan struct{})}
	go func() {
		defer close(fixture.done)
		hooks := competitionAgentCommandHooks{}
		if mode == "force" {
			// Cmd's force timer starts only after Cancel returns. The test
			// first positively observes TERM receipt, then arms that timer.
			hooks.canceled = func(error) { <-fixture.forceArmed }
		}
		fixture.err = exchangeCompetitionAgentCommandWithHooks(ctx, cancel, os.Args[0],
			[]string{"-test.run=^TestCompetitionAgentCancelProcess$", "--", mode, address}, 4096, joinTimeout,
			func(writer io.Writer) error { _, err := writer.Write([]byte("synthetic request")); return err },
			func(reader io.Reader) error { _, err := io.Copy(io.Discard, reader); return err }, hooks)
	}()
	var closeOnce sync.Once
	t.Cleanup(func() {
		closeOnce.Do(func() {
			cancel()
			fixture.armOnce.Do(func() { close(fixture.forceArmed) })
			for _, connection := range []net.Conn{fixture.owner, fixture.leaf} {
				if connection != nil {
					_ = connection.Close()
				}
			}
			_ = listener.Close()
			<-fixture.done
			finish()
		})
	})
	fixture.owner, fixture.ownerRead = fixture.accept(t, "owner-ready", deadline)
	if mode != "force" {
		fixture.leaf, fixture.leafRead = fixture.accept(t, "leaf-ready", deadline)
	}
	return fixture
}

// Readiness is a positive handshake, never process-list or queue polling.
func (self *competitionAgentCancelFixture) accept(t *testing.T, expected string, deadline time.Time) (net.Conn, *json.Decoder) {
	t.Helper()
	connection, err := self.listener.Accept()
	if err != nil {
		t.Fatal("control cancellation fixture did not acknowledge startup")
	}
	if err := connection.SetDeadline(deadline); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	decoder := json.NewDecoder(connection)
	var event competitionAgentCancelEvent
	if err := decoder.Decode(&event); err != nil || event.Event != expected {
		connection.Close()
		t.Fatal("control cancellation fixture changed readiness ordering")
	}
	return connection, decoder
}

// A parent may return only after its actual service child was signaled/reaped.
func (self *competitionAgentCancelFixture) joined(t *testing.T) {
	t.Helper()
	var leaf, owner competitionAgentCancelEvent
	if err := self.leafRead.Decode(&leaf); err != nil || leaf.Event != "leaf-stopped" {
		t.Fatal("service child did not acknowledge its scoped stop")
	}
	if err := self.ownerRead.Decode(&owner); err != nil || owner.Event != "owner-cleaned" || !owner.ChildExited || owner.ChildExitCode != 0 {
		t.Fatal("cleanup owner returned without physically reaping its actual child")
	}
	<-self.done
}

// The original transport cancel must not SIGKILL the cleanup-owning wrapper.
func TestCompetitionAgentLocalCancellationJoinsActualCleanupOwner(t *testing.T) {
	fixture := newCompetitionAgentCancelFixture(t, "cleanup", 2*time.Second)
	fixture.cancel()
	fixture.joined(t)
	var failure *CompetitionAgentTransportError
	if !errors.Is(fixture.err, context.Canceled) || !errors.As(fixture.err, &failure) || !failure.ChildExitKnown || failure.ChildExitCode != 0 || failure.ChildExitSignal != 0 {
		t.Fatal("local cancellation did not preserve a positively joined cleanup owner")
	}
}

// A non-cooperative wrapper receives TERM first and is then physically reaped
// under the finite grace; no timeout alone is counted as a positive process join.
func TestCompetitionAgentLocalCancellationForcesOnlyAfterPoliteSignal(t *testing.T) {
	fixture := newCompetitionAgentCancelFixture(t, "force", 50*time.Millisecond)
	fixture.cancel()
	var event competitionAgentCancelEvent
	if err := fixture.ownerRead.Decode(&event); err != nil || event.Event != "term-received" {
		t.Fatal("forced cleanup did not first notify the exact owner")
	}
	fixture.armOnce.Do(func() { close(fixture.forceArmed) })
	<-fixture.done
	var failure *CompetitionAgentTransportError
	if !errors.Is(fixture.err, context.Canceled) || !errors.As(fixture.err, &failure) || !failure.ChildExitKnown || failure.ChildExitSignal != int(syscall.SIGKILL) {
		t.Fatal("uncooperative owner did not reach its original bounded forced Wait")
	}
}

// Two cleanup owners share the test process group but not lifetime ownership.
func TestCompetitionAgentLocalCancellationPreservesHealthyCleanupSibling(t *testing.T) {
	canceled := newCompetitionAgentCancelFixture(t, "cleanup", 2*time.Second)
	healthy := newCompetitionAgentCancelFixture(t, "cleanup", 2*time.Second)
	canceled.cancel()
	canceled.joined(t)
	if err := json.NewEncoder(healthy.owner).Encode("finish"); err != nil {
		t.Fatal("one canceled owner killed its healthy sibling's control socket")
	}
	healthy.joined(t)
	if healthy.err != nil || healthy.ctx.Err() != nil {
		t.Fatal("healthy sibling did not finish independently")
	}
}

// An invalid bound refuses before any pipe, process or callback can be issued.
func TestCompetitionAgentControlCancellationRefusesUnboundedJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, timeout := range []time.Duration{0, -time.Second} {
		called := false
		err := exchangeCompetitionAgentCommandWithJoin(ctx, cancel, "synthetic-not-issued", nil, 1024, timeout,
			func(io.Writer) error { called = true; return nil }, func(io.Reader) error { called = true; return nil }, nil)
		if err == nil || called || ctx.Err() != nil {
			t.Fatal("invalid control join bound reached transport authority")
		}
	}
}

// The next production failure distinguishes a partial frame from EOF/trailer
// waiting without ever rendering a response, diagnostic or private cause.
func TestCompetitionAgentTransportResponsePhaseRemainsClosed(t *testing.T) {
	for _, phase := range []string{"frame", "authentication", "files", "trailer", "complete", "synthetic-private-phase"} {
		failure := &CompetitionAgentTransportError{responsePhase: phase}
		expected := phase
		if phase == "synthetic-private-phase" {
			expected = "unknown"
		}
		if failure.ResponsePhase() != expected || strings.Contains(failure.Error(), "synthetic-private-phase") {
			t.Fatal("response-phase diagnostic exposed an open-ended value")
		}
	}
}

// The helper participates in the ordinary top-level census as a no-op PASS.
// Every active helper has one explicit socket deadline and exact argv grammar.
func TestCompetitionAgentCancelProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	mode, address := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if mode != "cleanup" && mode != "force" && mode != "leaf" {
		os.Exit(92)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	connection, err := net.Dial("unix", address)
	if err != nil {
		os.Exit(93)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		os.Exit(94)
	}
	encoder := json.NewEncoder(connection)
	if mode == "leaf" {
		_ = encoder.Encode(competitionAgentCancelEvent{Event: "leaf-ready"})
		<-ctx.Done()
		_ = encoder.Encode(competitionAgentCancelEvent{Event: "leaf-stopped"})
		os.Exit(0)
	}
	_ = encoder.Encode(competitionAgentCancelEvent{Event: "owner-ready"})
	if mode == "force" {
		<-ctx.Done()
		_ = encoder.Encode(competitionAgentCancelEvent{Event: "term-received"})
		// The production WaitDelay owns this uncooperative exact process.
		for {
			if _, err := connection.Read(make([]byte, 1)); err != nil {
				os.Exit(95)
			}
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCompetitionAgentCancelProcess$", "--", "leaf", address)
	child.Env = []string{"PATH=/usr/bin:/bin", "GOMAXPROCS=1", "GOMEMLIMIT=32MiB"}
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(96)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		var command string
		if json.NewDecoder(connection).Decode(&command) == nil && command == "finish" {
			cancel()
		}
	}()
	<-ctx.Done()
	_ = connection.SetReadDeadline(time.Now())
	<-readDone
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		_ = child.Process.Kill()
	}
	err = child.Wait()
	if err != nil || child.ProcessState == nil || !child.ProcessState.Exited() {
		os.Exit(97)
	}
	_ = encoder.Encode(competitionAgentCancelEvent{Event: "owner-cleaned", ChildExited: true, ChildExitCode: child.ProcessState.ExitCode()})
	os.Exit(0)
}
