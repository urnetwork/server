package work

// The uptime prober's own control target (extender_probe_control.go).

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// A control that has never answered from this process holds nothing, so a
// control that cannot answer from where the prober runs never stops the
// revocations: dead extenders still leave the directory.
func TestExtenderProbeControlThatNeverAnsweredHoldsNothing(t *testing.T) {
	world := newTestChurnWorld(t)
	stubTestChurnControl(t, func(ipVersion int) string {
		return testChurnControlFailed
	})
	extenderIds := []server.Id{
		world.store.addExtender(server.NewId(), testExtenderChurnStart, 4),
		world.store.addExtender(server.NewId(), testExtenderChurnStart, 4),
	}
	world.setAnswer(testChurnAlwaysFail(errTestChurnExtenderDown))
	revoked := 0
	for range ExtenderMaxConsecutiveProbeFailures {
		result := world.pass()
		connect.AssertEqual(t, len(result.HeldFamilies), 0)
		revoked += result.Revoked
	}
	connect.AssertEqual(t, revoked, 2)
	for _, extenderId := range extenderIds {
		connect.AssertEqual(t, world.store.extender(extenderId).active, false)
	}
}

// A control proves itself by answering, and only then does its failure hold.
// An address it cannot find, a lookup that fails and a pass that ended learn
// nothing, and hold nothing.
func TestExtenderProbeControlState(t *testing.T) {
	answers := map[int]string{}
	stubTestChurnControl(t, func(ipVersion int) string {
		return answers[ipVersion]
	})
	controls := newExtenderProbeControlState()
	ctx := context.Background()

	check := func(ipVersion int) *extenderProbeControlCheck {
		return controls.check(ctx, "api.ur.example", []int{ipVersion})[ipVersion]
	}

	// failing before it ever answered
	answers[4] = extenderProbeControlFailed
	connect.AssertEqual(t, check(4).holds, false)

	// it answers, then fails: held
	answers[4] = extenderProbeControlOk
	connect.AssertEqual(t, check(4).holds, false)
	answers[4] = extenderProbeControlFailed
	connect.AssertEqual(t, check(4).result, extenderProbeControlFailed)
	connect.AssertEqual(t, check(4).holds, true)

	// each family proves itself
	answers[6] = extenderProbeControlFailed
	connect.AssertEqual(t, check(6).holds, false)

	// nothing learned holds nothing
	for _, result := range []string{extenderProbeControlAbsent, extenderProbeControlUnknown} {
		answers[4] = result
		connect.AssertEqual(t, check(4).holds, false)
	}

	// a pass that ended under the check learned nothing, whatever it said
	answers[4] = extenderProbeControlFailed
	endedCtx, cancel := context.WithCancel(ctx)
	cancel()
	ended := controls.check(endedCtx, "api.ur.example", []int{4})[4]
	connect.AssertEqual(t, ended.result, extenderProbeControlUnknown)
	connect.AssertEqual(t, ended.holds, false)
}

// What a failed control dial logs names no address: neither the operator's
// own, which the dial named, nor the prober's end of the socket, nor the
// resolver's.
func TestExtenderProbeControlErrorTextNamesNoAddress(t *testing.T) {
	for _, err := range []error{
		&net.OpError{
			Op:     "dial",
			Net:    "tcp4",
			Source: net.TCPAddrFromAddrPort(netip.MustParseAddrPort("10.0.0.5:51234")),
			Addr:   net.TCPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.9:443")),
			Err:    &net.OpError{Op: "connect", Err: syscall.ECONNREFUSED},
		},
		&net.OpError{
			Op:     "read",
			Net:    "tcp6",
			Source: net.TCPAddrFromAddrPort(netip.MustParseAddrPort("[2001:db8::5]:51234")),
			Addr:   net.TCPAddrFromAddrPort(netip.MustParseAddrPort("[2001:db8:1::9]:443")),
			Err:    syscall.ECONNRESET,
		},
		&net.DNSError{
			Err:    "server misbehaving",
			Name:   "api.ur.example",
			Server: "127.0.0.11:53",
		},
	} {
		text := extenderProbeControlErrorText(err)
		for _, address := range []string{"10.0.0.5", "203.0.113.9", "2001:db8", "127.0.0.11"} {
			if strings.Contains(text, address) {
				t.Errorf("the control's error text names an address: %q", text)
			}
		}
		if text == "" {
			t.Errorf("the control's error text says nothing of %v", err)
		}
	}
	connect.AssertEqual(t, extenderProbeControlErrorText(errors.New("tls: handshake failure")), "tls: handshake failure")
}
