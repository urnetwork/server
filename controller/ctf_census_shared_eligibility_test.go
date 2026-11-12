package controller

// Capture the flag's extender members judged by the shared eligibility rules
// (model/extender_eligibility.go): from an epoch's extender records, through
// the judgement both pools read, to the census decision. Each case says what
// the census decided before the rules were shared ("main" below: X4 compared
// each member only with the candidates before it, by its judged record's
// addresses, and X6 read the own networks without the gated tier), and so
// which differences the reconciliation makes; every other case is unchanged.

import (
	"crypto/sha256"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A unit key like §8.4's: the /29 of an IPv4 address, the /48 of an IPv6 one.
func ctfSharedTestUnit(ip netip.Addr) [32]byte {
	bits := 29
	if ip.Is6() {
		bits = 48
	}
	return sha256.Sum256([]byte(netip.PrefixFrom(ip, bits).Masked().String()))
}

// An active record of extender id byte extender for clientId, on the client's
// test network, with its active addresses.
func ctfSharedTestRecord(extender byte, clientId server.Id, addresses ...string) *model.StExtenderCensusRecord {
	record := &model.StExtenderCensusRecord{ExtenderId: server.Id{0x40, extender}, NetworkId: server.Id{0x30, clientId[1]}, ClientId: clientId, Active: true}
	for _, address := range addresses {
		record.Addresses = append(record.Addresses, model.StExtenderCensusAddress{Ip: netip.MustParseAddr(address), Active: true})
	}
	return record
}

// Every reconciled rule and every unchanged one, from records through the
// shared judgement to the census decision, with what main decided beside it.
func TestCtfCensusExtendersUnderTheSharedEligibility(t *testing.T) {
	ids := ctfTestClientIds(20)
	gated := ctfSharedTestRecord(11, ids[11], "192.0.2.111")
	gated.DirectoryTier = connect.ExtenderDirectoryTierGated
	records := []*model.StExtenderCensusRecord{
		// the chain: 1 and 2 share an IPv4 address, 2 and 3 an IPv6 /64
		ctfSharedTestRecord(1, ids[1], "198.51.100.1"),
		ctfSharedTestRecord(2, ids[2], "198.51.100.1", "2001:db8:aa:1::2"),
		ctfSharedTestRecord(3, ids[3], "2001:db8:aa:1::3"),
		// 4 has no wallet and shares its address with 5
		ctfSharedTestRecord(4, ids[4], "198.51.100.4"),
		ctfSharedTestRecord(5, ids[5], "198.51.100.4"),
		// 6 is present too little and shares its address with 7
		ctfSharedTestRecord(6, ids[6], "198.51.100.6"),
		ctfSharedTestRecord(7, ids[7], "198.51.100.6"),
		// 8 is active but never sampled present, so no member, and shares
		// its address with 9
		ctfSharedTestRecord(8, ids[8], "198.51.100.8"),
		ctfSharedTestRecord(9, ids[9], "198.51.100.8"),
		// 10 is on a network that hosts a canary
		ctfSharedTestRecord(10, ids[10], "192.0.2.110"),
		// 11 is in the gated tier (released), on a network nobody declares
		gated,
		// 12 has two records, the second on 13's address
		ctfSharedTestRecord(12, ids[12], "192.0.2.12"),
		ctfSharedTestRecord(32, ids[12], "192.0.2.13"),
		ctfSharedTestRecord(13, ids[13], "192.0.2.13"),
		// 14 shares nothing, 15 is head-bound, 16 has no wallet
		ctfSharedTestRecord(14, ids[14], "203.0.113.14"),
		ctfSharedTestRecord(15, ids[15], "203.0.113.15"),
		ctfSharedTestRecord(16, ids[16], "203.0.113.16"),
	}
	rules := &model.ExtenderEligibilityRules{
		Unit:               ctfSharedTestUnit,
		DurableNetworkIds:  map[server.Id]bool{},
		DeclaredNetworkIds: map[server.Id]bool{},
		CanaryNetworkIds:   map[server.Id]bool{records[9].NetworkId: true},
		ClientIdColdkeys:   map[server.Id][32]byte{},
		HeadBoundClientIds: map[server.Id]bool{ids[15]: true},
	}
	for _, record := range records {
		rules.ClientIdColdkeys[record.ClientId] = ctfTestHash("coldkey " + record.ClientId.String())
	}
	delete(rules.ClientIdColdkeys, ids[4])
	delete(rules.ClientIdColdkeys, ids[16])
	eligibilities, err := model.DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}

	f := newCtfCensusFixture()
	memberClientIds := []server.Id{}
	clientIdBoundaries := map[server.Id][]*model.CtfExtenderBoundary{}
	for _, record := range records {
		if record.ClientId == ids[8] {
			continue
		}
		clientIdBoundaries[record.ClientId] = append(clientIdBoundaries[record.ClientId], &model.CtfExtenderBoundary{ExtenderId: record.ExtenderId, ClientId: record.ClientId, CountryCode: "ir"})
		if len(clientIdBoundaries[record.ClientId]) == 1 {
			memberClientIds = append(memberClientIds, record.ClientId)
			samples := 672
			if record.ClientId == ids[6] {
				samples = 10
			}
			f.present(model.CtfRoleExtender, record.ClientId, "ir", 0, samples)
		}
	}
	f.facts.Extenders = ctfExtenderEnds(eligibilities, clientIdBoundaries, memberClientIds)
	decision := decideCtfCensus(f.facts)

	for _, c := range []struct {
		client     int
		want, main int
	}{
		{1, 0, 0},
		// transitive: 3 joins 1 through 2
		{2, model.CtfReasonDuplicate, model.CtfReasonDuplicate},
		{3, model.CtfReasonDuplicate, 0},
		// a lower record that cannot be paid still holds its address
		{4, model.CtfReasonNoWallet, model.CtfReasonNoWallet},
		{5, model.CtfReasonDuplicate, 0},
		// E3 does not read this census's presence
		{6, model.CtfReasonPresence, model.CtfReasonPresence},
		{7, model.CtfReasonDuplicate, 0},
		// nor its membership
		{9, model.CtfReasonDuplicate, 0},
		// a canary's network is the operator's own, as it was
		{10, model.CtfReasonOwnNetwork, model.CtfReasonOwnNetwork},
		// the gated tier is the operator's own fleet
		{11, model.CtfReasonOwnNetwork, 0},
		// a client's records are one: 13 joins 12 through 12's second record
		{12, 0, 0},
		{13, model.CtfReasonDuplicate, 0},
		{14, 0, 0},
		{15, model.CtfReasonHeadBound, model.CtfReasonHeadBound},
		{16, model.CtfReasonNoWallet, model.CtfReasonNoWallet},
	} {
		member := ctfMember(t, decision, model.CtfRoleExtender, ids[c.client])
		if member.Reason != c.want || member.Candidate != (c.want == 0) {
			t.Errorf("extender client %d: reason %d candidate %v, want reason %d (main: %d)", c.client, member.Reason, member.Candidate, c.want, c.main)
		}
	}
	// a member is judged by its client's lowest active record, whose unit it
	// enters with
	if member := ctfMember(t, decision, model.CtfRoleExtender, ids[12]); member.ExtenderId == nil || *member.ExtenderId != records[11].ExtenderId ||
		member.Unit == nil || *member.Unit != ctfSharedTestUnit(netip.MustParseAddr("192.0.2.12")) || member.UnitFamily != 4 {
		t.Errorf("client 12 is judged by %+v", member)
	}
	// both pools read the same judgement: the extender pool's eligible
	// records are exactly this census's extender candidates' records, less
	// those this census's presence leaves out (6, present too little, and 8,
	// never sampled)
	candidateExtenderIds := map[server.Id]bool{}
	for _, member := range decision.candidates() {
		if member.Role == model.CtfRoleExtender {
			candidateExtenderIds[*member.ExtenderId] = true
		}
	}
	for _, eligibility := range eligibilities {
		if eligibility.Record.ClientId == ids[6] || eligibility.Record.ClientId == ids[8] {
			continue
		}
		if eligibility.Eligible() != candidateExtenderIds[eligibility.Record.ExtenderId] {
			t.Errorf("extender %s: eligible %v in the extender pool, candidate %v here", eligibility.Record.ExtenderId, eligibility.Eligible(), candidateExtenderIds[eligibility.Record.ExtenderId])
		}
	}
}

