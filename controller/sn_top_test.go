package controller

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestSnTopCursor(t *testing.T) {
	snapshotId := server.NewId()
	encoded := encodeSnTopCursor(snTopCursor{SnapshotId: snapshotId, Filter: model.SnTopFilterClaimed, Position: 50})
	cursor, err := decodeSnTopCursor(" " + encoded + "\n")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, cursor.SnapshotId, snapshotId)
	connect.AssertEqual(t, cursor.Filter, model.SnTopFilterClaimed)
	connect.AssertEqual(t, cursor.Position, 50)
	for _, bad := range []string{
		"not a cursor",
		encodeSnTopCursor(snTopCursor{SnapshotId: snapshotId, Filter: "bogus", Position: 1}),
		encodeSnTopCursor(snTopCursor{SnapshotId: snapshotId, Filter: model.SnTopFilterAll, Position: -1}),
		// past the paged top 400
		encodeSnTopCursor(snTopCursor{SnapshotId: snapshotId, Filter: model.SnTopFilterAll, Position: 401}),
	} {
		_, err := decodeSnTopCursor(bad)
		connect.AssertNotEqual(t, err, nil)
	}
}

func TestSnTopFilterAndLimit(t *testing.T) {
	for input, want := range map[string]string{
		"":           model.SnTopFilterAll,
		"all":        model.SnTopFilterAll,
		" Claimed ":  model.SnTopFilterClaimed,
		"UNCLAIMED":  model.SnTopFilterUnclaimed,
		"unclaimed ": model.SnTopFilterUnclaimed,
	} {
		filter, ok := snTopFilter(input)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, filter, want)
	}
	_, ok := snTopFilter("mine")
	connect.AssertEqual(t, ok, false)
	connect.AssertEqual(t, snTopLimit(0), 50)
	connect.AssertEqual(t, snTopLimit(1), 1)
	connect.AssertEqual(t, snTopLimit(200), 200)
	connect.AssertEqual(t, snTopLimit(5000), 200)
}

// Q12: no row ever carries the network id; the name only with
// points_leaderboard_public and a clean name; the tag always.
func TestSnTopRowPrivacy(t *testing.T) {
	networkId := server.NewId()
	caller := server.NewId()
	base := model.SnTopNetworkRow{
		SnTopNetworkProjection: model.SnTopNetworkProjection{
			NetworkId:      networkId,
			Position:       3,
			ProjectedScore: 4.5,
			Label:          model.SnTopLabelEstimate,
			Measurable:     true,
			ClaimPrompt:    true,
		},
		NetworkName: "secret-name",
		EmojiTag:    "🦊",
	}
	hidden := snTopRowFromModel(&base, nil, &caller)
	connect.AssertEqual(t, hidden.Name, "")
	connect.AssertEqual(t, hidden.Tag, "🦊")
	connect.AssertEqual(t, hidden.IsMe, false)
	connect.AssertEqual(t, len(hidden.Fleets), 0)
	hiddenJson, err := json.Marshal(hidden)
	connect.AssertEqual(t, err, nil)
	if strings.Contains(string(hiddenJson), "network_id") || strings.Contains(string(hiddenJson), networkId.String()) || strings.Contains(string(hiddenJson), "secret-name") {
		t.Fatalf("an anonymous row identifies its network: %s", hiddenJson)
	}
	// fleets serialize as [] when unclaimed
	if !strings.Contains(string(hiddenJson), `"fleets":[]`) {
		t.Fatalf("an unclaimed row must carry an empty fleets list: %s", hiddenJson)
	}

	public := base
	public.PointsLeaderboardPublic = true
	shown := snTopRowFromModel(&public, nil, nil)
	connect.AssertEqual(t, shown.Name, "secret-name")
	shownJson, _ := json.Marshal(shown)
	if strings.Contains(string(shownJson), networkId.String()) {
		t.Fatalf("a named row carries its network id: %s", shownJson)
	}

	profane := public
	profane.ContainsProfanity = true
	connect.AssertEqual(t, snTopRowFromModel(&profane, nil, nil).Name, "")

	mine := snTopRowFromModel(&base, nil, &networkId)
	connect.AssertEqual(t, mine.IsMe, true)
	// even the caller's own anonymous row carries no name
	connect.AssertEqual(t, mine.Name, "")

	// an anonymous claimed row shows its fleets' UIDs and validator data
	score := 2.5
	rank, of := 1, 3
	selected := true
	claimed := snTopRowFromModel(&base, []*model.SnTopFleetRow{{
		Key:             model.SnTopFleetKey{Uid: 42, Generation: 2},
		ValidToEpoch:    19,
		ReplicaScore:    2.5,
		ValidatorScore:  &score,
		ValidatorRank:   &rank,
		ValidatorRankOf: &of,
		Selected:        &selected,
	}}, nil)
	connect.AssertEqual(t, claimed.Name, "")
	connect.AssertEqual(t, len(claimed.Fleets), 1)
	connect.AssertEqual(t, claimed.Fleets[0].Uid, 42)
	connect.AssertEqual(t, claimed.Fleets[0].Generation, uint64(2))
	connect.AssertEqual(t, *claimed.Fleets[0].ValidatorRank, 1)
	claimedJson, _ := json.Marshal(claimed)
	if strings.Contains(string(claimedJson), "hotkey") {
		t.Fatalf("a row carries a hotkey: %s", claimedJson)
	}
}

