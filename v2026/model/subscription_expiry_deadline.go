// Future expiration observations belong to the bounded expiry scan.
package model

import "time"

// Copy a new minimum so retained task arguments are never mutated. A deadline
// that passes during a long continuation stays retained until its next pass.
func earlierContractExpiration(current, candidate *time.Time) *time.Time {
	if candidate == nil || candidate.IsZero() || current != nil && !candidate.Before(*current) {
		return current
	}
	expiration := candidate.UTC()
	return &expiration
}
