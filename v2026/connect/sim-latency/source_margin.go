// Margin identities cross percent and fraction wire units at control-plane
// boundaries. Decimal conversion noise is not a different approved policy.
package main

import (
	"math"
	"math/big"
	"strconv"
)

// Accepts exact conversion in either direction, or exact equality of the
// shortest decimal wire representations. No epsilon permits a nearby policy;
// arbitrary score recommendations retain their full precision.
func sourceMarginMatches(percent, fraction float64) bool {
	if math.IsNaN(percent) || math.IsNaN(fraction) || math.IsInf(percent, 0) || math.IsInf(fraction, 0) ||
		percent < 0 || percent > 50 || fraction < 0 || fraction > 0.5 {
		return false
	}
	if percent == 0 || fraction == 0 {
		return percent == 0 && fraction == 0
	}
	if percent == fraction*100 || percent/100 == fraction {
		return true
	}
	percentValue, ok := new(big.Rat).SetString(strconv.FormatFloat(percent, 'f', -1, 64))
	if !ok {
		return false
	}
	fractionValue, ok := new(big.Rat).SetString(strconv.FormatFloat(fraction, 'f', -1, 64))
	if !ok {
		return false
	}
	return percentValue.Cmp(fractionValue.Mul(fractionValue, big.NewRat(100, 1))) == 0
}
