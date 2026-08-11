// This file retains every generated device PlatformTransport until the
// full-TUN fixture has cancelled its producer and joined concrete ownership.
package perfvar

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

// platformTransportCloser is the smallest ownership contract needed by the
// fixture and permits barrier-driven tests without constructing a carrier.
type platformTransportCloser interface {
	Close()
	CloseAndWait(context.Context) error
}

// retainedPlatformTransport is immutable after publication. A lock-free stack
// keeps the generator callback independent of teardown, socket closure, and
// test scheduling while retaining every migration replacement.
type retainedPlatformTransport struct {
	sequence  uint64
	client    *clientconnect.Client
	transport *clientconnect.PlatformTransport
	closer    platformTransportCloser
	next      *retainedPlatformTransport
}

// platformTransportOwner is one append-only fixture owner. New transports are
// published with a compare-and-swap because PlatformTransportCreated forbids
// blocking the window setup path. Teardown walks generation-stable snapshots.
type platformTransportOwner struct {
	head       atomic.Pointer[retainedPlatformTransport]
	next       atomic.Uint64
	publishing atomic.Int64
	progress   chan struct{}
	closing    atomic.Bool

	// Nil outside deterministic owner tests.
	beforePublishForTest     func()
	beforeStableCheckForTest func()
}

// Construction initializes the coalesced publication notification.
func newPlatformTransportOwner() *platformTransportOwner {
	return &platformTransportOwner{progress: make(chan struct{}, 1)}
}

// observe is the production callback. It only allocates, publishes, and sends
// a best-effort wakeup; it never waits for a consumer or carrier lifecycle.
func (self *platformTransportOwner) observe(
	client *clientconnect.Client,
	transport *clientconnect.PlatformTransport,
) {
	self.add(client, transport, transport)
}

// add is shared with deterministic recorder tests.
func (self *platformTransportOwner) add(
	client *clientconnect.Client,
	transport *clientconnect.PlatformTransport,
	closer platformTransportCloser,
) {
	self.publishing.Add(1)
	defer func() {
		self.publishing.Add(-1)
		self.notify()
	}()
	if self.beforePublishForTest != nil {
		self.beforePublishForTest()
	}
	node := &retainedPlatformTransport{
		sequence:  self.next.Add(1),
		client:    client,
		transport: transport,
		closer:    closer,
	}
	for {
		head := self.head.Load()
		node.next = head
		if self.head.CompareAndSwap(head, node) {
			break
		}
	}
	if self.closing.Load() {
		closer.Close()
	}
	self.notify()
}

// A best-effort edge wakes first-generation and stable-teardown waiters; both
// always re-read the retained stack and publication count as their truth.
func (self *platformTransportOwner) notify() {
	select {
	case self.progress <- struct{}{}:
	default:
	}
}

// waitFirst returns the first generated transport even if later replacements
// raced its consumer. The retained stack, rather than a bounded channel,
// provides the source of truth.
func (self *platformTransportOwner) waitFirst(
	ctx context.Context,
) (observedPlatformTransport, error) {
	for {
		var first *retainedPlatformTransport
		for node := self.head.Load(); node != nil; node = node.next {
			if first == nil || node.sequence < first.sequence {
				first = node
			}
		}
		if first != nil && first.client != nil && first.transport != nil {
			return observedPlatformTransport{
				client:    first.client,
				transport: first.transport,
			}, nil
		}
		select {
		case <-ctx.Done():
			return observedPlatformTransport{}, ctx.Err()
		case <-self.progress:
		}
	}
}

// waitCurrentClient returns the newest client published by the generated
// window callback after its transport ownership node is retained. The current
// pointer is checked again after the scan so a concurrent replacement cannot
// make an older matching node look current at the return boundary.
func (self *platformTransportOwner) waitCurrentClient(
	ctx context.Context,
	current *atomic.Pointer[clientconnect.Client],
) (*clientconnect.Client, error) {
	return self.waitCurrentClientAfter(ctx, current, nil)
}

// waitCurrentClientAfter waits until the generated callback has published and
// retained a current client distinct from the rejected prior snapshot.
func (self *platformTransportOwner) waitCurrentClientAfter(
	ctx context.Context,
	current *atomic.Pointer[clientconnect.Client],
	rejected *clientconnect.Client,
) (*clientconnect.Client, error) {
	for {
		candidate := current.Load()
		if candidate != nil && candidate != rejected {
			for node := self.head.Load(); node != nil; node = node.next {
				if node.client == candidate && current.Load() == candidate {
					return candidate, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-self.progress:
		}
	}
}

// closeAndWait cancels and joins every transport visible in a captured stack,
// then repeats when publication changed while those joins were in progress.
func (self *platformTransportOwner) closeAndWait(ctx context.Context) error {
	self.closing.Store(true)
	joined := map[*retainedPlatformTransport]bool{}
	joinedClients := map[*clientconnect.Client]bool{}
	for {
		for self.publishing.Load() != 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-self.progress:
			}
		}
		boundary := self.head.Load()
		for node := boundary; node != nil; node = node.next {
			if joined[node] {
				continue
			}
			if err := node.closer.CloseAndWait(ctx); err != nil {
				return fmt.Errorf("join generated platform transport %d: %w", node.sequence, err)
			}
			if node.client != nil && !joinedClients[node.client] {
				node.client.Flush()
				if err := node.client.CloseAndWait(ctx); err != nil {
					return fmt.Errorf("join generated client %d: %w", node.sequence, err)
				}
				joinedClients[node.client] = true
			}
			joined[node] = true
		}
		if self.beforeStableCheckForTest != nil {
			self.beforeStableCheckForTest()
		}
		if self.head.Load() == boundary && self.publishing.Load() == 0 {
			return nil
		}
	}
}

