package controller

// The contract gate's step as CreateContract runs it, on a warm state and with no store (§22.5's gate, §8.18's
// regression gate): BenchmarkAclContractGate, budgets.yml's acl-contract-gate row, < 1 us and 0 allocs, and beside it
// BenchmarkAclContractGateNoHead, the one map miss a network with no config repo pays over today's path. The relationship
// query before the step is CreateContract's own, so neither reads it; TestAclContractGateStepAllocatesNothing holds
// the allocations on every merge.

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A heads source answering a fixed set.
type aclGateWarmHeads map[sidecar.Id]uint64

func (self aclGateWarmHeads) Heads(ctx context.Context) (map[sidecar.Id]uint64, error) {
	return self, nil
}

// One warm CreateContract gate step: a network with a head row and a loaded state where a reaches b, the gate
// installed and switched on, and the facts the relationship's query would read for a→b.
type aclGateWarmStep struct {
	gate        *sidecar.AclContractGate
	source      server.Id
	destination server.Id
	request     *protocol.CreateContract
	details     *model.ProvideRelationshipDetails
	settings    *connect.ContractManagerSettings
}

// A warm step with its network headed or not, installed until the test ends.
func newAclGateWarmStep(tb testing.TB, headed bool) *aclGateWarmStep {
	tb.Helper()
	networkId := server.NewId()
	view := sidecartest.NewFakeAclView()
	owner := view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER)
	sa, sb := view.AddDevice(owner), view.AddDevice(owner)
	atom := sidecartest.FakeAclSidecarAtom(sa)
	rows := &sidecarprotocol.AclRows{
		HasDocument: true,
		Atoms:       []*sidecarprotocol.AclAtom{atom},
		Rules: []*sidecarprotocol.AclRule{{
			RuleId:   sidecar.NewId().Bytes(),
			SrcAtoms: [][]byte{atom.Atom},
			Targets:  []*sidecarprotocol.AclTarget{{SidecarId: sb.Bytes()}},
			Actions:  []uint32{uint32(sidecarprotocol.AclAction_ACL_ACTION_DIAL)},
		}},
	}
	states := sidecartest.NewFakeAclContractStates()
	if err := states.Install(sidecar.Id(networkId), rows, view); err != nil {
		tb.Fatal(err)
	}
	heads := aclGateWarmHeads{}
	if headed {
		heads[sidecar.Id(networkId)] = 1
	}
	loads := make(chan error, 1)
	settings := sidecar.DefaultAclContractGateSettings()
	settings.HeadsInterval = 0
	settings.OnHeads = func(networkCount int, err error) {
		loads <- err
	}
	gate := sidecar.NewAclContractGate(context.Background(), states, heads, nil, settings)
	tb.Cleanup(gate.Close)
	if err := <-loads; err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(SetAclContractGate(gate))
	tb.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: true}\n"))

	source, destination := server.NewId(), server.NewId()
	end := func(clientId server.Id, sidecarId sidecar.Id) *model.ProvideRelationshipAclEnd {
		serverSidecarId := server.Id(sidecarId)
		return &model.ProvideRelationshipAclEnd{NetworkId: networkId, SubjectClientId: &clientId, SidecarId: &serverSidecarId, StrictExp: true}
	}
	return &aclGateWarmStep{
		gate:        gate,
		source:      source,
		destination: destination,
		request:     &protocol.CreateContract{DestinationId: destination.Bytes()},
		details: &model.ProvideRelationshipDetails{
			Mode:           model.ProvideModeNetwork,
			SourceAcl:      end(source, sa),
			DestinationAcl: end(destination, sb),
		},
		settings: connect.DefaultContractManagerSettings(),
	}
}

// The step: the switch's read, then the gate on the request's facts.
func (self *aclGateWarmStep) run(ctx context.Context) sidecar.AclContractVerdict {
	gate, config := activeAclContractGate()
	verdict, refusal, err := contractAclGate(ctx, gate, config, nil, self.source, self.destination, self.request, self.details, self.settings)
	if refusal != nil || err != nil {
		return sidecar.AclContractVerdict{}
	}
	return verdict
}

func BenchmarkAclContractGate(b *testing.B) {
	step := newAclGateWarmStep(b, true)
	ctx := context.Background()
	if verdict := step.run(ctx); verdict.Outcome != sidecar.AclContractAllow {
		b.Fatalf("the warm step: %+v", verdict)
	}
	b.ReportAllocs()
	for b.Loop() {
		step.run(ctx)
	}
}

func BenchmarkAclContractGateNoHead(b *testing.B) {
	step := newAclGateWarmStep(b, false)
	ctx := context.Background()
	if verdict := step.run(ctx); verdict.Outcome != sidecar.AclContractSkip {
		b.Fatalf("the unheaded step: %+v", verdict)
	}
	b.ReportAllocs()
	for b.Loop() {
		step.run(ctx)
	}
}

// The step allocates nothing, headed or not.
func TestAclContractGateStepAllocatesNothing(t *testing.T) {
	ctx := context.Background()
	for _, headed := range []bool{true, false} {
		step := newAclGateWarmStep(t, headed)
		want := sidecar.AclContractSkip
		if headed {
			want = sidecar.AclContractAllow
		}
		if verdict := step.run(ctx); verdict.Outcome != want {
			t.Fatalf("headed %t: %+v", headed, verdict)
		}
		if allocations := testing.AllocsPerRun(200, func() { step.run(ctx) }); 0 < allocations {
			t.Errorf("headed %t: %v allocations", headed, allocations)
		}
	}
}

// With no gate installed, or the switch off, CreateContract takes today's path.
func TestAclContractGateDarkWithoutItsSwitch(t *testing.T) {
	if gate, _ := activeAclContractGate(); gate != nil {
		t.Fatal("a gate is installed before any test installs one")
	}
	newAclGateWarmStep(t, true)
	if gate, _ := activeAclContractGate(); gate == nil {
		t.Fatal("the installed gate is off with its switch on")
	}
	pop := server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: false}\n")
	defer pop()
	if gate, _ := activeAclContractGate(); gate != nil {
		t.Error("the switch off left the gate on")
	}
	verdict, refusal, err := contractAclGate(context.Background(), nil, nil, nil, server.NewId(), server.NewId(), &protocol.CreateContract{}, &model.ProvideRelationshipDetails{}, connect.DefaultContractManagerSettings())
	if refusal != nil || err != nil || verdict.Outcome != sidecar.AclContractSkip || verdict.Class != sidecar.AclContractClassData {
		t.Errorf("no gate: %+v", verdict)
	}
}
