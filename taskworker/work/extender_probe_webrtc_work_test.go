package work

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The uptime probe's choice of path (connect/EXTENDER.md C3, S8): an address
// is judged over the required paths it was accepted on. The two paths are
// fakes here, so nothing dials and nothing reads the database; the task's
// accounting over probeExtenderAddress is extender_probe_work_test.go's.

// Replaces the two paths and the head start's timer for one test, logging
// which path ran. The head start elapses only when the test sends on
// headStart; nil never elapses.
type testUptimePaths struct {
	stateLock sync.Mutex
	ran       []string
	tcp       func(ctx context.Context) error
	webRtc    func(ctx context.Context) error
	headStart chan time.Time
}

func (self *testUptimePaths) log(path string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ran = append(self.ran, path)
}

func (self *testUptimePaths) paths() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.ran)
}

func (self *testUptimePaths) install(t *testing.T) {
	previousTcp := probeExtenderTcpAddress
	previousWebRtc := probeExtenderWebRtcAddress
	previousHeadStart := extenderProbeWebRtcHeadStart
	t.Cleanup(func() {
		probeExtenderTcpAddress = previousTcp
		probeExtenderWebRtcAddress = previousWebRtc
		extenderProbeWebRtcHeadStart = previousHeadStart
	})
	extenderProbeWebRtcHeadStart = func() (<-chan time.Time, func()) {
		return self.headStart, func() {}
	}
	probeExtenderTcpAddress = func(ctx context.Context, target *model.NetworkExtenderProbeTarget, serverName string, destinationHost string) error {
		self.log(connect.ExtenderCarrierTcp)
		if self.tcp == nil {
			return nil
		}
		return self.tcp(ctx)
	}
	probeExtenderWebRtcAddress = func(ctx context.Context, target *model.NetworkExtenderProbeTarget, destinationHost string) error {
		self.log(connect.ExtenderCarrierWebRtc)
		if self.webRtc == nil {
			return nil
		}
		return self.webRtc(ctx)
	}
}

func testUptimeTarget(carriers ...string) *model.NetworkExtenderProbeTarget {
	return &model.NetworkExtenderProbeTarget{
		ExtenderId: server.NewId(),
		ClientId:   server.NewId(),
		PublicKey:  []byte("extender-public-key-uptime"),
		IpVersion:  4,
		Carriers:   carriers,
	}
}

// Root cause: the uptime task probed every address over tcp, so an extender
// accepted on webrtc, which has no public tcp port, would be deactivated in
// half an hour. Observable: a webrtc-only address is judged by webrtc alone
// and never dialed on tcp; a tcp address, and one that lists neither, by tcp
// alone; and an address that lists both, whose tcp passes within its head
// start, by tcp alone.
func TestExtenderUptimeProbesTheRequiredPathTheAddressWasAcceptedOn(t *testing.T) {
	for _, c := range []struct {
		carriers []string
		want     string
	}{
		{carriers: []string{connect.ExtenderCarrierWebRtc}, want: connect.ExtenderCarrierWebRtc},
		{carriers: []string{connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic}, want: connect.ExtenderCarrierWebRtc},
		{carriers: []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns}, want: connect.ExtenderCarrierTcp},
		{carriers: []string{connect.ExtenderCarrierQuic}, want: connect.ExtenderCarrierTcp},
		{carriers: []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}, want: connect.ExtenderCarrierTcp},
	} {
		paths := &testUptimePaths{}
		paths.install(t)
		if err := probeExtenderAddress(context.Background(), testUptimeTarget(c.carriers...), "spoof.example", "api.example"); err != nil {
			t.Fatalf("%v: err = %v", c.carriers, err)
		}
		if ran := paths.paths(); len(ran) != 1 || ran[0] != c.want {
			t.Fatalf("%v: paths = %v, want %s alone", c.carriers, ran, c.want)
		}
	}
}

