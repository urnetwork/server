// Validate grouped PostgreSQL projections before their values can suppress an
// incident. Rejected source values remain private; independent groups survive.
package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var pgGroupedDecimalRe = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// PostgreSQL integer output has a canonical base-ten spelling and a fixed SQL
// width. Reject parse errors and overflow instead of manufacturing zero.
func pgGroupedInteger(value string, bits int, minimum int64) (int64, bool) {
	number, err := strconv.ParseInt(value, 10, bits)
	return number, err == nil && minimum <= number && strconv.FormatInt(number, 10) == value
}

// Execution times are nonnegative finite decimal milliseconds, including zero.
func pgGroupedFloat(value string) (float64, bool) {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || !pgGroupedDecimalRe.MatchString(value) || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	mantissa := value
	if exponent := strings.IndexAny(mantissa, "eE"); exponent >= 0 {
		mantissa = mantissa[:exponent]
	}
	if number == 0 && strings.Trim(mantissa, "0.") != "" {
		return 0, false
	}
	return number, true
}

// Query IDs are signed bigint values; unavailable query IDs use the SQL sentinel.
func pgGroupedQueryId(value string) bool {
	if value == "unknown" {
		return true
	}
	_, ok := pgGroupedInteger(value, 64, math.MinInt64)
	return ok
}

// Finished-task UUIDs are internal correlation inputs, never public evidence.
func pgGroupedTaskId(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
			return false
		}
	}
	return true
}

// Both completion projections use nonnegative int durations/ages and bigint
// completion clocks. Rounding the strict 45-minute lookback can yield 2700.
func pgGroupedCompletionValues(row pgRow, taskColumn, durationColumn, ageColumn, clockColumn int) (int, int, int64, bool) {
	duration, durationOk := pgGroupedInteger(row.str(durationColumn), 32, 0)
	age, ageOk := pgGroupedInteger(row.str(ageColumn), 32, 0)
	clock, clockOk := pgGroupedInteger(row.str(clockColumn), 64, 1)
	return int(duration), int(age), clock, pgGroupedTaskId(row.str(taskColumn)) &&
		durationOk && ageOk && age <= 2700 && clockOk
}

// One fixed component finding records incomplete coverage without disclosing
// rejected row values or discarding independently qualified incidents.
func pgGroupedCoverageFinding(target, component string) finding {
	issue := cannotObserveFinding(target, fmt.Errorf("invalid response: PostgreSQL grouped projection"))
	issue.frame = component
	issue.observed += " component=" + component + " reason=invalid-grouped-projection"
	return issue
}

// Partial observations belong to a live owner. Preserve an ended owner's
// original returned source error for the public adapter; a successful late
// source return still carries the exact parent context sentinel.
func pgGroupedInterruptedSourceError(ctx context.Context, sourceErr error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		if sourceErr != nil {
			return sourceErr
		}
		return contextErr
	}
	return nil
}