// A member whose client has no record of the epoch has no facts, so it is
// inactive; a member is judged by its lowest record when none is active.
func TestCtfExtenderEndsWithoutAnActiveRecord(t *testing.T) {
	ids := ctfTestClientIds(3)
	inactive := ctfSharedTestRecord(1, ids[1], "198.51.100.1")
	inactive.Active = false
	rules := &model.ExtenderEligibilityRules{Unit: ctfSharedTestUnit, DurableNetworkIds: map[server.Id]bool{}, DeclaredNetworkIds: map[server.Id]bool{}, ClientIdColdkeys: map[server.Id][32]byte{}, HeadBoundClientIds: map[server.Id]bool{}}
	eligibilities, err := model.DecideExtenderEligibility([]*model.StExtenderCensusRecord{inactive}, rules)
	if err != nil {
		t.Fatal(err)
	}
	clientIdExtenderEnds := ctfExtenderEnds(eligibilities, nil, []server.Id{ids[1], ids[2]})
	if extenderEnd := clientIdExtenderEnds[ids[1]]; extenderEnd == nil || extenderEnd.Active || extenderEnd.Egress != nil || extenderEnd.ExtenderId != inactive.ExtenderId {
		t.Errorf("a client with only an inactive record: %+v", extenderEnd)
	}
	if clientIdExtenderEnds[ids[2]] != nil {
		t.Error("a client without a record has facts")
	}
}
