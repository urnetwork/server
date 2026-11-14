//go:build sidecar_pending

package model

// §15.15's route projection, POST /mail/enable's write and the host key
// removal effect (mail_route_model.go; MAIL-M7-22) on a private database and
// Redis: enabling routes the network within one projection, a lost Redis is
// rebuilt by the pass, a suspension and a released label route nothing, a
// verified direct MX drops the network, and a receiver's removal retires its
// key with its TLSA, twice being one result.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The route name stands at.
func testMailRoute(t testing.TB, ctx context.Context, name string) *MailRoute {
	t.Helper()
	route, err := GetMailRoute(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

// One run of the pass under root, from cursor.
func testMailRoutePass(t testing.TB, ctx context.Context, root string, cursor *MailRouteCursor) *MailRoutePass {
	t.Helper()
	pass, err := ProjectMailRoutesPass(ctx, root, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return pass
}

// Deletes keys, as a lost Redis does.
func testMailRouteLose(ctx context.Context, names ...string) {
	server.Redis(ctx, func(client server.RedisClient) {
		for _, name := range names {
			client.Del(ctx, MailRouteKey(name))
		}
		client.Del(ctx, mailRouteStandingKey)
	})
}

// An SNI's routable part: a label or alias under the root, a scope's two
// labels, and any other host's full name.
func TestMailRouteNameIsTheSnisRoutablePart(t *testing.T) {
	for sni, want := range map[string]string{
		"mx.ab12cd34ef.root.example":     "ab12cd34ef",
		"MX.Acme.Root.Example.":          "acme",
		"mx.eng.acme.root.example":       "eng.acme",
		"mx.customer.example":            "mx.customer.example",
		"mail.acme.root.example":         "mail.acme.root.example",
		"mx.root.example":                "mx.root.example",
		"":                               "",
		"mx.acme.other-root.example":     "mx.acme.other-root.example",
		"mx.acme.root.example.evil.test": "mx.acme.root.example.evil.test",
	} {
		if name := MailRouteName("root.example.", sni); name != want {
			t.Errorf("%q routes by %q, want %q", sni, name, want)
		}
	}
	if MailRouteKey("acme") != "{mxr_acme}" {
		t.Errorf("key %s", MailRouteKey("acme"))
	}
}

// POST /mail/enable's write: the row, the zone's mail records and the route,
// which a router finds at once; enabling again keeps the first time and
// writes nothing; a network that never enabled routes nothing.
func TestMailEnableRoutesTheNetworkWithinOneProjection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		_, otherApex := testDnsNetwork(t, ctx, root)
		otherLabel := strings.TrimSuffix(otherApex, "."+root)

		if status, err := GetMailStatus(ctx, networkId); err != nil || *status != (MailStatus{}) {
			t.Fatalf("status before: %+v %v", status, err)
		}
		mailNetwork, err := EnableMail(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		route := testMailRoute(t, ctx, MailRouteName(root, "mx."+apex))
		if route == nil || route.NetworkId != networkId || route.Host != "mx" || route.Flags != 0 || len(route.ReceiverSidecarIds) != 0 {
			t.Fatalf("route %+v", route)
		}
		if testMailRoute(t, ctx, otherLabel) != nil {
			t.Fatal("a network that never enabled mail routes")
		}
		mx := false
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getDnsZoneRowsInTx(ctx, conn, root, networkId) {
				mx = mx || row.set.Owner == apex && row.set.Type == dns.TypeMX && slices.Contains(row.set.Rdata, "10 mx."+apex)
			}
		})
		if !mx {
			t.Error("the zone names no mail host after the enable")
		}
		if status, err := GetMailStatus(ctx, networkId); err != nil || *status != (MailStatus{Enabled: true}) {
			t.Fatalf("status after: %+v %v", status, err)
		}

		state, err := GetDnsRootState(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		again, err := EnableMail(ctx, networkId, root)
		if err != nil || !again.CreateTime.Equal(mailNetwork.CreateTime) {
			t.Fatalf("enabling again: %+v %v", again, err)
		}
		if after, err := GetDnsRootState(ctx, root); err != nil || after.Serial != state.Serial {
			t.Fatalf("enabling again wrote records: %v", err)
		}
	})
}

