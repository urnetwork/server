// The SN25 extender amendment's reading of contract shares (sn WHITEPAPER.md
// §8.6.2, Q7). The pure tests are arithmetic over usage snapshots, so a failure
// names the rule rather than a database, a clock or an ordering accident.
package model

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// The amendment as SN25 sets it, from epoch 8 (§8.6.7).
func stExtenderAmendmentTestPolicy(platformEndpoints ...server.Id) stExtenderAmendment {
	amendment := stExtenderAmendment{FirstEpoch: 8, PlatformEndpoints: map[server.Id]bool{}}
	for _, clientId := range platformEndpoints {
		amendment.PlatformEndpoints[clientId] = true
	}
	return amendment
}

// A plain contract's participants as settlement gathers them: its source is
// the paying origin, its destination the egress, and retained holds the
// stream's intermediaries and the extender parties' provider clients, as
// contract_participant and contract_extender supply them.
func stExtenderTestSnapshot(
	t *testing.T,
	byteCount ByteCount,
	origin ContractParticipant,
	egress ContractParticipant,
	retained ...ContractParticipant,
) (*contractUsageSnapshot, []ContractParticipant) {
	t.Helper()
	contractId := contractPayoutTestId(250)
	participants, originNetworkId, err := contractParticipantsFromRows(contractId, contractParticipantOwner{
		sourceNetworkId: origin.NetworkId, sourceId: origin.ClientId,
		destinationNetworkId: egress.NetworkId, destinationId: egress.ClientId,
		payerNetworkId: &origin.NetworkId,
	}, nil, retained)
	if err != nil {
		t.Fatal(err)
	}
	if originNetworkId != origin.NetworkId {
		t.Fatalf("origin network = %s, want %s", originNetworkId, origin.NetworkId)
	}
	snapshot, err := newContractUsageSnapshot(byteCount, participants)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, participants
}

// Each share of the snapshot goes to exactly one place. A share stays provider
// usage, unchanged, exactly when it is neither a platform endpoint's nor on the
// origin's network; the pool parts are the sums of the others; and everything
// sums to the contract's bytes.
func assertStExtenderSharesEachCountedOnce(
	t *testing.T,
	snapshot *contractUsageSnapshot,
	shares *stContractEpochShares,
	originNetworkId server.Id,
	amendment stExtenderAmendment,
) {
	t.Helper()
	var kept, platform, sameNetwork ByteCount
	for _, share := range snapshot.Providers {
		isPlatform := amendment.PlatformEndpoints[share.ClientId]
		isSameNetwork := !isPlatform && share.NetworkId == originNetworkId
		switch {
		case isPlatform:
			platform += share.ByteCount
		case isSameNetwork:
			sameNetwork += share.ByteCount
		default:
			kept += share.ByteCount
		}
		if slices.Contains(shares.Providers, share) == (isPlatform || isSameNetwork) {
			t.Fatalf("share %+v: kept as provider usage %v, platform endpoint %v, on the origin's network %v",
				share, slices.Contains(shares.Providers, share), isPlatform, isSameNetwork)
		}
	}
	if len(shares.Providers) != len(snapshot.Providers)-countStExtenderPoolShares(snapshot, originNetworkId, amendment) {
		t.Fatalf("provider shares = %+v, from snapshot %+v", shares.Providers, snapshot.Providers)
	}
	if shares.IntraPlatform != platform || shares.IntraNetwork != sameNetwork {
		t.Fatalf("pool parts = %d intra-platform and %d intra-network, want %d and %d",
			shares.IntraPlatform, shares.IntraNetwork, platform, sameNetwork)
	}
	if kept+shares.ExtenderPoolBytes() != snapshot.ByteCount {
		t.Fatalf("provider usage %d and pool bytes %d do not sum to the contract's %d bytes",
			kept, shares.ExtenderPoolBytes(), snapshot.ByteCount)
	}
}

func countStExtenderPoolShares(snapshot *contractUsageSnapshot, originNetworkId server.Id, amendment stExtenderAmendment) int {
	count := 0
	for _, share := range snapshot.Providers {
		if amendment.PlatformEndpoints[share.ClientId] || share.NetworkId == originNetworkId {
			count++
		}
	}
	return count
}

