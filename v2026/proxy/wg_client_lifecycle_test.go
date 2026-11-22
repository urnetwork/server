package proxy

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
)

// Creating a TCP endpoint starts gVisor's protocol dispatcher. Canceling only
// the packet bridges leaves that dispatcher behind, which previously made the
// proxy package accumulate hundreds of workers between acceptance tests.
func TestWgClientStackCloseAndWaitJoinsTCPDispatcher(t *testing.T) {
	clientStack, err := newWgClientStack(
		context.Background(),
		netip.MustParseAddr("10.0.0.2"),
		1420,
		tuntest.NewChannelTUN(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var waitQueue waiter.Queue
	if _, tcpipErr := clientStack.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &waitQueue); tcpipErr != nil {
		clientStack.CloseAndWait()
		t.Fatalf("create TCP endpoint: %v", tcpipErr)
	}

	done := make(chan struct{})
	go func() {
		clientStack.CloseAndWait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wireguard client stack retained its TCP dispatcher")
	}

	// The client context watcher and the caller can race to close one leg.
	// Its completion boundary must therefore remain idempotent.
	clientStack.CloseAndWait()
}
