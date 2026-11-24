package sidecart3

// A T3 Net reports what its Close left running through its test's Errorf (net_close_failed.go): NetSettings sets the
// report unless the caller's settings take it, and on a Net whose wedged client cannot join within its bound the
// report fails the test once, naming the client and carrying connect's goroutines, where sidecartest's Net would
// panic. The wedge is a receive callback held until the test lets it go, which it does only after Close, so the join
// times out whatever the bound; the bound is a millisecond only to keep the test quick. Needs no store: the Net here is
// sidecartest's T2 one, with the report T3's settings install.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	connectprotocol "github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar/sidecartest"
)

// The test's own TB, keeping each Errorf rather than failing the test with it.
type t3CloseFailedRecorder struct {
	testing.TB

	stateLock sync.Mutex
	errors    []string
}

func (self *t3CloseFailedRecorder) Errorf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errors = append(self.errors, fmt.Sprintf(format, args...))
}

// Every Errorf so far.
func (self *t3CloseFailedRecorder) reported() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.errors...)
}

// NetSettings reports a close that left work running through the test's Errorf, and keeps a report the caller's
// settings give.
func TestT3NetSettingsReportACloseFailureThroughTheTest(t *testing.T) {
	recorder := &t3CloseFailedRecorder{TB: t}
	netSettings := NetSettings(recorder, nil)
	if netSettings.CloseFailed == nil {
		t.Fatal("a T3 Net's settings leave CloseFailed nil, so a client stuck at Close panics the binary")
	}
	netSettings.CloseFailed(errors.New(`client "stuck" did not join within 30s: context deadline exceeded`))
	reported := recorder.reported()
	if len(reported) != 1 || !strings.Contains(reported[0], `client "stuck" did not join`) {
		t.Errorf("the report reached the test as %q", reported)
	}

	settings := DefaultSettings()
	callerNet := *settings.Net
	callerErrs := []error{}
	callerNet.CloseFailed = func(err error) {
		callerErrs = append(callerErrs, err)
	}
	settings.Net = &callerNet
	keptRecorder := &t3CloseFailedRecorder{TB: t}
	NetSettings(keptRecorder, settings).CloseFailed(errors.New("the caller's"))
	if len(callerErrs) != 1 || 0 < len(keptRecorder.reported()) {
		t.Errorf("NetSettings replaced the caller's report: the caller took %d, the test %d", len(callerErrs), len(keptRecorder.reported()))
	}
}

// A Net whose wedged client cannot join within its bound: Close returns, nothing panics, and the test is failed once,
// by name, with connect's goroutines still running.
func TestT3NetCloseFailureFailsTheTestRatherThanPanicking(t *testing.T) {
	settings := sidecartest.DefaultNetSettings()
	settings.ClientCloseTimeout = time.Millisecond
	recorder := &t3CloseFailedRecorder{TB: t}
	settings.CloseFailed = t3NetCloseFailed(recorder, settings.CloseDumpByteCount)
	net := sidecartest.NewNet(t.Context(), settings)
	a := net.AddClient("a", nil)
	wedged := net.AddClient("wedged", nil)
	entered := make(chan struct{}, 1)
	held := make(chan struct{})
	wedged.Client.AddReceiveCallback(func(source connect.TransferPath, frames []*connectprotocol.Frame, peer connect.Peer) {
		for _, frame := range frames {
			message, err := connect.FromFrame(frame)
			if err != nil {
				continue
			}
			if simple, ok := message.(*connectprotocol.SimpleMessage); ok && simple.Content == "hold" {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-held
			}
		}
	})
	releaseOnce := sync.OnceFunc(func() {
		close(held)
	})
	// let the wedged client go and join it, so nothing of it outlives the test
	release := func() {
		releaseOnce()
		ctx, cancel := context.WithTimeout(context.Background(), sidecartest.DefaultNetSettings().ClientCloseTimeout)
		defer cancel()
		if err := wedged.Client.CloseAndWait(ctx); err != nil {
			t.Fatalf("the wedged client did not join once let go: %v", err)
		}
	}
	defer releaseOnce()
	if err := a.SendMessage(wedged.ClientId, "hold"); err != nil {
		release()
		net.Close()
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(settings.SendTimeout):
		release()
		net.Close()
		t.Fatal("the wedged client's callback was never reached")
	}
	sendCtx, cancelSend := context.WithTimeout(context.Background(), sidecartest.DefaultNetSettings().ClientCloseTimeout)
	defer cancelSend()
	if err := a.Client.CloseAndWait(sendCtx); err != nil {
		release()
		net.Close()
		t.Fatalf("the sender did not join: %v", err)
	}

	net.Close()
	reported := recorder.reported()
	release()
	if len(reported) != 1 {
		t.Fatalf("Close reported %d failures to the test, not one: %q", len(reported), reported)
	}
	if !strings.Contains(reported[0], `"wedged"`) || !strings.Contains(reported[0], "context deadline exceeded") {
		t.Errorf("the report does not name the wedged client's join running out its bound: %s", reported[0])
	}
	if !strings.Contains(reported[0], t3ConnectFramePrefix) {
		t.Errorf("the report carries no goroutine of connect's: %s", reported[0])
	}
}
