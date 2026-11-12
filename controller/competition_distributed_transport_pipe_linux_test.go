// Native synthetic descendants retain individual standard streams after the
// exact transport child exits. Every test owns, releases and observes their exit.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/competitionagent"
	"golang.org/x/sys/unix"
)

// No imported controller or installed configuration is present in the child.
type competitionAgentPipeFixtureConfig struct {
	Socket                  string    `json:"socket"`
	Mode                    string    `json:"mode"`
	Hold                    string    `json:"hold"`
	PrivateKeyBase64        string    `json:"private_key_base64"`
	CoordinatorPublicBase64 string    `json:"coordinator_public_base64"`
	Artifact                []byte    `json:"artifact"`
	DeadlineAt              time.Time `json:"deadline_at"`
}

// Control metadata is separate from the real signed protocol and inherited I/O.
type competitionAgentPipeFixtureEvent struct {
	Event string `json:"event"`
	Pid   int    `json:"pid,omitempty"`
}

// Each connection has exactly one test-owned command/response sequence.
type competitionAgentPipeFixtureControl struct {
	connection net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder
	pid        int
	pidFd      int
}

// A fixture's watchdog is only a safety bound; explicit protocol and native
// process-exit barriers establish the ordering before cancellation.
type competitionAgentPipeFixture struct {
	transport        competitionCommandAgentTransport
	request          competitionagent.Request
	artifact         []byte
	listener         net.Listener
	agent            *competitionAgentPipeFixtureControl
	holder           *competitionAgentPipeFixtureControl
	watchdog         context.Context
	transportContext context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	commandWaited    chan struct{}
	response         competitionagent.Response
	err              error
	output           string
	started          bool
	closeFixture     func()
}

// The already compiled dependency graph makes this a small test-owned build.
// It inherits the QA actor's exact Go workspace, offline cache and resource caps.
func buildCompetitionAgentPipeFixture(t *testing.T) string {
	t.Helper()
	command, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "agent-pipe-fixture")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	process := exec.CommandContext(ctx, command, "build", "-p=1", "-o", binary, "./testdata/competition-agent-pipe")
	process.WaitDelay = time.Second
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic pipe fixture: %v %s", err, output)
	}
	return binary
}

