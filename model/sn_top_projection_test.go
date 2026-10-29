package model

import (
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func snTopTestPrefix(n byte) [32]byte {
	var h [32]byte
	h[0] = n
	h[31] = n
	return h
}

func snTopTestId(n int) server.Id {
	var id server.Id
	id[14] = byte(n >> 8)
	id[15] = byte(n)
	return id
}

func snTopNear(t *testing.T, got float64, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func snTopRowOf(t *testing.T, projection *SnTopProjection, networkId server.Id) *SnTopNetworkProjection {
	t.Helper()
	for _, row := range projection.Networks {
		if row.NetworkId == networkId {
			return row
		}
	}
	t.Fatalf("network %s has no row", networkId)
	return nil
}

func snTopProvider(clientId server.Id, networkId server.Id, prefix [32]byte, presence float64, measured bool) SnTopProvider {
	return SnTopProvider{
		ClientId:   clientId,
		NetworkId:  networkId,
		Eligible:   true,
		PrefixHash: prefix,
		HasPrefix:  true,
		Presence:   presence,
		Measured:   measured,
	}
}

// Each prefix is split among the units present in it, and a network's
// providers in one prefix count once at their highest presence (§5.4).
func TestSnTopProjectionSplitsSharedPrefixes(t *testing.T) {
	a, b, c := snTopTestId(1), snTopTestId(2), snTopTestId(3)
	shared, aOwn, cOwn := snTopTestPrefix(1), snTopTestPrefix(2), snTopTestPrefix(3)
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 10,
		Providers: []SnTopProvider{
			snTopProvider(snTopTestId(101), a, shared, 1, true),
			snTopProvider(snTopTestId(102), a, aOwn, 1, true),
			// a second provider of a in the same prefix does not add to it
			snTopProvider(snTopTestId(103), a, aOwn, 0.5, true),
			snTopProvider(snTopTestId(201), b, shared, 1, true),
			snTopProvider(snTopTestId(301), c, cOwn, 0.75, true),
		},
	})
	connect.AssertEqual(t, projection.TotalRanked, 3)
	rowA := snTopRowOf(t, projection, a)
	snTopNear(t, rowA.RawLastTempo, 1.5)
	connect.AssertEqual(t, rowA.Prefixes, 2)
	connect.AssertEqual(t, rowA.SharedPrefixes, 1)
	connect.AssertEqual(t, rowA.SharedWithUnits, 1)
	rowB := snTopRowOf(t, projection, b)
	snTopNear(t, rowB.RawLastTempo, 0.5)
	connect.AssertEqual(t, rowB.SharedPrefixes, 1)
	rowC := snTopRowOf(t, projection, c)
	snTopNear(t, rowC.RawLastTempo, 0.75)
	connect.AssertEqual(t, rowC.SharedPrefixes, 0)
	snTopNear(t, rowC.Presence, 0.75)
	// a new unit starts at its raw score, and positions are a total order
	snTopNear(t, rowA.ProjectedScore, 1.5)
	connect.AssertEqual(t, rowA.Position, 1)
	connect.AssertEqual(t, rowC.Position, 2)
	connect.AssertEqual(t, rowB.Position, 3)
}

// A unit counts in claim(h) only from presence ½ (Q4), and claim(h) is at
// least 1.
func TestSnTopProjectionClaimNeedsHalfPresence(t *testing.T) {
	a, b := snTopTestId(1), snTopTestId(2)
	h := snTopTestPrefix(9)
	below := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 1,
		Providers: []SnTopProvider{
			snTopProvider(snTopTestId(11), a, h, 1, true),
			snTopProvider(snTopTestId(21), b, h, 0.4, true),
		},
	})
	snTopNear(t, snTopRowOf(t, below, a).RawLastTempo, 1)
	snTopNear(t, snTopRowOf(t, below, b).RawLastTempo, 0.4)
	connect.AssertEqual(t, snTopRowOf(t, below, a).SharedPrefixes, 0)

	at := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 1,
		Providers: []SnTopProvider{
			snTopProvider(snTopTestId(11), a, h, 1, true),
			snTopProvider(snTopTestId(21), b, h, 0.5, true),
		},
	})
	snTopNear(t, snTopRowOf(t, at, a).RawLastTempo, 0.5)
	snTopNear(t, snTopRowOf(t, at, b).RawLastTempo, 0.25)
	connect.AssertEqual(t, snTopRowOf(t, at, a).SharedPrefixes, 1)

	// alone below ½ still divides by 1, never by 0
	alone := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 1,
		Providers:  []SnTopProvider{snTopProvider(snTopTestId(21), b, h, 0.25, true)},
	})
	snTopNear(t, snTopRowOf(t, alone, b).RawLastTempo, 0.25)
}

