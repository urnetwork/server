package model

// The SN Top board's projection (LEADERTOP.md §5, owner decisions Q2-Q7,
// Q11, Q13 and Q17 of 2026-10-29). Pure: one tempo's evidence and the prior
// EMA state in, the ranked rows and the next state out. The controller reads
// the inputs and writes the outputs (controller/sn_top_controller.go).
//
// Each network is scored as one hypothetical fleet of its providers:
//
//	raw(n) = Σ_h p_n(h) / claim(h)
//
// p_n(h) is the highest presence among the network's providers in prefix h,
// the /29 or /48 egress prefix each provider pinned for the settlement
// window. A provider of the verify cohort whose hops of complete trails
// showed prefixes in the tempo is scored by those prefixes at presence 1
// instead (trail data wins). claim(h) counts the ranked units with presence
// of at least ½ in h, and is at least 1. The units are the claimed fleets and
// each network's providers outside every claimed fleet, so the split assumes
// everyone sharing a prefix claims (Q4).

import (
	"bytes"
	"math"
	"math/big"
	"math/bits"
	"slices"
	"sort"
	"time"

	"github.com/urnetwork/server"
)

const (
	// the head seats (sn protocol maximum_head_fleets on mainnet)
	SnTopHeadSeats = 200
	// the positions /sn/top pages through (Q5 addendum: the top 400)
	SnTopPagedPositions = 400
	// the label margin M (Q13)
	SnTopLabelMargin = 0.20
	// "likely" turns on once the measured precision at 200 reaches this (Q17)
	SnTopLikelyPrecision = 0.9
	// a unit counts in a prefix's claim from this presence (Q4)
	snTopClaimPresence = 0.5
	// an EMA below this is dropped: a unit absent for about 50 tempos
	snTopEmaFloor = 1e-6
	// "likely" needs measurability in this many of the last four tempos
	snTopLikelyMeasurableTempos = 3
	snTopMeasurableBitsMask     = 0x0f
)

const (
	SnTopLabelEstimate      = "estimate"
	SnTopLabelLikely        = "likely"
	SnTopLabelBorderline    = "borderline"
	SnTopLabelUnlikely      = "unlikely"
	SnTopLabelNotMeasurable = "not_measurable"
)

const (
	SnTopReasonNone         = ""
	SnTopReasonNotInCohort  = "not_in_cohort"
	SnTopReasonSharedEgress = "shared_egress"
	SnTopReasonNotProviding = "not_providing"
)

// SnTopProvider is one provider's evidence for a tempo.
type SnTopProvider struct {
	ClientId  server.Id
	NetworkId server.Id
	// in the verify-eligible set when the tempo was folded
	Eligible bool
	// the prefix hash pinned for the settlement window
	PrefixHash [32]byte
	HasPrefix  bool
	// the fraction of the tempo it was connected and providing, [0, 1]
	Presence float64
	// in the verify cohort: the validator can measure it
	Measured bool
	// the prefixes its non-seed hops of complete trails showed in the tempo
	TrailPrefixes [][32]byte
}

// SnTopFleetInput is one live claimed fleet identity and what the validator
// and the chain say about it. The nullable fields stay nil until their source
// has data.
type SnTopFleetInput struct {
	Key          SnTopFleetKey
	ValidToEpoch uint64
	ClientIds    []server.Id
	NetworkIds   []server.Id

	ValidatorScore  *float64
	ValidatorRank   *int
	ValidatorRankOf *int
	Selected        *bool
	Incentive       *float64
	IncentiveStale  bool
}

// SnTopUnitState is a unit's persisted EMA state.
type SnTopUnitState struct {
	Ema float64
	// fleets only: the exact EMA
	EmaExact       *big.Rat
	SoloEma        float64
	LastTempoIndex int64
	LastPosition   int
	// bit i: measurable i tempos before LastTempoIndex
	MeasurableBits uint8
}

type SnTopProjectionInput struct {
	TempoIndex int64
	Providers  []SnTopProvider
	// networks connected and providing in the tempo (per-minute reliability)
	ProvidingNetworks map[server.Id]bool
	Fleets            []SnTopFleetInput
	// network ties go to the older network
	NetworkCreateTimes map[server.Id]time.Time
	PriorNetworks      map[server.Id]SnTopUnitState
	PriorFleets        map[SnTopFleetKey]SnTopUnitState
	// Nil while complete authenticated evidence or its predecessor is missing.
	Replica *SnTopReplica
	// the measured precision at 200, nil until fleets bind (Q17)
	PrecisionAt200 *float64
	Math           SnTopHeadMath
}