// All pins and keys are fresh synthetic bytes; the transport's normal pin and
// signing checks are exercised rather than replaced by a fake Exchange.
func newCompetitionAgentPipeFixture(t *testing.T, binary, mode, hold string) *competitionAgentPipeFixture {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	// Linux abstract sockets avoid pathname limits under private QA temp roots.
	socket := "@competition-pipe-fixture-" + hex.EncodeToString(random)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	coordinatorKey, coordinatorPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	artifact := bytes.Repeat([]byte("synthetic artifact\n"), 4096)
	config, err := json.Marshal(competitionAgentPipeFixtureConfig{Socket: socket, Mode: mode, Hold: hold,
		PrivateKeyBase64: base64.StdEncoding.EncodeToString(privateKey), CoordinatorPublicBase64: base64.StdEncoding.EncodeToString(coordinatorKey), Artifact: artifact,
		DeadlineAt: time.Now().Add(30 * time.Second)})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "fixture.json")
	if err := writeExclusiveFile(configPath, config, 0400); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	binaryDigest, _, err := hashRegularFile(binary)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	configDigest := sha256.Sum256(config)
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), 20*time.Second)
	ctx, cancel := context.WithCancel(watchdog)
	fixture := &competitionAgentPipeFixture{
		transport: competitionCommandAgentTransport{
			profile:    &competitionDistributedCoordinatorProfile{PrivateKey: coordinatorPrivateKey, Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings()}},
			host:       CompetitionExecutorHostPolicy{HostId: "executor-synthetic", Transport: "local", AgentPublicKeyBase64: base64.StdEncoding.EncodeToString(publicKey)},
			connection: competitionExecutorConnection{AgentCommand: binary, AgentCommandSha256: binaryDigest, AgentConfigPath: configPath, AgentConfigSha256: hex.EncodeToString(configDigest[:])},
		},
		request: competitionagent.Request{Operation: "status", RunId: strings.Repeat("1", 64), FencingToken: 7}, artifact: artifact,
		listener: listener, watchdog: watchdog, cancel: cancel, done: make(chan struct{}), commandWaited: make(chan struct{}), output: filepath.Join(directory, "output"),
	}
	if err := os.Mkdir(fixture.output, 0700); err != nil {
		listener.Close()
		cancel()
		stopWatchdog()
		t.Fatal(err)
	}
	// Cleanup is registered before any goroutine or child can be created.
	var closeOnce sync.Once
	fixture.closeFixture = func() {
		closeOnce.Do(func() {
			cancel()
			cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(watchdog), 10*time.Second)
			defer stop()
			cleanupDeadline, _ := cleanupCtx.Deadline()
			for _, control := range []*competitionAgentPipeFixtureControl{fixture.agent, fixture.holder} {
				if control != nil {
					control.connection.SetDeadline(cleanupDeadline)
				}
			}
			if fixture.holder != nil {
				fixture.holder.send("release")
			}
			if fixture.agent != nil {
				fixture.agent.send("exit")
			}
			listener.Close()
			if fixture.started {
				select {
				case <-fixture.done:
				case <-cleanupCtx.Done():
					t.Error("transport I/O and exact child did not join")
				}
			}
			for _, control := range []*competitionAgentPipeFixtureControl{fixture.agent, fixture.holder} {
				if control != nil {
					if err := waitCompetitionAgentPipeProcess(cleanupCtx, control.pidFd); err != nil {
						t.Error("owned fixture process did not exit", err)
					}
					control.connection.Close()
					unix.Close(control.pidFd)
				}
			}
			stopWatchdog()
		})
	}
	t.Cleanup(fixture.closeFixture)
	// Keep the request context separate from the watchdog used to await joining.
	fixture.transport.profile.Config.Settings.RequestTimeout = time.Minute
	fixture.transportContext = ctx
	return fixture
}