// Complete-trail hops replace the projection for cohort members, and only
// for them (§5.3).
func TestSnTopProjectionTrailDataWinsForCohortMembers(t *testing.T) {
	measured, unmeasured := snTopTestId(1), snTopTestId(2)
	pin, hop1, hop2 := snTopTestPrefix(1), snTopTestPrefix(2), snTopTestPrefix(3)
	cohortProvider := snTopProvider(snTopTestId(11), measured, pin, 0.2, true)
	cohortProvider.TrailPrefixes = [][32]byte{hop1, hop2}
	// trail evidence for a provider outside the cohort is ignored: the
	// validator never measures it
	outsideProvider := snTopProvider(snTopTestId(21), unmeasured, snTopTestPrefix(4), 0.6, false)
	outsideProvider.TrailPrefixes = [][32]byte{snTopTestPrefix(5)}
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 3,
		Providers:  []SnTopProvider{cohortProvider, outsideProvider},
	})
	rowMeasured := snTopRowOf(t, projection, measured)
	snTopNear(t, rowMeasured.RawLastTempo, 2)
	connect.AssertEqual(t, rowMeasured.Prefixes, 2)
	connect.AssertEqual(t, rowMeasured.Measurable, true)
	rowOutside := snTopRowOf(t, projection, unmeasured)
	snTopNear(t, rowOutside.RawLastTempo, 0.6)
	connect.AssertEqual(t, rowOutside.Measurable, false)
	connect.AssertEqual(t, rowOutside.Label, SnTopLabelNotMeasurable)
	connect.AssertEqual(t, rowOutside.NotMeasurableReason, SnTopReasonNotInCohort)
	connect.AssertEqual(t, rowOutside.ClaimPrompt, false)
}

// The EMA folds ¼ of each tempo; a unit with no presence decays by ¾ and is
// dropped below the floor (§5.5).
func TestSnTopProjectionEma(t *testing.T) {
	a, b, gone := snTopTestId(1), snTopTestId(2), snTopTestId(3)
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 20,
		Providers: []SnTopProvider{
			snTopProvider(snTopTestId(11), a, snTopTestPrefix(1), 1, true),
		},
		PriorNetworks: map[server.Id]SnTopUnitState{
			a:    {Ema: 3, SoloEma: 3, LastTempoIndex: 19, LastPosition: 2},
			b:    {Ema: 2, SoloEma: 2, LastTempoIndex: 19, LastPosition: 1},
			gone: {Ema: snTopEmaFloor, SoloEma: snTopEmaFloor, LastTempoIndex: 19, LastPosition: 3},
		},
	})
	rowA := snTopRowOf(t, projection, a)
	snTopNear(t, rowA.ProjectedScore, 0.25*1+0.75*3)
	snTopNear(t, rowA.RawLastTempo, 1)
	rowB := snTopRowOf(t, projection, b)
	snTopNear(t, rowB.ProjectedScore, 0.75*2)
	snTopNear(t, rowB.RawLastTempo, 0)
	connect.AssertEqual(t, rowB.VerifyEligible, false)
	connect.AssertEqual(t, rowB.NotMeasurableReason, SnTopReasonNotProviding)
	// a moved up, b moved down
	connect.AssertEqual(t, rowA.Position, 1)
	connect.AssertEqual(t, rowA.Trend, 1)
	connect.AssertEqual(t, rowB.Trend, -1)
	// gone fell below the floor: no row and no state
	connect.AssertEqual(t, projection.TotalRanked, 2)
	_, kept := projection.NetworkStates[gone]
	connect.AssertEqual(t, kept, false)
	connect.AssertEqual(t, projection.NetworkStates[a].LastTempoIndex, int64(20))
	connect.AssertEqual(t, projection.NetworkStates[a].LastPosition, 1)
	snTopNear(t, SnTopFoldEma(0.25, 4, 0, false), 4)
}

