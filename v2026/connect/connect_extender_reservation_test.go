// The extender fixture must own its carrier continuously from port selection
// through serving, then release it when its joined lifecycle ends.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A competing bind at the explicit handoff boundary forces the former
// close-and-rebind race without a scheduler delay or an external process.
func testConnectExtenderCarrierReservation(t *testing.T, mode connect.ExtenderConnectMode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bind := func(address string) (io.Closer, error) {
		if mode == connect.ExtenderConnectModeTcpTls {
			return net.Listen("tcp4", address)
		}
		return net.ListenPacket("udp4", address)
	}
	reservationLost := false
	extender := newTestExtenderWithBindCheck(ctx, t, mode, func(port int) {
		rival, err := bind(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			reservationLost = true
			t.Cleanup(func() { rival.Close() })
		} else if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("%s competing bind: %v", mode, err)
		}
	})
	t.Cleanup(extender.server.CloseAndWait)
	select {
	case <-extender.server.Listening():
	case <-ctx.Done():
		t.Fatalf("%s carrier startup: %v", mode, ctx.Err())
	}
	if reservationLost {
		t.Fatalf("%s carrier reservation was released before serving: %v", mode, extender.server.ListenErrors())
	}
	carriers := extender.server.Carriers()
	if len(carriers) != 1 || carriers[0] != connect.ExtenderCarrierForConnectMode(mode) {
		t.Fatalf("%s served carriers = %v, errors=%v", mode, carriers, extender.server.ListenErrors())
	}
	extender.server.CloseAndWait()
	replacement, err := bind(net.JoinHostPort("127.0.0.1", strconv.Itoa(extender.config.Profile.Port)))
	if err != nil {
		t.Fatalf("%s carrier was not released after joined shutdown: %v", mode, err)
	}
	replacement.Close()
}

func TestConnectExtenderKeepsTcpReservationUntilServing(t *testing.T) {
	testConnectExtenderCarrierReservation(t, connect.ExtenderConnectModeTcpTls)
}

func TestConnectExtenderKeepsQuicReservationUntilServing(t *testing.T) {
	testConnectExtenderCarrierReservation(t, connect.ExtenderConnectModeQuic)
}

func TestConnectExtenderKeepsDnsReservationUntilServing(t *testing.T) {
	testConnectExtenderCarrierReservation(t, connect.ExtenderConnectModeDns)
}
