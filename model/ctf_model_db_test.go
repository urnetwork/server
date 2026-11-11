package model

// ctf_model_db_test.go — pg-backed tests of capture the flag's records
// (ctf_presence_model.go, ctf_model.go, ctf_schema.go).

import (
	"context"
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

func testCtfHash(label string) [32]byte {
	return sha256.Sum256([]byte(label))
}

// A sample is written once, each miner counted once per role and country; the
// bitmap's bits are PostgreSQL's set_bit positions, which CtfSampleSet reads.
func TestCtfPresenceSampleIsRecordedOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		provider, extender := server.NewId(), server.NewId()
		unit, later := testCtfHash("unit"), testCtfHash("later unit")
		sample := func(index int) *CtfPresenceSample {
			sourceTime := server.NowUtc().Add(-time.Minute)
			return &CtfPresenceSample{DeploymentKey: key, NoId: 1, Epoch: 8, SampleIndex: index, SampleTime: server.NowUtc(), HeadBlock: 9_600_028 + 75*uint64(index), ProviderSourceTime: &sourceTime, Providers: 1, Extenders: 1}
		}
		observations := []*CtfPresenceObservation{
			{Role: CtfRoleProvider, ClientId: provider, CountryCode: "IS", Unit: &unit, UnitFamily: 4},
			{Role: CtfRoleProvider, ClientId: provider, CountryCode: "is", Unit: &unit, UnitFamily: 4},
			{Role: CtfRoleExtender, ClientId: extender, CountryCode: "ir"},
		}
		if !RecordCtfPresenceSample(ctx, sample(5), observations) {
			t.Fatal("sample 5 not recorded")
		}
		if RecordCtfPresenceSample(ctx, sample(5), observations) {
			t.Fatal("sample 5 recorded twice")
		}
		observations[0].Unit = &later
		observations[0].UnitFamily = 6
		if !RecordCtfPresenceSample(ctx, sample(671), observations[:1]) || !RecordCtfPresenceSample(ctx, sample(9), observations) {
			t.Fatal("later samples not recorded")
		}
		if !HasCtfPresenceSample(ctx, key, 1, 8, 9) || HasCtfPresenceSample(ctx, key, 1, 8, 10) {
			t.Fatal("sample presence is wrong")
		}
		samples := GetCtfPresenceSamples(ctx, key, 1, 8)
		if len(samples) != 3 || samples[0].SampleIndex != 5 || samples[2].SampleIndex != 671 || samples[0].ProviderSourceTime == nil {
			t.Fatalf("samples %+v", samples)
		}
		presences := GetCtfPresences(ctx, key, 1, 8)
		if len(presences) != 2 {
			t.Fatalf("%d presences, want 2", len(presences))
		}
		providerPresence, extenderPresence := presences[0], presences[1]
		if providerPresence.Role != CtfRoleProvider || providerPresence.CountryCode != "is" || providerPresence.SampleCount != 3 ||
			!slices.Equal(providerPresence.Samples.Indexes(), []int{5, 9, 671}) {
			t.Fatalf("provider presence %+v %v", providerPresence, providerPresence.Samples.Indexes())
		}
		// the latest sample's unit, whatever order the samples arrived in
		if providerPresence.Unit == nil || *providerPresence.Unit != later || providerPresence.UnitFamily != 6 || providerPresence.LastSampleIndex != 671 {
			t.Fatalf("provider unit %+v", providerPresence)
		}
		if extenderPresence.Role != CtfRoleExtender || extenderPresence.SampleCount != 2 || extenderPresence.Unit != nil ||
			!slices.Equal(extenderPresence.Samples.Indexes(), []int{5, 9}) {
			t.Fatalf("extender presence %+v", extenderPresence)
		}
		// another deployment's epoch is apart
		if len(GetCtfPresences(ctx, StDeploymentKey("945:0x1000000000000000000000000000000000000001"), 1, 8)) != 0 {
			t.Fatal("presence leaks across deployments")
		}
		if removed := RemoveCtfPresenceBefore(ctx, key, 1, 9); removed != 5 || len(GetCtfPresences(ctx, key, 1, 8)) != 0 {
			t.Fatalf("retention removed %d", removed)
		}
	})
}