// Ties go to the older network, then the network id (§5.5).
func TestSnTopProjectionTiesGoToTheOlderNetwork(t *testing.T) {
	older, newer, unknown := snTopTestId(9), snTopTestId(1), snTopTestId(0)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	providers := []SnTopProvider{}
	for i, networkId := range []server.Id{older, newer, unknown} {
		providers = append(providers, snTopProvider(snTopTestId(100+i), networkId, snTopTestPrefix(byte(10+i)), 1, true))
	}
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 1,
		Providers:  providers,
		NetworkCreateTimes: map[server.Id]time.Time{
			older: created,
			newer: created.Add(time.Hour),
		},
	})
	connect.AssertEqual(t, snTopRowOf(t, projection, older).Position, 1)
	connect.AssertEqual(t, snTopRowOf(t, projection, newer).Position, 2)
	// an unknown age ranks after a known one
	connect.AssertEqual(t, snTopRowOf(t, projection, unknown).Position, 3)

	// equal ages fall back to the network id
	same := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 1,
		Providers:  providers[:2],
		NetworkCreateTimes: map[server.Id]time.Time{
			older: created,
			newer: created,
		},
	})
	connect.AssertEqual(t, snTopRowOf(t, same, newer).Position, 1)
	connect.AssertEqual(t, snTopRowOf(t, same, older).Position, 2)
}

// The cutoff is the 200th score; margins, the claim prompt and open seats
// follow it (§5.5, Q5, Q13).
func TestSnTopProjectionCutoffMarginAndClaimPrompt(t *testing.T) {
	providers := []SnTopProvider{}
	priors := map[server.Id]SnTopUnitState{}
	for i := 1; i <= 210; i += 1 {
		networkId := snTopTestId(i)
		// network i scores 211 - i, so position i; odd networks are outside
		// the cohort
		priors[networkId] = SnTopUnitState{Ema: float64(211 - i), LastTempoIndex: 4}
		provider := snTopProvider(snTopTestId(1000+i), networkId, snTopTestPrefix(byte(i)), 1, i%2 == 0)
		// no prefix: raw 0, so every score decays by the same ¾
		provider.HasPrefix = false
		providers = append(providers, provider)
	}
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex:    5,
		Providers:     providers,
		PriorNetworks: priors,
	})
	connect.AssertEqual(t, projection.TotalRanked, 210)
	// every EMA decayed by ¾ (raw 0), the order is unchanged
	snTopNear(t, projection.ProjectedCutoff, 0.75*11)
	connect.AssertEqual(t, projection.OpenSeats, 200)
	connect.AssertEqual(t, projection.ClaimedFleets, 0)
	if projection.ClaimedCutoff != nil {
		t.Fatal("claimed cutoff without a claimed fleet")
	}
	first := projection.Networks[0]
	connect.AssertEqual(t, first.Position, 1)
	snTopNear(t, first.Margin, (0.75*210-0.75*11)/(0.75*11))
	// before precision is measured every measurable row is an estimate
	at200 := projection.Networks[199]
	connect.AssertEqual(t, at200.Position, 200)
	snTopNear(t, at200.Margin, 0)
	connect.AssertEqual(t, at200.Label, SnTopLabelEstimate)
	connect.AssertEqual(t, at200.ClaimPrompt, true)
	at199 := projection.Networks[198]
	connect.AssertEqual(t, at199.Measurable, false)
	connect.AssertEqual(t, at199.ClaimPrompt, false)
	connect.AssertEqual(t, at199.Label, SnTopLabelNotMeasurable)
	at202 := projection.Networks[201]
	connect.AssertEqual(t, at202.Position, 202)
	connect.AssertEqual(t, at202.Measurable, true)
	connect.AssertEqual(t, at202.ClaimPrompt, false)
}

