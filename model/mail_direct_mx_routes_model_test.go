//go:build sidecar_pending

package model

// A Team's direct MX on a private database and Redis (mail_direct_mx_routes_model.go; MAIL-M7-23, QD-1103):
// NameMailDirectMx's refusals (no Team, mail off, a receiver with no published key, a private address, a third
// receiver); api's record of the prober's outcomes, behind the store's gate, pointing mx at a verified receiver and
// dropping the routes, a failure leaving a pending row pending and a verified one failing with no revert; the record
// refusing a network it does not know and writing nothing for a row that does not stand, a removal or a DELETE that
// came between the prober's list and its record among them; the store's record going through its gate alone, reaching
// no Postgres, a gate's error leaving the row as it was; DELETE taking the rows and the records in one write; a
// receiver's removal dropping its address and re-pointing mx at the other, then at the routers, applied twice as
// once; and the caller's Admin role. mail_direct_mx_probe_route_test.go runs the record through api's route itself.

import (
	"context"
	"crypto/sha256"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
)

// The store's gate as api's route answers it, in process: the route's format check, then api's record in root. The
// HTTP leg between the two is mail_direct_mx_probe_route_test.go's.
type testMailDirectMxApiGate struct {
	root string
}

func (self *testMailDirectMxApiGate) RecordMailDirectMxProbe(ctx context.Context, args *server.MailDirectMxProbeGateArgs) (*server.MailDirectMxProbeGateResult, error) {
	address, err := args.Validate()
	if err != nil {
		return nil, err
	}
	recorded, err := RecordMailDirectMxProbe(ctx, &MailDirectMxProbe{
		NetworkId: args.NetworkId, ReceiverSidecarId: args.ReceiverSidecarId, Address: address, Passed: args.Passed,
	}, self.root)
	if err != nil {
		return nil, err
	}
	return &server.MailDirectMxProbeGateResult{Recorded: recorded}, nil
}

// A gate that keeps what it is asked and answers err, as a route that refused would.
type testMailDirectMxRefusingGate struct {
	err error

	stateLock sync.Mutex
	asked     []*server.MailDirectMxProbeGateArgs
}

func (self *testMailDirectMxRefusingGate) RecordMailDirectMxProbe(ctx context.Context, args *server.MailDirectMxProbeGateArgs) (*server.MailDirectMxProbeGateResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asked = append(self.asked, args)
	if self.err != nil {
		return nil, self.err
	}
	return &server.MailDirectMxProbeGateResult{Recorded: true}, nil
}

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
	rows, err := NewMxDirectMxStore(self.root, nil).ListDirectMx(ctx)
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

		store := NewMxDirectMxStore(world.root, &testMailDirectMxApiGate{root: world.root})
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

// api's record refuses a network it has no row of and writes nothing for a row that does not stand: a network that
// names no direct MX, a receiver it never named, and a named receiver at an address it did not name each answer
// recorded false, the named row staying pending, mx keeping the routers and the routes standing; a probe naming no
// network is refused before anything is read.
func TestMailDirectMxProbeRecordRefusesAnUnknownNetworkOrRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestMailDirectMxWorld(t, ctx)
		defer Testing_SetDnsUserRoot(world.root)()
		routers := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		record := func(networkId server.Id, receiverId server.Id, address netip.Addr) (bool, error) {
			return RecordMailDirectMxProbe(ctx, &MailDirectMxProbe{NetworkId: networkId, ReceiverSidecarId: receiverId, Address: address, Passed: true}, world.root)
		}
		if recorded, err := record(server.NewId(), world.first, testDirectMxFirst); recorded || !errors.Is(err, ErrMailDirectMxProbeNetworkUnknown) {
			t.Fatalf("an unknown network: %t, %v", recorded, err)
		}
		if recorded, err := record(world.networkId, world.first, testDirectMxFirst); recorded || err != nil {
			t.Fatalf("a network naming no direct MX: %t, %v", recorded, err)
		}
		if _, err := NameMailDirectMx(ctx, world.networkId, []*MailDirectMxReceiver{{ReceiverSidecarId: world.first, Address: testDirectMxFirst}}); err != nil {
			t.Fatal(err)
		}
		if _, err := ProjectMailRoutes(ctx, world.networkId, world.root); err != nil {
			t.Fatal(err)
		}
		for name, probe := range map[string]*MailDirectMxProbe{
			"a receiver never named":          {ReceiverSidecarId: world.second, Address: testDirectMxSecond},
			"a receiver with no key":          {ReceiverSidecarId: server.NewId(), Address: testDirectMxFirst},
			"the receiver at another address": {ReceiverSidecarId: world.first, Address: testDirectMxSecond},
		} {
			if recorded, err := record(world.networkId, probe.ReceiverSidecarId, probe.Address); recorded || err != nil {
				t.Errorf("%s: %t, %v", name, recorded, err)
			}
		}
		if rows := ListMailDirectMx(ctx, world.networkId); len(rows) != 1 || rows[0].State != MailDirectMxStatePending || !rows[0].VerifyTime.IsZero() {
			t.Fatalf("a record of no row changed the rows: %+v", rows)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); rows[1] != routers[1] {
			t.Fatalf("a record of no row moved mx: %v", rows)
		}
		if route, err := GetMailRoute(ctx, GetNetworkLabel(ctx, world.networkId, world.root)); err != nil || route == nil {
			t.Fatalf("a record of no row dropped the routes: %v", err)
		}
		if _, err := RecordMailDirectMxProbe(ctx, &MailDirectMxProbe{ReceiverSidecarId: world.first, Address: testDirectMxFirst, Passed: true}, world.root); !errors.Is(err, ErrMailDirectMxInvalid) {
			t.Errorf("a probe naming no network: %v", err)
		}
	})
}