func TestCtfPresenceOutages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		start := time.Date(2026, 11, 27, 18, 29, 0, 0, time.UTC)
		DeclareCtfPresenceOutage(ctx, &CtfPresenceOutage{DeploymentKey: key, NoId: 1, StartTime: start.Add(time.Hour), EndTime: start.Add(2 * time.Hour), Reason: "connect down", DeclaredBy: "operator@example"})
		DeclareCtfPresenceOutage(ctx, &CtfPresenceOutage{DeploymentKey: key, NoId: 1, StartTime: start.Add(-2 * time.Hour), EndTime: start.Add(-time.Hour), Reason: "before", DeclaredBy: "operator@example"})
		outages := GetCtfPresenceOutages(ctx, key, 1, start, start.Add(7*24*time.Hour))
		if len(outages) != 1 || outages[0].Reason != "connect down" {
			t.Fatalf("outages %+v", outages)
		}
		excused := CtfExcusedSamples(start, start.Add(7*24*time.Hour), outages)
		if excused.Count() != 4 || !excused.Has(4) || !excused.Has(7) {
			t.Fatalf("excused %v", excused.Indexes())
		}
	})
}

func testCtfCensus(key StDeploymentKey, epoch uint64) (*CtfCensus, []*CtfCensusObject, []*CtfCensusMember, []*CtfCensusPot, []*CtfCensusExclusion) {
	header := []byte(`{"schema":"urnetwork-ctf-census-v1","epoch":` + string(rune('0'+epoch%10)) + `}`)
	chunk := []byte("rows")
	census := &CtfCensus{
		DeploymentKey: key, NoId: 1, Epoch: epoch, CensusHash: sha256.Sum256(header), Header: header,
		Candidates: 1, ProviderCandidates: 1, ProviderUnits: 1, ProviderMembers: 2, ExtenderMembers: 1,
		ExcusedSamples: []int{3, 600}, SamplesTaken: 670, ProviderSamplesTaken: 669, RequiredSamples: 335,
		StartBlock: 9_600_028, EndBlock: 9_650_427,
		StartTime: time.Date(2026, 11, 27, 18, 29, 0, 0, time.UTC), EndTime: time.Date(2026, 12, 4, 18, 30, 0, 0, time.UTC),
		CaptureTime: time.Date(2026, 12, 4, 18, 31, 0, 0, time.UTC), ProviderSourceTime: time.Date(2026, 12, 4, 18, 28, 0, 0, time.UTC),
	}
	objects := []*CtfCensusObject{{Ref: census.CensusHash, Data: header}, {Ref: sha256.Sum256(chunk), Data: chunk}, {Ref: sha256.Sum256(nil), Data: []byte{}}}
	candidate, nearMiss, extender := server.Id{1}, server.Id{2}, server.Id{3}
	networkId, extenderId := server.NewId(), server.NewId()
	unit, coldkey := testCtfHash("unit"), testCtfHash("coldkey")
	members := []*CtfCensusMember{
		{Role: CtfRoleProvider, ClientId: candidate, NetworkId: &networkId, Candidate: true, CountryCode: "is", DrawCountryCode: "is", PresentSamples: 670, Unit: &unit, UnitFamily: 4, Coldkey: &coldkey},
		{Role: CtfRoleProvider, ClientId: nearMiss, Reason: CtfReasonPresence, Failed: 1 << CtfReasonPresence, CountryCode: "is", PresentSamples: 200},
		{Role: CtfRoleExtender, ClientId: extender, ExtenderId: &extenderId, Reason: CtfReasonRevoked, Failed: 1<<CtfReasonRevoked | 1<<CtfReasonNoLocation, CountryCode: "zz"},
	}
	pots := []*CtfCensusPot{{CountryCode: "is", Role: CtfRoleProvider, Units: 1, Candidates: 1, NearMisses: 1}, {CountryCode: "is", Role: CtfRoleExtender}}
	exclusions := []*CtfCensusExclusion{{CountryCode: "is", Role: CtfRoleProvider, Reason: CtfReasonPresence, Count: 1}, {CountryCode: "zz", Role: CtfRoleExtender, Reason: CtfReasonRevoked, Count: 1}}
	return census, objects, members, pots, exclusions
}