// The labels follow Q7, Q13 and Q17 exactly.
func TestSnTopLabels(t *testing.T) {
	low, high := 0.89, 0.9
	all := uint8(0x0f)
	two := uint8(0x05)
	for _, c := range []struct {
		measurable bool
		margin     float64
		bits       uint8
		precision  *float64
		want       string
	}{
		{false, 1, all, &high, SnTopLabelNotMeasurable},
		{true, 1, all, nil, SnTopLabelEstimate},
		{true, 1, all, &low, SnTopLabelEstimate},
		{true, -1, all, &low, SnTopLabelEstimate},
		{true, 0.20, all, &high, SnTopLabelLikely},
		{true, 0.5, uint8(0x07), &high, SnTopLabelLikely},
		// inside by the margin but measurable in only 2 of the last 4 tempos
		{true, 0.5, two, &high, SnTopLabelBorderline},
		{true, 0.19, all, &high, SnTopLabelBorderline},
		{true, -0.19, all, &high, SnTopLabelBorderline},
		{true, -0.20, all, &high, SnTopLabelUnlikely},
		{true, -0.8, all, &high, SnTopLabelUnlikely},
	} {
		got := SnTopLabel(c.measurable, c.margin, c.bits, c.precision)
		if got != c.want {
			t.Errorf("label(%t, %v, %04b, %v) = %s, want %s", c.measurable, c.margin, c.bits, c.precision, got, c.want)
		}
	}
	connect.AssertEqual(t, SnTopClaimPrompt(true, 200, false), true)
	connect.AssertEqual(t, SnTopClaimPrompt(true, 201, false), false)
	connect.AssertEqual(t, SnTopClaimPrompt(false, 1, false), false)
	connect.AssertEqual(t, SnTopClaimPrompt(true, 0, false), false)
	// a network that already claimed is not prompted to claim
	connect.AssertEqual(t, SnTopClaimPrompt(true, 1, true), false)
	snTopNear(t, SnTopMargin(12, 10), 0.2)
	snTopNear(t, SnTopMargin(8, 10), -0.2)
	snTopNear(t, SnTopMargin(3, 0), 1)
	snTopNear(t, SnTopMargin(0, 0), 0)
}

// "Likely" needs measurability in 3 of the last 4 tempos, counted across
// gaps in the folding.
func TestSnTopMeasurableHistory(t *testing.T) {
	prior := SnTopUnitState{LastTempoIndex: 10, MeasurableBits: 0x03}
	connect.AssertEqual(t, snTopMeasurableBits(prior, true, 11, true), uint8(0x07))
	connect.AssertEqual(t, snTopMeasurableBits(prior, true, 11, false), uint8(0x06))
	// two tempos missed
	connect.AssertEqual(t, snTopMeasurableBits(prior, true, 13, true), uint8(0x09))
	// a long gap forgets everything
	connect.AssertEqual(t, snTopMeasurableBits(prior, true, 40, true), uint8(0x01))
	// refolding the same tempo replaces its own bit
	connect.AssertEqual(t, snTopMeasurableBits(prior, true, 10, false), uint8(0x02))
	connect.AssertEqual(t, snTopMeasurableBits(SnTopUnitState{}, false, 5, true), uint8(0x01))
	// only four tempos are kept
	connect.AssertEqual(t, snTopMeasurableBits(SnTopUnitState{LastTempoIndex: 1, MeasurableBits: 0x0f}, true, 2, true), uint8(0x0f))

	// three measurable tempos in a row make a network inside by the margin
	// likely once precision is good
	precision := 0.95
	a := snTopTestId(1)
	priors := map[server.Id]SnTopUnitState{a: {Ema: 1, SoloEma: 1, LastTempoIndex: 7, MeasurableBits: 0x03}}
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex:     8,
		Providers:      []SnTopProvider{snTopProvider(snTopTestId(11), a, snTopTestPrefix(1), 1, true)},
		PriorNetworks:  priors,
		PrecisionAt200: &precision,
	})
	row := snTopRowOf(t, projection, a)
	// alone: no cutoff, so a full margin inside
	connect.AssertEqual(t, row.Label, SnTopLabelLikely)
	connect.AssertEqual(t, projection.NetworkStates[a].MeasurableBits, uint8(0x07))
}

