//go:build sidecar_localstack && sidecar_testhooks && !windows

// FS-5 on T3 with stock clients (§22's FS-5, FI-9; §22.9's git row; EVS1-M2-43): evsgit's world with its evs on
// server/cli/evs's block (evshost.NewHost, through sidecart3's evs tier) over a T3 Net's stores, the private Redis and
// the local stack's MinIO, its devices presenting service tokens server's oauth mints, and stock git as every device's
// client through the local tier. FS-5's clauses (sidecartest/evsgit/fs5.go) run on one world, each on its own branch:
// the same schedules sidecartest's TestEvsGitFs5* tests run on T2. The Net's clock follows real time, since stock git,
// the devices and their queues run on it and evs holds a statement's time_ms to its own clock (5b). The stack must be
// up (sidecar/local/stack.sh), and this fails rather than skips without it.

package evshost_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/evsgit"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// What a T3 git world is built with: the Net clock's step behind real time, and how long the block may take to read
// ready.
type fs5T3Settings struct {
	ClockFollowInterval time.Duration
	ReadyTimeout        time.Duration
	// The files of the clause's push in parts: past PartMaxEntryWriteCount, 1,024 entry writes.
	PartsFileCount int
}

func defaultFs5T3Settings() *fs5T3Settings {
	return &fs5T3Settings{ClockFollowInterval: 10 * time.Millisecond, ReadyTimeout: time.Minute, PartsFileCount: 1100}
}

// Not a test: the hook the world's mounts exec relays to its device's daemon (evsgit.HookHelper).
func TestEvsGitHookHelperProcess(t *testing.T) {
	evsgit.HookHelper()
}

func TestFs5OnT3(t *testing.T) {
	settings := defaultFs5T3Settings()
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		world := newFs5T3World(tb, settings)
		evsgit.Fs5Siblings(tb, world)
		evsgit.Fs5RefusalsAreNoNonFastForward(tb, world)
		evsgit.Fs5PushInParts(tb, world, settings.PartsFileCount)
		evsgit.Fs5AtomicSecondRefConflicts(tb, world)
		evsgit.Fs5LostAnswerReconciles(tb, world)
	})
}

// A git world on a T3 Net: see the file header.
func newFs5T3World(tb testing.TB, settings *fs5T3Settings) *evsgit.World {
	tb.Helper()
	netSettings := sidecart3.DefaultSettings()
	netSettings.Net.ClockStart = time.Now()
	net := sidecart3.NewNet(tb, netSettings)
	tb.Cleanup(net.Close)
	fs5FollowRealTime(tb, net.Clock(), settings.ClockFollowInterval)
	tier := fault.EvsTierOf(net)
	if tier == nil || tier.NewHost == nil || tier.IssueToken == nil {
		tb.Fatal("a T3 Net with no evs tier")
	}
	engineSettings := evsservice.DefaultEvsSettings()
	engineSettings.BatchFlushDelay = 0
	engineSettings.PushUnknownRereadInterval = 0
	engineSettings.RepoCacheTtl = 0
	binary, err := os.Executable()
	if err != nil {
		tb.Fatal(err)
	}
	worldSettings := evsgit.DefaultWorldSettings()
	worldSettings.HookCommand = []string{binary, "-test.run=^TestEvsGitHookHelperProcess$", "--"}
	worldSettings.ClientSettings = evsgit.Fs5ClientSettings()
	worldSettings.Evs = &evsgit.WorldEvs{
		NewHost: func(ctx context.Context, deps *evsgit.WorldEvsDeps) (*evsgit.WorldEvsHost, error) {
			fakes := net.Fakes()
			network := connect.Id(deps.NetworkId).String()
			if err := fakes.Gates.Set(fmt.Sprintf("sidecar:\n  evs: {enabled: true, networks: [%q]}\n", network)); err != nil {
				return nil, err
			}
			fakes.Quota.SetBalance(connect.Id(deps.NetworkId), 1024*1024*1024*1024)
			host := net.Host()
			tierHost, err := tier.NewHost(ctx, host, engineSettings, &fault.EvsTierDeps{
				AuthState: deps.AuthState, GateViews: deps.GateViews, ReceiptKeys: deps.ReceiptKeys,
			})
			if err != nil {
				return nil, err
			}
			tb.Cleanup(tierHost.Close)
			if err := fs5AwaitReady(tierHost, settings.ReadyTimeout); err != nil {
				return nil, err
			}
			cluster, err := host.Blobs.FirstCluster()
			if err != nil {
				return nil, err
			}
			return &evsgit.WorldEvsHost{Handler: tierHost.Handler(), Engine: tierHost.Engine(), Blobs: host.Blobs, Cluster: cluster}, nil
		},
		IssueToken: tier.IssueToken,
	}
	return evsgit.NewWorld(tb, worldSettings)
}

// Keeps the Net's clock at real time until tb ends: see the file header.
func fs5FollowRealTime(tb testing.TB, clock *sidecartest.FakeClock, interval time.Duration) {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if now := time.Now(); clock.Now().Before(now) {
					clock.AdvanceTo(now)
				}
			}
		}
	}()
	tb.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// The block's first probe answered and it reads ready, as warp sends a block nothing until its /status does (§19.4).
// Real time bounds the wait only to fail it.
func fs5AwaitReady(host fault.EvsTierHost, timeout time.Duration) error {
	guard := time.After(timeout)
	for {
		count, changed := host.Engine().Probes()
		if 0 < count && host.Ready() {
			return nil
		}
		select {
		case <-changed:
		case <-guard:
			return errors.New("the evs block never read ready")
		}
	}
}
