//go:build sidecar_pending

package controller

// C13's seals through CreateContract on the private stack (PROTO-M4-02; D-PROTO-19, D-ACL-3, §6.10.1 item 4): the
// subject from the source's stored row whatever the request carries, each mode sealed, a companion and a same-network
// companion normalized to Network carrying the forward contract's seals, the gate's control class sealed, the control
// id's contracts left as they are, and the seals reading nothing past the relationship's one query. Subjects need
// MG2's acl_subject_client_id and the owner and binding rows MG0 and MG1 hold, so these run under -tags
// sidecar_pending, through .stack/server-test.sh. The fixtures are connect_acl_gate_test.go's.

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Registers a provide key for each mode, replacing the client's others.
func sealTestProvide(ctx context.Context, clientId server.Id, provideModes ...model.ProvideMode) {
	secretKeys := map[model.ProvideMode][]byte{}
	for _, provideMode := range provideModes {
		secretKeys[provideMode] = bytes.Repeat([]byte{byte(provideMode) + 1}, 32)
	}
	model.SetProvide(ctx, clientId, secretKeys)
}

// CreateContract for request from source, and the contract it signed, failing t on anything else.
func sealTestCreate(t testing.TB, ctx context.Context, source server.Id, request *protocol.CreateContract) (*protocol.Contract, *protocol.StoredContract) {
	t.Helper()
	if request.TransferByteCount == 0 {
		request.TransferByteCount = 1024 * 1024
	}
	frames, err := CreateContract(ctx, source, request, connect.DefaultContractManagerSettings())
	if err != nil || len(frames) != 1 {
		t.Fatalf("create contract: %d frames, %v", len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	returnConnectControlFrames(frames)
	if err != nil {
		t.Fatal(err)
	}
	result := message.(*protocol.CreateContractResult)
	if result.Error != nil || result.Contract == nil {
		t.Fatalf("create contract %s->%s: %+v", source, server.Id(request.DestinationId), result)
	}
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	return result.Contract, storedContract
}

// Fails t unless storedContract seals subject, network, class and delegation.
func sealTestExpect(t testing.TB, name string, storedContract *protocol.StoredContract, subject *server.Id, networkId server.Id, class protocol.ContractClass, delegation []byte) {
	t.Helper()
	switch {
	case subject == nil && storedContract.SourceSubjectClientId != nil:
		t.Errorf("%s: a subject sealed where the platform knows none: %x", name, storedContract.SourceSubjectClientId)
	case subject != nil && !bytes.Equal(storedContract.SourceSubjectClientId, subject.Bytes()):
		t.Errorf("%s: subject %x, want %s", name, storedContract.SourceSubjectClientId, *subject)
	}
	if !bytes.Equal(storedContract.SourceNetworkId, networkId.Bytes()) {
		t.Errorf("%s: network %x, want %s", name, storedContract.SourceNetworkId, networkId)
	}
	if storedContract.ContractClass != class {
		t.Errorf("%s: class %s, want %s", name, storedContract.ContractClass, class)
	}
	if !bytes.Equal(storedContract.WindowDelegation, delegation) {
		t.Errorf("%s: delegation %x, want %x", name, storedContract.WindowDelegation, delegation)
	}
}

// The subject is the source's stored acl_subject_client_id, never anything the request carries: a window client's
// delegation naming another sidecar, and an unknown field at the seal's own number, change nothing but the
// delegation, which is copied unchanged.
func TestCreateContractSealSubjectFromStoredRow(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, false)
		sa, sb, other := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		window := net.child(a, &a)
		forged := net.sidecarClient(other)

		delegation := bridgeSealTestDelegation(net.networkId, other, window, b)
		request := &protocol.CreateContract{DestinationId: b.Bytes(), WindowDelegation: delegation}
		unknown := protowire.AppendTag(nil, 13, protowire.BytesType)
		unknown = protowire.AppendBytes(unknown, forged.Bytes())
		request.ProtoReflect().SetUnknown(unknown)
		_, storedContract := sealTestCreate(t, ctx, window, request)
		sealTestExpect(t, "a's window client to b", storedContract, &a, net.networkId, protocol.ContractClass_ContractClassData, delegation)

		// a top-level sidecar's own client acts as itself
		_, storedContract = sealTestCreate(t, ctx, a, &protocol.CreateContract{DestinationId: b.Bytes()})
		sealTestExpect(t, "a to b", storedContract, &a, net.networkId, protocol.ContractClass_ContractClassData, nil)

		// a child minted before MG2, with no stored subject, is sealed with none rather than as itself (D-ACL-3)
		unresolved := net.child(net.appClient(true), nil)
		_, storedContract = sealTestCreate(t, ctx, unresolved, &protocol.CreateContract{DestinationId: b.Bytes()})
		sealTestExpect(t, "an unresolved child to b", storedContract, nil, net.networkId, protocol.ContractClass_ContractClassData, nil)
	})
}

