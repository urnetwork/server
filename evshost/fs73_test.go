//go:build sidecar_localstack && sidecar_testhooks && !windows

// FS-73 on T3 with stock clients (§22's FS-73, FI-9, FI-16; §22.9's git-lfs row; EVS2-M10-29): evsgit's FS-73 clauses
// (sidecartest/evsgit/fs73.go) on one world for each row of §22.9's git-lfs matrix this host holds, its evs
// server/cli/evs's block (evshost.NewHost, through sidecart3's evs tier) over a T3 Net's stores, the private Redis and the
// local stack's MinIO, its devices presenting service tokens server's oauth mints, the network in the format2 gate, and
// the row's stock git and git-lfs as every device's client. Each clone's git-lfs moves its objects through its
// standalone transfer agent, this test binary relaying to the device's daemon, which serves them from the repo's ur/lfs
// store (evsgit/lfs.go), and every push runs the daemon's receive path and its tip check before evs takes it. The Net's
// clock follows real time until a clause moves it by days (evsgit.FollowRealTime), which evs, every device and each
// commit's dates read; the race clause holds the prune at evs.lfs.prune.remove through the Net's barrier driver.
//
// The clauses share one world in this order, each on devices and refs of its own: store, skip, transports (git's
// http and ssh fronts and git-remote-ur, this test binary as the world's helper dialing the device's daemon in git
// mode), attributes, import, pruned, race, owned, missing, window, and last the twin, the pruned clause
// on a branch of its own with its pushing device's tip check switched off, which the clause must catch. The report
// names exactly what ran and what the host lacks (sidecartest.GitLfsMatrixReport), since a pass on this host's
// versions alone is not §22.9's evidence.
//
// FS-73 runs here as evsgit's clauses, as FS-5 does (fs_git_test.go), not as a builder sidecartest/fault registers:
// stock git's world is built on a test, which a fault.Builder has none of. What it does not run is in
// evsgit/fs73.go's header. The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.

package evshost_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/evsgit"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// What a T3 FS-73 world is built with.
type fs73T3Settings struct {
	// The Net clock's step behind real time.
	ClockFollowInterval time.Duration
	// How long the block may take to read ready.
	ReadyTimeout time.Duration
	// How long the matrix's version reads may take.
	MatrixTimeout time.Duration
}

func defaultFs73T3Settings() *fs73T3Settings {
	return &fs73T3Settings{ClockFollowInterval: 10 * time.Millisecond, ReadyTimeout: time.Minute, MatrixTimeout: time.Minute}
}

// Not a test: git-lfs's transfer agent the world's clones exec, relaying to its device's daemon (evsgit.LfsAgentHelper).
func TestEvsGitLfsAgentHelperProcess(t *testing.T) {
	evsgit.LfsAgentHelper()
}

// Not a test: git-remote-ur as the world's git execs it for a ur:// remote, dialing its device's daemon in git mode
// (evsgit.RemoteHelper).
func TestEvsGitRemoteHelperProcess(t *testing.T) {
	evsgit.RemoteHelper()
}

func TestFs73OnT3(t *testing.T) {
	settings := defaultFs73T3Settings()
	matrixCtx, cancel := context.WithTimeout(t.Context(), settings.MatrixTimeout)
	runs := sidecartest.PlanGitLfsMatrix(matrixCtx, sidecartest.HostGitLfsLookup())
	cancel()
	for _, line := range sidecartest.GitLfsMatrixReport(runs) {
		t.Log(line)
	}
	runnable := sidecartest.GitLfsMatrixRunnable(runs)
	if len(runnable) == 0 {
		t.Skip("no row of §22.9's git-lfs matrix runs on this host: see the report")
	}
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		for _, run := range runnable {
			world, net := newFs73T3World(tb, settings, run)
			tb.Logf("FS-73 on %s: git %s, git-lfs %s", run.Row.Name, run.GitVersion, run.GitLfsVersion)
			evsgit.Fs73StoreAndSmudge(tb, world)
			evsgit.Fs73SkipOptionsRefusedAlike(tb, world)
			evsgit.Fs73OverEachTransport(tb, world)
			evsgit.Fs73AttributesChangeChecksTheObject(tb, world)
			evsgit.Fs73ImportUploadsFirst(tb, world)
			evsgit.Fs73PrunedHistory(tb, world)
			evsgit.Fs73PruneLosesToATouch(tb, world, fs73T3Hold(net))
			evsgit.Fs73OwnedRefHoldsWhenPrunedMeanwhile(tb, world, fs73T3Hold(net))
			evsgit.Fs73PruneRefusesWithARefMissing(tb, world)
			evsgit.Fs73WindowBelowSevenReadsAsSeven(tb, world)
			evsgit.Fs73TipCheckOffTwin(tb, world)
		}
	})
}

// A git world with format 2 and git-lfs on a T3 Net, its clients run's git and git-lfs: see the file header.
func newFs73T3World(tb testing.TB, settings *fs73T3Settings, run *sidecartest.GitLfsMatrixRun) (*evsgit.World, *sidecartest.Net) {
	tb.Helper()
	netSettings := sidecart3.DefaultSettings()
	netSettings.Net.ClockStart = time.Now()
	net := sidecart3.NewNet(tb, netSettings)
	tb.Cleanup(net.Close)
	clock := evsgit.FollowRealTime(tb, net.Clock(), settings.ClockFollowInterval)
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
	worldSettings.ClientGit = run.GitPath
	worldSettings.ClientSettings = evsgit.Fs73ClientSettings()
	worldSettings.Clock = clock
	worldSettings.Format2 = true
	worldSettings.Lfs = &evsgit.WorldLfs{AgentCommand: []string{binary, "-test.run=^TestEvsGitLfsAgentHelperProcess$"}, GitLfs: run.GitLfsPath}
	worldSettings.RemoteHelperCommand = []string{binary, "-test.run=^TestEvsGitRemoteHelperProcess$", "--"}
	worldSettings.Evs = &evsgit.WorldEvs{
		NewHost: func(ctx context.Context, deps *evsgit.WorldEvsDeps) (*evsgit.WorldEvsHost, error) {
			fakes := net.Fakes()
			if err := fakes.Gates.Set(evsgit.WorldGates(deps.NetworkId, true)); err != nil {
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
	return evsgit.NewWorld(tb, worldSettings), net
}

// The race clause's hold on the Net's barrier driver: the site's next arrival, from the prune's goroutine, which no
// actor owns, holds until release.
func fs73T3Hold(net *sidecartest.Net) evsgit.Fs73Hold {
	return func(t testing.TB, name string) (<-chan struct{}, func()) {
		t.Helper()
		driver := net.Barriers()
		if err := driver.Arm(name, "", 0); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(net.Context())
		arrived := make(chan struct{})
		go func() {
			if err := driver.AwaitHeld(ctx, name, ""); err == nil {
				close(arrived)
			}
		}()
		var once sync.Once
		return arrived, func() {
			once.Do(func() {
				cancel()
				if err := driver.Release(name); err != nil {
					t.Logf("%s: %v", name, err)
				}
			})
		}
	}
}
