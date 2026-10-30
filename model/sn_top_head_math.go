package model

// The validator's exact head arithmetic for the SN Top board (LEADERTOP.md
// §5.5, §5.8, design question 8; owner decision 2026-10-29). sn exports it as
// `protocol/headscore`; until the server's sn dependency carries that package,
// snTopValidatorHeadMath implements the same rules from sn
// validator/head_ema.go, release_measurement.go (releaseRawHeadScores) and
// head_selection.go, and everything else reaches it only through
// SnTopHeadMath, so the switch replaces one value.
//
//   - Raw score of a fleet identity per tempo: Σ_h 1/claim(h) over the
//     distinct prefix hashes h it showed, claim(h) being the number of live
//     identities that showed h. An identity that showed nothing scores 0.
//   - EMA: next = α·raw + (1−α)·prior, α = 1/4. A new identity starts at its
//     raw score; a live identity with no hops decays to (1−α)·prior; an
//     identity whose next value is exactly 0 is dropped.
//   - Selection: positive scores, descending, ties to the lower UID; the first
//     maximum are selected.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// The policy's head_score_ema on mainnet (sn deploy/mainnet/policy-v1.yml).
var snTopHeadEmaAlpha = big.NewRat(1, 4)

// SnTopFleetKey is the validator's fleet identity.
type SnTopFleetKey struct {
	FleetId    [32]byte
	Hotkey     [32]byte
	Generation uint64
	Uid        uint16
}

// String is the identity's stable key: the validator's FleetScoreKey form.
func (self SnTopFleetKey) String() string {
	return fmt.Sprintf("%x:%x:%d:%d", self.FleetId, self.Hotkey, self.Generation, self.Uid)
}

// ParseSnTopFleetKey reads String's form back.
func ParseSnTopFleetKey(value string) (key SnTopFleetKey, err error) {
	parts := strings.Split(value, ":")
	if len(parts) != 4 {
		return key, fmt.Errorf("fleet key %q has %d parts", value, len(parts))
	}
	fleetId, fleetErr := hex.DecodeString(parts[0])
	hotkey, hotkeyErr := hex.DecodeString(parts[1])
	generation, generationErr := strconv.ParseUint(parts[2], 10, 64)
	uid, uidErr := strconv.ParseUint(parts[3], 10, 16)
	if fleetErr != nil || hotkeyErr != nil || generationErr != nil || uidErr != nil || len(fleetId) != 32 || len(hotkey) != 32 {
		return key, fmt.Errorf("fleet key %q is malformed", value)
	}
	key.FleetId = [32]byte(fleetId)
	key.Hotkey = [32]byte(hotkey)
	key.Generation = generation
	key.Uid = uint16(uid)
	return key, nil
}

// SnTopHeadMath is the validator's head arithmetic, exact.
type SnTopHeadMath interface {
	// Alpha is the EMA weight of the newest tempo.
	Alpha() *big.Rat
	// RawScores splits each prefix between the live identities that showed it.
	// Every key of `showed` is live; an empty set scores 0.
	RawScores(showed map[SnTopFleetKey]map[[32]byte]bool) map[SnTopFleetKey]*big.Rat
	// Fold folds one tempo's raw scores into the prior EMA values. Identities
	// whose next value is 0 are absent from the result.
	Fold(raw map[SnTopFleetKey]*big.Rat, prior map[SnTopFleetKey]*big.Rat) (map[SnTopFleetKey]*big.Rat, error)
	// Select ranks the positive scores (descending, ties to the lower UID) and
	// returns the first `maximum` as selected, the rest as rejected.
	Select(scores map[uint16]*big.Rat, maximum int) (selected []uint16, rejected []uint16, err error)
}

// snTopHeadMath is the arithmetic the board uses.
var snTopHeadMath SnTopHeadMath = snTopValidatorHeadMath{}

// GetSnTopHeadMath returns the board's head arithmetic.
func GetSnTopHeadMath() SnTopHeadMath {
	return snTopHeadMath
}