// A WindowDelegation's bytes as a window client passes them: synthetic, never verified by the signer.
func bridgeSealTestDelegation(networkId server.Id, subject sidecar.Id, window server.Id, target server.Id) []byte {
	return []byte("synthetic-delegation:" + networkId.String() + ":" + subject.String() + ":" + window.String() + ":" + target.String())
}

// Each mode CreateContract mints today is sealed: Network between two clients of one network, Public across networks
// with its source paying, and the Stream companion answering it, which carries the forward contract's seals.
// FriendsAndFamily waits for ACL-M6-03's rule, and Service for PROTO-M5-04's.
func TestCreateContractSealEachMode(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		x := newAclGateTestNet(t, ctx, states, false)
		p := newAclGateTestNet(t, ctx, states, false)
		dialer, peer := x.client(nil), x.client(nil)
		provider := p.client(nil)
		sealTestProvide(ctx, dialer, model.ProvideModeNetwork, model.ProvideModeStream)
		sealTestProvide(ctx, provider, model.ProvideModePublic, model.ProvideModeStream)

		contract, storedContract := sealTestCreate(t, ctx, dialer, &protocol.CreateContract{DestinationId: peer.Bytes()})
		if contract.ProvideMode != protocol.ProvideMode_Network {
			t.Fatalf("same network: %s", contract.ProvideMode)
		}
		sealTestExpect(t, "network", storedContract, &dialer, x.networkId, protocol.ContractClass_ContractClassData, nil)

		contract, storedContract = sealTestCreate(t, ctx, dialer, &protocol.CreateContract{DestinationId: provider.Bytes()})
		if contract.ProvideMode != protocol.ProvideMode_Public {
			t.Fatalf("across networks: %s", contract.ProvideMode)
		}
		sealTestExpect(t, "public", storedContract, &dialer, x.networkId, protocol.ContractClass_ContractClassData, nil)

		contract, storedContract = sealTestCreate(t, ctx, provider, &protocol.CreateContract{DestinationId: dialer.Bytes(), Companion: true})
		if contract.ProvideMode != protocol.ProvideMode_Stream {
			t.Fatalf("the provider's companion: %s", contract.ProvideMode)
		}
		// the forward contract's seals: its source is the dialer, in the dialer's network
		sealTestExpect(t, "the provider's companion", storedContract, &dialer, x.networkId, protocol.ContractClass_ContractClassData, nil)
	})
}

// A same-network companion is settled as a non-companion Network contract (the network-normalize branch) and still
// carries the forward contract's seals, its delegation aside, which no companion carries.
func TestCreateContractSealCompanionNormalized(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, false)
		sa, sb := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b := net.sidecarClient(sa), net.sidecarClient(sb)
		window := net.child(a, &a)

		delegation := bridgeSealTestDelegation(net.networkId, sa, window, b)
		_, forward := sealTestCreate(t, ctx, window, &protocol.CreateContract{DestinationId: b.Bytes(), WindowDelegation: delegation})
		sealTestExpect(t, "the forward contract", forward, &a, net.networkId, protocol.ContractClass_ContractClassData, delegation)

		contract, companion := sealTestCreate(t, ctx, b, &protocol.CreateContract{DestinationId: window.Bytes(), Companion: true})
		if contract.ProvideMode != protocol.ProvideMode_Network {
			t.Fatalf("a same-network companion: %s", contract.ProvideMode)
		}
		sealTestExpect(t, "the normalized companion", companion, &a, net.networkId, protocol.ContractClass_ContractClassData, nil)
		if !bytes.Equal(companion.SourceSubjectClientId, forward.SourceSubjectClientId) || !bytes.Equal(companion.SourceNetworkId, forward.SourceNetworkId) || companion.ContractClass != forward.ContractClass {
			t.Errorf("the companion's seals differ from its forward contract's")
		}
	})
}

