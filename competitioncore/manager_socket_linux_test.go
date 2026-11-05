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
		select {
		case <-done:
		case <-guard.Done():
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
