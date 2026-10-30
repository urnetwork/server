// Pins an initial admission schedule without changing historical source intents.
package main

import (
	"errors"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// An omitted value recovers the durable schedule, never resets it. New explicit
// schedules must be future instants exactly representable by PostgreSQL.
func bootstrapSourceOpening(value string, intent *model.CompetitionSourceActivation, now time.Time) (time.Time, error) {
	if value == "" {
		if intent != nil {
			return intent.OpensAt, nil
		}
		return time.Time{}, nil
	}
	opensAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || opensAt.IsZero() || !opensAt.Equal(opensAt.Truncate(time.Microsecond)) {
		return time.Time{}, errors.New("--opens-at requires an RFC3339 timestamp with at most microsecond precision")
	}
	opensAt = opensAt.UTC()
	if intent != nil {
		if !intent.OpensAt.Equal(opensAt) {
			return time.Time{}, errors.New("--opens-at conflicts with the immutable baseline opening")
		}
	} else if !opensAt.After(now) {
		return time.Time{}, errors.New("a new baseline opening must be in the future")
	}
	return opensAt, nil
}