type snTopValidatorHeadMath struct{}

func (snTopValidatorHeadMath) Alpha() *big.Rat {
	return new(big.Rat).Set(snTopHeadEmaAlpha)
}

func (snTopValidatorHeadMath) RawScores(showed map[SnTopFleetKey]map[[32]byte]bool) map[SnTopFleetKey]*big.Rat {
	claims := map[[32]byte]int64{}
	for _, hashes := range showed {
		for hash := range hashes {
			claims[hash] += 1
		}
	}
	raw := make(map[SnTopFleetKey]*big.Rat, len(showed))
	for key, hashes := range showed {
		score := new(big.Rat)
		for hash := range hashes {
			score.Add(score, big.NewRat(1, claims[hash]))
		}
		raw[key] = score
	}
	return raw
}

func (self snTopValidatorHeadMath) Fold(raw map[SnTopFleetKey]*big.Rat, prior map[SnTopFleetKey]*big.Rat) (map[SnTopFleetKey]*big.Rat, error) {
	alpha := self.Alpha()
	oneMinus := new(big.Rat).Sub(big.NewRat(1, 1), alpha)
	keys := map[SnTopFleetKey]bool{}
	for key, value := range raw {
		if value == nil || value.Sign() < 0 {
			return nil, errors.New("head EMA raw score is nil or negative")
		}
		keys[key] = true
	}
	for key, value := range prior {
		if value == nil || value.Sign() <= 0 {
			return nil, errors.New("head EMA prior is nil or not positive")
		}
		keys[key] = true
	}
	next := map[SnTopFleetKey]*big.Rat{}
	for key := range keys {
		current, hasRaw := raw[key]
		if !hasRaw {
			current = new(big.Rat)
		}
		value := new(big.Rat)
		if priorValue, hasPrior := prior[key]; hasPrior {
			value.Add(new(big.Rat).Mul(alpha, current), new(big.Rat).Mul(oneMinus, priorValue))
		} else {
			value.Set(current)
		}
		if value.Sign() != 0 {
			next[key] = value
		}
	}
	return next, nil
}

func (snTopValidatorHeadMath) Select(scores map[uint16]*big.Rat, maximum int) (selected []uint16, rejected []uint16, err error) {
	if maximum <= 0 {
		return nil, nil, errors.New("maximum head fleets is zero")
	}
	type ranked struct {
		uid   uint16
		score *big.Rat
	}
	positive := []ranked{}
	for uid, score := range scores {
		if score == nil || score.Sign() < 0 {
			return nil, nil, errors.New("head score is nil or negative")
		}
		if 0 < score.Sign() {
			positive = append(positive, ranked{uid: uid, score: score})
		}
	}
	sort.Slice(positive, func(i, j int) bool {
		if comparison := positive[i].score.Cmp(positive[j].score); comparison != 0 {
			return 0 < comparison
		}
		return positive[i].uid < positive[j].uid
	})
	selected, rejected = []uint16{}, []uint16{}
	for i, entry := range positive {
		if i < maximum {
			selected = append(selected, entry.uid)
		} else {
			rejected = append(rejected, entry.uid)
		}
	}
	return selected, rejected, nil
}

// snTopRatString is an exact rational's stored form, "numerator/denominator".
func snTopRatString(value *big.Rat) string {
	return value.Num().String() + "/" + value.Denom().String()
}

// parseSnTopRat reads snTopRatString's form.
func parseSnTopRat(value string) (*big.Rat, error) {
	numerator, denominator, ok := strings.Cut(value, "/")
	if !ok {
		return nil, fmt.Errorf("exact value %q is not a fraction", value)
	}
	n, nOk := new(big.Int).SetString(numerator, 10)
	d, dOk := new(big.Int).SetString(denominator, 10)
	if !nOk || !dOk || d.Sign() <= 0 {
		return nil, fmt.Errorf("exact value %q is malformed", value)
	}
	return new(big.Rat).SetFrac(n, d), nil
}
