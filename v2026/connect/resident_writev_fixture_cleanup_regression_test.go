// Real socket workers and exact pool references exercise early fixture exits.
package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Embedding keeps native writev available while exposing the receive owner.
type exchangeWritevCleanupReadConn struct {
	*net.TCPConn
	readEntered chan struct{}
	readOnce    sync.Once
}

// Signals before the real socket read, which closes during fixture teardown.
func (self *exchangeWritevCleanupReadConn) Read(message []byte) (int, error) {
	self.readOnce.Do(func() { close(self.readEntered) })
	return self.TCPConn.Read(message)
}

// Starts only the real ExchangeConnection socket lifecycle and its tcp peer.
// The peer drains at most the offered payload and frame header before observing
// a real terminal read. Callbacks expose writer ownership and positive reads.
func newExchangeWritevCleanupSocketFixture(
	t *testing.T,
	offeredPayloadByteCount int,
	afterDequeue func(context.Context),
	afterPeerRead func(int),
) (*exchangeWritevFixtureOwner, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	owner := newExchangeWritevFixtureOwner(t, cancel)
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	owner.listener = listener
	owner.peerDone = make(chan struct{})
	peerReadEntered := make(chan struct{})
	wireByteCount := offeredPayloadByteCount
	if 0 < wireByteCount {
		wireByteCount += exchangeIOFrameHeaderByteCount
	}
	go func() {
		defer close(owner.peerDone)
		owner.peerErr = func() error {
			peer, err := listener.AcceptTCP()
			if err != nil {
				return err
			}
			if !owner.acceptPeer(peer) {
				return ctx.Err()
			}
			defer peer.Close()
			deadline, _ := ctx.Deadline()
			if err := peer.SetReadDeadline(deadline); err != nil {
				return err
			}
			close(peerReadEntered)
			var message [1024]byte
			readByteCount := 0
			for {
				// One extra byte detects an overrun without treating the known
				// payload bound itself as proof that the socket terminated.
				n, err := peer.Read(message[:min(len(message), wireByteCount-readByteCount+1)])
				readByteCount += n
				if wireByteCount < readByteCount {
					return fmt.Errorf("cleanup peer read %d bytes beyond offered wire bound %d", readByteCount, wireByteCount)
				}
				if 0 < n && afterPeerRead != nil {
					afterPeerRead(n)
				}
				if err != nil {
					return err
				}
				if n == 0 {
					return io.ErrNoProgress
				}
			}
		}()
	}()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	socket := &exchangeWritevCleanupReadConn{
		TCPConn:     conn.(*net.TCPConn),
		readEntered: make(chan struct{}),
	}
	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = time.Hour
	connectionCtx, connectionCancel := context.WithCancel(ctx)
	connection := &ExchangeConnection{
		ctx:           connectionCtx,
		cancel:        connectionCancel,
		done:          make(chan struct{}),
		conn:          socket,
		sendBuffer:    NewDefaultExchangeBuffer(settings),
		receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
		send:          make(chan []byte, settings.ExchangeBufferSize),
		receive:       make(chan []byte, settings.ExchangeBufferSize),
		settings:      settings,
		header:        ExchangeHeader{Op: ExchangeOpTransport},
	}
	if afterDequeue != nil {
		connection.afterSendDequeueForTest = func() { afterDequeue(connectionCtx) }
	}
	owner.connection = connection
	go connection.Run()
	for _, entered := range []<-chan struct{}{peerReadEntered, socket.readEntered} {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("real socket read owner did not enter")
		}
	}
	return owner, ctx
}

// Cancellation joins the peer read, exchange read and held writer before the
// retained writer witness is reconciled. No writes or return counts are faked.
func TestExchangeWritevFixtureCleanupJoinsRealSocketOwners(t *testing.T) {
	writerEntered := make(chan struct{})
	owner, ctx := newExchangeWritevCleanupSocketFixture(t, 8*1024, func(ctx context.Context) {
		close(writerEntered)
		<-ctx.Done()
	}, nil)
	message := clientconnect.MessagePoolGet(8 * 1024)
	index := owner.keepPending(message)
	owner.witnesses = append(owner.witnesses, retainResidentPoolWitness(message))
	if result := owner.connection.sendMessage(ctx.Done(), owner.takePending(index), nil, time.Second); result != pooledMessageSendDelivered {
		t.Fatalf("writer admission = %v", result)
	}
	select {
	case <-writerEntered:
	case <-ctx.Done():
		t.Fatal("write worker did not take the pooled message")
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.connection.done:
	default:
		t.Fatal("fixture returned before exchange socket workers joined")
	}
	select {
	case <-owner.peerDone:
	default:
		t.Fatal("fixture returned before its tcp peer joined")
	}
	if !errors.Is(owner.peerErr, net.ErrClosed) && !errors.Is(owner.peerErr, io.EOF) {
		t.Fatalf("closed peer read = %v", owner.peerErr)
	}
}