// SnTopNetworkProjection is one network's row.
type SnTopNetworkProjection struct {
	NetworkId server.Id
	// 1-based place in the total order; 0 for an unranked row
	Position            int
	ProjectedScore      float64
	RawLastTempo        float64
	SoloScore           float64
	Margin              float64
	Trend               int
	Prefixes            int
	SharedPrefixes      int
	SharedWithUnits     int
	Presence            float64
	Label               string
	Measurable          bool
	VerifyEligible      bool
	ClaimPrompt         bool
	Claimed             bool
	NotMeasurableReason string
}

// SnTopFleetProjection is one claimed fleet's row: its exact replica.
type SnTopFleetProjection struct {
	Fleet            SnTopFleetInput
	ReplicaScore     float64
	ReplicaExact     *big.Rat
	ReplicaRaw       float64
	ReplicaSelected  bool
	ReplicaAvailable bool
}

type SnTopProjection struct {
	// ranked rows by position, then the unranked rows
	Networks          []*SnTopNetworkProjection
	Fleets            []*SnTopFleetProjection
	TotalRanked       int
	ProjectedCutoff   float64
	ClaimedCutoff     *float64
	ClaimedFleets     int
	OpenSeats         int
	EligibleProviders int
	NetworkStates     map[server.Id]SnTopUnitState
	FleetStates       map[SnTopFleetKey]SnTopUnitState
	ReplicaUidScores  map[uint16]*big.Rat
}

// prefix presence of one unit
type snTopPresence map[[32]byte]float64

func (self snTopPresence) add(h [32]byte, presence float64) {
	if self[h] < presence {
		self[h] = presence
	}
}

