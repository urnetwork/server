package sidecarhost

// Run given listeners already bound (e2e-hosts-in-process): a test's on 127.0.0.1:0, so several blocks run in one
// process on ports the OS picks. Run serves /status on the status listener from the first moment and a stream
// engine's port on its listener once ready, the listener wrapped as a bound one is, and closes both however it ends.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/router"
)

// A stream engine, always ready, that hands each listener it is given to the test and serves it until it closes.
type runListenerTestEngine struct {
	served chan net.Listener
}

func (self *runListenerTestEngine) Ready() bool { return true }
func (self *runListenerTestEngine) Drain()      {}
func (self *runListenerTestEngine) Close()      {}

func (self *runListenerTestEngine) Serve(listener net.Listener) error {
	self.served <- listener
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		conn.Close()
	}
}

// A listener the Wrap marks, so the test sees the engine was served the wrapped one.
type runListenerTestWrapped struct {
	net.Listener
}

// Listens on loopback at a port the OS picks.
func runListenerTestListen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// /status on the status listener answers from the first moment and ok once ready, the stream port's listener reaches
// the engine wrapped, and at ctx's end Run returns with both listeners closed.
func TestRunServesTheListenersItIsGiven(t *testing.T) {
	statusListener := runListenerTestListen(t)
	streamListener := runListenerTestListen(t)
	engine := &runListenerTestEngine{served: make(chan net.Listener, 1)}
	settings := DefaultRunSettings()
	settings.ReadyPollInterval = 10 * time.Millisecond
	// no lb holds this test's connections, so the drain needs no keepalive grace
	settings.HttpServer.KeepaliveDrainTimeout = 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, &RunOptions{
			Service:        "listenertest",
			StatusListener: statusListener,
			Engine:         engine,
			Stream:         engine,
			Listeners: []*RunListener{{
				Port:     443,
				Listener: streamListener,
				Wrap: func(listener net.Listener) (net.Listener, error) {
					return &runListenerTestWrapped{Listener: listener}, nil
				},
			}},
			Metrics:  sidecartest.NewFakeMetrics(nil),
			Settings: settings,
		})
	}()

	var served net.Listener
	select {
	case served = <-engine.served:
	case err := <-done:
		t.Fatalf("Run ended before it served the stream port: %v", err)
	case <-time.After(runStepTimeout):
		t.Fatal("the engine was never given the stream port's listener")
	}
	wrapped, ok := served.(*runListenerTestWrapped)
	if !ok || wrapped.Listener != streamListener {
		t.Fatalf("the engine was given %T, not the bound listener wrapped", served)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	status := ""
	deadline := time.After(runStepTimeout)
	for status != "ok" {
		func() {
			response, err := client.Get("http://" + statusListener.Addr().String() + "/status")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			result := &router.WarpStatusResult{}
			if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(result) != nil {
				t.Fatalf("/status answered %d", response.StatusCode)
			}
			status = result.Status
		}()
		if status == "ok" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("/status never answered ok: %q", status)
		case <-time.After(settings.ReadyPollInterval):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(runStepTimeout):
		t.Fatal("Run did not return at ctx's end")
	}
	for name, listener := range map[string]net.Listener{"status": statusListener, "stream": streamListener} {
		if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("the %s listener is open after Run returned: %v", name, err)
		}
	}
}

// A listener Run was given is closed when its options do not validate, so a caller's listener never outlives a Run
// that refused it.
func TestRunClosesTheListenersOfOptionsItRefuses(t *testing.T) {
	statusListener := runListenerTestListen(t)
	streamListener := runListenerTestListen(t)
	err := Run(t.Context(), &RunOptions{
		Service:        "Not a service",
		StatusListener: statusListener,
		Engine:         &runListenerTestEngine{served: make(chan net.Listener, 1)},
		Listeners:      []*RunListener{{Port: 443, Listener: streamListener}},
	})
	if err == nil {
		t.Fatal("options naming no service ran")
	}
	for name, listener := range map[string]net.Listener{"status": statusListener, "stream": streamListener} {
		if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("the %s listener is open after Run refused its options: %v", name, err)
		}
	}
}

// A bound status listener needs no status port, and a bound stream listener takes no addresses beside it.
func TestRunOptionsWithBoundListeners(t *testing.T) {
	engine := &runListenerTestEngine{served: make(chan net.Listener, 1)}
	statusListener := runListenerTestListen(t)
	defer statusListener.Close()
	streamListener := runListenerTestListen(t)
	defer streamListener.Close()
	valid := &RunOptions{Service: "listenertest", StatusListener: statusListener, Engine: engine}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	unbound := &RunOptions{Service: "listenertest", Engine: engine}
	if unbound.Validate() == nil {
		t.Fatal("options with no status port and no status listener validated")
	}
	both := &RunOptions{
		Service:        "listenertest",
		StatusListener: statusListener,
		Engine:         engine,
		Stream:         engine,
		Listeners: []*RunListener{{
			Port:      443,
			Listener:  streamListener,
			Addresses: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:443")},
		}},
	}
	if both.Validate() == nil {
		t.Fatal("a port with a bound listener and addresses validated")
	}
}
