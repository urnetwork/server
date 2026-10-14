package controller

import "time"

// Shared stats caches encode source clocks as integer milliseconds. Convert
// that same integer to metric seconds: converting via float nanoseconds first
// can round to a different binary64 value at contemporary Unix timestamps.
func statsSourceTimestampSeconds(at time.Time) float64 {
	return float64(at.UnixMilli()) / 1000
}
