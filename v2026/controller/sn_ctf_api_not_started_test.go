package controller

// sn_ctf_api_not_started_test.go — capture the flag configured before its pool
// starts: a ctf.yml with every block on as both of its readers take it, and the
// source's view of a pool that vault2 has not registered, or whose first epoch
// is still to come while st.yml names no carry, with the answers from it.
// Nothing here reads a database, Redis or a network.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A ctf.yml with every block on is read whole by both of its readers, each
// taking its own blocks: presence, and the census from epoch 7 with no network
// declared the operator's own (model.CtfDataSettings), and the routes
// (snCtfApiConfigOf).
func TestSnCtfCtfYmlTurnsEveryBlockOn(t *testing.T) {
	pop := server.Vault.PushSimpleResource("ctf.yml", []byte(snCtfTestCtfYml))
	model.Testing_SetCtfDataSettings(nil)
	resetSnCtfApiConfigCache()
	t.Cleanup(func() {
		pop()
		model.Testing_SetCtfDataSettings(nil)
		resetSnCtfApiConfigCache()
	})
	settings := model.CtfDataSettings()
	if !settings.Presence.Enabled || !model.CtfPresenceEnabled() || settings.PresenceRetentionEpochs() != 4 {
		t.Fatalf("presence %+v", settings.Presence)
	}
	if !settings.Census.Enabled || settings.Census.FirstEpoch != 7 || len(settings.OwnNetworkIds()) != 0 ||
		settings.ObjectRetentionEpochs() != 8 || settings.MemberRetentionEpochs() != 60 {
		t.Fatalf("census %+v", settings.Census)
	}
	if api := snCtfApiConfigOf(); !api.Enabled || api.PageUrl != "https://ur.xyz/ctf" {
		t.Fatalf("api %+v", api)
	}
}

// newSnCtfNotStartedTestSource is the core source over st.yml's v2 identity
// without a ctf block, vault2 answering for the CTF pool, and the clock in
// epoch, whose start is clock.StartTime.
func newSnCtfNotStartedTestSource(t testing.TB, registered bool, firstEpoch uint64, epoch uint64) (source snCtfSource, clock snCtfClock) {
	t.Helper()
	chain := newFakeLedgerV2Chain(&[]string{})
	chain.registered, chain.firstEpoch = registered, firstEpoch
	clock = snCtfClock{Epoch: epoch, StartBlock: 9_499_228, EndBlock: 9_549_627, StartTime: time.Date(2026, 11, 13, 18, 27, 36, 0, time.UTC)}
	clockOf := snCtfClockOf
	SetStConfig(ledgerV2TestConfig())
	SetStLedgerV2Client(chain)
	snCtfClockOf = func(context.Context, *StConfig) (*snCtfClock, error) {
		read := clock
		return &read, nil
	}
	snCtfPoolRegistrations.Clear()
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStLedgerV2Client(nil)
		snCtfClockOf = clockOf
		snCtfPoolRegistrations.Clear()
	})
	source, err := newSnCtfCoreSource()
	if err != nil {
		t.Fatal(err)
	}
	return source, clock
}

// Before the pool starts, the source's view is read, never an error: no root,
// no covered epoch and no pending draw, as of this epoch's start. A client's
// answer from it, and its network's, is the answer before the pool's first
// root, no epochs and no wins, and the routes that describe the pool are
// unavailable. Once the pool's first epoch has begun, a missing carry is a
// driver that is not configured, as before.
func TestSnCtfViewBeforeThePoolStarts(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name       string
		registered bool
		firstEpoch uint64
	}{
		{"vault2 has not registered the pool", false, 0},
		{"registered from epoch 8, in epoch 6, before st.yml names a carry", true, 8},
	} {
		source, clock := newSnCtfNotStartedTestSource(t, c.registered, c.firstEpoch, 6)
		view, err := source.View(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(view.Roots) != 0 || len(view.Epochs) != 0 || view.Pending != nil || !view.AsOf.Equal(clock.StartTime) || view.Clock.Epoch != 6 ||
			view.Pool.NoId != 1 || view.Pool.PoolId != 1<<32+1 || view.Pool.FirstEpoch != c.firstEpoch || view.Pool.CarryContract != (common.Address{}) {
			t.Fatalf("%s: view %+v", c.name, view)
		}
		clientId := server.NewId()
		for _, scope := range []snCtfClientScope{{ClientId: &clientId, NetworkId: server.NewId()}, {NetworkId: server.NewId()}} {
			result, err := snCtfClientAnswer(ctx, source, view, scope, snCtfClientDefaultLimit)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			want := map[string]any{"as_of": "2026-11-13T18:27:36Z", "unit": snCtfTestJson(t, snCtfUnit()), "client_id": nil, "epochs": []any{}, "wins": []any{}}
			if scope.ClientId != nil {
				want["client_id"] = clientId.String()
			}
			requireSnCtfEqual(t, c.name, snCtfTestJson(t, result), want)
		}
		withSnCtfTestSource(t, source)
		requireSnProblem(t, serveSnCtf(SnCtf, "/sn/ctf"), http.StatusServiceUnavailable, SnProblemUnavailable)
		requireSnProblem(t, serveSnCtf(SnCtfTop, "/sn/ctf/top"), http.StatusServiceUnavailable, SnProblemUnavailable)
		requireSnProblem(t, serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfCountry(w, r, "is") }, "/sn/ctf/country/is"), http.StatusServiceUnavailable, SnProblemUnavailable)
		requireSnProblem(t, serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfEpoch(w, r, "8") }, "/sn/ctf/epoch/8"), http.StatusServiceUnavailable, SnProblemUnavailable)
	}

	// the pool's first epoch has begun and st.yml names no carry
	source, _ := newSnCtfNotStartedTestSource(t, true, 6, 6)
	if _, err := source.View(ctx); !errors.Is(err, errSnCtfNotConfigured) {
		t.Fatalf("a started pool without a carry: %v", err)
	}
}
