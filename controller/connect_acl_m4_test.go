//go:build sidecar_pending

package controller

// ACL-M4-04's M4-out rows through CreateContract on the private stack (§22.5's §8 row, §8.5, §8.10, FS-21's gate
// half; R-ACL-9, D-ACL-19, I-10): §8.5's no-document rules end to end, the implicit document compiled by the binary
// as every writer compiles it, so untagged sidecars and app devices keep today's reach and tagged sidecars only the
// implicit rules; and a document giving the writer's peers no reach to each other, under which they get only the
// witness exception's control contracts, sealed control, never data. Each network's admissions are hand-made
// (sidecartest's fakes); its head row is network_acl_head's. Run under -tags sidecar_pending through
// .stack/server-test.sh.

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The rows a writer compiles for a network with no document: the implicit document over these members and
// admissions, as CompileAcl writes it, has_document false.
func testingAclImplicitRows(t testing.TB, networkId server.Id, members []*bridge.MemberRow, sidecars []*bridge.SidecarRow) *sidecarprotocol.AclRows {
	t.Helper()
	rows, err := sidecar.CompileAcl(&sidecar.AclCompileInput{
		NetworkId:  sidecar.Id(networkId),
		HeadTimeMs: server.NowUtc().UnixMilli(),
		Members:    members,
		Sidecars:   sidecars,
		Statements: &sidecarprotocol.AclRows{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows.GetHasDocument() {
		t.Fatal("the implicit document compiled as a document")
	}
	return rows
}

// A contract the gate passes and its class: data or control, else refused.
func testingAclContractClass(t testing.TB, ctx context.Context, source server.Id, destination server.Id) protocol.ContractClass {
	t.Helper()
	result := testingAclCreateContract(t, ctx, source, destination, false)
	if testingAclContractRefused(result) {
		return protocol.ContractClass_ContractClassUnset
	}
	if !testingAclContractCreated(result) {
		t.Fatalf("%s→%s: %+v", source, destination, result)
	}
	return testingAclStoredContract(t, result).GetContractClass()
}

// The stored contract a created result signs.
func testingAclStoredContract(t testing.TB, result *protocol.CreateContractResult) *protocol.StoredContract {
	t.Helper()
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	return storedContract
}

// §8.5's no-document rules through the gate: untagged sidecars reach every subject and app devices and legacy clients
// keep today's reach, while a tagged sidecar reaches only its own sponsor's subjects (autogroup:self), and another
// member's device only as a witness's control.
func TestCreateContractAclNoDocumentRules(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		other := net.view.AddMember(bridge.MemberRole_MEMBER_ROLE_MEMBER)
		tagId := sidecar.NewId()
		sa, sb, sn := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(other)
		st := net.view.AddAgent(net.owner, tagId)
		members := []*bridge.MemberRow{
			{MemberId: net.owner.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_OWNER},
			{MemberId: other.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER},
		}
		sidecars := []*bridge.SidecarRow{}
		for _, sidecarId := range []sidecar.Id{sa, sb, sn} {
			sidecars = append(sidecars, &bridge.SidecarRow{SidecarId: sidecarId.Bytes(), MemberId: net.view.Sidecar(sidecarId).MemberId.Bytes(), AdmittedAt: net.view.Sidecar(sidecarId).Row.GetAdmittedAt()})
		}
		sidecars = append(sidecars, &bridge.SidecarRow{SidecarId: st.Bytes(), TagIds: [][]byte{tagId.Bytes()}, Sponsor: net.owner.Bytes(), AdmittedAt: net.view.Sidecar(st).Row.GetAdmittedAt()})
		net.rows = testingAclImplicitRows(t, net.networkId, members, sidecars)

		a, b, n, tagged := net.sidecarClient(sa), net.sidecarClient(sb), net.sidecarClient(sn), net.sidecarClient(st)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_owner SET tagged = true WHERE client_id = $1`, tagged))
		})
		app := net.appClient(true)
		otherMemberId := server.Id(other)
		otherApp := net.client(&aclGateTestOwner{memberId: &otherMemberId, aclEnforcing: true})
		legacy := net.client(nil)
		net.install()
		testingAclContractGate(t, ctx, states)

		data, control, refused := protocol.ContractClass_ContractClassData, protocol.ContractClass_ContractClassControl, protocol.ContractClass_ContractClassUnset
		cases := []struct {
			name                string
			source, destination server.Id
			want                protocol.ContractClass
		}{
			// owner decision 105: untagged sidecars and app devices keep today's reach
			{name: "a member device to another's", source: a, destination: n, want: data},
			{name: "a member device to its own", source: a, destination: b, want: data},
			{name: "a member device to an app device", source: a, destination: otherApp, want: data},
			{name: "an app device to a member device", source: app, destination: n, want: data},
			{name: "a legacy client to a member device", source: legacy, destination: a, want: data},
			{name: "a member device to a legacy client", source: a, destination: legacy, want: data},
			// a tagged sidecar: autogroup:self's dial on tcp:443, its sponsor's subjects, ports left to the endpoints
			{name: "a tagged sidecar to its sponsor's device", source: tagged, destination: a, want: data},
			{name: "a tagged sidecar to its sponsor's app device", source: tagged, destination: app, want: data},
			// and nothing else: another member's device only as a witness, another member's app device and a legacy
			// client not at all
			{name: "a tagged sidecar to another member's device", source: tagged, destination: n, want: control},
			{name: "a tagged sidecar to another member's app device", source: tagged, destination: otherApp, want: refused},
			{name: "a tagged sidecar to a legacy client", source: tagged, destination: legacy, want: refused},
		}
		for _, c := range cases {
			if class := testingAclContractClass(t, ctx, c.source, c.destination); class != c.want {
				t.Errorf("%s: %s, want %s", c.name, class, c.want)
			}
		}
	})
}

// FS-21's gate half (D-PROTO-18, §8.10's witness exception): a personal network's document that gives its writer's
// peers no reach to each other leaves every pair of them a control contract, sealed control in both directions and
// for a companion, never data; a revoked peer gets none, and a grant between two of them makes that pair data.
func TestCreateContractAclPeersWithNoReachWitnessOnly(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sb, sc, sr := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		net.view.Revoke(sr)
		a, b, c, revoked := net.sidecarClient(sa), net.sidecarClient(sb), net.sidecarClient(sc), net.sidecarClient(sr)
		// the document: one grant, to an app device, so no rule reaches between the writer's peers
		app := net.appClient(true)
		net.dial(sa, &sidecarprotocol.AclTarget{ClientId: app.Bytes()})
		net.install()
		testingAclContractGate(t, ctx, states)

		control := protocol.ContractClass_ContractClassControl
		for _, pair := range [][2]server.Id{{a, b}, {b, a}, {a, c}, {c, b}} {
			if class := testingAclContractClass(t, ctx, pair[0], pair[1]); class != control {
				t.Errorf("%s→%s: %s, want control", pair[0], pair[1], class)
			}
		}
		companion := testingAclCreateContract(t, ctx, b, a, true)
		if !testingAclContractCreated(companion) {
			t.Fatalf("b's companion to a: %+v", companion)
		}
		if class := testingAclStoredContract(t, companion).GetContractClass(); class != control {
			t.Errorf("b's companion to a: %s, want control", class)
		}
		for _, pair := range [][2]server.Id{{a, revoked}, {revoked, a}} {
			if class := testingAclContractClass(t, ctx, pair[0], pair[1]); class != protocol.ContractClass_ContractClassUnset {
				t.Errorf("a revoked peer %s→%s: %s, want refused", pair[0], pair[1], class)
			}
		}

		// a grant between two of them: that pair is data, the rest stay control
		net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
		net.install()
		if class := testingAclContractClass(t, ctx, a, b); class != protocol.ContractClass_ContractClassData {
			t.Errorf("a→b under a grant: %s", class)
		}
		if class := testingAclContractClass(t, ctx, a, c); class != control {
			t.Errorf("a→c: %s, want control", class)
		}
	})
}