// Nullable snapshot fields serialize as null, never omitted (CONTRACT.md).
func TestSnTopSnapshotWireShape(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	snapshot := snTopSnapshotFromModel(&model.SnTopSnapshot{
		SnapshotId: server.NewId(),
		ComputedAt: now.Add(-time.Hour),
		HeadSeats:  200,
		OpenSeats:  200,
	}, now)
	connect.AssertEqual(t, snapshot.Stale, false)
	snapshotJson, err := json.Marshal(snapshot)
	connect.AssertEqual(t, err, nil)
	for _, field := range []string{`"claimed_cutoff":null`, `"validator_summary_at":null`, `"incentive_at":null`, `"precision_at_200":null`, `"computed_at":"2026-10-29T11:00:00Z"`} {
		if !strings.Contains(string(snapshotJson), field) {
			t.Fatalf("snapshot json lacks %s: %s", field, snapshotJson)
		}
	}
	// stale past three tempos
	connect.AssertEqual(t, snTopStale(now.Add(-3*model.SnTopTempoDuration), now), false)
	connect.AssertEqual(t, snTopStale(now.Add(-3*model.SnTopTempoDuration-time.Second), now), true)
	empty := snTopEmptySnapshot()
	connect.AssertEqual(t, empty.HeadSeats, 200)
	connect.AssertEqual(t, empty.OpenSeats, 200)
	connect.AssertEqual(t, empty.BindingOpen, false)
	connect.AssertEqual(t, empty.BindingNote, "Top 200 binding isn't open on mainnet yet")
}

// One fold per tempo: wait for it to settle, never fold one twice, and never
// fold twice within half a tempo even when the tempo source changes.
func TestSnTopShouldFold(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tempo := model.SnTopTempo{Source: model.SnTopTempoSourceChain, Index: 100, EndTime: now.Add(-5 * time.Minute)}
	fold, _ := snTopShouldFold(tempo, nil, now)
	connect.AssertEqual(t, fold, true)
	settling := tempo
	settling.EndTime = now.Add(-time.Minute)
	fold, reason := snTopShouldFold(settling, nil, now)
	connect.AssertEqual(t, fold, false)
	connect.AssertEqual(t, reason, "settling")
	folded := &model.SnTopSnapshot{TempoSource: model.SnTopTempoSourceChain, TempoIndex: 100, ComputedAt: now.Add(-2 * time.Hour)}
	fold, reason = snTopShouldFold(tempo, folded, now)
	connect.AssertEqual(t, fold, false)
	connect.AssertEqual(t, reason, "folded")
	previous := &model.SnTopSnapshot{TempoSource: model.SnTopTempoSourceChain, TempoIndex: 99, ComputedAt: now.Add(-70 * time.Minute)}
	fold, _ = snTopShouldFold(tempo, previous, now)
	connect.AssertEqual(t, fold, true)
	// a source change right after a fold waits for half a tempo
	switched := &model.SnTopSnapshot{TempoSource: model.SnTopTempoSourceClock, TempoIndex: 500000, ComputedAt: now.Add(-10 * time.Minute)}
	fold, reason = snTopShouldFold(tempo, switched, now)
	connect.AssertEqual(t, fold, false)
	connect.AssertEqual(t, reason, "recent")
}

