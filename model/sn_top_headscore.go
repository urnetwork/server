package model

import (
	"math/big"

	"github.com/urfoundation/sn/protocol/headscore"
)

// snTopHeadscoreMath is SnTopHeadMath on sn's protocol/headscore, the
// validator's own exact head arithmetic (design question 8): the board's
// claimed-fleet replica computes a fleet's head score bit for bit as the
// validator does.
type snTopHeadscoreMath struct{}

// the policy's head_score_ema on mainnet (sn deploy/mainnet/policy-v1.yml)
var snTopHeadscoreAlpha = headscore.Ratio{Numerator: 1, Denominator: 4}

func snTopHeadscoreKey(key SnTopFleetKey) headscore.FleetKey {
	return headscore.FleetKey{FleetID: key.FleetId, Hotkey: key.Hotkey, Generation: key.Generation, UID: key.Uid}
}

func (snTopHeadscoreMath) Alpha() *big.Rat {
	return new(big.Rat).SetFrac(
		new(big.Int).SetUint64(snTopHeadscoreAlpha.Numerator),
		new(big.Int).SetUint64(snTopHeadscoreAlpha.Denominator),
	)
}

func (snTopHeadscoreMath) RawScores(showed map[SnTopFleetKey]map[[32]byte]bool) map[SnTopFleetKey]*big.Rat {
	return headscore.RawScores(showed)
}

func (snTopHeadscoreMath) Fold(raw map[SnTopFleetKey]*big.Rat, prior map[SnTopFleetKey]*big.Rat) (map[SnTopFleetKey]*big.Rat, error) {
	keys := map[headscore.FleetKey]SnTopFleetKey{}
	scoreRaw := map[headscore.FleetKey]*big.Rat{}
	for key, value := range raw {
		converted := snTopHeadscoreKey(key)
		keys[converted] = key
		scoreRaw[converted] = value
	}
	scorePrior := map[headscore.FleetKey]*big.Rat{}
	for key, value := range prior {
		converted := snTopHeadscoreKey(key)
		keys[converted] = key
		scorePrior[converted] = value
	}
	records, err := headscore.Fold(scorePrior, scoreRaw, snTopHeadscoreAlpha)
	if err != nil {
		return nil, err
	}
	next := map[SnTopFleetKey]*big.Rat{}
	for key, value := range headscore.State(records) {
		next[keys[key]] = value
	}
	return next, nil
}

func (snTopHeadscoreMath) Select(scores map[uint16]*big.Rat, maximum int) (selected []uint16, rejected []uint16, err error) {
	if maximum <= 0 || 0xffff < maximum {
		maximum = 0
	}
	inputs := make([]headscore.UIDScore, 0, len(scores))
	for uid, score := range scores {
		inputs = append(inputs, headscore.UIDScore{UID: uid, Score: score})
	}
	selection, err := headscore.Select(inputs, uint16(maximum))
	if err != nil {
		return nil, nil, err
	}
	return headscore.UIDs(selection.Selected), headscore.UIDs(selection.Rejected), nil
}