// Q4c: the solo score splits a prefix only with the fleets that already
// claimed it, while the ranking splits it with every unit.
func TestSnTopProjectionSoloScoreAndClaimedFleets(t *testing.T) {
	mine, other, claimedNet := snTopTestId(1), snTopTestId(2), snTopTestId(3)
	h1, h2 := snTopTestPrefix(1), snTopTestPrefix(2)
	fleetClient := snTopTestId(31)
	key := SnTopFleetKey{FleetId: snTopTestPrefix(7), Hotkey: snTopTestPrefix(8), Generation: 1, Uid: 42}
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 2,
		Providers: []SnTopProvider{
			snTopProvider(snTopTestId(11), mine, h1, 1, true),
			snTopProvider(snTopTestId(12), mine, h2, 1, true),
			// an unclaimed network shares h1
			snTopProvider(snTopTestId(21), other, h1, 1, true),
			// a claimed fleet shares h2
			snTopProvider(fleetClient, claimedNet, h2, 1, true),
		},
		Fleets: []SnTopFleetInput{{Key: key, ClientIds: []server.Id{fleetClient}, NetworkIds: []server.Id{claimedNet}, ValidToEpoch: 9}},
	})
	row := snTopRowOf(t, projection, mine)
	// ranking: h1 split with other, h2 split with the fleet
	snTopNear(t, row.RawLastTempo, 1)
	// solo: only the fleet already claimed, so h1 counts whole
	snTopNear(t, row.SoloScore, 1.5)
	connect.AssertEqual(t, row.SharedWithUnits, 2)
	connect.AssertEqual(t, row.Claimed, false)
	claimedRow := snTopRowOf(t, projection, claimedNet)
	connect.AssertEqual(t, claimedRow.Claimed, true)
	// measurable and inside the head, but it already claimed: no prompt;
	// the unclaimed networks beside it are prompted
	connect.AssertEqual(t, claimedRow.Measurable, true)
	connect.AssertEqual(t, claimedRow.Position <= SnTopHeadSeats, true)
	connect.AssertEqual(t, claimedRow.ClaimPrompt, false)
	connect.AssertEqual(t, row.ClaimPrompt, true)
	connect.AssertEqual(t, snTopRowOf(t, projection, other).ClaimPrompt, true)
	// the claimed network's own fleet is not "shared with others"
	connect.AssertEqual(t, claimedRow.SharedWithUnits, 1)
	snTopNear(t, claimedRow.RawLastTempo, 0.5)
	connect.AssertEqual(t, projection.ClaimedFleets, 1)
	connect.AssertEqual(t, projection.OpenSeats, 199)
	connect.AssertEqual(t, len(projection.Fleets), 1)
	// no trail hops: the fleet's exact replica is 0 and it is not selected
	connect.AssertEqual(t, projection.Fleets[0].ReplicaExact.Sign(), 0)
	connect.AssertEqual(t, projection.Fleets[0].ReplicaSelected, false)
	if projection.ClaimedCutoff != nil {
		t.Fatal("no fleet is selected, so there is no claimed floor")
	}
}

