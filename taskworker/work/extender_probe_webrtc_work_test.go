package work

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The uptime probe's choice of path (connect/EXTENDER.md C3, S8): an address
// is judged over the required paths it was accepted on. The two paths are
// fakes here, so nothing dials and nothing reads the database; the task's
// accounting over probeExtenderAddress is extender_probe_work_test.go's.

// Replaces the two paths for one test, logging which ran.
type testUptimePaths struct {
	stateLock sync.Mutex
	ran       []string
	tcp       func(ctx context.Context) error
	webRtc    func(ctx context.Context) error
}

func (self *testUptimePaths) log(path string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ran = append(self.ran, path)
}

func (self *testUptimePaths) install(t *testing.T) {
	previousTcp := probeExtenderTcpAddress
	previousWebRtc := probeExtenderWebRtcAddress
	t.Cleanup(func() {
		probeExtenderTcpAddress = previousTcp
		probeExtenderWebRtcAddress = previousWebRtc
	})
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
// alone.
func TestExtenderUptimeProbesTheRequiredPathTheAddressWasAcceptedOn(t *testing.T) {
	for _, c := range []struct {
		carriers []string
		want     string
	}{
		{carriers: []string{connect.ExtenderCarrierWebRtc}, want: connect.ExtenderCarrierWebRtc},
		{carriers: []string{connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic}, want: connect.ExtenderCarrierWebRtc},
		{carriers: []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns}, want: connect.ExtenderCarrierTcp},
		{carriers: []string{connect.ExtenderCarrierQuic}, want: connect.ExtenderCarrierTcp},
	} {
		paths := &testUptimePaths{}
		paths.install(t)
		if err := probeExtenderAddress(context.Background(), testUptimeTarget(c.carriers...), "spoof.example", "api.example"); err != nil {
			t.Fatalf("%v: err = %v", c.carriers, err)
		}
		if len(paths.ran) != 1 || paths.ran[0] != c.want {
			t.Fatalf("%v: paths = %v, want %s alone", c.carriers, paths.ran, c.want)
		}
	}
}

// Root cause: an address accepted on both paths whose tcp port stopped
// answering would wait out the address budget on tcp and leave webrtc none.
// Observable: both are probed at once, webrtc passing while tcp has not
// answered passes the address and ends the tcp probe; both failing fails it
// with both reasons.
func TestExtenderUptimeProbesBothPathsOfADualPathAddressAtOnce(t *testing.T) {
	paths := &testUptimePaths{}
	tcpEnded := make(chan error, 1)
	paths.tcp = func(ctx context.Context) error {
		// no answer: only the end of the probe ends it
		<-ctx.Done()
		tcpEnded <- ctx.Err()
		return ctx.Err()
	}
	paths.install(t)
	target := testUptimeTarget(connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)
	if err := probeExtenderAddress(context.Background(), target, "spoof.example", "api.example"); err != nil {
		t.Fatalf("err = %v, want the webrtc pass", err)
	}
	if err := <-tcpEnded; !errors.Is(err, context.Canceled) {
		t.Fatalf("the tcp probe ended with %v, want the pass to cancel it", err)
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
}

// A path that panics fails alone: the other path still judges the address.
func TestExtenderUptimeContainsAPathPanic(t *testing.T) {
	paths := &testUptimePaths{
		webRtc: func(ctx context.Context) error {
			panic("synthetic uptime probe failure")
		},
	}
	paths.install(t)
	target := testUptimeTarget(connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)
	if err := probeExtenderAddress(context.Background(), target, "spoof.example", "api.example"); err != nil {
		t.Fatalf("err = %v, want the tcp pass", err)
	}
	webRtcOnly := testUptimeTarget(connect.ExtenderCarrierWebRtc)
	if err := probeExtenderAddress(context.Background(), webRtcOnly, "spoof.example", "api.example"); err == nil ||
		!strings.Contains(err.Error(), "synthetic uptime probe failure") {
		t.Fatalf("err = %v, want the panic as the failure", err)
	}
}
