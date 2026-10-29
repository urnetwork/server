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
	fleets := []model.SnTopFleetInput{{Key: model.SnTopFleetKey{Uid: 7}}, {Key: model.SnTopFleetKey{Uid: 8}}}
	untouched := snTopAttachChainData(append([]model.SnTopFleetInput{}, fleets...), nil, nil)
	connect.AssertEqual(t, untouched[0].ValidatorScore, (*float64)(nil))
	connect.AssertEqual(t, untouched[0].Selected, (*bool)(nil))
	connect.AssertEqual(t, untouched[0].Incentive, (*float64)(nil))

	summary := &SnTopValidatorSummary{Entries: []SnTopValidatorSummaryEntry{
		{Rank: 1, Uid: 7, Score: big.NewRat(3, 2), Selected: true},
		{Rank: 2, Uid: 9, Score: big.NewRat(1, 2), Selected: false},
	}}
	incentive := &SnTopIncentive{ByUid: map[uint16]float64{7: 0.25}, Stale: true}
	attached := snTopAttachChainData(append([]model.SnTopFleetInput{}, fleets...), summary, incentive)
	connect.AssertEqual(t, *attached[0].ValidatorScore, 1.5)
	connect.AssertEqual(t, *attached[0].ValidatorRank, 1)
	connect.AssertEqual(t, *attached[0].ValidatorRankOf, 2)
	connect.AssertEqual(t, *attached[0].Selected, true)
	connect.AssertEqual(t, *attached[0].Incentive, 0.25)
	connect.AssertEqual(t, attached[0].IncentiveStale, true)
	// uid 8 is not eligible at the validator: score 0, not selected, no rank
	connect.AssertEqual(t, *attached[1].ValidatorScore, 0.0)
	connect.AssertEqual(t, attached[1].ValidatorRank, (*int)(nil))
	connect.AssertEqual(t, *attached[1].Selected, false)
	connect.AssertEqual(t, attached[1].Incentive, (*float64)(nil))

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