// sortedSnTopPrefixes orders a unit's prefixes so float sums are the same on
// every run.
func sortedSnTopPrefixes(presence snTopPresence) [][32]byte {
	prefixes := make([][32]byte, 0, len(presence))
	for h := range presence {
		prefixes = append(prefixes, h)
	}
	slices.SortFunc(prefixes, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	return prefixes
}

// snTopProviderContribution is what one provider shows in the tempo: its
// complete-trail prefixes when the cohort measured it, else its pinned prefix
// weighted by presence.
func snTopProviderContribution(provider *SnTopProvider) snTopPresence {
	contribution := snTopPresence{}
	if provider.Measured && 0 < len(provider.TrailPrefixes) {
		for _, h := range provider.TrailPrefixes {
			contribution.add(h, 1)
		}
		return contribution
	}
	if provider.HasPrefix && 0 < provider.Presence {
		contribution.add(provider.PrefixHash, min(1, provider.Presence))
	}
	return contribution
}

// SnTopFoldEma is the validator's EMA rule in float: a new unit starts at its
// raw score, a unit with a prior folds α of the new tempo.
func SnTopFoldEma(alpha float64, raw float64, prior float64, hasPrior bool) float64 {
	if !hasPrior {
		return raw
	}
	return alpha*raw + (1-alpha)*prior
}

// snTopMeasurableBits shifts the prior history by the tempos since it was
// last folded and records this tempo.
func snTopMeasurableBits(prior SnTopUnitState, hasPrior bool, tempoIndex int64, measurable bool) uint8 {
	var history uint8
	if hasPrior {
		gap := tempoIndex - prior.LastTempoIndex
		switch {
		case gap <= 0:
			// refolding the same tempo replaces its bit
			history = prior.MeasurableBits &^ 1
		case gap < 8:
			history = prior.MeasurableBits << gap
		}
	}
	if measurable {
		history |= 1
	}
	return history & snTopMeasurableBitsMask
}

// SnTopMargin is the fraction above (+) or below (−) the cutoff. Without a
// cutoff (fewer than 200 ranked units) every positive score is inside by a
// full margin.
func SnTopMargin(score float64, cutoff float64) float64 {
	if cutoff <= 0 {
		if 0 < score {
			return 1
		}
		return 0
	}
	return (score - cutoff) / cutoff
}

// SnTopLabel is the recommendation label (Q7, Q13, Q17). A network inside the
// margin that was not measurable often enough is borderline, never likely.
func SnTopLabel(measurable bool, margin float64, measurableBits uint8, precisionAt200 *float64) string {
	if !measurable {
		return SnTopLabelNotMeasurable
	}
	if precisionAt200 == nil || *precisionAt200 < SnTopLikelyPrecision {
		return SnTopLabelEstimate
	}
	switch {
	case SnTopLabelMargin <= margin:
		if snTopLikelyMeasurableTempos <= bits.OnesCount8(measurableBits&snTopMeasurableBitsMask) {
			return SnTopLabelLikely
		}
		return SnTopLabelBorderline
	case margin <= -SnTopLabelMargin:
		return SnTopLabelUnlikely
	default:
		return SnTopLabelBorderline
	}
}

// SnTopClaimPrompt directs a measurable network inside the head to claim
// (Q13). A network with an active claimed fleet already claimed and is not
// prompted (contract amendment 2026-10-29).
func SnTopClaimPrompt(measurable bool, position int, claimed bool) bool {
	return measurable && 0 < position && position <= SnTopHeadSeats && !claimed
}

// snTopNetworkLess is the total order: score descending, the older network
// first, then the network id.
func snTopNetworkLess(
	a server.Id, aScore float64,
	b server.Id, bScore float64,
	createTimes map[server.Id]time.Time,
) bool {
	if aScore != bScore {
		return bScore < aScore
	}
	aTime, aKnown := createTimes[a]
	bTime, bKnown := createTimes[b]
	switch {
	case aKnown && bKnown && !aTime.Equal(bTime):
		return aTime.Before(bTime)
	case aKnown != bKnown:
		// a known age is older than an unknown one
		return aKnown
	}
	return a.Less(b)
}

// ProjectSnTop computes one tempo's board.
func ProjectSnTop(input *SnTopProjectionInput) *SnTopProjection {
	headMath := input.Math
	if headMath == nil {
		headMath = GetSnTopHeadMath()
	}
	alpha, _ := headMath.Alpha().Float64()

	// fleet membership
	fleetOfClient := map[server.Id][]int{}
	fleetNetworks := make([]map[server.Id]bool, len(input.Fleets))
	for i := range input.Fleets {
		fleet := &input.Fleets[i]
		fleetNetworks[i] = map[server.Id]bool{}
		for _, networkId := range fleet.NetworkIds {
			fleetNetworks[i][networkId] = true
		}
		for _, clientId := range fleet.ClientIds {
			fleetOfClient[clientId] = append(fleetOfClient[clientId], i)
		}
	}

	// each unit's prefix presence
	networkPresence := map[server.Id]snTopPresence{}
	unclaimedPresence := map[server.Id]snTopPresence{}
	fleetPresence := make([]snTopPresence, len(input.Fleets))
	for i := range input.Fleets {
		fleetPresence[i] = snTopPresence{}
	}
	verifyEligible := map[server.Id]bool{}
	measurable := map[server.Id]bool{}
	inputNetworks := map[server.Id]bool{}
	eligibleProviders := 0
	for i := range input.Providers {
		provider := &input.Providers[i]
		networkId := provider.NetworkId
		inputNetworks[networkId] = true
		if provider.Eligible {
			eligibleProviders += 1
			verifyEligible[networkId] = true
		}
		if provider.Measured && (provider.Eligible || 0 < len(provider.TrailPrefixes)) {
			measurable[networkId] = true
		}
		contribution := snTopProviderContribution(provider)
		if networkPresence[networkId] == nil {
			networkPresence[networkId] = snTopPresence{}
		}
		for h, presence := range contribution {
			networkPresence[networkId].add(h, presence)
		}
		fleets := fleetOfClient[provider.ClientId]
		for _, fleetIndex := range fleets {
			for h, presence := range contribution {
				fleetPresence[fleetIndex].add(h, presence)
			}
		}
		if len(fleets) == 0 {
			if unclaimedPresence[networkId] == nil {
				unclaimedPresence[networkId] = snTopPresence{}
			}
			for h, presence := range contribution {
				unclaimedPresence[networkId].add(h, presence)
			}
		}
	}

	// the units holding each prefix at presence ½ or more
	type unit struct {
		fleet     int // -1 for a network's unclaimed providers
		networkId server.Id
	}
	holders := map[[32]byte][]unit{}
	for networkId, presence := range unclaimedPresence {
		for h, p := range presence {
			if snTopClaimPresence <= p {
				holders[h] = append(holders[h], unit{fleet: -1, networkId: networkId})
			}
		}
	}
	for i, presence := range fleetPresence {
		for h, p := range presence {
			if snTopClaimPresence <= p {
				holders[h] = append(holders[h], unit{fleet: i})
			}
		}
	}
	ownUnit := func(networkId server.Id, u unit) bool {
		if u.fleet < 0 {
			return u.networkId == networkId
		}
		return fleetNetworks[u.fleet][networkId]
	}
	claim := func(h [32]byte) float64 {
		return float64(max(1, len(holders[h])))
	}

	// raw scores and the per-network breakdown
	type networkTempo struct {
		raw             float64
		soloRaw         float64
		prefixes        int
		sharedPrefixes  int
		sharedWithUnits int
		presence        float64
	}
	tempo := map[server.Id]*networkTempo{}
	for networkId, presence := range networkPresence {
		values := &networkTempo{}
		sharedWith := map[unit]bool{}
		presenceSum := 0.0
		for _, h := range sortedSnTopPrefixes(presence) {
			p := presence[h]
			if p <= 0 {
				continue
			}
			values.prefixes += 1
			presenceSum += p
			values.raw += p / claim(h)
			shared := false
			otherFleets := 0
			for _, u := range holders[h] {
				if ownUnit(networkId, u) {
					continue
				}
				shared = true
				sharedWith[u] = true
				if 0 <= u.fleet {
					otherFleets += 1
				}
			}
			if shared {
				values.sharedPrefixes += 1
			}
			// "if you claimed alone today": split only with the fleets
			// already claimed in the prefix (Q4c)
			values.soloRaw += p / float64(1+otherFleets)
		}
		values.sharedWithUnits = len(sharedWith)
		if 0 < values.prefixes {
			values.presence = presenceSum / float64(values.prefixes)
		}
		tempo[networkId] = values
	}

	// fold the network EMAs
	networkStates := map[server.Id]SnTopUnitState{}
	candidates := map[server.Id]bool{}
	for networkId := range inputNetworks {
		candidates[networkId] = true
	}
	for networkId := range input.PriorNetworks {
		candidates[networkId] = true
	}
	type rankedNetwork struct {
		networkId server.Id
		ema       float64
	}
	ranked := []rankedNetwork{}
	for networkId := range candidates {
		prior, hasPrior := input.PriorNetworks[networkId]
		values := tempo[networkId]
		if values == nil {
			values = &networkTempo{}
		}
		ema := SnTopFoldEma(alpha, values.raw, prior.Ema, hasPrior)
		soloEma := SnTopFoldEma(alpha, values.soloRaw, prior.SoloEma, hasPrior)
		if ema < snTopEmaFloor {
			continue
		}
		networkStates[networkId] = SnTopUnitState{
			Ema:            ema,
			SoloEma:        soloEma,
			LastTempoIndex: input.TempoIndex,
			MeasurableBits: snTopMeasurableBits(prior, hasPrior, input.TempoIndex, measurable[networkId]),
		}
		ranked = append(ranked, rankedNetwork{networkId: networkId, ema: ema})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return snTopNetworkLess(ranked[i].networkId, ranked[i].ema, ranked[j].networkId, ranked[j].ema, input.NetworkCreateTimes)
	})
	projectedCutoff := 0.0
	if SnTopHeadSeats <= len(ranked) {
		projectedCutoff = ranked[SnTopHeadSeats-1].ema
	}

	claimedNetworks := map[server.Id]bool{}
	for i := range input.Fleets {
		for networkId := range fleetNetworks[i] {
			claimedNetworks[networkId] = true
		}
	}

	projection := &SnTopProjection{
		TotalRanked:       len(ranked),
		ProjectedCutoff:   projectedCutoff,
		EligibleProviders: eligibleProviders,
		NetworkStates:     networkStates,
	}
	for index, entry := range ranked {
		position := index + 1
		state := networkStates[entry.networkId]
		state.LastPosition = position
		networkStates[entry.networkId] = state
		prior, hasPrior := input.PriorNetworks[entry.networkId]
		values := tempo[entry.networkId]
		if values == nil {
			values = &networkTempo{}
		}
		margin := SnTopMargin(entry.ema, projectedCutoff)
		isMeasurable := measurable[entry.networkId]
		row := &SnTopNetworkProjection{
			NetworkId:       entry.networkId,
			Position:        position,
			ProjectedScore:  entry.ema,
			RawLastTempo:    values.raw,
			SoloScore:       state.SoloEma,
			Margin:          margin,
			Prefixes:        values.prefixes,
			SharedPrefixes:  values.sharedPrefixes,
			SharedWithUnits: values.sharedWithUnits,
			Presence:        values.presence,
			Label:           SnTopLabel(isMeasurable, margin, state.MeasurableBits, input.PrecisionAt200),
			Measurable:      isMeasurable,
			VerifyEligible:  verifyEligible[entry.networkId],
			ClaimPrompt:     SnTopClaimPrompt(isMeasurable, position, claimedNetworks[entry.networkId]),
			Claimed:         claimedNetworks[entry.networkId],
		}
		if hasPrior && 0 < prior.LastPosition {
			row.Trend = prior.LastPosition - position
		}
		switch {
		case isMeasurable:
			row.NotMeasurableReason = SnTopReasonNone
		case row.VerifyEligible:
			row.NotMeasurableReason = SnTopReasonNotInCohort
		case input.ProvidingNetworks[entry.networkId]:
			row.NotMeasurableReason = SnTopReasonSharedEgress
		default:
			row.NotMeasurableReason = SnTopReasonNotProviding
		}
		projection.Networks = append(projection.Networks, row)
	}

	// providing networks with no eligible provider and no score: their
	// egress does not attribute to them alone
	unranked := []server.Id{}
	for networkId := range input.ProvidingNetworks {
		if _, ok := networkStates[networkId]; ok || verifyEligible[networkId] {
			continue
		}
		unranked = append(unranked, networkId)
	}
	slices.SortFunc(unranked, func(a, b server.Id) int { return a.Cmp(b) })
	for _, networkId := range unranked {
		projection.Networks = append(projection.Networks, &SnTopNetworkProjection{
			NetworkId:           networkId,
			Label:               SnTopLabelNotMeasurable,
			Claimed:             claimedNetworks[networkId],
			NotMeasurableReason: SnTopReasonSharedEgress,
		})
	}

	// The replica is prepared separately from authenticated sequence cuts.
	// Preserve its prior checkpoint while evidence is unavailable.
	projection.FleetStates = input.PriorFleets
	if input.Replica != nil {
		projection.FleetStates = input.Replica.States
	}
	// A UID's score sums the signed census, independent of current membership.
	uidScores := map[uint16]*big.Rat{}
	if input.Replica != nil {
		for key := range input.Replica.Live {
			if uidScores[key.Uid] == nil {
				uidScores[key.Uid] = new(big.Rat)
			}
			if value := input.Replica.States[key].EmaExact; value != nil {
				uidScores[key.Uid].Add(uidScores[key.Uid], value)
			}
		}
		projection.ReplicaUidScores = uidScores
	}
	maximum := SnTopHeadSeats
	if input.Replica != nil {
		maximum = input.Replica.MaximumHeadFleets
	}
	selected, _, err := headMath.Select(uidScores, maximum)
	if err != nil {
		panic(err)
	}
	selectedUids := map[uint16]bool{}
	for _, uid := range selected {
		selectedUids[uid] = true
	}
	claimedUids := map[uint16]bool{}
	for i := range input.Fleets {
		fleet := input.Fleets[i]
		claimedUids[fleet.Key.Uid] = true
		row := &SnTopFleetProjection{
			Fleet:           fleet,
			ReplicaSelected: selectedUids[fleet.Key.Uid],
			ReplicaExact:    new(big.Rat),
		}
		if input.Replica != nil && input.Replica.Live[fleet.Key] {
			row.ReplicaAvailable = true
			if value := input.Replica.States[fleet.Key].EmaExact; value != nil {
				row.ReplicaExact = value
				row.ReplicaScore, _ = value.Float64()
			}
			if value, ok := input.Replica.Raw[fleet.Key]; ok {
				row.ReplicaRaw, _ = value.Float64()
			}
		}
		projection.Fleets = append(projection.Fleets, row)
	}
	slices.SortFunc(projection.Fleets, func(a, b *SnTopFleetProjection) int {
		if a.Fleet.Key.Uid != b.Fleet.Key.Uid {
			return int(a.Fleet.Key.Uid) - int(b.Fleet.Key.Uid)
		}
		return bytes.Compare([]byte(a.Fleet.Key.String()), []byte(b.Fleet.Key.String()))
	})
	projection.ClaimedFleets = len(claimedUids)
	projection.OpenSeats = max(0, SnTopHeadSeats-len(claimedUids))
	if 0 < len(selected) {
		floor := uidScores[selected[len(selected)-1]]
		value, _ := floor.Float64()
		projection.ClaimedCutoff = &value
	}
	return projection
}

// SnTopFinite keeps a value JSON-safe.
func SnTopFinite(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
