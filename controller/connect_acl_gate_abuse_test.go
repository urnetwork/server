//go:build sidecar_pending

package controller

// §21's hard deny in CreateContract (ABUSE-M5-07; D-ABUSE-4, I-12, QD-049): a suspended network's sidecar subjects get
// no new contract, either end, while its ordinary VPN clients keep their reach; a snapshot not in force refuses every
// sidecar end and no other; with no snapshot installed nothing is held. The step runs under DenyPostgresForTest and
// makes no query, warm or refusing, with the ACL gate on and off; through CreateContract on the private stack a
// suspension refuses, and its lift, once the snapshot reloads, creates again, the refusal never recorded as the
// route's. MG0, MG1, MG2 and MG5 hold the tables, so these run under -tags sidecar_pending, through
// .stack/server-test.sh.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A hard deny snapshot over a fixed set, or a failure, its one load waited on, installed until the test ends.
func testingAbuseHardDeny(t testing.TB, networkIdStandings map[server.Id]model.AbuseStanding, err error) *model.AbuseHardDenySnapshot {
	t.Helper()
	loads := make(chan error, 1)
	settings := model.DefaultAbuseHardDenySettings()
	settings.Interval = 0
	settings.OnLoad = func(count int, err error) {
		loads <- err
	}
	snapshot := model.Testing_NewAbuseHardDenySnapshot(context.Background(), func(ctx context.Context) (map[server.Id]model.AbuseStanding, error) {
		if err != nil {
			return nil, err
		}
		return networkIdStandings, nil
	}, nil, settings)
	t.Cleanup(snapshot.Close)
	<-loads
	t.Cleanup(SetAbuseHardDeny(snapshot))
	return snapshot
}

// The relationship's facts for source and destination in their networks, each a sidecar's subject or not.
func testingAbuseDetails(sourceNetworkId server.Id, sourceSidecar bool, destinationNetworkId server.Id, destinationSidecar bool) *model.ProvideRelationshipDetails {
	end := func(networkId server.Id, sidecarSubject bool) *model.ProvideRelationshipAclEnd {
		clientId := server.NewId()
		acl := &model.ProvideRelationshipAclEnd{NetworkId: networkId, SubjectClientId: &clientId}
		if sidecarSubject {
			sidecarId := server.NewId()
			acl.SidecarId = &sidecarId
			acl.StrictExp = true
		}
		return acl
	}
	return &model.ProvideRelationshipDetails{
		Mode:                 model.ProvideModeNetwork,
		SourceNetworkId:      &sourceNetworkId,
		DestinationNetworkId: &destinationNetworkId,
		SourceStrictExp:      sourceSidecar,
		DestinationStrictExp: destinationSidecar,
		SourceAcl:            end(sourceNetworkId, sourceSidecar),
		DestinationAcl:       end(destinationNetworkId, destinationSidecar),
	}
}

// One step of CreateContract's gate with no ACL gate installed, its verdict and whether it refused.
func testingAbuseStep(ctx context.Context, details *model.ProvideRelationshipDetails) (sidecar.AclContractVerdict, bool) {
	source, destination := server.NewId(), server.NewId()
	verdict, refusal, err := contractAclGate(ctx, nil, nil, nil, source, destination, &protocol.CreateContract{DestinationId: destination.Bytes()}, details, connect.DefaultContractManagerSettings())
	if err != nil {
		panic(err)
	}
	refused := refusal != nil
	returnConnectControlFrames(refusal)
	return verdict, refused
}

// A suspended network's sidecar ends are refused, source or destination, with no query; its legacy clients and every
// other network's ends pass.
func TestContractAbuseHardDenyRefusesSidecarEnds(t *testing.T) {
	ctx := context.Background()
	suspended, unknown, free := server.NewId(), server.NewId(), server.NewId()
	testingAbuseHardDeny(t, map[server.Id]model.AbuseStanding{
		suspended: model.AbuseStandingSuspended,
		unknown:   model.AbuseStanding("banished"),
	}, nil)

	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	for _, c := range []struct {
		name    string
		details *model.ProvideRelationshipDetails
		refused bool
	}{
		{name: "a suspended sidecar's source", details: testingAbuseDetails(suspended, true, suspended, true), refused: true},
		{name: "a suspended sidecar as destination", details: testingAbuseDetails(free, true, suspended, true), refused: true},
		{name: "a suspended sidecar to a legacy client", details: testingAbuseDetails(suspended, true, suspended, false), refused: true},
		{name: "a state this build does not know", details: testingAbuseDetails(unknown, true, free, true), refused: true},
		{name: "a suspended network's legacy clients", details: testingAbuseDetails(suspended, false, suspended, false), refused: false},
		{name: "a legacy client of a suspended network to another network", details: testingAbuseDetails(suspended, false, free, true), refused: false},
		{name: "networks not held", details: testingAbuseDetails(free, true, free, true), refused: false},
	} {
		verdict, refused := testingAbuseStep(ctx, c.details)
		if refused != c.refused {
			t.Errorf("%s: refused %t, %+v", c.name, refused, verdict)
			continue
		}
		if refused && (verdict.Reason != sidecar.AclContractReasonDenied || verdict.Decision.Reason != sidecar.AclReasonAbuseHalt ||
			contractAclRejectionCause(verdict) != "acl_no_permission") {
			t.Errorf("%s: %+v", c.name, verdict)
		}
	}
	// a sidecar end whose subject the binding names only through its gate facts, with the relationship's own marks
	// unread
	details := testingAbuseDetails(suspended, true, free, false)
	details.SourceNetworkId = nil
	details.SourceStrictExp = false
	details.SourceAcl.StrictExp = false
	if _, refused := testingAbuseStep(ctx, details); !refused {
		t.Error("a bound sidecar read from its gate facts alone passed")
	}
	if 0 < attempts() {
		t.Fatalf("the hard deny reached PostgreSQL %d times", attempts())
	}
}