// POLLIN proves exit on the qualified kernel too. Linux 6.8 does not emit HUP
// for a reaped pidfd, so reaping is proved separately through actual Wait/ECHILD.
func waitCompetitionAgentPipeProcess(ctx context.Context, pidFd int) error {
	deadline, ok := ctx.Deadline()
	if !ok || pidFd < 0 {
		return errors.New("fixture process has no bounded native exit witness")
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			return context.DeadlineExceeded
		}
		pollFds := []unix.PollFd{{Fd: int32(pidFd), Events: unix.POLLIN}}
		count, err := unix.Poll(pollFds, int((remaining+time.Millisecond-1)/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || count != 1 || pollFds[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 || pollFds[0].Revents&unix.POLLIN == 0 {
			return errors.Join(err, errors.New("fixture process did not acknowledge its exact lifecycle event"))
		}
		return nil
	}
}

// Commands contain only the closed synthetic fixture vocabulary.
func (self *competitionAgentPipeFixtureControl) send(command string) error {
	return self.encoder.Encode(struct {
		Command string `json:"command"`
	}{Command: command})
}

// The listener and each accepted connection share the finite test watchdog.
func (self *competitionAgentPipeFixture) accept(t *testing.T, expected string) *competitionAgentPipeFixtureControl {
	t.Helper()
	deadline, _ := self.watchdog.Deadline()
	if err := self.listener.(*net.UnixListener).SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	connection, err := self.listener.Accept()
	if err != nil {
		t.Fatal("fixture did not acknowledge its control generation", err)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	control := &competitionAgentPipeFixtureControl{connection: connection, encoder: json.NewEncoder(connection), decoder: json.NewDecoder(connection), pidFd: -1}
	event := competitionAgentPipeFixtureEvent{}
	if err := control.decoder.Decode(&event); err != nil || event.Event != expected || event.Pid <= 0 {
		connection.Close()
		t.Fatal("unexpected fixture process acknowledgement", event, err)
	}
	control.pid = event.Pid
	control.pidFd, err = unix.PidfdOpen(event.Pid, 0)
	if err != nil {
		connection.Close()
		t.Fatal("native fixture requires positive pidfd identity", err)
	}
	return control
}

// The command's launch is the real Exchange entry point, not a protocol mock.
func (self *competitionAgentPipeFixture) start(t *testing.T, input string) {
	t.Helper()
	self.startOperation(t, func() (competitionagent.Response, error) {
		return self.transport.Exchange(self.transportContext, self.request, input, self.output)
	})
}

// Production framing is retained while a caller can hold an exact native read
// boundary, or positively witness its kernel's empty/full-pipe acknowledgement.
func (self *competitionAgentPipeFixture) startCommand(t *testing.T, writeRequest func(io.Writer) error, readResponse func(io.Reader) error) {
	t.Helper()
	self.request.Schema, self.request.HostId = competitionagent.Schema, self.transport.host.HostId
	if err := competitionagent.SignRequest(&self.request, self.transport.profile.PrivateKey); err != nil {
		t.Fatal(err)
	}
	command, arguments, err := self.transport.commandArguments()
	if err != nil {
		t.Fatal(err)
	}
	self.startOperation(t, func() (competitionagent.Response, error) {
		return competitionagent.Response{}, exchangeCompetitionAgentCommandWithWaitObserver(self.transportContext, self.cancel, command, arguments,
			int(self.transport.profile.Config.Settings.MaximumDiagnosticBytes), writeRequest, readResponse, func() { close(self.commandWaited) })
	})
}

// A closed result channel acknowledges all work before test cleanup can join.
func (self *competitionAgentPipeFixture) startOperation(t *testing.T, operation func() (competitionagent.Response, error)) {
	t.Helper()
	self.started = true
	go func() {
		defer close(self.done)
		self.response, self.err = operation()
	}()
	self.agent = self.accept(t, "agent-ready")
}

// Both actual stream inheritance and the emitted response precede child exit.
func (self *competitionAgentPipeFixture) emitAndExit(t *testing.T, hold bool) {
	t.Helper()
	if err := self.agent.send("proceed"); err != nil {
		t.Fatal(err)
	}
	if hold {
		self.holder = self.accept(t, "holder-ready")
	}
	event := competitionAgentPipeFixtureEvent{}
	if err := self.agent.decoder.Decode(&event); err != nil || event.Event != "response-emitted" {
		t.Fatal("fixture did not emit its exact response prefix", event, err)
	}
	if err := self.agent.send("exit"); err != nil {
		t.Fatal(err)
	}
	if err := waitCompetitionAgentPipeProcess(self.watchdog, self.agent.pidFd); err != nil {
		t.Fatal(err)
	}
}

// No cancellation assertion is satisfied until the whole Exchange has joined.
func (self *competitionAgentPipeFixture) await(t *testing.T) {
	t.Helper()
	select {
	case <-self.done:
	case <-self.watchdog.Done():
		t.Fatal("transport retained inherited pipe ownership after cancellation")
	}
	if self.agent != nil {
		if err := waitCompetitionAgentPipeProcess(self.watchdog, self.agent.pidFd); err != nil {
			t.Fatal(err)
		}
		if err := requireCompetitionAgentPipeReaped(self.agent.pidFd); err != nil {
			t.Fatal(err)
		}
	}
}

// A native WNOWAIT query never steals another waiter's exit status. Only ECHILD
// accepts reaping, after the same generation's independent exit/Wait witnesses.
func requireCompetitionAgentPipeReaped(pidFd int) error {
	if pidFd < 0 {
		return errors.New("invalid generation-pinned child witness")
	}
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PIDFD, pidFd, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if errors.Is(err, unix.ECHILD) {
		return nil
	}
	return errors.Join(err, errors.New("generation-pinned exact child was not physically reaped"))
}

// Closing request-local descriptors must not kill an inherited stream holder.
func (self *competitionAgentPipeFixture) holderAlive(t *testing.T) {
	t.Helper()
	if err := self.holder.send("ping"); err != nil {
		t.Fatal(err)
	}
	event := competitionAgentPipeFixtureEvent{}
	if err := self.holder.decoder.Decode(&event); err != nil || event.Event != "holder-alive" {
		t.Fatal("transport cancellation signaled an unrelated descendant", event, err)
	}
}

// Only an actual EAGAIN from the owned kernel pipe closes the witness channel.
// This observes the production descriptor rather than substituting fake I/O.
type competitionAgentPipeProbe struct {
	connection syscall.RawConn
	blocked    chan struct{}
	blockOnce  sync.Once
}

// Both directions share the same real native runtime-poller ownership.
func newCompetitionAgentPipeProbe(stream any, blocked chan struct{}) (*competitionAgentPipeProbe, error) {
	file, ok := stream.(*os.File)
	if !ok {
		return nil, errors.New("transport did not expose its owned native pipe")
	}
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	return &competitionAgentPipeProbe{connection: connection, blocked: blocked}, nil
}

// The runtime poller remains the native cancellation boundary after EAGAIN.
func (self *competitionAgentPipeProbe) Read(content []byte) (int, error) {
	count := 0
	var readErr error
	err := self.connection.Read(func(fd uintptr) bool {
		for {
			count, readErr = syscall.Read(int(fd), content)
			if readErr == syscall.EINTR {
				continue
			}
			if readErr == syscall.EAGAIN {
				self.blockOnce.Do(func() { close(self.blocked) })
				return false
			}
			return true
		}
	})
	if err != nil {
		return 0, err
	}
	if count < 0 {
		count = 0
	}
	if count == 0 && readErr == nil && len(content) != 0 {
		readErr = io.EOF
	}
	return count, readErr
}

// A full write, not merely one partial syscall, owns the entire caller frame.
func (self *competitionAgentPipeProbe) Write(content []byte) (int, error) {
	written := 0
	for written < len(content) {
		count := 0
		var writeErr error
		err := self.connection.Write(func(fd uintptr) bool {
			for {
				count, writeErr = syscall.Write(int(fd), content[written:])
				if writeErr == syscall.EINTR {
					continue
				}
				if writeErr == syscall.EAGAIN {
					self.blockOnce.Do(func() { close(self.blocked) })
					return false
				}
				return true
			}
		})
		if count > 0 {
			written += count
		}
		if err != nil || writeErr != nil {
			return written, errors.Join(err, writeErr)
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

// A watchdog bounds a missing causal acknowledgement; it is not a sleep-based
// assertion that a goroutine probably reached its native I/O boundary.
func awaitCompetitionAgentPipeSignal(t *testing.T, watchdog context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-watchdog.Done():
		t.Fatal("native pipe operation did not acknowledge its boundary")
	}
}

// The complete child has exited, but a positive inherited stdout witness keeps
// the header, partial body or final EOF incomplete until local cancellation.
func TestCompetitionAgentPipeCancellationJoinsInheritedStdout(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	for _, mode := range []string{"before-response", "partial-document", "partial-document-body", "partial-file", "complete"} {
		fixture := newCompetitionAgentPipeFixture(t, binary, mode, "stdout")
		if mode == "partial-file" {
			fixture.request.Operation = "artifacts"
		}
		fixture.start(t, "")
		fixture.emitAndExit(t, true)
		fixture.cancel()
		fixture.await(t)
		if !errors.Is(fixture.err, context.Canceled) || errors.Is(fixture.err, competitionagent.ErrIntegrity) || fixture.response.Schema != 0 {
			t.Fatal("canceled inherited response gained authority or integrity classification", mode, fixture.err)
		}
		fixture.holderAlive(t)
		fixture.closeFixture()
	}
}

// A valid response and actual stdout EOF do not hide a descendant's stderr
// copier ownership. Cancellation must join without killing the holder.
func TestCompetitionAgentPipeCancellationJoinsInheritedStderr(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "stderr")
	fixture.start(t, "")
	fixture.emitAndExit(t, true)
	fixture.cancel()
	fixture.await(t)
	if !errors.Is(fixture.err, context.Canceled) || fixture.response.Schema != 0 {
		t.Fatal("inherited stderr outlived canceled transport", fixture.err)
	}
	fixture.holderAlive(t)
}

// The full signed response cannot make a blocked request writer unowned. A
// retained read end leaves the start-input pipe full after exact-child exit.
func TestCompetitionAgentPipeCancellationJoinsInheritedStdin(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "stdin")
	input := t.TempDir()
	content := bytes.Repeat([]byte("synthetic input\n"), 128*1024)
	digest := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(input, "input.bin"), content, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.request.Operation = "start"
	fixture.request.Start = &competitionagent.StartManifest{Inputs: []competitionagent.File{{Path: "input.bin", Bytes: int64(len(content)), Sha256: hex.EncodeToString(digest[:])}}}
	fixture.start(t, input)
	fixture.emitAndExit(t, true)
	fixture.cancel()
	fixture.await(t)
	if !errors.Is(fixture.err, context.Canceled) || fixture.response.Schema != 0 {
		t.Fatal("inherited stdin outlived canceled writer", fixture.err)
	}
	fixture.holderAlive(t)
}

// Forces the pre-Wait trailer read to report real EAGAIN while the exact child
// has exited. The old manual-pipe implementation cannot join on cancellation.
func TestCompetitionAgentPipeCancellationInterruptsAcknowledgedRead(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "stdout")
	blocked, readerJoined, writerJoined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fixture.startCommand(t, func(writer io.Writer) error {
		defer close(writerJoined)
		return competitionagent.WriteDocument(writer, fixture.request, competitionagent.DefaultLimits().DocumentBytes)
	}, func(reader io.Reader) error {
		defer close(readerJoined)
		response := competitionagent.Response{}
		if err := competitionagent.ReadDocument(reader, &response, competitionagent.DefaultLimits().DocumentBytes); err != nil {
			return err
		}
		probe, err := newCompetitionAgentPipeProbe(reader, blocked)
		if err != nil {
			return err
		}
		return readCompetitionAgentTrailer(probe)
	})
	fixture.emitAndExit(t, true)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, blocked)
	fixture.cancel()
	fixture.await(t)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, readerJoined)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, writerJoined)
	if !errors.Is(fixture.err, context.Canceled) || errors.Is(fixture.err, competitionagent.ErrIntegrity) {
		t.Fatal("acknowledged read did not retain cancellation classification", fixture.err)
	}
	fixture.holderAlive(t)
}