// A suspended zone refuses the enable whole: no row, so mail is never on in
// one table and off in the other.
func TestMailEnableOnASuspendedZoneWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := EnsureNetworkZone(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		if err := SuspendZone(ctx, networkId, "review", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := EnableMail(ctx, networkId, root); !errors.Is(err, ErrDnsZoneSuspended) {
			t.Fatalf("enable on a suspended zone: %v", err)
		}
		if GetMailNetwork(ctx, networkId) != nil || testMailRoute(t, ctx, strings.TrimSuffix(apex, "."+root)) != nil {
			t.Fatal("a refused enable left mail on")
		}
	})
}

// Redis lost, then the pass rebuilds: the route is back within the round,
// which starts again at the first network when the standing mark is gone.
func TestMailRoutePassRebuildsALostRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		labels := []string{}
		networkIds := []server.Id{}
		for range 3 {
			networkId, apex := testDnsNetwork(t, ctx, root)
			if _, err := EnableMail(ctx, networkId, root); err != nil {
				t.Fatal(err)
			}
			networkIds = append(networkIds, networkId)
			labels = append(labels, strings.TrimSuffix(apex, "."+root))
		}
		first := testMailRoutePass(t, ctx, root, nil)
		if !first.Cursor.Started || !first.RoundDone || first.PageNetworkCount < 3 {
			t.Fatalf("the first pass: %+v", first)
		}

		testMailRouteLose(ctx, labels...)
		for _, label := range labels {
			if testMailRoute(t, ctx, label) != nil {
				t.Fatal("a lost key still routes")
			}
		}
		// a round in the middle starts again once the mark is gone
		middle := *first.Cursor
		middle.After = &networkIds[0]
		rebuilt := testMailRoutePass(t, ctx, root, &middle)
		if !rebuilt.Rebuilt || !rebuilt.RoundDone {
			t.Fatalf("the pass after the loss: %+v", rebuilt)
		}
		for index, label := range labels {
			if route := testMailRoute(t, ctx, label); route == nil || route.NetworkId != networkIds[index] {
				t.Fatalf("the pass did not rebuild a route: %+v", route)
			}
		}
		// with the mark standing, a pass with no change projects only its page
		quiet := testMailRoutePass(t, ctx, root, rebuilt.Cursor)
		if quiet.Rebuilt || quiet.ChangedNetworkCount != 0 {
			t.Fatalf("a quiet pass: %+v", quiet)
		}
	})
}

// A suspension removes the route at the next pass, through the label map, and
// its lift puts it back; a withheld mail host routes nothing either.
func TestMailRouteFollowsSuspensions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		if _, err := EnableMail(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		pass := testMailRoutePass(t, ctx, root, nil)

		if err := SuspendZone(ctx, networkId, "review", nil); err != nil {
			t.Fatal(err)
		}
		pass = testMailRoutePass(t, ctx, root, pass.Cursor)
		if pass.ChangedNetworkCount != 1 || testMailRoute(t, ctx, label) != nil {
			t.Fatalf("a suspended network still routes: %+v", pass)
		}
		if err := Restore(ctx, networkId, ""); err != nil {
			t.Fatal(err)
		}
		pass = testMailRoutePass(t, ctx, root, pass.Cursor)
		if testMailRoute(t, ctx, label) == nil {
			t.Fatalf("a restored network does not route: %+v", pass)
		}

		if err := SuspendHost(ctx, networkId, "mx"); err != nil {
			t.Fatal(err)
		}
		pass = testMailRoutePass(t, ctx, root, pass.Cursor)
		if testMailRoute(t, ctx, label) != nil {
			t.Fatalf("a withheld mail host still routes: %+v", pass)
		}
		if err := SuspendHost(ctx, networkId, "www"); err != nil {
			t.Fatal(err)
		}
		if err := Restore(ctx, networkId, "mx"); err != nil {
			t.Fatal(err)
		}
		testMailRoutePass(t, ctx, root, pass.Cursor)
		if testMailRoute(t, ctx, label) == nil {
			t.Fatal("another host's suspension stopped mail")
		}
	})
}