// A current head summary names the validator's own tempo; a stale one
// hands the cadence back to the chain anchor.
func TestSnTopFoldTempoFromTheValidatorSummary(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	anchor := &model.SnTopTempoAnchor{Source: model.SnTopTempoSourceChain, Block: 1_000_000, TimeMs: now.UnixMilli(), Epoch: 7, EpochStartBlock: 999_000, EpochBlocks: 50400}
	summary := &SnTopValidatorSummary{NativeEpoch: 4321, NativeSnapshotBlock: 999_990, SettlementEpoch: 7, ReceivedAt: now.Add(-time.Minute)}
	previous := &model.SnTopSnapshot{TempoSource: model.SnTopTempoSourceSummary, TempoIndex: 4320, TempoEndTime: now.Add(-73 * time.Minute), ComputedAt: now.Add(-10 * time.Minute)}
	tempo := snTopFoldTempo(anchor, summary, previous, now)
	connect.AssertEqual(t, tempo.Source, model.SnTopTempoSourceSummary)
	connect.AssertEqual(t, tempo.Index, int64(4321))
	connect.AssertEqual(t, tempo.EndBlock, uint64(999_990))
	connect.AssertEqual(t, tempo.StartTime, previous.TempoEndTime)
	connect.AssertEqual(t, tempo.EndTime, summary.ReceivedAt)
	// the previous fold was ten minutes ago: wait
	fold, _ := snTopShouldFold(tempo, previous, now)
	connect.AssertEqual(t, fold, false)
	previous.ComputedAt = now.Add(-73 * time.Minute)
	fold, _ = snTopShouldFold(tempo, previous, now)
	connect.AssertEqual(t, fold, true)

	stale := *summary
	stale.ReceivedAt = now.Add(-3 * model.SnTopTempoDuration)
	connect.AssertEqual(t, snTopFoldTempo(anchor, &stale, previous, now).Source, model.SnTopTempoSourceChain)
	connect.AssertEqual(t, snTopFoldTempo(nil, nil, nil, now).Source, model.SnTopTempoSourceClock)
}

