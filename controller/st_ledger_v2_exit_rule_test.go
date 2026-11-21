package controller

// st_ledger_v2_exit_rule_test.go — the SN25 exit rule in an epoch's provider
// rows (owner decision 2026-11-20, "forward-only rule from epoch 7"). The root
// cause: a proxy slot that connects from a residential proxy while its users'
// traffic exits from a datacenter was paid for its usage like any provider,
// though the probe that judged it found its exit in none of its networks. From
// epoch 7 such a provider earns no usage: its row keeps its bytes with a zero
// coldkey, as a provider without a selected wallet's does, nothing is retained
// for it, and the validator's audit approves the root. Epoch 6 and earlier are
// built as before, and only the durable record's differing judgements in the
// epoch's window count. Judgements run through model.JudgeProviderEgressExit;
// fixtures use RFC 5737 documentation addresses and RFC 5398 documentation
// systems.

import (
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The fixtures' autonomous systems.
const (
	// the residential ISP whose addresses a proxy list resells
	ledgerV2ExitResidentialAsn = uint32(64496)
	// the datacenter a slot's users' traffic exits from
	ledgerV2ExitDatacenterAsn = uint32(64497)
	// an ISP with a carrier NAT
	ledgerV2ExitCarrierAsn = uint32(64498)
)

// Numbers the fixtures' addresses for the rest of the test. Any other address
// has no known system.
func ledgerV2ExitAsns(t testing.TB) {
	t.Helper()
	systems := []struct {
		prefix netip.Prefix
		asn    uint32
	}{
		{prefix: netip.MustParsePrefix("198.51.100.0/24"), asn: ledgerV2ExitResidentialAsn},
		{prefix: netip.MustParsePrefix("203.0.113.0/24"), asn: ledgerV2ExitDatacenterAsn},
		{prefix: netip.MustParsePrefix("192.0.2.0/24"), asn: ledgerV2ExitCarrierAsn},
	}
	t.Cleanup(model.Testing_SetProviderNetworkAsn(func(addr netip.Addr) uint32 {
		for _, system := range systems {
			if system.prefix.Contains(addr.Unmap()) {
				return system.asn
			}
		}
		return 0
	}))
}

// A provider alone in its network, connected from one address, whose network
// consents to a coldkey of its own.
type ledgerV2ExitProvider struct {
	clientId  server.Id
	networkId server.Id
	coldkey   [32]byte
}

// exitProvider connects a provider from ip and retains its network's consent
// to the coldkey of seed. recorded records its network's system, as the
// connect announce does for a probed provider; without it a probe finds no
// evidence. The caller flushes the consents.
func (self *ledgerV2BuildFixture) exitProvider(ip string, seed uint32, recorded bool) *ledgerV2ExitProvider {
	t := self.t
	t.Helper()
	provider := &ledgerV2ExitProvider{clientId: server.NewId(), networkId: server.NewId()}
	model.Testing_CreateDevice(self.ctx, provider.networkId, server.NewId(), provider.clientId, "exit-rule-provider", "test")
	if _, _, _, _, err := model.ConnectNetworkClient(self.ctx, provider.clientId, net.JoinHostPort(ip, "40000"), model.CreateNetworkClientHandler(self.ctx)); err != nil {
		t.Fatal(err)
	}
	if recorded {
		model.SetProviderConnectionNetworkAsn(self.ctx, provider.clientId, ip)
	}
	self.consents.networkChain(t, provider.networkId, seed, 1)
	provider.coldkey = self.consents.public(t, seed)
	return provider
}

// judgeExit judges the provider by the exit a probe saw at observedAt, as the
// probe's ingest does, and returns whether the exit differs.
func (self *ledgerV2BuildFixture) judgeExit(provider *ledgerV2ExitProvider, exitIp string, observedAt time.Time) bool {
	self.t.Helper()
	judged, latest := model.JudgeProviderEgressExit(self.ctx, provider.clientId, netip.MustParseAddr(exitIp),
		model.ProviderEgressExitJudgement{ObservedAt: observedAt}, server.NewId(), time.Minute)
	if !latest {
		self.t.Fatalf("the probe of the exit %s at %s was not the latest", exitIp, observedAt)
	}
	return judged.ExitDiffers
}

func (self *ledgerV2ExitProvider) usage(bytes int64) ledgerV2BuildUsage {
	return ledgerV2BuildUsage{clientId: self.clientId, networkId: self.networkId, bytes: bytes}
}

// From epoch 7, a slot that connects from a residential proxy and whose probe
// saw its users' traffic exit from a datacenter in the epoch's window earns
// nothing: its row keeps its bytes with a zero coldkey, as a provider without
// a selected wallet's does, no resolution is retained for it, the epoch's
// amount goes to the other provider, and the validator's audit approves the
// root. Before the rule the slot was paid for its usage.
func TestStLedgerV2ExitRuleHoldsADifferingSlotFromEpoch7(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		ledgerV2ExitAsns(t)
		slot := f.exitProvider("198.51.100.8", 21, true)
		direct := f.exitProvider("192.0.2.17", 22, true)
		f.consents.flush(t)
		seven := ledgerV2BuildAuthority(7)
		if !f.judgeExit(slot, "203.0.113.9", seven.StartTime.Add(time.Minute)) {
			t.Fatal("the slot's datacenter exit does not differ")
		}
		if f.judgeExit(direct, "192.0.2.17", seven.StartTime.Add(time.Minute)) {
			t.Fatal("the direct provider's own exit differs")
		}
		f.contract(seven.StartTime.Add(2*time.Minute), slot.usage(300), direct.usage(100))

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 7, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(7, 1_000, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		var zero [32]byte
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(ledgerV2Row(slot.clientId, zero, 300, false), ledgerV2Row(direct.clientId, direct.coldkey, 100, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 7 rows %+v\nwant %+v", got, want)
		}
		if want := []merkle.LedgerAccount{ledgerV2Account(direct.coldkey, 1_000)}; !reflect.DeepEqual(build.Accounts, want) {
			t.Fatalf("accounts %v, want the direct provider's alone", build.Accounts)
		}
		if got, want := f.resolutionModes(7), map[server.Id]string{direct.clientId: protocol.EarningWalletModeNetwork}; !reflect.DeepEqual(got, want) {
			t.Fatalf("retained resolutions %v, want %v", got, want)
		}
		if result := f.audit(input, root, ledgerV2Heads{7: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// Epoch 6 is built exactly as before. In one root over epochs 6 and 7, a slot
// whose exit differed in both windows keeps its coldkey, its share and its
// retained resolution in epoch 6, and earns nothing in epoch 7 on the same
// record.
func TestStLedgerV2ExitRuleLeavesEpoch6AsBuilt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		ledgerV2ExitAsns(t)
		slot := f.exitProvider("198.51.100.8", 21, true)
		direct := f.exitProvider("192.0.2.17", 22, true)
		f.consents.flush(t)
		six, seven := ledgerV2BuildAuthority(6), ledgerV2BuildAuthority(7)
		for _, observedAt := range []time.Time{six.StartTime.Add(time.Minute), seven.StartTime.Add(time.Minute)} {
			if !f.judgeExit(slot, "203.0.113.9", observedAt) {
				t.Fatalf("the slot's datacenter exit at %s does not differ", observedAt)
			}
		}
		f.contract(six.StartTime.Add(2*time.Minute), slot.usage(300), direct.usage(100))
		f.contract(seven.StartTime.Add(2*time.Minute), slot.usage(300), direct.usage(100))

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 7, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(6, 4_000, nil), ledgerV2Covered(7, 1_000, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		var zero [32]byte
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(ledgerV2Row(slot.clientId, slot.coldkey, 300, false), ledgerV2Row(direct.clientId, direct.coldkey, 100, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 6 rows %+v\nwant %+v", got, want)
		}
		if got, want := f.rows(build.Epochs[1]), ledgerV2SortedRows(ledgerV2Row(slot.clientId, zero, 300, false), ledgerV2Row(direct.clientId, direct.coldkey, 100, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 7 rows %+v\nwant %+v", got, want)
		}
		want := map[string]uint64{
			ledgerV2AmountKey(6, slot.coldkey):   3_000,
			ledgerV2AmountKey(6, direct.coldkey): 1_000,
			ledgerV2AmountKey(7, direct.coldkey): 1_000,
		}
		if got := ledgerV2Amounts(build.Amounts); !reflect.DeepEqual(got, want) {
			t.Fatalf("amounts %v, want %v", got, want)
		}
		for epoch, modes := range map[uint64]map[server.Id]string{
			6: {slot.clientId: protocol.EarningWalletModeNetwork, direct.clientId: protocol.EarningWalletModeNetwork},
			7: {direct.clientId: protocol.EarningWalletModeNetwork},
		} {
			if got := f.resolutionModes(epoch); !reflect.DeepEqual(got, modes) {
				t.Fatalf("epoch %d retained resolutions %v, want %v", epoch, got, modes)
			}
		}
		if result := f.audit(input, root, ledgerV2Heads{6: {}, 7: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// Only the durable record holds a provider. In epoch 7, a provider no probe
// judged, and one whose networks have no recorded system, so that its
// datacenter exit is no evidence and records nothing, earn as before, beside
// the slot the record holds.
func TestStLedgerV2ExitRuleNeedsARecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		ledgerV2ExitAsns(t)
		slot := f.exitProvider("198.51.100.8", 21, true)
		unjudged := f.exitProvider("198.51.100.40", 22, true)
		unrecorded := f.exitProvider("198.51.100.72", 23, false)
		f.consents.flush(t)
		seven := ledgerV2BuildAuthority(7)
		if !f.judgeExit(slot, "203.0.113.9", seven.StartTime.Add(time.Minute)) {
			t.Fatal("the slot's datacenter exit does not differ")
		}
		if f.judgeExit(unrecorded, "203.0.113.10", seven.StartTime.Add(time.Minute)) {
			t.Fatal("an exit compared with no recorded system differs")
		}
		f.contract(seven.StartTime.Add(2*time.Minute), slot.usage(300), unjudged.usage(200), unrecorded.usage(100))

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 7, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(7, 900, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		var zero [32]byte
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(ledgerV2Row(slot.clientId, zero, 300, false),
			ledgerV2Row(unjudged.clientId, unjudged.coldkey, 200, false), ledgerV2Row(unrecorded.clientId, unrecorded.coldkey, 100, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 7 rows %+v\nwant %+v", got, want)
		}
		want := map[string]uint64{ledgerV2AmountKey(7, unjudged.coldkey): 600, ledgerV2AmountKey(7, unrecorded.coldkey): 300}
		if got := ledgerV2Amounts(build.Amounts); !reflect.DeepEqual(got, want) {
			t.Fatalf("amounts %v, want %v", got, want)
		}
		if result := f.audit(input, root, ledgerV2Heads{7: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// A probe that finds a provider's exit in its own networks records nothing,
// so it holds nothing in epoch 7: a direct provider's own address, another
// address of its /29, and a carrier NAT's other network of its ISP earn as
// before. Neither does a differing judgement outside the window: one a second
// before epoch 7's start, and one at its end, which is epoch 8's. One at its
// start is in it.
func TestStLedgerV2ExitRuleCountsOnlyDifferingJudgementsInTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		ledgerV2ExitAsns(t)
		slot := f.exitProvider("198.51.100.8", 21, true)
		direct := f.exitProvider("192.0.2.17", 22, true)
		neighbor := f.exitProvider("192.0.2.33", 23, true)
		carrier := f.exitProvider("192.0.2.49", 24, true)
		before := f.exitProvider("198.51.100.40", 25, true)
		after := f.exitProvider("198.51.100.72", 26, true)
		f.consents.flush(t)
		seven := ledgerV2BuildAuthority(7)
		for _, c := range []struct {
			name       string
			provider   *ledgerV2ExitProvider
			exitIp     string
			observedAt time.Time
			differs    bool
		}{
			{name: "the slot's datacenter exit at the start", provider: slot, exitIp: "203.0.113.9", observedAt: seven.StartTime, differs: true},
			{name: "the direct provider's own exit", provider: direct, exitIp: "192.0.2.17", observedAt: seven.StartTime.Add(time.Minute), differs: false},
			{name: "an exit in the provider's own /29", provider: neighbor, exitIp: "192.0.2.38", observedAt: seven.StartTime.Add(time.Minute), differs: false},
			{name: "a carrier NAT's other network", provider: carrier, exitIp: "192.0.2.70", observedAt: seven.StartTime.Add(time.Minute), differs: false},
			{name: "a datacenter exit before the start", provider: before, exitIp: "203.0.113.10", observedAt: seven.StartTime.Add(-time.Second), differs: true},
			{name: "a datacenter exit at the end", provider: after, exitIp: "203.0.113.11", observedAt: seven.EndTime, differs: true},
		} {
			if differs := f.judgeExit(c.provider, c.exitIp, c.observedAt); differs != c.differs {
				t.Fatalf("%s: differs %t, want %t", c.name, differs, c.differs)
			}
		}
		f.contract(seven.StartTime.Add(2*time.Minute), slot.usage(600), direct.usage(100), neighbor.usage(100), carrier.usage(100), before.usage(100), after.usage(100))

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 7, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(7, 500, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		var zero [32]byte
		paid := []*ledgerV2ExitProvider{direct, neighbor, carrier, before, after}
		wantRows := []protocol.LedgerUsageRow{ledgerV2Row(slot.clientId, zero, 600, false)}
		wantAmounts := map[string]uint64{}
		wantModes := map[server.Id]string{}
		for _, provider := range paid {
			wantRows = append(wantRows, ledgerV2Row(provider.clientId, provider.coldkey, 100, false))
			wantAmounts[ledgerV2AmountKey(7, provider.coldkey)] = 100
			wantModes[provider.clientId] = protocol.EarningWalletModeNetwork
		}
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(wantRows...); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 7 rows %+v\nwant %+v", got, want)
		}
		if got := ledgerV2Amounts(build.Amounts); !reflect.DeepEqual(got, wantAmounts) {
			t.Fatalf("amounts %v, want %v", got, wantAmounts)
		}
		if got := f.resolutionModes(7); !reflect.DeepEqual(got, wantModes) {
			t.Fatalf("retained resolutions %v, want %v", got, wantModes)
		}
		if result := f.audit(input, root, ledgerV2Heads{7: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// The key ledgerV2Amounts gives a coldkey's amount in an epoch.
func ledgerV2AmountKey(epoch uint64, coldkey [32]byte) string {
	return fmt.Sprintf("%d %x", epoch, coldkey[:4])
}