// A released label routes nothing (I-14): a deleted network's names are
// tombstoned, and the next pass deletes their keys.
func TestMailRouteOfAReleasedLabelRoutesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		if _, err := EnableMail(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		pass := testMailRoutePass(t, ctx, root, nil)
		if testMailRoute(t, ctx, label) == nil {
			t.Fatal("no route before the release")
		}
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				if err := DnsWithdrawNetworkInTx(ctx, tx, networkId); err != nil {
					panic(dnsWriteRefusal{err: err})
				}
			}, server.TxReadCommitted)
		})
		if err != nil {
			t.Fatal(err)
		}
		// the mail row still stands: the tombstone alone stops the route
		pass = testMailRoutePass(t, ctx, root, pass.Cursor)
		if pass.ChangedNetworkCount != 1 || testMailRoute(t, ctx, label) != nil {
			t.Fatalf("a tombstoned label still routes: %+v", pass)
		}
	})
}

// A route lists the receivers whose key for mx is published, newest first and
// once each, at most the bound; a scope's key and a retired key list no one.
// A verified direct MX drops the network, and a pending one does not.
func TestMailRouteListsPublishedReceiversAndDropsADirectMx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		if _, err := EnableMail(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		older, newer, retired, scoped := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		spki := func(text string) []byte {
			sum := sha256.Sum256([]byte(text))
			return sum[:]
		}
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, key := range []*MailHostKey{
				{NetworkId: networkId, Host: "mx", ReceiverSidecarId: older, SpkiSha256: spki("older")},
				{NetworkId: networkId, Host: "mx", ReceiverSidecarId: newer, SpkiSha256: spki("newer")},
				{NetworkId: networkId, Host: "mx", ReceiverSidecarId: newer, SpkiSha256: spki("newer, rotated")},
				{NetworkId: networkId, Host: "mx", ReceiverSidecarId: retired, SpkiSha256: spki("retired")},
				{NetworkId: networkId, Host: "mx.eng", ReceiverSidecarId: scoped, SpkiSha256: spki("scoped")},
			} {
				if _, err := UpsertMailHostKeyInTx(ctx, tx, key); err != nil {
					t.Fatal(err)
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE mail_host_key SET publish_time = $2 WHERE network_id = $1 AND receiver_sidecar_id = $3`, networkId, now.Add(-time.Hour), older))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE mail_host_key SET publish_time = $2 WHERE network_id = $1 AND receiver_sidecar_id = $3`, networkId, now.Add(-time.Minute), newer))
			if _, err := RetireMailHostKeysInTx(ctx, tx, networkId, retired); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := ProjectMailRoutes(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		route := testMailRoute(t, ctx, label)
		if route == nil || !slices.Equal(route.ReceiverSidecarIds, []server.Id{newer, older}) {
			t.Fatalf("route %+v, want %s then %s", route, newer, older)
		}

		settings := DefaultMailRouteSettings()
		settings.ReceiverMaxCount = 1
		if _, err := projectMailRoutes(ctx, networkId, root, settings); err != nil {
			t.Fatal(err)
		}
		if route := testMailRoute(t, ctx, label); route == nil || !slices.Equal(route.ReceiverSidecarIds, []server.Id{newer}) {
			t.Fatalf("a bounded route %+v", route)
		}

		address := netip.MustParseAddr("192.0.2.25")
		server.Tx(ctx, func(tx server.PgTx) {
			if err := PutMailDirectMxInTx(ctx, tx, networkId, newer, address); err != nil {
				t.Fatal(err)
			}
		})
		projection, err := ProjectMailRoutes(ctx, networkId, root)
		if err != nil || !slices.Equal(projection.Routed, []string{label}) {
			t.Fatalf("a pending direct MX: %+v %v", projection, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := SetMailDirectMxProbeInTx(ctx, tx, networkId, newer, address, true); err != nil {
				t.Fatal(err)
			}
		})
		projection, err = ProjectMailRoutes(ctx, networkId, root)
		if err != nil || len(projection.Routed) != 0 || !slices.Equal(projection.Removed, []string{label}) || testMailRoute(t, ctx, label) != nil {
			t.Fatalf("a verified direct MX: %+v %v", projection, err)
		}
		if status, err := GetMailStatus(ctx, networkId); err != nil || !status.DirectMx || !status.Enabled {
			t.Fatalf("status with a direct MX: %+v %v", status, err)
		}
	})
}