// blockingPlatformTransportCloser exposes exact close entry and release.
type blockingPlatformTransportCloser struct {
	closeOnce sync.Once
	started   chan struct{}
	release   chan struct{}
}

// Construction leaves teardown held until the test releases it explicitly.
func newBlockingPlatformTransportCloser() *blockingPlatformTransportCloser {
	return &blockingPlatformTransportCloser{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// The first close attempt publishes an exact observable lifecycle edge.
func (self *blockingPlatformTransportCloser) Close() {
	self.closeOnce.Do(func() {
		close(self.started)
	})
}

// Joining blocks at the explicit barrier or returns the caller's cancellation.
func (self *blockingPlatformTransportCloser) CloseAndWait(ctx context.Context) error {
	self.Close()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.release:
		return nil
	}
}

// A replacement published after the first captured generation has joined is
// included in the next generation. This deterministically reproduces the old
// bounded-channel ownership loss without using goroutine leak timing.
func TestPlatformTransportOwnerJoinsPublicationAcrossTeardownCapture(t *testing.T) {
	owner := newPlatformTransportOwner()
	first := newBlockingPlatformTransportCloser()
	second := newBlockingPlatformTransportCloser()
	owner.add(nil, nil, first)

	stableCheckEntered := make(chan struct{})
	releaseStableCheck := make(chan struct{})
	secondPublishEntered := make(chan struct{})
	releaseSecondPublish := make(chan struct{})
	var stableCheckOnce sync.Once
	owner.beforeStableCheckForTest = func() {
		stableCheckOnce.Do(func() {
			close(stableCheckEntered)
			<-releaseStableCheck
		})
	}
	owner.beforePublishForTest = func() {
		close(secondPublishEntered)
		<-releaseSecondPublish
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- owner.closeAndWait(ctx)
	}()
	<-first.started
	close(first.release)
	<-stableCheckEntered
	secondPublished := make(chan struct{})
	go func() {
		owner.add(nil, nil, second)
		close(secondPublished)
	}()
	<-secondPublishEntered
	close(releaseStableCheck)
	select {
	case err := <-closeResult:
		t.Fatalf("owner skipped callback held before publication: %v", err)
	default:
	}
	close(releaseSecondPublish)
	<-secondPublished
	<-second.started
	select {
	case err := <-closeResult:
		t.Fatalf("owner returned before replacement release: %v", err)
	default:
	}
	close(second.release)
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
}

// The former four-entry observation channel silently discarded later
// transports. More than four generated replacements remain exact ownership.
func TestPlatformTransportOwnerRetainsEveryGeneratedReplacement(t *testing.T) {
	owner := newPlatformTransportOwner()
	closers := make([]*blockingPlatformTransportCloser, 8)
	for index := range closers {
		closer := newBlockingPlatformTransportCloser()
		close(closer.release)
		closers[index] = closer
		owner.add(nil, nil, closer)
	}
	if err := owner.closeAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	for index, closer := range closers {
		select {
		case <-closer.started:
		default:
			t.Fatalf("generated replacement %d was not closed", index)
		}
	}
}

// Generated transport ownership includes the generated Client whose transfer
// workers can otherwise retain pooled messages after its carrier is joined.
func TestPlatformTransportOwnerJoinsGeneratedClient(t *testing.T) {
	owner := newPlatformTransportOwner()
	closer := newBlockingPlatformTransportCloser()
	close(closer.release)
	client := clientconnect.NewClient(
		t.Context(),
		clientconnect.NewId(),
		clientconnect.NewNoContractClientOob(),
		clientconnect.DefaultClientSettings(),
	)
	owner.add(client, nil, closer)

	if err := owner.closeAndWait(t.Context()); err != nil {
		t.Fatalf("join generated transport owner: %v", err)
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("generated client remained open after owner join")
	}
}

// P2P priming must use the active generated window, not the first retained
// generation whose route manager may already have been retired to zero routes.
func TestPlatformTransportOwnerCurrentClientSkipsRetiredFirstGeneration(t *testing.T) {
	owner := newPlatformTransportOwner()
	olderCloser := newBlockingPlatformTransportCloser()
	newerCloser := newBlockingPlatformTransportCloser()
	close(olderCloser.release)
	close(newerCloser.release)
	olderClient := clientconnect.NewClient(
		t.Context(),
		clientconnect.NewId(),
		clientconnect.NewNoContractClientOob(),
		clientconnect.DefaultClientSettings(),
	)
	newerClient := clientconnect.NewClient(
		t.Context(),
		clientconnect.NewId(),
		clientconnect.NewNoContractClientOob(),
		clientconnect.DefaultClientSettings(),
	)
	current := &atomic.Pointer[clientconnect.Client]{}
	current.Store(olderClient)
	owner.add(olderClient, nil, olderCloser)
	current.Store(newerClient)
	owner.add(newerClient, nil, newerCloser)
	defer func() {
		if err := owner.closeAndWait(t.Context()); err != nil {
			t.Errorf("close generated transport owner: %v", err)
		}
	}()

	var first *retainedPlatformTransport
	for node := owner.head.Load(); node != nil; node = node.next {
		if first == nil || node.sequence < first.sequence {
			first = node
		}
	}
	if first == nil || first.client != olderClient {
		t.Fatal("fixture did not retain the older client as its first generation")
	}
	active, err := owner.waitCurrentClient(t.Context(), current)
	if err != nil {
		t.Fatalf("wait for current generated client: %v", err)
	}
	if active != newerClient {
		t.Fatal("current generated-client lookup selected the retired first generation")
	}
}