// Refusal consumes the offered reference itself. Only its never-offered
// sibling belongs to fixture cleanup, followed by the final-owner witness.
func TestExchangeWritevFixtureCleanupReturnsUnofferedSibling(t *testing.T) {
	owner, ctx := newExchangeWritevCleanupSocketFixture(t, 0, nil, nil)
	owner.connection.sendAdmission.close()
	message := clientconnect.MessagePoolGet(8 * 1024)
	messageIndex := owner.keepPending(message)
	sharedIndex := owner.keepPending(clientconnect.MessagePoolShareReadOnly(message))
	owner.witnesses = append(owner.witnesses, retainResidentPoolWitness(message))
	result := owner.connection.sendMessage(ctx.Done(), owner.takePending(messageIndex), nil, time.Second)
	if result != pooledMessageSendDone {
		t.Fatalf("closed admission = %v, want done", result)
	}
	if owner.pending[messageIndex] != nil || owner.pending[sharedIndex] == nil {
		t.Fatal("ledger did not separate consumed and never-offered references")
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if owner.pending[sharedIndex] != nil || owner.witnesses[0] != nil {
		t.Fatal("fixture retained its own references after close")
	}
	if err := owner.close(); err != nil {
		t.Fatalf("repeat cleanup = %v", err)
	}
}

// Converts an expected fixture fatal into a recoverable unwind, while holding
// that invocation's cleanups locally. Other test reports still reach testing.
type exchangeWritevExpectedFailure struct {
	*testing.T
	cleanups []func()
	fatal    string
}

// Retains the same last-in, first-out cleanup order as the test owner.
func (self *exchangeWritevExpectedFailure) Cleanup(cleanup func()) {
	self.cleanups = append(self.cleanups, cleanup)
}

// Preserves deferred unwinding without marking the parent as an expected fail.
func (self *exchangeWritevExpectedFailure) Fatal(args ...any) {
	self.fatal = fmt.Sprint(args...)
	panic(self)
}

// Preserves deferred unwinding for the fixture's formatted admission failure.
func (self *exchangeWritevExpectedFailure) Fatalf(format string, args ...any) {
	self.fatal = fmt.Sprintf(format, args...)
	panic(self)
}

// Exercises the actual native fixture's early-fatal branch after admission
// consumes one reference and before its sibling is offered. The first writer
// and both real socket readers are still owned at that exact failure barrier.
func TestExchangeWritevFixtureEarlyFailureRunsCleanup(t *testing.T) {
	expected := &exchangeWritevExpectedFailure{T: t}
	var owner *exchangeWritevFixtureOwner
	defer func() {
		// This independent backstop also joins the intentionally broken cleanup
		// control before its assertion failure returns from the test.
		if owner != nil {
			if err := owner.close(); err != nil {
				t.Error(err)
			}
		}
	}()
	func() {
		defer func() {
			failure := recover()
			for i := len(expected.cleanups) - 1; 0 <= i; i-- {
				expected.cleanups[i]()
			}
			if failure != nil && failure != expected {
				panic(failure)
			}
		}()
		testExchangeConnectionWritevTerminalBatchWithFailure(expected, ExchangeOpTransport, "success", func(fixture *exchangeWritevFixtureOwner) {
			owner = fixture
			fixture.connection.sendAdmission.close()
		})
	}()
	if expected.fatal != fmt.Sprintf("batch admission = %v", pooledMessageSendDone) {
		t.Fatalf("expected first batch admission failure, got %q", expected.fatal)
	}
	if owner == nil || !owner.closed {
		t.Fatal("early native fixture failure bypassed its registered cleanup")
	}
	select {
	case <-owner.connection.done:
	default:
		t.Error("early failure left exchange socket workers unjoined")
	}
	select {
	case <-owner.peerDone:
	default:
		t.Error("early failure left its accepted peer unjoined")
	}
	for _, message := range owner.pending {
		if message != nil {
			t.Error("early failure retained a never-offered pool reference")
		}
	}
	for _, witness := range owner.witnesses {
		if witness != nil {
			t.Error("early failure bypassed final-owner reconciliation")
		}
	}
}

// A real positive peer read precedes cleanup. Delivered data is not a terminal
// read, and draining it must still leave every socket and pooled owner joined.
func TestExchangeWritevFixtureCleanupDrainsPositiveReadBeforeTerminal(t *testing.T) {
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	firstPeerRead := make(chan int, 1)
	var firstPeerReadOnce sync.Once
	owner, ctx := newExchangeWritevCleanupSocketFixture(t, 8*1024, func(ctx context.Context) {
		close(writerEntered)
		select {
		case <-releaseWriter:
		case <-ctx.Done():
		}
	}, func(n int) {
		firstPeerReadOnce.Do(func() { firstPeerRead <- n })
	})
	message := clientconnect.MessagePoolGet(8 * 1024)
	index := owner.keepPending(message)
	owner.witnesses = append(owner.witnesses, retainResidentPoolWitness(message))
	if result := owner.connection.sendMessage(ctx.Done(), owner.takePending(index), nil, time.Second); result != pooledMessageSendDelivered {
		t.Fatalf("writer admission = %v", result)
	}
	select {
	case <-writerEntered:
	case <-ctx.Done():
		t.Fatal("write worker did not take the pooled message")
	}
	close(releaseWriter)
	var positiveReadByteCount int
	select {
	case positiveReadByteCount = <-firstPeerRead:
	case <-ctx.Done():
		t.Fatal("real peer did not read pending bytes before cleanup")
	}
	if positiveReadByteCount <= 0 {
		t.Fatal("positive-read barrier carried no bytes")
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.connection.done:
	default:
		t.Fatal("fixture returned before exchange socket workers joined")
	}
	select {
	case <-owner.peerDone:
	default:
		t.Fatal("fixture returned before its tcp peer joined")
	}
	if owner.pending[index] != nil || owner.witnesses[0] != nil {
		t.Fatal("fixture retained its own pooled references after the socket joins")
	}
	t.Logf("positive_read_bytes=%d exchange_joined=true peer_joined=true final_pool_owner_returned=true terminal_read=%v", positiveReadByteCount, owner.peerErr)
	if !errors.Is(owner.peerErr, net.ErrClosed) && !errors.Is(owner.peerErr, io.EOF) {
		t.Fatalf("WRITEV_CLEANUP_TERMINAL_AFTER_POSITIVE_READ: closed peer read = %v", owner.peerErr)
	}
}