// A retained stdin reader causes real EAGAIN after the signed document, while
// the independent stdout response and exact child have already completed.
func TestCompetitionAgentPipeCancellationInterruptsAcknowledgedWrite(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "stdin")
	blocked, writerJoined := make(chan struct{}), make(chan struct{})
	fixture.startCommand(t, func(writer io.Writer) error {
		defer close(writerJoined)
		if err := competitionagent.WriteDocument(writer, fixture.request, competitionagent.DefaultLimits().DocumentBytes); err != nil {
			return err
		}
		probe, err := newCompetitionAgentPipeProbe(writer, blocked)
		if err != nil {
			return err
		}
		_, err = probe.Write(bytes.Repeat([]byte("synthetic input"), 128*1024))
		return err
	}, func(reader io.Reader) error {
		_, err := io.Copy(io.Discard, reader)
		return err
	})
	fixture.emitAndExit(t, true)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, blocked)
	fixture.cancel()
	fixture.await(t)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, writerJoined)
	if !errors.Is(fixture.err, context.Canceled) {
		t.Fatal("acknowledged write outlived cancellation", fixture.err)
	}
	fixture.holderAlive(t)
}

// Wait is intentionally allowed to reap before the first response read. Its
// ownership must not close the unread authenticated bytes or their EOF trailer.
func TestCompetitionAgentPipeWaitPreservesBufferedFrames(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "")
	allowRead := make(chan struct{})
	var response competitionagent.Response
	fixture.startCommand(t, func(writer io.Writer) error {
		return competitionagent.WriteDocument(writer, fixture.request, competitionagent.DefaultLimits().DocumentBytes)
	}, func(reader io.Reader) error {
		select {
		case <-allowRead:
		case <-fixture.transportContext.Done():
			return fixture.transportContext.Err()
		}
		if err := competitionagent.ReadDocument(reader, &response, competitionagent.DefaultLimits().DocumentBytes); err != nil {
			return err
		}
		return readCompetitionAgentTrailer(reader)
	})
	// The child is positively waiting for its control release here. Neither a
	// live generation nor an invalid descriptor can stand in for a reaped child.
	if err := requireCompetitionAgentPipeReaped(fixture.agent.pidFd); err == nil {
		t.Fatal("live fixture generation was accepted as physically reaped")
	}
	if err := requireCompetitionAgentPipeReaped(-1); err == nil {
		t.Fatal("invalid native witness was accepted as physically reaped")
	}
	unrelatedFile, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedErr := requireCompetitionAgentPipeReaped(int(unrelatedFile.Fd()))
	if err := unrelatedFile.Close(); err != nil {
		t.Fatal(err)
	}
	if unrelatedErr == nil {
		t.Fatal("non-pidfd descriptor was accepted as physically reaped")
	}
	fixture.emitAndExit(t, false)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, fixture.commandWaited)
	if err := requireCompetitionAgentPipeReaped(fixture.agent.pidFd); err != nil {
		t.Fatal("actual waiter did not reap before the deliberately unread frame", err)
	}
	close(allowRead)
	fixture.await(t)
	key, err := base64.StdEncoding.DecodeString(fixture.transport.host.AgentPublicKeyBase64)
	if fixture.err != nil || err != nil || competitionagent.VerifyResponse(response, ed25519.PublicKey(key)) != nil || fixture.watchdog.Err() != nil {
		t.Fatal("healthy buffered response lost its full request budget", fixture.err, err)
	}
}