// The validator's summary and the incentive read attach to each fleet by
// UID; without them every field stays null.
func TestSnTopAttachChainData(t *testing.T) {
	hotkey7, hotkey8 := [32]byte{7}, [32]byte{8}
	key7 := model.SnTopFleetKey{FleetId: [32]byte{0x70}, Hotkey: hotkey7, Generation: 1, Uid: 7}
	key8 := model.SnTopFleetKey{FleetId: [32]byte{0x80}, Hotkey: hotkey8, Generation: 1, Uid: 8}
	fleets := []model.SnTopFleetInput{{Key: key7}, {Key: key8}}
	untouched := snTopAttachChainData(append([]model.SnTopFleetInput{}, fleets...), nil, nil)
	connect.AssertEqual(t, untouched[0].ValidatorScore, (*float64)(nil))
	connect.AssertEqual(t, untouched[0].ValidatorRankOf, (*int)(nil))
	connect.AssertEqual(t, untouched[0].Selected, (*bool)(nil))
	connect.AssertEqual(t, untouched[0].Incentive, (*float64)(nil))

	summary := &SnTopValidatorSummary{EntryCount: 2, Entries: []SnTopValidatorSummaryEntry{
		{Rank: 1, Uid: 7, Hotkey: hotkey7, Score: big.NewRat(3, 2), Selected: true,
			Fleets: []SnTopValidatorSummaryFleet{{FleetId: key7.FleetId, Generation: 1}}},
		{Rank: 2, Uid: 9, Hotkey: [32]byte{9}, Score: big.NewRat(1, 2), Selected: false,
			Fleets: []SnTopValidatorSummaryFleet{{FleetId: [32]byte{0x90}, Generation: 1}}},
	}}
	incentive := &SnTopIncentive{
		ByUid:   map[uint16]uint16{7: 16384, 8: 100},
		Holders: map[uint16][32]byte{7: hotkey7},
		Stale:   true,
	}
	attached := snTopAttachChainData(append([]model.SnTopFleetInput{}, fleets...), summary, incentive)
	connect.AssertEqual(t, *attached[0].ValidatorScore, 1.5)
	connect.AssertEqual(t, *attached[0].ValidatorRank, 1)
	connect.AssertEqual(t, *attached[0].ValidatorRankOf, 2)
	connect.AssertEqual(t, *attached[0].Selected, true)
	connect.AssertEqual(t, *attached[0].Incentive, 16384.0/65535)
	connect.AssertEqual(t, attached[0].IncentiveStale, true)
	// uid 8 is not eligible at the validator: score 0, not selected, no
	// rank; and its holder was not read, so no incentive is claimed for it
	connect.AssertEqual(t, *attached[1].ValidatorScore, 0.0)
	connect.AssertEqual(t, attached[1].ValidatorRank, (*int)(nil))
	connect.AssertEqual(t, *attached[1].Selected, false)
	connect.AssertEqual(t, attached[1].Incentive, (*float64)(nil))
	connect.AssertEqual(t, attached[1].IncentiveStale, true)

	// a UID that changed hands shows nothing of its new holder's
	newHolder := [32]byte{2}
	movedSummary := &SnTopValidatorSummary{EntryCount: 1, Entries: []SnTopValidatorSummaryEntry{
		{Rank: 1, Uid: 7, Hotkey: newHolder, Score: big.NewRat(3, 2), Selected: true,
			Fleets: []SnTopValidatorSummaryFleet{{FleetId: key7.FleetId, Generation: 1}}},
	}}
	movedIncentive := &SnTopIncentive{ByUid: map[uint16]uint16{7: 65535}, Holders: map[uint16][32]byte{7: newHolder}}
	moved := snTopAttachChainData([]model.SnTopFleetInput{{Key: key7}}, movedSummary, movedIncentive)
	connect.AssertEqual(t, *moved[0].ValidatorScore, 0.0)
	connect.AssertEqual(t, moved[0].ValidatorRank, (*int)(nil))
	connect.AssertEqual(t, *moved[0].Selected, false)
	connect.AssertEqual(t, moved[0].Incentive, (*float64)(nil))
	newKey := key7
	newKey.Hotkey = newHolder
	kept := snTopAttachChainData([]model.SnTopFleetInput{{Key: newKey}}, movedSummary, movedIncentive)
	connect.AssertEqual(t, *kept[0].ValidatorRank, 1)
	connect.AssertEqual(t, *kept[0].Incentive, 1.0)
	// an older generation of the fleet is not one of the identities that
	// make up the UID's score
	oldGeneration := key7
	oldGeneration.Generation = 0
	old := snTopAttachChainData([]model.SnTopFleetInput{{Key: oldGeneration}}, summary, nil)
	connect.AssertEqual(t, old[0].ValidatorRank, (*int)(nil))

	// the replica check compares exact values, UID by UID
	agreement := snTopAgreement{}
	snTopMeasureReplica(&agreement, &model.SnTopProjection{Fleets: []*model.SnTopFleetProjection{
		{Fleet: fleets[0], ReplicaExact: big.NewRat(3, 2)},
	}}, summary)
	connect.AssertEqual(t, agreement.replicaMatches, 1)
	connect.AssertEqual(t, agreement.replicaMismatches, 0)
	snTopMeasureReplica(&agreement, &model.SnTopProjection{Fleets: []*model.SnTopFleetProjection{
		{Fleet: fleets[0], ReplicaExact: big.NewRat(3, 4)},
	}}, summary)
	connect.AssertEqual(t, agreement.replicaMismatches, 1)
	snTopMeasureIncentive(&agreement, summary, incentive)
	connect.AssertEqual(t, agreement.selectedEarning, 1)
	connect.AssertEqual(t, agreement.selectedNotEarning, 0)
}

