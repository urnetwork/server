package fleetprobe

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Tests of the probe path: re-opening and retiring tunnels, failed re-opens, and
// the pins each tunnel is opened with.

// Stands in for a providertunnel.Tunnel: a client per tunnel, a
// loss signal the test fires, and a count of closes.
type fakeTunnel struct {
	id     int
	lost   context.Context
	lose   context.CancelCauseFunc
	closed atomic.Int32
	hosts  []string
}

// Implements probeTunnel, recording the hosts its client was built for.
func (self *fakeTunnel) HttpClientForHosts(timeout time.Duration, hosts []string) *http.Client {
	self.hosts = hosts
	return &http.Client{Timeout: timeout}
}

// Implements probeTunnel.
func (self *fakeTunnel) Lost() context.Context { return self.lost }

// Implements probeTunnel: a closed tunnel reads as lost.
func (self *fakeTunnel) Close() error {
	self.closed.Add(1)
	self.lose(providertunnel.ErrTunnelClosed)
	return nil
}

// Opens fakeTunnels, or fails when told to.
type fakeOpener struct {
	stateLock sync.Mutex
	opened    []*fakeTunnel
	failing   bool
}

// A tunnelOpener: opens the next fakeTunnel, or fails when told to.
func (self *fakeOpener) open(context.Context) (probeTunnel, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failing {
		return nil, errors.New("the provider is not taking contracts")
	}
	lost, lose := context.WithCancelCause(context.Background())
	tunnel := &fakeTunnel{id: len(self.opened) + 1, lost: lost, lose: lose}
	self.opened = append(self.opened, tunnel)
	return tunnel, nil
}

// A re-open makes a new tunnel
// current, with a client built for the same hosts, and closes the dead one;
// Close closes the current one and waits for every retired one.
func TestProbePathReopensAndRetiresTheDeadTunnel(t *testing.T) {
	opener := &fakeOpener{}
	hosts := []string{"site.example", "api.example"}
	path, err := openProbePath(context.Background(), opener.open, hosts, time.Minute)
	if err != nil {
		t.Fatalf("openProbePath: %v", err)
	}
	first, firstLost := path.Current()
	if firstLost.Err() != nil {
		t.Fatal("a fresh tunnel is already lost")
	}

	opener.opened[0].lose(providertunnel.ErrTunnelLost)
	if err := path.Reopen(context.Background()); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	second, secondLost := path.Current()
	if second == first || secondLost.Err() != nil {
		t.Fatal("Reopen did not make a live new tunnel current")
	}
	if got := opener.opened[1].hosts; len(got) != 2 || got[0] != "site.example" {
		t.Errorf("the re-opened tunnel's client was built for %v, want the same hosts", got)
	}
	if path.reopens() != 1 {
		t.Errorf("reopens = %d, want 1", path.reopens())
	}

	if err := path.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, tunnel := range opener.opened {
		if got := tunnel.closed.Load(); got != 1 {
			t.Errorf("tunnel %d closed %d time(s), want once", tunnel.id, got)
		}
	}
	if err := path.Close(); err != nil {
		t.Errorf("a second Close returned %v, want nil", err)
	}
	if err := path.Reopen(context.Background()); !errors.Is(err, errPathClosed) {
		t.Errorf("Reopen after Close = %v, want errPathClosed", err)
	}
	if len(opener.opened) != 2 {
		t.Errorf("%d tunnels opened; a closed path must not open another", len(opener.opened))
	}
}

// A failed re-open
// leaves the lost tunnel as it is -- still lost, so the next attempt tries
// again -- and costs nothing but the error.
func TestProbePathReopenFailureKeepsTheDeadTunnelCurrent(t *testing.T) {
	opener := &fakeOpener{}
	path, err := openProbePath(context.Background(), opener.open, nil, time.Minute)
	if err != nil {
		t.Fatalf("openProbePath: %v", err)
	}
	defer path.Close()
	opener.opened[0].lose(providertunnel.ErrTunnelLost)
	opener.failing = true
	if err := path.Reopen(context.Background()); err == nil {
		t.Fatal("a failed open was reported as a re-open")
	}
	if _, lost := path.Current(); lost.Err() == nil {
		t.Fatal("after a failed re-open the current tunnel reads as live")
	}
	if path.reopens() != 0 {
		t.Errorf("reopens = %d after a failed re-open, want 0", path.reopens())
	}
}

// Every tunnel a probe opens gets
// the served pins cut down to the hosts it dials, so a pin for a host nothing
// dials never widens the allowlist.
func TestProviderTunnelOpenerRestrictsThePins(t *testing.T) {
	config := providertunnel.Config{Pins: map[string][]string{
		"api.example":        {"leaf", "int"},
		"retired.example":    {"leaf", "int"},
		"SITE.example:443":   {"leaf2", "int2"},
		"not-dialed.example": {"leaf3", "int3"},
	}}
	restricted := restrictPins(config.Pins, []string{"api.example", "site.example"})
	if len(restricted) != 2 || restricted["api.example"] == nil || restricted["SITE.example:443"] == nil {
		t.Fatalf("restricted pins = %v, want exactly the two dialed hosts", restricted)
	}
	if _, ok := restricted["retired.example"]; ok {
		t.Error("a pin for a host the probe does not dial survived")
	}
	// the caller's map is untouched
	if len(config.Pins) != 4 {
		t.Error("restrictPins changed the caller's map")
	}
	// and the opener applies it before Open ever sees the config
	_ = providerTunnelOpener(config, connect.NewId(), []string{"api.example"})
	if len(config.Pins) != 4 {
		t.Error("providerTunnelOpener changed the caller's config")
	}
}