// A census is written once, all of it; a second build of the epoch writes
// nothing; every part reads back as written.
func TestCtfCensusIsWrittenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		census, objects, members, pots, exclusions := testCtfCensus(key, 8)
		if !CreateCtfCensus(ctx, census, objects, members, pots, exclusions) {
			t.Fatal("census not created")
		}
		other := *census
		other.Candidates, other.ProviderCandidates = 0, 0
		if CreateCtfCensus(ctx, &other, nil, nil, nil, nil) {
			t.Fatal("census written twice")
		}
		read := GetCtfCensus(ctx, key, 1, 8)
		if read == nil || read.CensusHash != census.CensusHash || read.Candidates != 1 || !slices.Equal(read.ExcusedSamples, []int{3, 600}) ||
			read.RequiredSamples != 335 || read.StartBlock != 9_600_028 || !read.EndTime.Equal(census.EndTime) || read.PublishedTime != nil {
			t.Fatalf("census %+v", read)
		}
		if read := GetCtfCensusObjects(ctx, key, 1, 8); len(read) != 3 || string(read[census.CensusHash]) != string(census.Header) {
			t.Fatalf("census objects %d", len(read))
		}
		readMembers := GetCtfCensusMembers(ctx, key, 1, 8)
		if len(readMembers) != 3 {
			t.Fatalf("%d members", len(readMembers))
		}
		if m := readMembers[0]; !m.Candidate || m.DrawCountryCode != "is" || *m.Unit != *members[0].Unit || m.UnitFamily != 4 || *m.Coldkey != *members[0].Coldkey || *m.NetworkId != *members[0].NetworkId || m.Epoch != 8 {
			t.Fatalf("candidate %+v", m)
		}
		if m := readMembers[1]; m.Candidate || !m.NearMiss() || m.DrawCountryCode != "" || m.Unit != nil || m.Coldkey != nil || m.NetworkId != nil {
			t.Fatalf("near miss %+v", m)
		}
		if m := readMembers[2]; m.Reason != CtfReasonRevoked || m.Failed != members[2].Failed || *m.ExtenderId != *members[2].ExtenderId {
			t.Fatalf("extender %+v", m)
		}
		if client := GetCtfCensusClientMembers(ctx, key, 1, server.Id{2}, 1, 8); len(client) != 1 || client[0].ClientId != (server.Id{2}) {
			t.Fatalf("client members %+v", client)
		}
		if coldkeys := GetCtfCensusColdkeys(ctx, key, 1, 8); len(coldkeys) != 1 || coldkeys[server.Id{1}] != *members[0].Coldkey {
			t.Fatalf("coldkeys %v", coldkeys)
		}
		if read := GetCtfCensusPots(ctx, key, 1, 8); len(read) != 2 || *read[0] != *pots[0] {
			t.Fatalf("pots %+v", read)
		}
		if read := GetCtfCensusExclusions(ctx, key, 1, 8); len(read) != 2 || *read[1] != *exclusions[1] {
			t.Fatalf("exclusions %+v", read)
		}
		SetCtfCensusPublished(ctx, key, 1, 8, server.NowUtc())
		if GetCtfCensus(ctx, key, 1, 8).PublishedTime == nil {
			t.Fatal("census not published")
		}
		if censuses := GetCtfCensuses(ctx, key, 1, 1, 9); len(censuses) != 1 || censuses[0].Epoch != 8 {
			t.Fatalf("censuses %+v", censuses)
		}
	})
}