// The gate's class is what is sealed: data on a reach, control under the witness exception (D-PROTO-18).
func TestCreateContractSealGateClass(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		net := newAclGateTestNet(t, ctx, states, true)
		sa, sb, sc := net.view.AddDevice(net.owner), net.view.AddDevice(net.owner), net.view.AddDevice(net.owner)
		a, b, c := net.sidecarClient(sa), net.sidecarClient(sb), net.sidecarClient(sc)
		net.dial(sa, &sidecarprotocol.AclTarget{SidecarId: sb.Bytes()})
		net.install()
		testingAclContractGate(t, ctx, states)

		_, storedContract := sealTestCreate(t, ctx, a, &protocol.CreateContract{DestinationId: b.Bytes()})
		sealTestExpect(t, "a reach", storedContract, &a, net.networkId, protocol.ContractClass_ContractClassData, nil)
		_, storedContract = sealTestCreate(t, ctx, a, &protocol.CreateContract{DestinationId: c.Bytes()})
		sealTestExpect(t, "the witness exception", storedContract, &a, net.networkId, protocol.ContractClass_ContractClassControl, nil)
	})
}

// The seals come from the relationship's one read and the gate's verdict: building them reads no store, so
// CreateContract's path gains no query (I-12's tripwire refuses every acquisition). The control id's contracts carry
// none, and a class the gate did not give is sealed unset, which receivers read as the narrowest.
func TestContractSealsReadNothing(t *testing.T) {
	networkId, otherNetworkId := server.NewId(), server.NewId()
	source, destination := server.NewId(), server.NewId()
	sourceSubject, destinationSubject := server.NewId(), server.NewId()
	details := &model.ProvideRelationshipDetails{
		Mode:                 model.ProvideModePublic,
		SourceNetworkId:      &networkId,
		DestinationNetworkId: &otherNetworkId,
		SourceAcl:            &model.ProvideRelationshipAclEnd{NetworkId: networkId, SubjectClientId: &sourceSubject},
		DestinationAcl:       &model.ProvideRelationshipAclEnd{NetworkId: otherNetworkId, SubjectClientId: &destinationSubject},
	}
	delegation := []byte("synthetic-delegation")

	attempts, stop := server.DenyPostgresForTest(t)
	forward := contractSealsFor(source, destination, details, &protocol.CreateContract{WindowDelegation: delegation}, false, sidecar.AclContractClassData)
	reply := contractSealsFor(source, destination, details, &protocol.CreateContract{Companion: true, WindowDelegation: delegation}, true, sidecar.AclContractClassControl)
	fallback := contractSealsFor(source, destination, details, &protocol.CreateContract{WindowDelegation: delegation}, true, sidecar.AclContractClassData)
	control := contractSealsFor(ControlId, destination, details, &protocol.CreateContract{WindowDelegation: delegation}, false, sidecar.AclContractClassData)
	unclassed := contractSealsFor(source, destination, details, &protocol.CreateContract{}, false, 0)
	stop()
	connect.AssertEqual(t, attempts(), uint64(0))

	connect.AssertEqual(t, *forward.sourceSubjectClientId, sourceSubject)
	connect.AssertEqual(t, *forward.sourceNetworkId, networkId)
	connect.AssertEqual(t, forward.contractClass, protocol.ContractClass_ContractClassData)
	connect.AssertEqual(t, forward.windowDelegation, delegation)
	for name, seals := range map[string]contractSeals{"a companion request": reply, "a stream fallback": fallback} {
		if *seals.sourceSubjectClientId != destinationSubject || *seals.sourceNetworkId != otherNetworkId || seals.windowDelegation != nil {
			t.Errorf("%s: %+v", name, seals)
		}
	}
	connect.AssertEqual(t, reply.contractClass, protocol.ContractClass_ContractClassControl)
	connect.AssertEqual(t, control, contractSeals{})
	connect.AssertEqual(t, unclassed.contractClass, protocol.ContractClass_ContractClassUnset)

	storedContract := &protocol.StoredContract{}
	control.seal(storedContract)
	if storedContract.SourceSubjectClientId != nil || storedContract.SourceNetworkId != nil || storedContract.ContractClass != protocol.ContractClass_ContractClassUnset || storedContract.WindowDelegation != nil {
		t.Errorf("the control id's contract was sealed: %+v", storedContract)
	}
}