// The claimed fleets' replica is the validator's exact arithmetic over trail
// hops only (§5.8): shared prefixes split exactly, a live fleet with no hops
// decays by exactly ¾, and selection ties go to the lower UID.
func TestSnTopProjectionFleetReplicaIsExact(t *testing.T) {
	networkA, networkB := snTopTestId(1), snTopTestId(2)
	clientA, clientB := snTopTestId(11), snTopTestId(21)
	keyA := SnTopFleetKey{FleetId: snTopTestPrefix(1), Hotkey: snTopTestPrefix(2), Generation: 1, Uid: 9}
	keyB := SnTopFleetKey{FleetId: snTopTestPrefix(3), Hotkey: snTopTestPrefix(4), Generation: 1, Uid: 7}
	shared, onlyA := snTopTestPrefix(5), snTopTestPrefix(6)
	providerA := snTopProvider(clientA, networkA, onlyA, 1, true)
	providerA.TrailPrefixes = [][32]byte{shared, onlyA}
	providerB := snTopProvider(clientB, networkB, shared, 1, true)
	providerB.TrailPrefixes = [][32]byte{shared}
	fleets := []SnTopFleetInput{
		{Key: keyA, ClientIds: []server.Id{clientA}, NetworkIds: []server.Id{networkA}},
		{Key: keyB, ClientIds: []server.Id{clientB}, NetworkIds: []server.Id{networkB}},
	}
	first := ProjectSnTop(&SnTopProjectionInput{TempoIndex: 1, Providers: []SnTopProvider{providerA, providerB}, Fleets: fleets})
	connect.AssertEqual(t, first.FleetStates[keyA].EmaExact.Cmp(big.NewRat(3, 2)), 0)
	connect.AssertEqual(t, first.FleetStates[keyB].EmaExact.Cmp(big.NewRat(1, 2)), 0)
	connect.AssertEqual(t, len(first.Fleets), 2)
	// fleets list by UID: 7 then 9
	connect.AssertEqual(t, first.Fleets[0].Fleet.Key.Uid, uint16(7))
	for _, fleet := range first.Fleets {
		connect.AssertEqual(t, fleet.ReplicaSelected, true)
	}
	connect.AssertEqual(t, *first.ClaimedCutoff, 0.5)

	// next tempo, no hops: both decay by exactly ¾
	second := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex:  2,
		Providers:   []SnTopProvider{snTopProvider(clientA, networkA, onlyA, 1, true), snTopProvider(clientB, networkB, shared, 1, true)},
		Fleets:      fleets,
		PriorFleets: first.FleetStates,
	})
	connect.AssertEqual(t, second.FleetStates[keyA].EmaExact.Cmp(big.NewRat(9, 8)), 0)
	connect.AssertEqual(t, second.FleetStates[keyB].EmaExact.Cmp(big.NewRat(3, 8)), 0)
}

func TestSnTopHeadMathMatchesTheValidator(t *testing.T) {
	headMath := GetSnTopHeadMath()
	connect.AssertEqual(t, headMath.Alpha().Cmp(big.NewRat(1, 4)), 0)
	a := SnTopFleetKey{Uid: 1}
	b := SnTopFleetKey{Uid: 2}
	c := SnTopFleetKey{Uid: 3}
	h1, h2, h3 := snTopTestPrefix(1), snTopTestPrefix(2), snTopTestPrefix(3)
	raw := headMath.RawScores(map[SnTopFleetKey]map[[32]byte]bool{
		a: {h1: true, h2: true},
		b: {h2: true, h3: true},
		c: {h2: true},
	})
	connect.AssertEqual(t, raw[a].Cmp(big.NewRat(4, 3)), 0)
	connect.AssertEqual(t, raw[b].Cmp(big.NewRat(4, 3)), 0)
	connect.AssertEqual(t, raw[c].Cmp(big.NewRat(1, 3)), 0)

	// a new identity starts at its raw score; a prior with raw folds ¼; a
	// prior without raw decays ¾; a zero result is dropped
	next, err := headMath.Fold(
		map[SnTopFleetKey]*big.Rat{a: big.NewRat(2, 1), b: big.NewRat(1, 1), c: new(big.Rat)},
		map[SnTopFleetKey]*big.Rat{b: big.NewRat(1, 1), {Uid: 4}: big.NewRat(4, 1)},
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, next[a].Cmp(big.NewRat(2, 1)), 0)
	connect.AssertEqual(t, next[b].Cmp(big.NewRat(1, 1)), 0)
	_, cKept := next[c]
	connect.AssertEqual(t, cKept, false)
	connect.AssertEqual(t, next[SnTopFleetKey{Uid: 4}].Cmp(big.NewRat(3, 1)), 0)
	_, err = headMath.Fold(map[SnTopFleetKey]*big.Rat{a: big.NewRat(-1, 1)}, nil)
	connect.AssertNotEqual(t, err, nil)

	// selection: positive scores, descending, ties to the lower UID
	selected, rejected, err := headMath.Select(map[uint16]*big.Rat{
		5: big.NewRat(1, 2),
		3: big.NewRat(1, 2),
		9: big.NewRat(2, 1),
		1: new(big.Rat),
	}, 2)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, selected, []uint16{9, 3})
	connect.AssertEqual(t, rejected, []uint16{5})
	_, _, err = headMath.Select(nil, 0)
	connect.AssertNotEqual(t, err, nil)
}