// The chain sync's records map onto the board's inputs: live fleets with
// their members' networks once each, the summary by UID in rank order with
// its exact scores, and the incentive census's stored u16 with its holders.
func TestSnTopChainSourcesFromTheChainSync(t *testing.T) {
	networkA, networkB := server.NewId(), server.NewId()
	clientA1, clientA2, clientB1, clientNone := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	key := model.SnFleetKey{FleetId: [32]byte{1}, Hotkey: [32]byte{2}, Generation: 3, Uid: 42}
	inputs := snTopFleetInputs([]*model.SnClaimedFleet{{
		Key:          key,
		ValidToEpoch: 19,
		UidHeld:      model.SnUidHeldYes,
		Members: []*model.SnClaimedFleetMember{
			{ClientId: clientA1, NetworkId: &networkA},
			{ClientId: clientA2, NetworkId: &networkA},
			{ClientId: clientB1, NetworkId: &networkB},
			{ClientId: clientNone},
		},
	}})
	connect.AssertEqual(t, len(inputs), 1)
	connect.AssertEqual(t, inputs[0].Key, model.SnTopFleetKey(key))
	connect.AssertEqual(t, inputs[0].ValidToEpoch, uint64(19))
	connect.AssertEqual(t, inputs[0].ClientIds, []server.Id{clientA1, clientA2, clientB1, clientNone})
	connect.AssertEqual(t, inputs[0].NetworkIds, []server.Id{networkA, networkB})

	second := model.SnFleetKey{FleetId: [32]byte{5}, Hotkey: [32]byte{6}, Generation: 1, Uid: 7}
	summary := snTopValidatorSummaryFromModel(&model.SnHeadSummary{
		NativeEpoch:         4321,
		NativeSnapshotBlock: 999_990,
		SettlementEpoch:     7,
		MaximumHeadFleets:   200,
		EntryCount:          2,
		Fleets: []*model.SnHeadSummaryFleetScore{
			{Key: second, Rank: 2, ScoreNumerator: "1", ScoreDenominator: "3", Score: 1.0 / 3},
			{Key: key, Rank: 1, ScoreNumerator: "9", ScoreDenominator: "8", Score: 1.125, Selected: true},
		},
	})
	connect.AssertEqual(t, summary.NativeEpoch, uint64(4321))
	connect.AssertEqual(t, summary.EntryCount, 2)
	connect.AssertEqual(t, len(summary.Entries), 2)
	connect.AssertEqual(t, summary.Entries[0].Uid, uint16(42))
	connect.AssertEqual(t, summary.Entries[0].Score.Cmp(big.NewRat(9, 8)), 0)
	connect.AssertEqual(t, summary.Entries[1].Uid, uint16(7))
	attached := snTopAttachChainData([]model.SnTopFleetInput{{Key: model.SnTopFleetKey(key)}}, summary, nil)
	connect.AssertEqual(t, *attached[0].ValidatorRank, 1)
	connect.AssertEqual(t, *attached[0].ValidatorRankOf, 2)
	connect.AssertEqual(t, *attached[0].ValidatorScore, 1.125)
	connect.AssertEqual(t, *attached[0].Selected, true)

	rows := make([]*model.SnIncentiveUid, 43)
	for uid := range rows {
		rows[uid] = &model.SnIncentiveUid{Uid: uint16(uid)}
	}
	rows[42] = &model.SnIncentiveUid{Uid: 42, Incentive: 1180, HolderRead: true, Hotkey: key.Hotkey}
	rows[7] = &model.SnIncentiveUid{Uid: 7, Incentive: 300}
	incentive := snTopIncentiveFromModel(&model.SnIncentiveSnapshot{Uids: rows, Stale: true})
	connect.AssertEqual(t, incentive.ByUid[42], uint16(1180))
	connect.AssertEqual(t, incentive.Stale, true)
	_, holderOf7 := incentive.Holders[7]
	connect.AssertEqual(t, holderOf7, false)
	withIncentive := snTopAttachChainData([]model.SnTopFleetInput{{Key: model.SnTopFleetKey(key)}}, nil, incentive)
	// the board serves the chain sync's own fraction
	connect.AssertEqual(t, *withIncentive[0].Incentive, rows[42].IncentiveFraction())
	for _, raw := range []uint16{0, 1, 1180, 32768, 65535} {
		connect.AssertEqual(t, SnTopIncentiveFraction(raw), (&model.SnIncentiveUid{Incentive: raw}).IncentiveFraction())
	}
}

