//go:build sidecar_pending

package model_test

// A Team direct MX's probe outcomes through api's route (QD-1103, owner decision 639; MAIL-M7-23): mxedge's direct-MX
// store, on a real server.PlatformClientSource holding the edge bearer, posts each outcome over HTTP to api's real
// handler, which records it in the test's Postgres and Redis. A call on another class's bearer is refused 403 and
// leaves the row as it was; a failing probe of a pending row leaves it pending; a pass verifies it, points
// mx.<net>.<root> at the receiver's address and drops the network's {mxr_} route; a later failure marks it failing
// and keeps mx, which only DELETE reverts; and a probe of a row a DELETE took between the list and the record is a
// no-op success, mx staying at the routers. Time is a fake clock, which the source's own loops wait on: nothing
// sleeps.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// A bearer of the internal gate's format and its SHA-256 in hex, as the verify file holds it.
func testMailDirectMxRouteBearer(t testing.TB) (string, string) {
	t.Helper()
	raw := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(bearer))
	return bearer, hex.EncodeToString(digest[:])
}

func TestMailDirectMxProbeThroughTheRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := model.Testing_NewMailDirectMxWorld(t, ctx)
		// api's own root, as dns.yml names it
		defer model.Testing_SetDnsUserRoot(world.Root)()
		routers := world.MxRows(t, ctx)
		if _, err := model.NameMailDirectMx(ctx, world.NetworkId, []*model.MailDirectMxReceiver{{ReceiverSidecarId: world.First, Address: world.FirstAddress}}); err != nil {
			t.Fatal(err)
		}
		if _, err := model.ProjectMailRoutes(ctx, world.NetworkId, world.Root); err != nil {
			t.Fatal(err)
		}
		label := model.GetNetworkLabel(ctx, world.NetworkId, world.Root)

		// api's edge class routes on a verify file of the edge and mxedge bearers, with no edge.yml, so the sources' own
		// mints answer 503 and their loops wait on the fake clock
		edge, edgeDigest := testMailDirectMxRouteBearer(t)
		mxedge, mxedgeDigest := testMailDirectMxRouteBearer(t)
		verifier, err := server.NewInternalGateVerifier([]byte(
			"edge: {class: edge, sha256: [" + edgeDigest + "]}\nmxedge: {class: mxedge, sha256: [" + mxedgeDigest + "]}\n",
		))
		if err != nil {
			t.Fatal(err)
		}
		apiHandlers := handlers.NewInternalGateEdgeClientHandlers(&handlers.InternalGateEdgeClientDeps{
			Verifier: verifier,
			Config: func() (*server.EdgeClientsConfig, error) {
				return nil, server.ErrEdgeClientRefused
			},
		})
		api := httptest.NewServer(router.NewRouter(ctx, apiHandlers.Routes()))
		defer api.Close()

		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		// mxedge's store as the router's block makes it, on a source holding bearer
		newStore := func(bearer string) mxedgeservice.MxDirectMxStore {
			settings := server.DefaultPlatformClientSourceSettings()
			settings.ClientCount = 1
			source, err := server.NewPlatformClientSource(ctx, &server.PlatformClientSourceArgs{
				ApiUrl:   api.URL,
				Block:    "mxedge",
				Host:     "lb-a.example",
				Bearer:   bearer,
				Clock:    clock,
				Settings: settings,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(source.Close)
			return model.NewMxDirectMxStore(world.Root, source)
		}
		store := newStore(edge)
		listed := func() *mxedgeservice.MxDirectMxRow {
			t.Helper()
			rows, err := store.ListDirectMx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if server.Id(row.NetworkId) == world.NetworkId && server.Id(row.ReceiverSidecarId) == world.First {
					return row
				}
			}
			t.Fatal("the prober lists no row of the receiver")
			return nil
		}
		state := func() (model.MailDirectMxState, bool) {
			t.Helper()
			rows := model.ListMailDirectMx(ctx, world.NetworkId)
			if len(rows) != 1 {
				t.Fatalf("the network holds %d rows", len(rows))
			}
			return rows[0].State, !rows[0].VerifyTime.IsZero()
		}
		routeStands := func() bool {
			t.Helper()
			route, err := model.GetMailRoute(ctx, label)
			if err != nil {
				t.Fatal(err)
			}
			return route != nil
		}
		row := listed()

		if err := newStore(mxedge).RecordDirectMxProbe(ctx, row, true); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("mxedge's bearer recorded: %v", err)
		}
		if state, verified := state(); state != model.MailDirectMxStatePending || verified || world.MxRows(t, ctx)[dns.TypeA] != routers[dns.TypeA] || !routeStands() {
			t.Fatalf("a refused call changed the row or mx: %s, %t", state, verified)
		}

		if err := store.RecordDirectMxProbe(ctx, row, false); err != nil {
			t.Fatal(err)
		}
		if state, verified := state(); state != model.MailDirectMxStatePending || verified || world.MxRows(t, ctx)[dns.TypeA] != routers[dns.TypeA] {
			t.Fatalf("a never-verified row's failure reads %s, %t", state, verified)
		}

		if err := store.RecordDirectMxProbe(ctx, row, true); err != nil {
			t.Fatal(err)
		}
		if state, verified := state(); state != model.MailDirectMxStateVerified || !verified {
			t.Fatalf("a pass reads %s, %t", state, verified)
		}
		if rows := world.MxRows(t, ctx); !strings.Contains(rows[dns.TypeA], world.FirstAddress.String()) {
			t.Fatalf("mx after the pass: %v", rows)
		}
		if routeStands() {
			t.Fatal("a direct-MX network still routes after the pass")
		}

		if err := store.RecordDirectMxProbe(ctx, row, false); err != nil {
			t.Fatal(err)
		}
		if state, _ := state(); state != model.MailDirectMxStateFailing || !strings.Contains(world.MxRows(t, ctx)[dns.TypeA], world.FirstAddress.String()) {
			t.Fatalf("a verified row's failure reads %s, mx %v", state, world.MxRows(t, ctx))
		}

		row = listed()
		if _, err := model.ClearMailDirectMx(ctx, world.NetworkId, world.Root); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordDirectMxProbe(ctx, row, true); err != nil {
			t.Fatalf("a probe of a row DELETE took: %v", err)
		}
		if rows := world.MxRows(t, ctx); rows[dns.TypeA] != routers[dns.TypeA] || 0 < len(model.ListMailDirectMx(ctx, world.NetworkId)) || !routeStands() {
			t.Fatalf("after a probe that raced DELETE, mx %v, routers %v", rows, routers)
		}
	})
}