// A receiver's removal retires its key and deletes its TLSA in one
// transaction, another receiver's staying, and the removal applied again, as
// §19.10's replay does (FS-42), is one result. The removed receiver then
// leaves the route's list, and re-registering after a restore publishes its
// key again (FS-41).
func TestMailHostKeyRemovalEffectIsOneResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		if _, err := EnableMail(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		removed := newTestDnsReceiver(t, ctx, networkId)
		kept := newTestDnsReceiver(t, ctx, networkId)
		publish := func(receiver *testDnsReceiver) {
			if _, err := PublishDnsKeyRecord(ctx, &DnsKeyRecord{Root: root, NetworkId: networkId, ReceiverSidecarId: receiver.sidecarId, Request: &sidecar.DnsKeyRecordRequest{
				Host: "mx", Spki: receiver.spki, Attestation: receiver.attest(t, networkId, root, label, receiver.spki),
			}}); err != nil {
				t.Fatal(err)
			}
		}
		publish(removed)
		publish(kept)
		if _, err := ProjectMailRoutes(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		if route := testMailRoute(t, ctx, label); route == nil || len(route.ReceiverSidecarIds) != 2 {
			t.Fatalf("route before the removal %+v", route)
		}

		removal := &Removal{Subjects: []*RemovalSubject{{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: removed.sidecarId}}, Time: server.NowUtc()}
		apply := func(effect RemovalEffect) {
			err := dnsCatchRefusal(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if err := effect(ctx, tx, removal); err != nil {
						panic(dnsWriteRefusal{err: err})
					}
				}, server.TxReadCommitted)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		// the deactivation's transaction runs both effects, in name order
		both := func(ctx context.Context, tx server.PgTx, removal *Removal) error {
			if err := applyDnsTlsaRemovalEffectInTx(ctx, tx, removal); err != nil {
				return err
			}
			return applyMailHostKeyRemovalEffectInTx(ctx, tx, removal)
		}
		apply(both)
		published := func() []server.Id {
			ids := []server.Id{}
			for _, key := range ListMailHostKeys(ctx, networkId) {
				if key.State == MailHostKeyStatePublished {
					ids = append(ids, key.ReceiverSidecarId)
				}
			}
			return ids
		}
		if ids := published(); !slices.Equal(ids, []server.Id{kept.sidecarId}) {
			t.Fatalf("published after the removal: %v", ids)
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); len(values) != 1 {
			t.Fatalf("tlsa after the removal: %v", values)
		}
		state, err := GetDnsRootState(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		retiredBefore := ListMailHostKeys(ctx, networkId)
		apply(both)
		// the mail effect alone, as it runs where §12's has nothing left
		apply(applyMailHostKeyRemovalEffectInTx)
		after, err := GetDnsRootState(ctx, root)
		if err != nil || after.Serial != state.Serial {
			t.Fatalf("the removal again wrote records: %v", err)
		}
		retiredAfter := ListMailHostKeys(ctx, networkId)
		if len(retiredAfter) != len(retiredBefore) {
			t.Fatalf("%d rows, then %d", len(retiredBefore), len(retiredAfter))
		}
		for index := range retiredAfter {
			if !retiredAfter[index].RetireTime.Equal(retiredBefore[index].RetireTime) || retiredAfter[index].State != retiredBefore[index].State {
				t.Fatalf("the removal again changed a row: %+v then %+v", retiredBefore[index], retiredAfter[index])
			}
		}

		if _, err := ProjectMailRoutes(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		if route := testMailRoute(t, ctx, label); route == nil || !slices.Equal(route.ReceiverSidecarIds, []server.Id{kept.sidecarId}) {
			t.Fatalf("route after the removal %+v", route)
		}

		// a kept receiver's re-registration after restore_required changes nothing
		publish(kept)
		if ids := published(); !slices.Equal(ids, []server.Id{kept.sidecarId}) {
			t.Fatalf("published after a re-registration: %v", ids)
		}
		if after, err := GetDnsRootState(ctx, root); err != nil || after.Serial != state.Serial {
			t.Fatalf("a re-registration wrote records: %v", err)
		}
	})
}

// The effect is registered, so every removal runs it.
func TestMailHostKeyRemovalEffectIsRegistered(t *testing.T) {
	removalEffectsLock.Lock()
	defer removalEffectsLock.Unlock()
	if removalEffects[RemovalEffectMailHostKey] == nil {
		t.Fatal("no mail_host_key removal effect")
	}
}