// A contract with k providers and an extender party splits into k+1 equal
// shares, the remainder bytes going to the lowest client ids, and the
// amendment keeps every one of them provider usage, before its first epoch and
// from it (Q7: "extenders count for one of n in payout weighting for
// contracts"). The extender takes the remainder byte when its client id is the
// lowest and none when it is the highest, like any other hop.
func TestStExtenderPartyIsOneOfTheContractsHops(t *testing.T) {
	const byteCount = ByteCount(1_000_000_007)
	origin := ContractParticipant{ClientId: contractPayoutTestId(100), NetworkId: contractPayoutTestId(101)}
	amendment := stExtenderAmendmentTestPolicy()
	for k := 1; k <= 4; k++ {
		for _, extenderClientByte := range []byte{1, 200} {
			name := fmt.Sprintf("k=%d, extender client %d", k, extenderClientByte)
			egress := ContractParticipant{ClientId: contractPayoutTestId(10), NetworkId: contractPayoutTestId(110)}
			retained := []ContractParticipant{}
			for i := 1; i < k; i++ {
				retained = append(retained, ContractParticipant{
					ClientId: contractPayoutTestId(byte(10 + i)), NetworkId: contractPayoutTestId(byte(110 + i)),
				})
			}
			extender := ContractParticipant{ClientId: contractPayoutTestId(extenderClientByte), NetworkId: contractPayoutTestId(240)}
			retained = append(retained, extender)

			snapshot, participants := stExtenderTestSnapshot(t, byteCount, origin, egress, retained...)
			if len(participants) != k+1 || len(snapshot.Providers) != k+1 {
				t.Fatalf("%s: %d participants and %d shares, want %d hops", name, len(participants), len(snapshot.Providers), k+1)
			}
			extenderIndex := -1
			for i, share := range snapshot.Providers {
				want := byteCount / ByteCount(k+1)
				if ByteCount(i) < byteCount%ByteCount(k+1) {
					want++
				}
				if share.ByteCount != want {
					t.Fatalf("%s: hop %d holds %d bytes, want %d of an even %d-way split", name, i, share.ByteCount, want, k+1)
				}
				if share.ClientId == extender.ClientId {
					extenderIndex = i
				}
			}
			switch {
			case extenderIndex < 0:
				t.Fatalf("%s: the extender holds no share: %+v", name, snapshot.Providers)
			case extenderClientByte == 1 && extenderIndex != 0, extenderClientByte == 200 && extenderIndex != k:
				t.Fatalf("%s: the extender's share is at %d, not in its client id order", name, extenderIndex)
			}

			for _, epoch := range []uint64{7, 8, 9} {
				shares, err := stContractSharesForEpoch(snapshot, epoch, origin.NetworkId, amendment)
				if err != nil {
					t.Fatalf("%s, epoch %d: %v", name, epoch, err)
				}
				if !slices.Equal(shares.Providers, snapshot.Providers) || shares.ExtenderPoolBytes() != 0 {
					t.Fatalf("%s, epoch %d: shares = %+v, want every hop's share as provider usage", name, epoch, shares)
				}
			}
		}
	}
}

// Before the amendment's first epoch, and with no amendment in force at all,
// every share is provider usage exactly as the snapshot holds it: a platform
// endpoint's, a same-network client's and an extender's alike, and no origin
// network is needed to read them. From the first epoch the same contract
// reads differently, so the epoch is what changes it.
func TestStExtenderAmendmentChangesNothingBeforeItsFirstEpoch(t *testing.T) {
	const byteCount = ByteCount(1_000_000_003)
	originNetworkId := contractPayoutTestId(101)
	platformEndpoint := contractProviderUsage{ClientId: contractPayoutTestId(1), NetworkId: contractPayoutTestId(160)}
	sameNetwork := contractProviderUsage{ClientId: contractPayoutTestId(2), NetworkId: originNetworkId}
	extender := contractProviderUsage{ClientId: contractPayoutTestId(3), NetworkId: contractPayoutTestId(240)}
	provider := contractProviderUsage{ClientId: contractPayoutTestId(4), NetworkId: contractPayoutTestId(110)}
	snapshot, err := newContractUsageSnapshot(byteCount, []ContractParticipant{
		{ClientId: provider.ClientId, NetworkId: provider.NetworkId},
		{ClientId: extender.ClientId, NetworkId: extender.NetworkId},
		{ClientId: sameNetwork.ClientId, NetworkId: sameNetwork.NetworkId},
		{ClientId: platformEndpoint.ClientId, NetworkId: platformEndpoint.NetworkId},
	})
	if err != nil {
		t.Fatal(err)
	}
	amendment := stExtenderAmendmentTestPolicy(platformEndpoint.ClientId)

	for _, c := range []struct {
		name      string
		amendment stExtenderAmendment
		epoch     uint64
	}{
		{name: "epoch 1", amendment: amendment, epoch: 1},
		{name: "epoch 7, the last before the amendment", amendment: amendment, epoch: 7},
		{name: "no amendment entry, epoch 8", amendment: stExtenderAmendment{PlatformEndpoints: amendment.PlatformEndpoints}, epoch: 8},
		{name: "no amendment entry, epoch 1000", amendment: stExtenderAmendment{}, epoch: 1000},
	} {
		for _, origin := range []server.Id{originNetworkId, {}} {
			shares, err := stContractSharesForEpoch(snapshot, c.epoch, origin, c.amendment)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !slices.Equal(shares.Providers, snapshot.Providers) || shares.IntraPlatform != 0 || shares.IntraNetwork != 0 {
				t.Fatalf("%s: shares = %+v, want the snapshot's %+v unchanged", c.name, shares, snapshot.Providers)
			}
		}
	}

	shares, err := stContractSharesForEpoch(snapshot, 8, originNetworkId, amendment)
	if err != nil {
		t.Fatal(err)
	}
	// four hops of 250,000,001, 250,000,001, 250,000,001 and 250,000,000 bytes
	// in client id order: the platform endpoint, the same-network client, the
	// extender and the provider
	if shares.IntraPlatform != 250_000_001 || shares.IntraNetwork != 250_000_001 ||
		!slices.Equal(shares.Providers, []contractProviderUsage{
			{ClientId: extender.ClientId, NetworkId: extender.NetworkId, ByteCount: 250_000_001},
			{ClientId: provider.ClientId, NetworkId: provider.NetworkId, ByteCount: 250_000_000},
		}) {
		t.Fatalf("epoch 8: shares = %+v", shares)
	}
	assertStExtenderSharesEachCountedOnce(t, snapshot, shares, originNetworkId, amendment)
}