// A committed census's objects stay until a finalized ledger covers its
// epoch; members go by age alone, the census itself never.
func TestCtfCensusRetentionWaitsForAFinalizedLedger(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		for epoch := uint64(7); epoch <= 10; epoch++ {
			census, objects, members, pots, exclusions := testCtfCensus(key, epoch)
			census.CensusHash = testCtfHash(string(rune('a' + epoch)))
			objects[0].Ref = census.CensusHash
			CreateCtfCensus(ctx, census, objects, members, pots, exclusions)
			SetCtfCensusPublished(ctx, key, 1, epoch, server.NowUtc())
		}
		CreateCtfLedger(ctx, &CtfLedger{DeploymentKey: key, NoId: 1, Epoch: 9, FirstEpoch: 8, LedgerHash: testCtfHash("ledger 9"), Root: testCtfHash("root"), Header: []byte("{}"), Status: CtfLedgerStatusBuilt})
		if removed := RemoveCtfCensusObjectsBefore(ctx, key, 1, 11); removed != 0 {
			t.Fatalf("removed %d objects before any ledger is final", removed)
		}
		seq := uint64(3)
		if !SetCtfLedgerStatus(ctx, key, 1, 9, testCtfHash("ledger 9"), &CtfLedgerStatusUpdate{Status: CtfLedgerStatusFinalized, Seq: &seq}) {
			t.Fatal("ledger status not set")
		}
		if removed := RemoveCtfCensusObjectsBefore(ctx, key, 1, 11); removed != 6 {
			t.Fatalf("removed %d objects, want epochs 8 and 9's 6", removed)
		}
		for epoch, want := range map[uint64]int{7: 3, 8: 0, 9: 0, 10: 3} {
			if got := len(GetCtfCensusObjects(ctx, key, 1, epoch)); got != want {
				t.Errorf("epoch %d keeps %d objects, want %d", epoch, got, want)
			}
		}
		if removed := RemoveCtfCensusMembersBefore(ctx, key, 1, 9); removed != 6 || GetCtfCensus(ctx, key, 1, 7) == nil {
			t.Fatalf("member retention removed %d", removed)
		}
	})
}

