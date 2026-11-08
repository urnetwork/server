// Completion is later than every call AND final case custody. Read-only replay
// after expiry preserves an authenticated original terminal, never a new clock.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

func validateWinnerMultipassCompletionClock(owner winnerMultipassOwner, completedAt, latestProvider time.Time) error {
	if completedAt.IsZero() || completedAt.Before(owner.StartedAt) || !completedAt.Before(owner.Deadline) || completedAt.Before(latestProvider) {
		return errors.New("qualification final custody/outcomes did not complete inside the immutable original owner")
	}
	return nil
}

func readWinnerMultipassCompletedReport(pin currentQgQualificationPin, target any) error {
	content, err := readCurrentQgQualificationPin(pin, int64(defaultWinnerMultipassProviderLimits().MaximumResultBytes), true)
	if err != nil {
		return err
	}
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, target, "original completed qualification terminal") != nil {
		return errors.New("completed qualification replay lacks its original typed terminal")
	}
	canonical, _ := json.Marshal(target)
	if !bytes.Equal(content, canonical) {
		return errors.New("completed qualification terminal is not its exact canonical original")
	}
	return nil
}

func replayWinnerMultipassCompletedCase(pin currentQgQualificationPin, evidence winnerMultipassEvidence) error {
	expected, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	actual, err := readCurrentQgQualificationPin(pin, int64(len(expected)), true)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.New("completed replay lost its original retained case evidence")
	}
	return nil
}