// Even physical full-frame/EOF completion cannot authorize a result if the
// owner cancels before the read callback and joined request return to the caller.
func TestCompetitionAgentPipeLateCompletionCannotBecomeSuccess(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "")
	readComplete, returnRead := make(chan struct{}), make(chan struct{})
	fixture.startCommand(t, func(writer io.Writer) error {
		return competitionagent.WriteDocument(writer, fixture.request, competitionagent.DefaultLimits().DocumentBytes)
	}, func(reader io.Reader) error {
		_, err := io.Copy(io.Discard, reader)
		if err != nil {
			return err
		}
		close(readComplete)
		select {
		case <-returnRead:
		case <-fixture.watchdog.Done():
			return fixture.watchdog.Err()
		}
		return nil
	})
	fixture.emitAndExit(t, false)
	awaitCompetitionAgentPipeSignal(t, fixture.watchdog, readComplete)
	fixture.cancel()
	close(returnRead)
	fixture.await(t)
	if !errors.Is(fixture.err, context.Canceled) {
		t.Fatal("late complete frame escaped its canceled owner", fixture.err)
	}
}

// Independent requests retain independent cancellation and full healthy budget.
func TestCompetitionAgentPipeCancellationPreservesHealthySibling(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	canceled := newCompetitionAgentPipeFixture(t, binary, "complete", "stdout")
	healthy := newCompetitionAgentPipeFixture(t, binary, "complete", "")
	canceled.start(t, "")
	canceled.emitAndExit(t, true)
	healthy.start(t, "")
	canceled.cancel()
	canceled.await(t)
	canceled.holderAlive(t)
	healthy.emitAndExit(t, false)
	healthy.await(t)
	if !errors.Is(canceled.err, context.Canceled) || healthy.err != nil || healthy.response.State != "succeeded" || healthy.transportContext.Err() != nil {
		t.Fatal("one request changed its sibling or accepted canceled authority", canceled.err, healthy.err)
	}
}

