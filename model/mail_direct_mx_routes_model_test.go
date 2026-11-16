//go:build sidecar_pending

package model

// A Team's direct MX on a private database and Redis (mail_direct_mx_routes_model.go; MAIL-M7-23): NameMailDirectMx's
// refusals (no Team, mail off, a receiver with no published key, a private address, a third receiver); the prober's
// store pointing mx at a verified receiver and dropping the routes, a failure leaving a pending row pending and a
// verified one failing with no revert; DELETE taking the rows and the records in one write; a receiver's removal
// dropping its address and re-pointing mx at the other, then at the routers, applied twice as once; and the caller's
// Admin role.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
)

// A team world with mail on and two receivers whose keys for mx are published.
type testMailDirectMxWorld struct {
	*testDnsRecordsWorld
	first  server.Id
	second server.Id
	spki   map[server.Id][]byte
}

func newTestMailDirectMxWorld(t testing.TB, ctx context.Context) *testMailDirectMxWorld {
	t.Helper()
	world := &testMailDirectMxWorld{testDnsRecordsWorld: newTestDirectMxWorld(t, ctx), first: server.NewId(), second: server.NewId(), spki: map[server.Id][]byte{}}
	defer Testing_SetDnsUserRoot(world.root)()
	server.Tx(ctx, func(tx server.PgTx) {
		_, _, err := EnableMailInTx(ctx, tx, world.networkId)
		server.Raise(err)
		for _, receiverId := range []server.Id{world.first, world.second} {
			sum := sha256.Sum256(receiverId.Bytes())
			world.spki[receiverId] = sum[:]
			_, err := UpsertMailHostKeyInTx(ctx, tx, &MailHostKey{
				NetworkId: world.networkId, Host: mailRouteHost, ReceiverSidecarId: receiverId, SpkiSha256: sum[:], State: MailHostKeyStatePublished,
			})
			server.Raise(err)
		}
	})
	return world
}

// The row of receiverId the prober's store lists.
func (self *testMailDirectMxWorld) listed(t testing.TB, ctx context.Context, receiverId server.Id) *mxedgeservice.MxDirectMxRow {
	t.Helper()
	rows, err := NewMxDirectMxStore(self.root).ListDirectMx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if server.Id(row.NetworkId) == self.networkId && server.Id(row.ReceiverSidecarId) == receiverId {
			return row
		}
	}
	return nil
}

// Every refusal writes nothing: a private or repeated address, a third receiver, a receiver with no published key,
// mail off, and a network that is not a team.
func TestMailDirectMxNameRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestMailDirectMxWorld(t, ctx)
		name := func(receivers ...*MailDirectMxReceiver) error {
			_, err := NameMailDirectMx(ctx, world.networkId, receivers)
			return err
		}
		if err := name(&MailDirectMxReceiver{ReceiverSidecarId: world.first, Address: netip.MustParseAddr("10.1.2.3")}); !errors.Is(err, ErrMailDirectMxInvalid) {
			t.Errorf("a private address: %v", err)
		}
		if err := name(&MailDirectMxReceiver{ReceiverSidecarId: server.NewId(), Address: testDirectMxFirst}); !errors.Is(err, ErrMailDirectMxNoReceiver) {
			t.Errorf("a receiver with no published key: %v", err)
		}
		third := &MailDirectMxReceiver{ReceiverSidecarId: server.NewId(), Address: testDirectMxIpv6}
		if err := name(&MailDirectMxReceiver{ReceiverSidecarId: world.first, Address: testDirectMxFirst}, &MailDirectMxReceiver{ReceiverSidecarId: world.second, Address: testDirectMxSecond}, third); !errors.Is(err, ErrMailDirectMxInvalid) {
			t.Errorf("three receivers: %v", err)
		}
		if rows := ListMailDirectMx(ctx, world.networkId); len(rows) != 0 {
			t.Fatalf("a refusal wrote %+v", rows)
		}
		other := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		testDnsSetTeam(t, map[server.Id]bool{world.networkId: true, other.networkId: true})
		if _, err := NameMailDirectMx(ctx, other.networkId, []*MailDirectMxReceiver{{ReceiverSidecarId: world.first, Address: testDirectMxFirst}}); !errors.Is(err, ErrMailDirectMxMailOff) {
			t.Errorf("mail off: %v", err)
		}
		testDnsSetTeam(t, map[server.Id]bool{})
		var quota *QuotaRefusalError
		if err := name(&MailDirectMxReceiver{ReceiverSidecarId: world.first, Address: testDirectMxFirst}); !errors.As(err, &quota) {
			t.Errorf("not a team: %v", err)
		}
	})
}

