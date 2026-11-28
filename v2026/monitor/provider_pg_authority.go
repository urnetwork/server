// Provider scalar queries validate their own numeric and SQL NULL domains
// before judging product state; optional grouped queries retain their contracts.
package monitor

import (
	"fmt"
	"math"
	"math/bits"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	providerPgIntegerPattern  = regexp.MustCompile(`^-?[0-9]+$`)
	providerPgFloatPattern    = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	providerPgLocationPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Match the owning SQL integer width and minimum without rendering a cell.
func providerPgInteger(raw string, bits int, minimum int64) (int64, error) {
	if !providerPgIntegerPattern.MatchString(raw) {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL integer")
	}
	value, err := strconv.ParseInt(raw, 10, bits)
	if err != nil || value < minimum {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL integer domain")
	}
	return value, nil
}

// Weights may exceed one and medians may be fractional, but neither SQL
// numeric output nor an underflowed nonzero value can become an invented zero.
func providerPgFloat(raw string) (float64, error) {
	if !providerPgFloatPattern.MatchString(raw) {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL number")
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL number domain")
	}
	if value == 0 {
		mantissa := strings.SplitN(strings.ToLower(raw), "e", 2)[0]
		if strings.ContainsAny(mantissa, "123456789") {
			return 0, fmt.Errorf("invalid response for provider PostgreSQL number underflow")
		}
	}
	return value, nil
}

// Preserve textual Boolean adapters while rejecting numeric or unknown states.
func providerPgBool(raw string) (bool, error) {
	switch raw {
	case "t", "true":
		return true, nil
	case "f", "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid response for provider PostgreSQL Boolean")
	}
}

// Round the exact counter delta to connections/minute before consuming it.
// A 128-bit numerator preserves valid large rates without float-to-int overflow.
func providerPgConnectionRate(delta int64, elapsed time.Duration) (int, error) {
	if delta < 0 || elapsed <= 0 {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL connection rate domain")
	}
	high, low := bits.Mul64(uint64(delta), uint64(time.Minute))
	denominator := uint64(elapsed)
	if high >= denominator {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL connection rate overflow")
	}
	rate, remainder := bits.Div64(high, low, denominator)
	roundUp := remainder >= denominator/2+denominator%2
	maximum := uint64(^uint(0) >> 1)
	if rate > maximum || rate == maximum && roundUp {
		return 0, fmt.Errorf("invalid response for provider PostgreSQL connection rate overflow")
	}
	if roundUp {
		rate++
	}
	return int(rate), nil
}

// Compare exact nonnegative cohort counts; a missing parent denominator never
// proves churn. Both cross-products, including the factor two, fit 128 bits.
func providerPgChildRatioDoubled(currentChildren, currentParents, baselineChildren, baselineParents int) bool {
	if currentParents <= 0 || baselineParents <= 0 || currentChildren < 0 || baselineChildren < 0 {
		return false
	}
	leftHigh, leftLow := bits.Mul64(uint64(currentChildren), uint64(baselineParents))
	rightHigh, rightLow := bits.Mul64(uint64(baselineChildren), uint64(currentParents))
	rightHigh, rightLow = rightHigh<<1|rightLow>>63, rightLow<<1
	return leftHigh > rightHigh || leftHigh == rightHigh && leftLow > rightLow
}
