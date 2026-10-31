package model

// Exact fleet state advances only after replay of an authenticated cut and a
// matching predecessor. The estimate's wall-clock cadence never supplies it.

import "math/big"

// SnTopReplica is one fully replayed signed measurement. Live includes eligible
// identities with no prefixes, preserving the distinction between zero and
// unavailable evidence. States also retains decaying historical identities.
type SnTopReplica struct {
	States            map[SnTopFleetKey]SnTopUnitState
	Raw               map[SnTopFleetKey]*big.Rat
	Live              map[SnTopFleetKey]bool
	MaximumHeadFleets int
}

// FoldSnTopReplica applies exactly one actual validator transition. A gap in
// native epoch numbers does not manufacture empty measurements.
func FoldSnTopReplica(showed map[SnTopFleetKey]map[[32]byte]bool, prior map[SnTopFleetKey]SnTopUnitState, nativeEpoch uint64, maximum int, headMath SnTopHeadMath) (*SnTopReplica, error) {
	raw := headMath.RawScores(showed)
	priorExact := map[SnTopFleetKey]*big.Rat{}
	for key, state := range prior {
		if state.EmaExact != nil && 0 < state.EmaExact.Sign() {
			priorExact[key] = state.EmaExact
		}
	}
	next, err := headMath.Fold(raw, priorExact)
	if err != nil {
		return nil, err
	}
	replica := &SnTopReplica{States: map[SnTopFleetKey]SnTopUnitState{}, Raw: raw, Live: map[SnTopFleetKey]bool{}, MaximumHeadFleets: maximum}
	for key := range showed {
		replica.Live[key] = true
	}
	for key, exact := range next {
		score, _ := exact.Float64()
		replica.States[key] = SnTopUnitState{Ema: score, EmaExact: exact, LastTempoIndex: int64(nativeEpoch)}
	}
	return replica, nil
}