// §8.6.5's c2: a platform contract that an extender carried. The platform
// endpoint stays one of the contract's two hops, so the extender keeps half,
// as J3 gives it, and the platform endpoint's half is intra-platform. The
// contract neither stays whole with its extender, which removing the platform
// endpoint before splitting would do, nor moves whole into the pool. With no
// extender the platform endpoint is the only hop and all of it is
// intra-platform; with an intermediary too, each takes a third.
func TestStExtenderCarriedPlatformContractKeepsTheExtendersShare(t *testing.T) {
	origin := ContractParticipant{ClientId: contractPayoutTestId(100), NetworkId: contractPayoutTestId(101)}
	platformEndpoint := ContractParticipant{ClientId: contractPayoutTestId(5), NetworkId: contractPayoutTestId(160)}
	extender := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	intermediary := ContractParticipant{ClientId: contractPayoutTestId(11), NetworkId: contractPayoutTestId(111)}
	amendment := stExtenderAmendmentTestPolicy(platformEndpoint.ClientId)

	for _, c := range []struct {
		name              string
		byteCount         ByteCount
		retained          []ContractParticipant
		wantProviders     []contractProviderUsage
		wantIntraPlatform ByteCount
	}{
		{
			name:              "through an extender",
			byteCount:         3_000_000_001,
			retained:          []ContractParticipant{extender},
			wantProviders:     []contractProviderUsage{{ClientId: extender.ClientId, NetworkId: extender.NetworkId, ByteCount: 1_500_000_000}},
			wantIntraPlatform: 1_500_000_001,
		},
		{
			name:              "direct",
			byteCount:         3_000_000_001,
			wantProviders:     []contractProviderUsage{},
			wantIntraPlatform: 3_000_000_001,
		},
		{
			name:      "through an intermediary and an extender",
			byteCount: 3_000_000_001,
			retained:  []ContractParticipant{extender, intermediary},
			wantProviders: []contractProviderUsage{
				{ClientId: intermediary.ClientId, NetworkId: intermediary.NetworkId, ByteCount: 1_000_000_000},
				{ClientId: extender.ClientId, NetworkId: extender.NetworkId, ByteCount: 1_000_000_000},
			},
			wantIntraPlatform: 1_000_000_001,
		},
	} {
		snapshot, participants := stExtenderTestSnapshot(t, c.byteCount, origin, platformEndpoint, c.retained...)
		if len(participants) != 1+len(c.retained) || !slices.ContainsFunc(participants, func(p ContractParticipant) bool {
			return p.ClientId == platformEndpoint.ClientId
		}) {
			t.Fatalf("%s: the platform endpoint is not one of the hops %+v", c.name, participants)
		}
		shares, err := stContractSharesForEpoch(snapshot, 8, origin.NetworkId, amendment)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(shares.Providers, c.wantProviders) || shares.IntraPlatform != c.wantIntraPlatform || shares.IntraNetwork != 0 {
			t.Fatalf("%s: shares = %+v, want providers %+v and %d intra-platform bytes",
				c.name, shares, c.wantProviders, c.wantIntraPlatform)
		}
		assertStExtenderSharesEachCountedOnce(t, snapshot, shares, origin.NetworkId, amendment)
	}
}