// Root cause: an address accepted on both paths whose tcp port stopped
// answering would wait out the address budget on tcp and leave webrtc none;
// and probing both at once sent the extender's device an offer every pass
// that a working tcp port made pointless, and returned with the losing probe
// still running. Observable: webrtc is probed beside tcp once the head start
// elapses with tcp unanswered, passes the address and ends the tcp probe,
// and the call returns only once the tcp probe has ended; a tcp failure
// within the head start probes webrtc at once; both failing fails the address
// with both reasons; and a pass whose budget has ended starts no webrtc probe.
func TestExtenderUptimeGivesTcpAHeadStartOnADualPathAddress(t *testing.T) {
	target := testUptimeTarget(connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)

	tcpEnded := make(chan error, 1)
	silentTcp := &testUptimePaths{
		headStart: make(chan time.Time, 1),
	}
	silentTcp.tcp = func(ctx context.Context) error {
		// the head start elapses with tcp unanswered, and only the end of
		// the probe ends it
		silentTcp.headStart <- time.Time{}
		<-ctx.Done()
		tcpEnded <- ctx.Err()
		return ctx.Err()
	}
	silentTcp.install(t)
	if err := probeExtenderAddress(context.Background(), target, "spoof.example", "api.example"); err != nil {
		t.Fatalf("err = %v, want the webrtc pass", err)
	}
	select {
	case err := <-tcpEnded:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the tcp probe ended with %v, want the pass to cancel it", err)
		}
	default:
		t.Fatal("the call returned with the tcp probe still running")
	}
	if ran := silentTcp.paths(); !slices.Equal(ran, []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}) {
		t.Fatalf("paths = %v, want tcp then webrtc", ran)
	}

	refusedTcp := &testUptimePaths{
		tcp: func(ctx context.Context) error {
			return errors.New("tcp refused")
		},
	}
	refusedTcp.install(t)
	if err := probeExtenderAddress(context.Background(), target, "spoof.example", "api.example"); err != nil {
		t.Fatalf("err = %v, want the webrtc pass without the head start", err)
	}
	if ran := refusedTcp.paths(); !slices.Equal(ran, []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}) {
		t.Fatalf("paths = %v, want tcp then webrtc", ran)
	}

	failing := &testUptimePaths{
		tcp: func(ctx context.Context) error {
			return errors.New("tcp refused")
		},
		webRtc: func(ctx context.Context) error {
			return errors.New("the extender's client did not answer the webrtc offer")
		},
	}
	failing.install(t)
	err := probeExtenderAddress(context.Background(), target, "spoof.example", "api.example")
	if err == nil || !strings.Contains(err.Error(), "tcp refused") || !strings.Contains(err.Error(), "did not answer the webrtc offer") {
		t.Fatalf("err = %v, want both reasons", err)
	}

	ended := &testUptimePaths{
		tcp: func(ctx context.Context) error {
			return ctx.Err()
		},
	}
	ended.install(t)
	endedCtx, endedCancel := context.WithCancel(context.Background())
	endedCancel()
	if err := probeExtenderAddress(endedCtx, target, "spoof.example", "api.example"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the ended budget", err)
	}
	if ran := ended.paths(); !slices.Equal(ran, []string{connect.ExtenderCarrierTcp}) {
		t.Fatalf("paths = %v, want tcp alone once the budget has ended", ran)
	}
}

// A path that panics fails alone: the other path still judges the address.
func TestExtenderUptimeContainsAPathPanic(t *testing.T) {
	paths := &testUptimePaths{
		tcp: func(ctx context.Context) error {
			panic("synthetic uptime probe failure")
		},
	}
	paths.install(t)
	target := testUptimeTarget(connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)
	if err := probeExtenderAddress(context.Background(), target, "spoof.example", "api.example"); err != nil {
		t.Fatalf("err = %v, want the webrtc pass", err)
	}
	webRtcOnly := testUptimeTarget(connect.ExtenderCarrierWebRtc)
	panicking := &testUptimePaths{
		webRtc: func(ctx context.Context) error {
			panic("synthetic uptime probe failure")
		},
	}
	panicking.install(t)
	if err := probeExtenderAddress(context.Background(), webRtcOnly, "spoof.example", "api.example"); err == nil ||
		!strings.Contains(err.Error(), "synthetic uptime probe failure") {
		t.Fatalf("err = %v, want the panic as the failure", err)
	}
}
