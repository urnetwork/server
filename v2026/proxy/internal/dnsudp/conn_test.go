package dnsudp

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/waiter"
)

type errorEndpoint struct {
	tcpip.Endpoint
	queue   *waiter.Queue
	mu      sync.Mutex
	err     tcpip.Error
	closed  bool
	closes  int
	reads   int
	blocked chan struct{}
	release chan struct{}
}

func (e *errorEndpoint) Read(io.Writer, tcpip.ReadOptions) (tcpip.ReadResult, tcpip.Error) {
	e.mu.Lock()
	e.reads++
	var err tcpip.Error = &tcpip.ErrWouldBlock{}
	if e.err != nil {
		err, e.err = e.err, nil
	} else if e.closed {
		err = &tcpip.ErrClosedForReceive{}
	}
	pause := e.reads == 2 && e.blocked != nil
	e.mu.Unlock()
	if pause {
		close(e.blocked)
		<-e.release
	}
	return tcpip.ReadResult{}, err
}

func (e *errorEndpoint) refuse() {
	e.mu.Lock()
	if !e.closed {
		e.err = &tcpip.ErrConnectionRefused{}
	}
	e.mu.Unlock()
	e.queue.Notify(waiter.EventErr)
}

func (e *errorEndpoint) Close() {
	e.mu.Lock()
	e.closed = true
	e.closes++
	e.mu.Unlock()
	e.queue.Notify(waiter.ReadableEvents | waiter.EventErr | waiter.EventHUp)
}

func (*errorEndpoint) GetLocalAddress() (tcpip.FullAddress, tcpip.Error) {
	return tcpip.FullAddress{}, nil
}

func (*errorEndpoint) GetRemoteAddress() (tcpip.FullAddress, tcpip.Error) {
	return tcpip.FullAddress{}, nil
}

func TestErrorReadinessWakesBlockedDNSRead(t *testing.T) {
	var queue waiter.Queue
	e := &errorEndpoint{queue: &queue, blocked: make(chan struct{}), release: make(chan struct{})}
	c := NewConn(&queue, e)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(e.release) }) }
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, err := c.Read(make([]byte, 64))
		done <- err
	}()
	t.Cleanup(func() { release(); c.Close(); <-joined })
	select {
	case <-e.blocked:
	case <-time.After(time.Second):
		t.Fatal("read never registered its waiter")
	}
	if queue.Events()&waiter.EventErr == 0 {
		t.Fatal("socket has no owner for error readiness")
	}
	e.refuse()
	release()
	select {
	case err := <-done:
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Timeout() || !strings.Contains(opErr.Err.Error(), (&tcpip.ErrConnectionRefused{}).String()) {
			t.Fatalf("blocked read did not preserve endpoint refusal: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("error-only notification did not wake read")
	}
	c.Close()
	if !queue.IsEmpty() {
		t.Fatal("closed socket retained an error or read waiter")
	}
}

func TestCloseJoinsNotificationOwnerAndIsIdempotent(t *testing.T) {
	var queue waiter.Queue
	e := &errorEndpoint{queue: &queue}
	c := NewConn(&queue, e)
	// A queued notification and multiple close callers must still retire
	// one owner, without recursive Notify or a callback/Close lock cycle.
	e.refuse()
	var closers sync.WaitGroup
	for range 8 {
		closers.Add(1)
		go func() { defer closers.Done(); c.Close() }()
	}
	closers.Wait()
	select {
	case <-c.done:
	default:
		t.Fatal("Close returned before its notification worker joined")
	}
	if !queue.IsEmpty() || e.closes != 1 {
		t.Fatalf("close ownership: queue empty=%v endpoint closes=%d", queue.IsEmpty(), e.closes)
	}
	queue.Notify(waiter.EventErr) // no stale callback after close
}

func TestCloseReleasesBlockedDNSRead(t *testing.T) {
	var queue waiter.Queue
	e := &errorEndpoint{queue: &queue, blocked: make(chan struct{}), release: make(chan struct{})}
	c := NewConn(&queue, e)
	done := make(chan error, 1)
	joined := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(e.release) }) }
	go func() { defer close(joined); _, err := c.Read(make([]byte, 1)); done <- err }()
	t.Cleanup(func() { release(); c.Close(); <-joined })
	select {
	case <-e.blocked:
	case <-time.After(time.Second):
		t.Fatal("read never reached close boundary")
	}
	c.Close()
	release()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("closed read = %v, want unchanged EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not release read")
	}
	if !queue.IsEmpty() {
		t.Fatal("close/read join retained waiter")
	}
}

func TestDNSReadDeadlineIsUnchanged(t *testing.T) {
	var queue waiter.Queue
	c := NewConn(&queue, &errorEndpoint{queue: &queue})
	defer c.Close()
	if _, ok := interface{}(c).(net.PacketConn); !ok {
		t.Fatal("adapter lost UDP framing")
	}
	c.SetReadDeadline(time.Now().Add(-time.Second))
	_, err := c.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("original UDP deadline behavior changed: %v", err)
	}
}
