//go:build sidecar_localstack

// A T3 Net's catch-ups with its clock on its real stores (faults.go's clockCatchUp; QD-1111): writes after an
// Advance past CheckStaleAfter, sent at once or one arriving during the other's reread, are answered on the admin
// info one reread read from the local stack's MinIO; and a Redis call arriving while the store clock ages the Redis
// families waits for that aging, so a key the move passed reads as gone. Like net_test.go these fail rather than skip
// without the stack.

package sidecart3

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A T3 Net whose catch-ups driven sees, on settings (nil for the defaults), and its first cluster with admin info
// read once already, as the first request reads it whatever the clock says.
func newDrivenT3Net(t *testing.T, settings *Settings, driven *drivenCatchUps) (*sidecartest.Net, sidecar.BlobCluster) {
	t.Helper()
	if settings == nil {
		settings = DefaultSettings()
	}
	settings.catchUpHooks = driven.hooks()
	net := NewNet(t, settings)
	cluster, err := net.Host().Blobs.FirstCluster()
	if err != nil {
		net.Close()
		t.Fatal(err)
	}
	if _, err := cluster.PutIfNoneMatch(net.Context(), runKey("catchup/first"), []byte("first"), nil); err != nil {
		net.Close()
		t.Fatalf("the first write read %v", err)
	}
	return net, cluster
}

// A write of the request named on cluster, once gate closes (nil for at once).
func startT3Write(net *sidecartest.Net, cluster sidecar.BlobCluster, gate <-chan struct{}, name string) *pendingCall {
	key := runKey("catchup/" + name + "/")
	return startCall(gate, func() error {
		_, err := cluster.PutIfNoneMatch(withRequestName(net.Context(), name), key, []byte(name), nil)
		return err
	})
}

// Two writes sent at once right after an Advance past CheckStaleAfter are both answered, after one reread of the
// clusters' admin info: before QD-1111 the second went on while the first's reread was in flight and was refused on
// admin info still stale (ErrFileClusterPools). sidecar-t3.sh's suites run it once; the fix's own check runs it
// -count=200 -race.
func TestT3TwoWritesAtOnceAfterAnAdvanceShareOneReread(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		settings := DefaultSettings()
		driven := newDrivenCatchUps("")
		net, cluster := newDrivenT3Net(t, settings, driven)
		defer net.Close()
		rereads := driven.workCount("admin info reread")
		if err := Advance(net.Context(), net, settings.FileClusters.CheckStaleAfter+time.Second); err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		a := startT3Write(net, cluster, gate, "A")
		b := startT3Write(net, cluster, gate, "B")
		close(gate)
		if err := awaitCall(t, net, "A's write", a); err != nil {
			t.Errorf("A read %v", err)
		}
		if err := awaitCall(t, net, "B's write", b); err != nil {
			t.Errorf("B read %v", err)
		}
		if got := driven.workCount("admin info reread") - rereads; got != 1 {
			t.Errorf("the move past CheckStaleAfter took %d rereads", got)
		}
	})
}

// A write arriving while another's reread is in flight after an Advance past CheckStaleAfter waits for it, and is
// answered on the admin info that reread read: before QD-1111 it went on and was refused on admin info the move made
// stale.
func TestT3AWriteDuringARereadIsAnsweredOnWhatItRead(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		settings := DefaultSettings()
		driven := newDrivenCatchUps("A")
		net, cluster := newDrivenT3Net(t, settings, driven)
		defer net.Close()
		// before the Net closes, whatever fails
		defer driven.release.fire()
		rereads := driven.workCount("admin info reread")
		if err := Advance(net.Context(), net, settings.FileClusters.CheckStaleAfter+time.Second); err != nil {
			t.Fatal(err)
		}
		a := startT3Write(net, cluster, nil, "A")
		awaitSignal(t, net, "A's reread", driven.started)
		b := startT3Write(net, cluster, nil, "B")
		awaitWaiting(t, net, driven, "B", b)
		driven.release.fire()
		if err := awaitCall(t, net, "A's write", a); err != nil {
			t.Errorf("A read %v", err)
		}
		if err := awaitCall(t, net, "B's write", b); err != nil {
			t.Errorf("B read %v", err)
		}
		if got := driven.workCount("admin info reread") - rereads; got != 1 {
			t.Errorf("the move past CheckStaleAfter took %d rereads", got)
		}
	})
}

// A Redis call arriving while the store clock ages the Redis families after a move waits for that aging, so a key the
// move passed reads as gone, and the families age once: before, the call went on during the aging and read the key
// still there.
func TestT3ARedisCallDuringTheStoreClocksAgingWaitsForIt(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		tag := runKey("t3catchup_")
		driven := newDrivenCatchUps("A")
		net, _ := newDrivenT3Net(t, storeClockSettings("{"+tag+"}*"), driven)
		defer net.Close()
		defer driven.release.fire()
		redis := net.Host().Redis
		passed := "{" + tag + "}k"
		if err := redis.Set(net.Context(), passed, []byte("v"), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		// a step's own move, as fault's rules make it: the next call ages first
		net.Clock().Advance(11 * time.Minute)
		a := startCall(nil, func() error {
			_, _, err := redis.Get(withRequestName(net.Context(), "A"), "{"+tag+"}other")
			return err
		})
		awaitSignal(t, net, "A's aging", driven.started)
		var found bool
		b := startCall(nil, func() error {
			var err error
			_, found, err = redis.Get(withRequestName(net.Context(), "B"), passed)
			return err
		})
		awaitWaiting(t, net, driven, "B", b)
		driven.release.fire()
		if err := awaitCall(t, net, "A's call", a); err != nil {
			t.Errorf("A read %v", err)
		}
		if err := awaitCall(t, net, "B's call", b); err != nil || found {
			t.Errorf("B read the key the move passed: found %t, %v", found, err)
		}
		if got := driven.workCount("store clock aging"); got != 1 {
			t.Errorf("the move took %d agings", got)
		}
		if errs := StoreClockOf(net).Errors(); len(errs) != 0 {
			t.Errorf("aging failed: %v", errs)
		}
	})
}