// §8.6.5's three contracts, then J3's edge cases. Each share goes to one
// place, and over the epoch the provider usage and the pool bytes sum to the
// contracts' bytes, so no byte is credited both to a provider and to Z:
//   - c1, ordinary: 2 providers and an extender, 3 equal shares, none pooled;
//   - c2, platform: the extender keeps half, tlsedge's half is intra-platform;
//   - c3, same-network: the extender keeps half, the receiving client's half is
//     intra-network;
//   - an extender on the origin's network: its share is intra-network;
//   - an extender whose provider client is the egress: one hop, paid once;
//   - a platform endpoint on the origin's network: its share is
//     intra-platform, never counted again as intra-network.
func TestStExtenderPoolNeverCreditsAShareTwice(t *testing.T) {
	p := ContractParticipant{ClientId: contractPayoutTestId(10), NetworkId: contractPayoutTestId(110)}
	q := ContractParticipant{ClientId: contractPayoutTestId(11), NetworkId: contractPayoutTestId(111)}
	x := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	tlsedge := ContractParticipant{ClientId: contractPayoutTestId(5), NetworkId: contractPayoutTestId(160)}
	amendment := stExtenderAmendmentTestPolicy(tlsedge.ClientId)

	type contract struct {
		name   string
		bytes  ByteCount
		origin ContractParticipant
		egress ContractParticipant
		hops   []ContractParticipant
	}
	originOn := func(clientByte byte, networkId server.Id) ContractParticipant {
		return ContractParticipant{ClientId: contractPayoutTestId(clientByte), NetworkId: networkId}
	}
	o3 := contractPayoutTestId(103)
	o4 := contractPayoutTestId(104)
	contracts := []contract{
		{name: "c1", bytes: 3_000_000_002, origin: originOn(201, contractPayoutTestId(102)), egress: p, hops: []ContractParticipant{q, x}},
		{name: "c2", bytes: 3_000_000_001, origin: originOn(202, contractPayoutTestId(102)), egress: tlsedge, hops: []ContractParticipant{x}},
		{name: "c3", bytes: 1_000_000_001, origin: originOn(203, o3), egress: ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: o3}, hops: []ContractParticipant{x}},
		{name: "same-network extender", bytes: 101, origin: originOn(204, o4), egress: p, hops: []ContractParticipant{{ClientId: contractPayoutTestId(20), NetworkId: o4}}},
		{name: "extender client is the egress", bytes: 77, origin: originOn(205, contractPayoutTestId(105)), egress: p, hops: []ContractParticipant{p}},
		{name: "platform endpoint on the origin's network", bytes: 9, origin: originOn(206, tlsedge.NetworkId), egress: tlsedge},
	}

	providerUsage := map[server.Id]ByteCount{}
	var intraPlatform, intraNetwork, total ByteCount
	for _, c := range contracts {
		snapshot, _ := stExtenderTestSnapshot(t, c.bytes, c.origin, c.egress, c.hops...)
		shares, err := stContractSharesForEpoch(snapshot, 8, c.origin.NetworkId, amendment)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		assertStExtenderSharesEachCountedOnce(t, snapshot, shares, c.origin.NetworkId, amendment)
		for _, provider := range shares.Providers {
			providerUsage[provider.ClientId] += provider.ByteCount
		}
		intraPlatform += shares.IntraPlatform
		intraNetwork += shares.IntraNetwork
		total += c.bytes
		if c.name == "c3" && (shares.IntraNetwork != 500_000_001 || !slices.Equal(shares.Providers, []contractProviderUsage{
			{ClientId: x.ClientId, NetworkId: x.NetworkId, ByteCount: 500_000_000},
		})) {
			t.Fatalf("c3: shares = %+v", shares)
		}
	}

	// §8.6.5: p and q 1,000,000,001 each from c1, x 3,000,000,000 from its three
	// hop shares; then p's 51 of 101 and its whole 77, the same-network
	// extender's 50 being intra-network, and tlsedge's 9 intra-platform
	want := map[server.Id]ByteCount{
		p.ClientId: 1_000_000_001 + 51 + 77,
		q.ClientId: 1_000_000_001,
		x.ClientId: 3_000_000_000,
	}
	if len(providerUsage) != len(want) {
		t.Fatalf("provider usage = %v, want %v", providerUsage, want)
	}
	var providerTotal ByteCount
	for clientId, byteCount := range want {
		if providerUsage[clientId] != byteCount {
			t.Fatalf("provider usage of %s = %d, want %d", clientId, providerUsage[clientId], byteCount)
		}
		providerTotal += byteCount
	}
	if intraPlatform != 1_500_000_001+9 || intraNetwork != 500_000_001+50 {
		t.Fatalf("pool = %d intra-platform and %d intra-network bytes", intraPlatform, intraNetwork)
	}
	if providerTotal+intraPlatform+intraNetwork != total {
		t.Fatalf("provider usage %d and pool bytes %d do not sum to the epoch's %d contract bytes",
			providerTotal, intraPlatform+intraNetwork, total)
	}
}