// A named receiver is pending and points nothing; a failing probe leaves it pending; a pass verifies it, points mx at
// it and drops the network's route; a later failure marks it failing and reverts nothing; DELETE takes it all back.
func TestMailDirectMxProbeVerifiesThenDeleteReverts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestMailDirectMxWorld(t, ctx)
		defer Testing_SetDnsUserRoot(world.root)()
		routers := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		rows, err := NameMailDirectMx(ctx, world.networkId, []*MailDirectMxReceiver{{ReceiverSidecarId: world.first, Address: testDirectMxFirst}})
		if err != nil || len(rows) != 1 || rows[0].State != MailDirectMxStatePending {
			t.Fatalf("named %+v: %v", rows, err)
		}
		if again, err := NameMailDirectMx(ctx, world.networkId, []*MailDirectMxReceiver{{ReceiverSidecarId: world.first, Address: testDirectMxFirst}}); err != nil || len(again) != 1 {
			t.Fatalf("named again %+v: %v", again, err)
		}
		listed := world.listed(t, ctx, world.first)
		label := GetNetworkLabel(ctx, world.networkId, world.root)
		if listed == nil || listed.Host != "mx."+label+"."+world.root || !slices.ContainsFunc(listed.SpkiSha256s, func(spki []byte) bool { return string(spki) == string(world.spki[world.first]) }) ||
			len(listed.HintSidecarIds) != 2 {
			t.Fatalf("the prober's row %+v", listed)
		}
		if _, err := ProjectMailRoutes(ctx, world.networkId, world.root); err != nil {
			t.Fatal(err)
		}
		if route, err := GetMailRoute(ctx, label); err != nil || route == nil {
			t.Fatalf("no route before the verification: %v", err)
		}

		store := NewMxDirectMxStore(world.root)
		if err := store.RecordDirectMxProbe(ctx, listed, false); err != nil {
			t.Fatal(err)
		}
		if state := ListMailDirectMx(ctx, world.networkId)[0].State; state != MailDirectMxStatePending {
			t.Fatalf("a never-verified row failing reads %s", state)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); rows[1] != routers[1] {
			t.Fatalf("a pending row moved mx: %v", rows)
		}
		if err := store.RecordDirectMxProbe(ctx, listed, true); err != nil {
			t.Fatal(err)
		}
		if row := ListMailDirectMx(ctx, world.networkId)[0]; row.State != MailDirectMxStateVerified || row.VerifyTime.IsZero() {
			t.Fatalf("a verified row %+v", row)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); !strings.Contains(rows[1], testDirectMxFirst.String()) {
			t.Fatalf("mx after the verification: %v", rows)
		}
		if route, err := GetMailRoute(ctx, label); err != nil || route != nil {
			t.Fatalf("a direct-MX network still routes: %+v %v", route, err)
		}
		if err := store.RecordDirectMxProbe(ctx, listed, false); err != nil {
			t.Fatal(err)
		}
		if state := ListMailDirectMx(ctx, world.networkId)[0].State; state != MailDirectMxStateFailing {
			t.Fatalf("a failing verified row reads %s", state)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); !strings.Contains(rows[1], testDirectMxFirst.String()) {
			t.Fatalf("a failure reverted mx: %v", rows)
		}

		removed, err := ClearMailDirectMx(ctx, world.networkId, world.root)
		if err != nil || removed != 1 || len(ListMailDirectMx(ctx, world.networkId)) != 0 {
			t.Fatalf("delete: %d, %v", removed, err)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); rows[1] != routers[1] {
			t.Fatalf("mx after delete %v, routers %v", rows, routers)
		}
		if route, err := GetMailRoute(ctx, label); err != nil || route == nil {
			t.Fatalf("no route after delete: %v", err)
		}
	})
}

// A receiver's removal drops its rows and points mx at the other verified receiver, then back at the routers once
// none is left; applied again it changes nothing (I-15).
func TestMailDirectMxRemovalDropsTheReceiversAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestMailDirectMxWorld(t, ctx)
		defer Testing_SetDnsUserRoot(world.root)()
		routers := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		if _, err := NameMailDirectMx(ctx, world.networkId, []*MailDirectMxReceiver{
			{ReceiverSidecarId: world.first, Address: testDirectMxFirst}, {ReceiverSidecarId: world.second, Address: testDirectMxSecond},
		}); err != nil {
			t.Fatal(err)
		}
		store := NewMxDirectMxStore(world.root)
		for _, receiverId := range []server.Id{world.first, world.second} {
			if err := store.RecordDirectMxProbe(ctx, world.listed(t, ctx, receiverId), true); err != nil {
				t.Fatal(err)
			}
		}
		remove := func(receiverId server.Id) {
			t.Helper()
			removal := &Removal{Subjects: []*RemovalSubject{{NetworkId: world.networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: receiverId}}}
			err := dnsCatchRefusal(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if err := applyMailDirectMxRemovalEffectInTx(ctx, tx, removal); err != nil {
						panic(dnsWriteRefusal{err: err})
					}
				}, server.TxReadCommitted)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		remove(world.first)
		rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		if strings.Contains(rows[1], testDirectMxFirst.String()) || !strings.Contains(rows[1], testDirectMxSecond.String()) {
			t.Fatalf("mx after the first removal: %v", rows)
		}
		remove(world.first)
		if again := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); again[1] != rows[1] {
			t.Fatalf("a removal applied again changed mx: %v", again)
		}
		remove(world.second)
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); rows[1] != routers[1] || len(ListMailDirectMx(ctx, world.networkId)) != 0 {
			t.Fatalf("mx after both removals %v, routers %v", rows, routers)
		}
	})
}

// Only a Team's Owner or Admin names a direct MX: a member, a pending admin and a client token are refused.
func TestMailDirectMxCallerIsAnAdmin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _ := newTestSignInTeam(t, ctx, false)
		cases := map[string]bool{"owner": true, "admin": true, "member": false, "pending admin": false}
		sessions := map[string]bool{}
		for name, want := range cases {
			role := NetworkMemberRoleMember
			switch name {
			case "owner":
				role = NetworkMemberRoleOwner
			case "admin", "pending admin":
				role = NetworkMemberRoleAdmin
			}
			clientSession := newTestTeamMemberSession(t, ctx, networkId, role, name == "pending admin")
			sessions[name] = MailDirectMxCallerPermitted(ctx, clientSession)
			if sessions[name] != want {
				t.Errorf("%s: %t", name, sessions[name])
			}
		}
		admin := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAdmin, false)
		clientId := server.NewId()
		admin.ByJwt.ClientId = &clientId
		if MailDirectMxCallerPermitted(ctx, admin) {
			t.Error("an admin's client token is permitted")
		}
	})
}