// A receiver's removal, then a DELETE, each between the prober's list and its record: the record finds the row gone,
// answers recorded false and writes nothing, a no-op success for the store, so a probe that raced a removal never
// points mx back at the address the removal took away, and one that raced a DELETE leaves mx at the routers.
func TestMailDirectMxProbeRecordOfARowGoneIsANoOp(t *testing.T) {
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
		gate := &testMailDirectMxApiGate{root: world.root}
		store := NewMxDirectMxStore(world.root, gate)
		if err := store.RecordDirectMxProbe(ctx, world.listed(t, ctx, world.first), true); err != nil {
			t.Fatal(err)
		}
		second := world.listed(t, ctx, world.second)
		removal := &Removal{Subjects: []*RemovalSubject{{NetworkId: world.networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: world.second}}}
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
		before := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		result, err := gate.RecordMailDirectMxProbe(ctx, &server.MailDirectMxProbeGateArgs{
			NetworkId: world.networkId, ReceiverSidecarId: world.second, Address: testDirectMxSecond.String(), Passed: true,
		})
		if err != nil || result.Recorded {
			t.Fatalf("a removed receiver's probe: %+v, %v", result, err)
		}
		if err := store.RecordDirectMxProbe(ctx, second, true); err != nil {
			t.Fatalf("a removed receiver's probe through the store: %v", err)
		}
		after := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		if after[1] != before[1] || strings.Contains(after[1], testDirectMxSecond.String()) || !strings.Contains(after[1], testDirectMxFirst.String()) {
			t.Fatalf("mx after a removed receiver's probe %v, before %v", after, before)
		}
		if rows := ListMailDirectMx(ctx, world.networkId); len(rows) != 1 || rows[0].ReceiverSidecarId != world.first {
			t.Fatalf("the rows after a removed receiver's probe: %+v", rows)
		}

		first := world.listed(t, ctx, world.first)
		if _, err := ClearMailDirectMx(ctx, world.networkId, world.root); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordDirectMxProbe(ctx, first, true); err != nil {
			t.Fatalf("a probe after DELETE: %v", err)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); rows[1] != routers[1] || 0 < len(ListMailDirectMx(ctx, world.networkId)) {
			t.Fatalf("mx after a probe that raced DELETE %v, routers %v", rows, routers)
		}
		if route, err := GetMailRoute(ctx, GetNetworkLabel(ctx, world.networkId, world.root)); err != nil || route == nil {
			t.Fatalf("no route after a probe that raced DELETE: %v", err)
		}
	})
}

// The store's record goes through its gate alone: the gate is asked the row's network, receiver and address and the
// outcome, and nothing reaches Postgres; a gate's error, as a route's refusal reaches it, is the store's error and
// leaves the row and mx as they were; with no gate nothing is recorded.
func TestMxDirectMxStoreRecordsThroughItsGateAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestMailDirectMxWorld(t, ctx)
		defer Testing_SetDnsUserRoot(world.root)()
		routers := testDirectMxRows(t, ctx, world.testDnsRecordsWorld)
		if _, err := NameMailDirectMx(ctx, world.networkId, []*MailDirectMxReceiver{{ReceiverSidecarId: world.first, Address: testDirectMxIpv6}}); err != nil {
			t.Fatal(err)
		}
		listed := world.listed(t, ctx, world.first)
		gate := &testMailDirectMxRefusingGate{}
		store := NewMxDirectMxStore(world.root, gate)

		attempts, stop := server.DenyPostgresForTest(t)
		for _, passed := range []bool{true, false} {
			if err := store.RecordDirectMxProbe(ctx, listed, passed); err != nil {
				t.Fatal(err)
			}
		}
		stop()
		if count := attempts(); count != 0 {
			t.Errorf("the store's record reached Postgres %d times", count)
		}
		asked := func() []*server.MailDirectMxProbeGateArgs {
			gate.stateLock.Lock()
			defer gate.stateLock.Unlock()
			return slices.Clone(gate.asked)
		}()
		if len(asked) != 2 {
			t.Fatalf("the gate was asked %d times", len(asked))
		}
		for index, passed := range []bool{true, false} {
			want := server.MailDirectMxProbeGateArgs{NetworkId: world.networkId, ReceiverSidecarId: world.first, Address: testDirectMxIpv6.String(), Passed: passed}
			if *asked[index] != want {
				t.Errorf("the gate was asked %+v, want %+v", asked[index], want)
			}
		}

		refused := errors.New("api answered 403")
		func() {
			gate.stateLock.Lock()
			defer gate.stateLock.Unlock()
			gate.err = refused
		}()
		if err := store.RecordDirectMxProbe(ctx, listed, true); !errors.Is(err, refused) {
			t.Fatalf("a refused record answered %v", err)
		}
		if rows := ListMailDirectMx(ctx, world.networkId); len(rows) != 1 || rows[0].State != MailDirectMxStatePending || !rows[0].VerifyTime.IsZero() {
			t.Fatalf("a refused record changed the row: %+v", rows)
		}
		if rows := testDirectMxRows(t, ctx, world.testDnsRecordsWorld); !maps.Equal(rows, routers) {
			t.Fatalf("a refused record moved mx: %v, routers %v", rows, routers)
		}
		if err := NewMxDirectMxStore(world.root, nil).RecordDirectMxProbe(ctx, listed, true); !errors.Is(err, ErrMailDirectMxProbeNoGate) {
			t.Errorf("a store with no gate answered %v", err)
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
		store := NewMxDirectMxStore(world.root, &testMailDirectMxApiGate{root: world.root})
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