// The driver's records: a commitment of another hash is refused, a draw and
// an epoch's result are written once and read back exactly, a ledger's state
// moves, and recycles sum per epoch however often they are mirrored.
func TestCtfDriverRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		censusHash := testCtfHash("census")
		if !SetCtfCensusCommitment(ctx, &CtfCensusCommitment{DeploymentKey: key, NoId: 1, Epoch: 8, CensusHash: censusHash, Status: CtfCommitmentStatusSent, TxHash: "0x01"}) {
			t.Fatal("commitment not set")
		}
		block, blockTime, round := uint64(9_650_500), uint64(1_796_000_000), uint64(30_000_000)
		if !SetCtfCensusCommitment(ctx, &CtfCensusCommitment{DeploymentKey: key, NoId: 1, Epoch: 8, CensusHash: censusHash, Status: CtfCommitmentStatusCommitted, CommitBlock: &block, CommitBlockTime: &blockTime, Round: &round}) {
			t.Fatal("commitment not updated")
		}
		if SetCtfCensusCommitment(ctx, &CtfCensusCommitment{DeploymentKey: key, NoId: 1, Epoch: 8, CensusHash: testCtfHash("other"), Status: CtfCommitmentStatusCommitted, CommitBlock: &block, CommitBlockTime: &blockTime}) {
			t.Fatal("a commitment of another hash replaced the epoch's")
		}
		commitment := GetCtfCensusCommitment(ctx, key, 1, 8)
		if commitment.Status != CtfCommitmentStatusCommitted || commitment.TxHash != "0x01" || *commitment.CommitBlock != block || *commitment.Round != round {
			t.Fatalf("commitment %+v", commitment)
		}

		draw := &CtfDraw{DeploymentKey: key, NoId: 1, Epoch: 8, CensusHash: censusHash, Round: round, Signature: make([]byte, 48), Randomness: testCtfHash("randomness"), Seed: testCtfHash("seed")}
		if !CreateCtfDraw(ctx, draw) || CreateCtfDraw(ctx, draw) {
			t.Fatal("draw not written once")
		}
		if read := GetCtfDraw(ctx, key, 1, 8); read.Round != round || read.Seed != draw.Seed || len(read.Signature) != 48 {
			t.Fatalf("draw %+v", read)
		}

		result := &CtfEpochResult{
			DeploymentKey: key, NoId: 1, Epoch: 8, LedgerEpoch: 9, LedgerHash: testCtfHash("ledger"), CensusHash: &censusHash, CensusBlock: &block,
			Drawn: true, Round: round, Candidates: 4217, Matured: true, SlotRemainder: 172, Awards: 1,
			AwardsRef: testCtfHash("awards"), PotsRef: testCtfHash("pots"), PendingRef: testCtfHash("pending"), Report: []byte(`{"epoch":8}`),
		}
		result.Funding.SetUint64(119_583_893_572)
		result.Fresh.SetUint64(119_583_893_572)
		result.SlotBase.SetUint64(48_025_660)
		result.Carried.SetFromDecimal("340282366920938463463374607431768211455")
		award := &CtfAward{Role: CtfRoleProvider, CountryCode: "tm", Rank: 1, Unit: testCtfHash("unit"), UnitFamily: 4, ClientId: server.Id{9}, Coldkey: testCtfHash("coldkey"), Deferred: true}
		award.Slot.SetUint64(48_025_660)
		award.Pot.SetUint64(2_401_283_000)
		pot := &CtfPot{CountryCode: "tm", Role: CtfRoleProvider, Units: 1, Winners: 1}
		pot.In.SetUint64(12_006_415_000)
		pot.Out.SetUint64(9_797_234_640)
		pending := &CtfPendingAward{AwardEpoch: 8, Role: CtfRoleProvider, CountryCode: "tm", Rank: 1, Coldkey: award.Coldkey}
		pending.Amount.SetUint64(2_401_283_000)
		if !CreateCtfEpochResult(ctx, result, []*CtfAward{award}, []*CtfPot{pot}, []*CtfPendingAward{pending}) {
			t.Fatal("result not written")
		}
		if CreateCtfEpochResult(ctx, result, nil, nil, nil) {
			t.Fatal("result written twice")
		}
		read := GetCtfEpochResult(ctx, key, 1, 8)
		if read == nil || read.Funding != result.Funding || read.Carried != result.Carried || read.SlotBase != result.SlotBase || !read.Drawn || *read.CensusHash != censusHash || string(read.Report) != `{"epoch": 8}` && string(read.Report) != `{"epoch":8}` {
			t.Fatalf("result %+v", read)
		}
		if awards := GetCtfAwards(ctx, key, 1, 8); len(awards) != 1 || awards[0].Pot != award.Pot || !awards[0].Deferred || awards[0].UnitFamily != 4 {
			t.Fatalf("awards %+v", awards)
		}
		if awards := GetCtfClientAwards(ctx, key, 1, server.Id{9}, 1); len(awards) != 1 || awards[0].Epoch != 8 {
			t.Fatalf("client awards %+v", awards)
		}
		if pots := GetCtfPots(ctx, key, 1, 8); len(pots) != 1 || pots[0].Out != pot.Out {
			t.Fatalf("pots %+v", pots)
		}
		if pots := GetCtfCountryPots(ctx, key, 1, "tm", 1); len(pots) != 1 || pots[0].Epoch != 8 {
			t.Fatalf("country pots %+v", pots)
		}
		if read := GetCtfPendingAwards(ctx, key, 1, 8); len(read) != 1 || read[0].Amount != pending.Amount {
			t.Fatalf("pending %+v", read)
		}
		RemoveCtfEpochResult(ctx, key, 1, 8)
		if GetCtfEpochResult(ctx, key, 1, 8) != nil || len(GetCtfAwards(ctx, key, 1, 8)) != 0 {
			t.Fatal("result not removed")
		}

		ledger := &CtfLedger{DeploymentKey: key, NoId: 1, Epoch: 7, FirstEpoch: 7, LedgerHash: testCtfHash("shadow"), Shadow: true, Root: testCtfHash("root"), Header: []byte("{}"), Status: CtfLedgerStatusShadow}
		ledger.Total.SetUint64(1)
		if !CreateCtfLedger(ctx, ledger) || CreateCtfLedger(ctx, ledger) {
			t.Fatal("ledger not written once")
		}
		if ledgers := GetCtfLedgers(ctx, key, 1, 1); len(ledgers) != 1 || !ledgers[0].Shadow || ledgers[0].Total != ledger.Total {
			t.Fatalf("ledgers %+v", ledgers)
		}

		var amount uint256.Int
		amount.SetUint64(90_000_000_000)
		recycles := []*CtfRecycle{
			{BlockNumber: 9_660_000, LogIndex: 1, Epoch: 9, Amount: amount, TxHash: "0x02"},
			{BlockNumber: 9_660_100, LogIndex: 0, Epoch: 9, Amount: amount, TxHash: "0x03"},
		}
		AddCtfRecycles(ctx, key, 1, recycles)
		AddCtfRecycles(ctx, key, 1, recycles)
		recycled := GetCtfRecycledFor(ctx, key, 1, 8, 10)
		if total, ok := recycled[9]; !ok || total.Uint64() != 180_000_000_000 || len(recycled) != 1 {
			t.Fatalf("recycled %v", recycled)
		}
	})
}