func TestSnTopFleetKeyRoundTrip(t *testing.T) {
	key := SnTopFleetKey{FleetId: snTopTestPrefix(1), Hotkey: snTopTestPrefix(2), Generation: 1 << 60, Uid: 65535}
	parsed, err := ParseSnTopFleetKey(key.String())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed, key)
	for _, bad := range []string{"", "a:b:c", fmt.Sprintf("%x:%x:1:70000", key.FleetId, key.Hotkey), "zz:yy:1:1"} {
		_, err := ParseSnTopFleetKey(bad)
		connect.AssertNotEqual(t, err, nil)
	}
	value := big.NewRat(-7, 21)
	parsedValue, err := parseSnTopRat(snTopRatString(value))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsedValue.Cmp(value), 0)
	_, err = parseSnTopRat("1/0")
	connect.AssertNotEqual(t, err, nil)
}

// A network that provides but has no verify-eligible provider is listed
// unranked as sharing its egress; it never takes a position.
func TestSnTopProjectionSharedEgressNetworks(t *testing.T) {
	ranked, shared := snTopTestId(1), snTopTestId(2)
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex:        1,
		Providers:         []SnTopProvider{snTopProvider(snTopTestId(11), ranked, snTopTestPrefix(1), 1, true)},
		ProvidingNetworks: map[server.Id]bool{ranked: true, shared: true},
	})
	connect.AssertEqual(t, projection.TotalRanked, 1)
	row := snTopRowOf(t, projection, shared)
	connect.AssertEqual(t, row.Position, 0)
	connect.AssertEqual(t, row.Label, SnTopLabelNotMeasurable)
	connect.AssertEqual(t, row.NotMeasurableReason, SnTopReasonSharedEgress)
	connect.AssertEqual(t, projection.Networks[len(projection.Networks)-1].NetworkId, shared)
}

// The same inputs always produce the same rows, whatever the map order.
func TestSnTopProjectionIsDeterministic(t *testing.T) {
	providers := []SnTopProvider{}
	for i := 0; i < 300; i += 1 {
		providers = append(providers, snTopProvider(snTopTestId(1000+i), snTopTestId(i%37), snTopTestPrefix(byte(i%53)), float64(i%10)/9, i%3 == 0))
	}
	first := ProjectSnTop(&SnTopProjectionInput{TempoIndex: 1, Providers: providers})
	for run := 0; run < 5; run += 1 {
		again := ProjectSnTop(&SnTopProjectionInput{TempoIndex: 1, Providers: providers})
		connect.AssertEqual(t, len(again.Networks), len(first.Networks))
		for i := range first.Networks {
			if *again.Networks[i] != *first.Networks[i] {
				t.Fatalf("run %d row %d differs: %+v vs %+v", run, i, again.Networks[i], first.Networks[i])
			}
		}
	}
}