// Neither cancellation races nor local process exit may soften authentication,
// exact ownership, artifact hashes, trailing bytes or a nonzero process result.
func TestCompetitionAgentPipePreservesIntegrityAndExitChecks(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	for _, mode := range []string{"bad-signature", "wrong-fence", "trailing-byte", "corrupt-file", "exit-failure"} {
		fixture := newCompetitionAgentPipeFixture(t, binary, mode, "")
		if mode == "corrupt-file" {
			fixture.request.Operation = "artifacts"
		}
		fixture.start(t, "")
		if err := fixture.agent.send("proceed"); err != nil {
			t.Fatal(err)
		}
		// Queue the exit release before a rejected response can cancel the child.
		fixture.agent.send("exit")
		fixture.await(t)
		if fixture.err == nil || fixture.response.Schema != 0 || errors.Is(fixture.err, competitionagent.ErrIntegrity) != (mode != "exit-failure") {
			t.Fatal("closed response checks were weakened", mode, fixture.err)
		}
		fixture.closeFixture()
	}
}

// Diagnostics still obey the existing finite byte bound and remain private.
func TestCompetitionAgentPipeDiagnosticOverflowIsBoundedAndPrivate(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "diagnostic-overflow", "")
	fixture.transport.profile.Config.Settings.MaximumDiagnosticBytes = 16
	fixture.start(t, "")
	if err := fixture.agent.send("proceed"); err != nil {
		t.Fatal(err)
	}
	fixture.agent.send("exit")
	fixture.await(t)
	if fixture.err == nil || fixture.response.Schema != 0 || strings.Contains(fixture.err.Error(), "private synthetic diagnostic") {
		t.Fatal("overflow was accepted or disclosed private bytes", fixture.err)
	}
}