// A snapshot not in force refuses every sidecar end and no legacy one; with none installed nothing is held.
func TestContractAbuseHardDenyNotLoaded(t *testing.T) {
	ctx := context.Background()
	networkId := server.NewId()
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()

	// none installed: today's path
	if _, refused := testingAbuseStep(ctx, testingAbuseDetails(networkId, true, networkId, true)); refused {
		t.Fatal("refused with no snapshot installed")
	}

	testingAbuseHardDeny(t, nil, errors.New("no store"))
	verdict, refused := testingAbuseStep(ctx, testingAbuseDetails(networkId, true, networkId, true))
	if !refused || verdict.Reason != sidecar.AclContractReasonNotLoaded || contractAclRejectionCause(verdict) != "acl_unavailable" {
		t.Fatalf("a snapshot never loaded: %t %+v", refused, verdict)
	}
	if _, refused := testingAbuseStep(ctx, testingAbuseDetails(networkId, false, networkId, false)); refused {
		t.Fatal("a snapshot never loaded refused legacy clients")
	}
	if 0 < attempts() {
		t.Fatalf("the hard deny reached PostgreSQL %d times", attempts())
	}
}

// With the ACL gate warm and switched on, a network not held still costs no query, and a held one is refused by the
// hard deny before the gate allows it.
func TestContractAbuseHardDenyWarmGateNoQuery(t *testing.T) {
	ctx := context.Background()
	step := newAclGateWarmStep(t, true)
	networkId := step.details.SourceAcl.NetworkId
	snapshot := testingAbuseHardDeny(t, map[server.Id]model.AbuseStanding{}, nil)
	if !snapshot.Ready() {
		t.Fatal("the snapshot is not in force")
	}
	attempts, stop := server.DenyPostgresForTest(t)
	if verdict := step.run(ctx); verdict.Outcome != sidecar.AclContractAllow {
		t.Fatalf("a network not held: %+v", verdict)
	}
	stop()
	if 0 < attempts() {
		t.Fatalf("the warm path reached PostgreSQL %d times", attempts())
	}

	testingAbuseHardDeny(t, map[server.Id]model.AbuseStanding{networkId: model.AbuseStandingSuspended}, nil)
	attempts, stop = server.DenyPostgresForTest(t)
	defer stop()
	gate, config := activeAclContractGate()
	verdict, refusal, err := contractAclGate(ctx, gate, config, nil, step.source, step.destination, step.request, step.details, step.settings)
	if err != nil || refusal == nil || verdict.Reason != sidecar.AclContractReasonDenied {
		t.Fatalf("a held network under the gate: %+v %v", verdict, err)
	}
	returnConnectControlFrames(refusal)
	if 0 < attempts() {
		t.Fatalf("the refusing path reached PostgreSQL %d times", attempts())
	}
}

// Through CreateContract on the private stack: a suspended network's sidecars get no contract, to or from each other or
// its legacy clients, while two legacy clients still contract; once the suspension ends and the snapshot reloads, the
// sidecars contract again, since a hard deny's refusal is never recorded as the route's rejection.
func TestCreateContractAbuseSuspendedNetwork(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, false)
		sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		legacyA, legacyB := net.client(nil), net.client(nil)
		caseId := server.NewId()
		if err := model.SetNetworkAbuseSuspended(ctx, net.networkId, caseId, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		reload := func() {
			loads := make(chan error, 1)
			settings := model.DefaultAbuseHardDenySettings()
			settings.Interval = 0
			settings.OnLoad = func(count int, err error) {
				loads <- err
			}
			snapshot := model.NewAbuseHardDenySnapshot(ctx, settings)
			t.Cleanup(snapshot.Close)
			select {
			case err := <-loads:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("no load")
			}
			t.Cleanup(SetAbuseHardDeny(snapshot))
		}
		reload()

		for _, pair := range [][2]server.Id{{a, b}, {b, a}, {a, legacyA}, {legacyA, a}} {
			if result := testingAclCreateContract(t, ctx, pair[0], pair[1], false); !testingAclContractRefused(result) {
				t.Errorf("%s→%s while suspended: %+v", pair[0], pair[1], result)
			}
		}
		if result := testingAclCreateContract(t, ctx, legacyA, legacyB, false); !testingAclContractCreated(result) {
			t.Errorf("the suspended network's legacy clients: %+v", result)
		}

		if _, err := model.EndNetworkAbuseSuspended(ctx, net.networkId, nil, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		reload()
		if result := testingAclCreateContract(t, ctx, a, b, false); !testingAclContractCreated(result) {
			t.Errorf("a→b after the lift: %+v", result)
		}
	})
}