// Q12 for the points board: an anonymous row never carries its network id;
// a named row and the caller's own row do, and the caller's row says is_me.
func TestPointsLeaderboardRowPrivacy(t *testing.T) {
	anonymousId, publicId, callerId := server.NewId(), server.NewId(), server.NewId()
	anonymous := pointsLeaderboardRowFromModel(&model.PointsLeaderboardRow{
		PointsLeaderboardEntry: model.PointsLeaderboardEntry{NetworkId: anonymousId, TotalNanoPoints: 1},
		NetworkName:            "hidden",
	}, model.PointsLeaderboardSortPoints, &callerId)
	connect.AssertEqual(t, anonymous.NetworkId, (*server.Id)(nil))
	connect.AssertEqual(t, anonymous.IsMe, false)
	anonymousJson, err := json.Marshal(anonymous)
	connect.AssertEqual(t, err, nil)
	if strings.Contains(string(anonymousJson), "network_id") || strings.Contains(string(anonymousJson), anonymousId.String()) {
		t.Fatalf("an anonymous points row identifies its network: %s", anonymousJson)
	}
	public := pointsLeaderboardRowFromModel(&model.PointsLeaderboardRow{
		PointsLeaderboardEntry:  model.PointsLeaderboardEntry{NetworkId: publicId, TotalNanoPoints: 1},
		NetworkName:             "shown",
		PointsLeaderboardPublic: true,
	}, model.PointsLeaderboardSortPoints, nil)
	connect.AssertEqual(t, *public.NetworkId, publicId)
	connect.AssertEqual(t, public.IsMe, false)
	own := pointsLeaderboardRowFromModel(&model.PointsLeaderboardRow{
		PointsLeaderboardEntry: model.PointsLeaderboardEntry{NetworkId: callerId, TotalNanoPoints: 1},
		NetworkName:            "mine",
	}, model.PointsLeaderboardSortPoints, &callerId)
	// the apps find their own row by its id, so the caller's anonymous row
	// keeps it, and is_me says so outright
	connect.AssertEqual(t, *own.NetworkId, callerId)
	connect.AssertEqual(t, own.IsMe, true)
	connect.AssertEqual(t, own.NetworkName, "")
	connect.AssertEqual(t, own.Anonymous, true)
}

// Contract amendment 2026-10-29: fleets[].incentive is the UID's share of
// SN25 incentive as a fraction from 0 to 1, whatever unit the chain read
// keeps: subtensor's u16 share of 65535 becomes 0..1, and the wire carries
// the fraction the apps show as a percent.
func TestSnTopIncentiveIsAFraction(t *testing.T) {
	connect.AssertEqual(t, SnTopIncentiveFraction(0), 0.0)
	connect.AssertEqual(t, SnTopIncentiveFraction(65535), 1.0)
	snTopFractionNear := func(got float64, want float64) {
		t.Helper()
		if got < want-0.0001 || want+0.0001 < got {
			t.Fatalf("incentive fraction %v, want %v", got, want)
		}
	}
	// 1180 of 65535 is the "1.8%" of the contract's example
	snTopFractionNear(SnTopIncentiveFraction(1180), 0.018)
	snTopFractionNear(SnTopIncentiveFraction(32768), 0.5)

	key := model.SnTopFleetKey{Uid: 42, Hotkey: [32]byte{9}}
	fleets := snTopAttachChainData(
		[]model.SnTopFleetInput{{Key: key}},
		nil,
		&SnTopIncentive{ByUid: map[uint16]uint16{42: 1180}, Holders: map[uint16][32]byte{42: key.Hotkey}},
	)
	snTopFractionNear(*fleets[0].Incentive, 0.018)
	if *fleets[0].Incentive < 0 || 1 < *fleets[0].Incentive {
		t.Fatalf("incentive %v is not a fraction", *fleets[0].Incentive)
	}
	// on the wire, as the apps read it
	row := snTopRowFromModel(&model.SnTopNetworkRow{SnTopNetworkProjection: model.SnTopNetworkProjection{Position: 1, Claimed: true}}, []*model.SnTopFleetRow{{
		Key:       key,
		Incentive: fleets[0].Incentive,
	}}, nil)
	rowJson, err := json.Marshal(row)
	connect.AssertEqual(t, err, nil)
	var decoded struct {
		Fleets []struct {
			Incentive *float64 `json:"incentive"`
		} `json:"fleets"`
	}
	connect.AssertEqual(t, json.Unmarshal(rowJson, &decoded), nil)
	snTopFractionNear(*decoded.Fleets[0].Incentive, 0.018)
}