// The reading refuses what it cannot classify rather than guess: no snapshot,
// a missing origin network once the amendment is in force, and shares that do
// not sum to the contract's bytes, before the amendment as after it.
func TestStExtenderAmendmentRefusesWhatItCannotClassify(t *testing.T) {
	amendment := stExtenderAmendmentTestPolicy()
	originNetworkId := contractPayoutTestId(101)
	share := contractProviderUsage{ClientId: contractPayoutTestId(10), NetworkId: contractPayoutTestId(110), ByteCount: 10}
	if _, err := stContractSharesForEpoch(nil, 8, originNetworkId, amendment); err == nil {
		t.Fatal("read a missing snapshot")
	}
	valid := &contractUsageSnapshot{Version: 1, ByteCount: 10, Providers: []contractProviderUsage{share}}
	if _, err := stContractSharesForEpoch(valid, 8, server.Id{}, amendment); err == nil {
		t.Fatal("classified shares without the contract's origin network")
	}
	for _, snapshot := range []*contractUsageSnapshot{
		{Version: 1, ByteCount: 11, Providers: []contractProviderUsage{share}},
		{Version: 1, ByteCount: 9, Providers: []contractProviderUsage{share}},
		{Version: 1, ByteCount: 1, Providers: []contractProviderUsage{}},
		{Version: 1, ByteCount: -1, Providers: []contractProviderUsage{{ClientId: share.ClientId, NetworkId: share.NetworkId, ByteCount: -1}}},
	} {
		for _, epoch := range []uint64{7, 8} {
			if shares, err := stContractSharesForEpoch(snapshot, epoch, originNetworkId, amendment); err == nil {
				t.Fatalf("epoch %d: read %+v as %+v", epoch, snapshot, shares)
			}
		}
	}
}

// The usage snapshot that settlement wrote for a contract with extender
// parties holds one even share per hop, the extenders' included, the payer
// network's too (SN usage is not billing), and the amendment's reading from
// its first epoch moves only the payer network's shares to the pool.
func assertStContractUsageSnapshotHops(
	t testing.TB,
	ctx context.Context,
	contractId server.Id,
	sortedParticipants []ContractParticipant,
	byteCount ByteCount,
	originNetworkId server.Id,
) {
	t.Helper()
	var data []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&data))
	})
	snapshot, err := decodeContractUsageSnapshot(data)
	if err != nil {
		t.Fatalf("usage snapshot: %v", err)
	}
	want := []contractProviderUsage{}
	wantProviders := []contractProviderUsage{}
	var wantIntraNetwork ByteCount
	for index, participant := range sortedParticipants {
		share := contractProviderUsage{ClientId: participant.ClientId, NetworkId: participant.NetworkId,
			ByteCount: ByteCount(evenContractPayoutShare(int64(byteCount), index, len(sortedParticipants)))}
		want = append(want, share)
		if participant.NetworkId == originNetworkId {
			wantIntraNetwork += share.ByteCount
		} else {
			wantProviders = append(wantProviders, share)
		}
	}
	if snapshot.ByteCount != byteCount || !slices.Equal(snapshot.Providers, want) {
		t.Fatalf("usage snapshot = %+v, want %d bytes in the hop shares %+v", snapshot, byteCount, want)
	}
	shares, err := stContractSharesForEpoch(snapshot, 8, originNetworkId, stExtenderAmendmentTestPolicy())
	if err != nil {
		t.Fatalf("read the usage snapshot: %v", err)
	}
	if !slices.Equal(shares.Providers, wantProviders) || shares.IntraNetwork != wantIntraNetwork || shares.IntraPlatform != 0 {
		t.Fatalf("epoch 8 reads %+v, want providers %+v and %d intra-network bytes", shares, wantProviders, wantIntraNetwork)
	}
}
