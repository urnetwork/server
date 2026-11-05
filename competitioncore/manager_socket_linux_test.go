// Private Unix socket pairs exercise recvmsg and credentials without ever
// connecting the real systemd endpoint or using TCP, a daemon or a core pipe.
package competitioncore

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
)

// Both endpoints are anonymous, test-owned Unix descriptors, never host paths.
func testManagerSocketPair(t *testing.T) (*nativeManagerStream, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	connections := make([]*net.UnixConn, 0, 2)
	files := make([]*os.File, 0, 2)
	for _, fd := range fds {
		file := os.NewFile(uintptr(fd), "private-manager-test-socket")
		files = append(files, file)
		t.Cleanup(func() { file.Close() })
	}
	for _, file := range files {
		connection, err := net.FileConn(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		unix, ok := connection.(*net.UnixConn)
		if !ok {
			connection.Close()
			t.Fatal("private socket pair is not Unix")
		}
		connections = append(connections, unix)
		t.Cleanup(func() { unix.Close() })
	}
	return &nativeManagerStream{connection: connections[0]}, connections[1]
}

func TestManagerNativePeerCheckReadsActualPrivateSocketCredentials(t *testing.T) {
	stream, _ := testManagerSocketPair(t)
	peer, err := stream.Peer()
	if err != nil || peer.pid != int32(os.Getpid()) || peer.uid != uint32(os.Geteuid()) {
		t.Fatal("native peer credential adapter did not read kernel facts", peer, err)
	}
	if peer.pid != 1 {
		bus := &managerBus{stream: stream, ctx: t.Context(), settings: DefaultManagerSettings()}
		if bus.authenticate() == nil || bus.used != 0 {
			t.Fatal("non-PID1 private peer received authentication or proof authority")
		}
	}
}

func TestManagerNativeRecvmsgRejectsAndClosesActuallyTransferredPrivateRights(t *testing.T) {
	stream, server := testManagerSocketPair(t)
	file, err := os.CreateTemp(t.TempDir(), "private-rights-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, _, err := server.WriteMsgUnix([]byte{1}, syscall.UnixRights(int(file.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	data, control := make([]byte, 1), make([]byte, DefaultManagerSettings().ControlBytes)
	n, oob, flags, err := stream.ReadMsg(data, control)
	if err != nil || n != 1 || oob == 0 {
		t.Fatal("private rights fixture was not delivered", n, oob, err)
	}
	messages, err := syscall.ParseSocketControlMessage(control[:oob])
	if err != nil {
		t.Fatal(err)
	}
	var received []int
	for _, message := range messages {
		rights, err := syscall.ParseUnixRights(&message)
		if err != nil {
			t.Fatal(err)
		}
		received = append(received, rights...)
	}
	if len(received) != 1 || rejectManagerControl(control[:oob], flags) == nil {
		t.Fatal("actual private SCM_RIGHTS escaped refusal")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(received[0], &stat); err != syscall.EBADF {
		syscall.Close(received[0])
		t.Fatal("received rights were not closed", err)
	}
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal("refusal closed the sender's original file instead of received rights", err)
	}
}

func TestManagerNativePendingRejectsActualTrailingPrivateRightsWithoutWaiting(t *testing.T) {
	stream, server := testManagerSocketPair(t)
	if err := stream.Pending(DefaultManagerSettings().ControlBytes); err != nil {
		t.Fatal("empty private socket had unexpected pending work", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "trailing-rights-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, _, err := server.WriteMsgUnix([]byte{1}, syscall.UnixRights(int(file.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	if err := stream.Pending(DefaultManagerSettings().ControlBytes); err == nil {
		t.Fatal("trailing private rights became completed manager custody")
	}
}

// Explicit read/close barriers prove cancellation owns the blocking transport.
type blockingManagerStream struct {
	*testManagerStream
	entered              chan struct{}
	closed               chan struct{}
	enterOnce, closeOnce sync.Once
}

func (self *blockingManagerStream) ReadMsg([]byte, []byte) (int, int, int, error) {
	self.enterOnce.Do(func() { close(self.entered) })
	<-self.closed
	return 0, 0, 0, errors.New("closed blocking private transport")
}
func (self *blockingManagerStream) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func TestManagerCancellationClosesAndJoinsBlockedPrivateRead(t *testing.T) {
	policy, settings, base, pin := newTestManager(t)
	stream := &blockingManagerStream{testManagerStream: base, entered: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	guard, stop := context.WithTimeout(t.Context(), 2*settings.Manager.Timeout)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- verifyManager(ctx, policy, settings, func(context.Context) (managerStream, error) { return stream, nil }, pin)
	}()
	t.Cleanup(func() {
		cancel()
		stream.Close()
		join, finish := context.WithTimeout(context.Background(), 2*settings.Manager.Timeout)
		defer finish()
		select {
		case <-done:
		case <-join.Done():
			t.Error("private manager proof did not join during cleanup")
		}
	})
	select {
	case <-stream.entered:
	case <-guard.Done():
		t.Fatal("private read did not enter its barrier")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled proof manufactured manager authority")
		}
		done <- err
	case <-guard.Done():
		t.Fatal("cancellation failed to close/join the blocked private read")
	}
}

// The actual native socket is closed first, releasing recvmsg, while the test
// deliberately withholds acknowledgment of the callback's Close return.
type acknowledgedManagerClose struct {
	*nativeManagerStream
	stateLock              sync.Mutex
	readStarted            chan struct{}
	readReturned           chan struct{}
	closeEntered           chan struct{}
	allowCloseReturn       chan struct{}
	readOnce, returnedOnce sync.Once
	closes                 int
	closeReturned          bool
}

// Only peer identity is a private seam; every socket read/close is native and
// confined to the anonymous pair, never the real systemd connection.
func (*acknowledgedManagerClose) Peer() (managerPeer, error) { return managerPeer{uid: 0, pid: 1}, nil }

func (self *acknowledgedManagerClose) ReadMsg(content, control []byte) (int, int, int, error) {
	self.readOnce.Do(func() { close(self.readStarted) })
	n, oob, flags, err := self.nativeManagerStream.ReadMsg(content, control)
	self.returnedOnce.Do(func() { close(self.readReturned) })
	return n, oob, flags, err
}

// A duplicate deferred Close is observable even if a faulty proof return races
// the test's final acknowledgment, so the regression needs no negative timer.
func (self *acknowledgedManagerClose) Close() error {
	self.stateLock.Lock()
	self.closes++
	first := self.closes == 1
	self.stateLock.Unlock()
	err := self.nativeManagerStream.Close()
	if first {
		close(self.closeEntered)
		<-self.allowCloseReturn
		self.stateLock.Lock()
		self.closeReturned = true
		self.stateLock.Unlock()
	}
	return err
}

// AfterFunc's callback must join, not merely start Close and release recvmsg.
// The original implementation also performs a second concurrent/deferred Close
// and deterministically fails the sole-owner assertion even under either race.
func TestManagerCancellationJoinsAcknowledgedNativeCloseBeforeProofReturn(t *testing.T) {
	policy, settings, _, pin := newTestManager(t)
	native, _ := testManagerSocketPair(t)
	stream := &acknowledgedManagerClose{nativeManagerStream: native, readStarted: make(chan struct{}), readReturned: make(chan struct{}), closeEntered: make(chan struct{}), allowCloseReturn: make(chan struct{})}
	guard, stop := context.WithTimeout(t.Context(), 2*settings.Manager.Timeout)
	defer stop()
	ctx, cancel := context.WithCancel(guard)
	defer cancel()
	type result struct {
		err          error
		acknowledged bool
		closes       int
	}
	done := make(chan result, 1)
	go func() {
		err := verifyManager(ctx, policy, settings, func(context.Context) (managerStream, error) { return stream, nil }, pin)
		stream.stateLock.Lock()
		completed := result{err: err, acknowledged: stream.closeReturned, closes: stream.closes}
		stream.stateLock.Unlock()
		done <- completed
	}()
	var acknowledge sync.Once
	t.Cleanup(func() {
		cancel()
		acknowledge.Do(func() { close(stream.allowCloseReturn) })
		native.Close()
		join, finish := context.WithTimeout(context.Background(), 2*settings.Manager.Timeout)
		defer finish()
		select {
		case <-done:
		case <-join.Done():
			t.Error("acknowledged manager callback did not join during cleanup")
		}
	})
	select {
	case <-stream.readStarted:
	case <-guard.Done():
		t.Fatal("native private read did not enter its explicit barrier")
	}
	cancel()
	for _, event := range []<-chan struct{}{stream.closeEntered, stream.readReturned} {
		select {
		case <-event:
		case <-guard.Done():
			t.Fatal("cancellation did not close and interrupt the actual private socket read")
		}
	}
	stream.stateLock.Lock()
	pending := !stream.closeReturned
	stream.stateLock.Unlock()
	if !pending {
		t.Fatal("Close-return acknowledgment barrier was bypassed")
	}
	acknowledge.Do(func() { close(stream.allowCloseReturn) })
	select {
	case completed := <-done:
		if completed.err == nil || !completed.acknowledged || completed.closes != 1 {
			t.Fatal("proof returned before cancellation Close finished or raced a duplicate close", completed)
		}
		done <- completed
	case <-guard.Done():
		t.Fatal("proof failed to join the acknowledged native cancellation Close")
	}
}