// A context already canceled before Start must create no executable generation.
func TestCompetitionAgentPipeCanceledBeforeStart(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "")
	fixture.cancel()
	response, err := fixture.transport.Exchange(fixture.transportContext, fixture.request, "", fixture.output)
	if !errors.Is(err, context.Canceled) || response.Schema != 0 {
		t.Fatal("canceled request reached process authority", err)
	}
}

// A healthy complete authenticated artifact still crosses every unchanged
// signature, file-hash, exact-owner and EOF gate under the full caller budget.
func TestCompetitionAgentPipeHealthyArtifactsKeepOriginalFrameIntegrity(t *testing.T) {
	binary := buildCompetitionAgentPipeFixture(t)
	fixture := newCompetitionAgentPipeFixture(t, binary, "complete", "")
	fixture.request.Operation = "artifacts"
	fixture.start(t, "")
	fixture.emitAndExit(t, false)
	fixture.await(t)
	content, err := os.ReadFile(filepath.Join(fixture.output, "synthetic-artifact.bin"))
	if fixture.err != nil || err != nil || fixture.response.State != "succeeded" || len(fixture.response.Files) != 1 || !bytes.Equal(content, fixture.artifact) || fixture.transportContext.Err() != nil {
		t.Fatal("healthy complete artifact framing changed", fixture.err, err)
	}
}

// Failed Start must not launch the writer, reader or any owned waiter. Repeated
// failures exercise all six pre-launch descriptor cleanup paths without a child.
func TestCompetitionAgentPipeStartFailureDoesNotRunIo(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for index := 0; index < 16; index++ {
		called := false
		err := exchangeCompetitionAgentCommand(ctx, cancel, filepath.Join(t.TempDir(), "missing-command"), nil, 16, func(io.Writer) error {
			called = true
			return nil
		}, func(io.Reader) error {
			called = true
			return nil
		})
		if err == nil || called || ctx.Err() != nil {
			t.Fatal("failed process start leaked into request I/O", err, called)
		}
	}
}
