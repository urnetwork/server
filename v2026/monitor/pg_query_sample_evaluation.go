// Same-call durable collection proof distinguishes successful PG collection
// from an interrupted adapter verdict, without publishing canceled findings.
package monitor

import "time"

// Created only by the successful immutable-receipt/terminal-state path. No
// mutable cadence read or timestamp coincidence can create this authority.
type pgSampleCompletedCollection struct {
	ReceiptSha256  string
	RequestedAt    time.Time
	FinishedAt     time.Time
	NextEligibleAt time.Time
	Samples        int
}

// The original parent error remains the lifecycle authority. This finite proof
// concerns the collector only; it does not certify all findings or DB health.
type pgSampleEvaluationInterrupted struct {
	contextErr error
	sourceErr  error
	collection pgSampleCompletedCollection
}

// Public error text remains the original lifecycle text, without private data.
func (self *pgSampleEvaluationInterrupted) Error() string { return self.contextErr.Error() }

// Multi-error children must be non-nil, including successful collection where
// only the parent lifecycle failed and the source returned no error.
func (self *pgSampleEvaluationInterrupted) Unwrap() []error {
	var causes []error
	if self.contextErr != nil {
		causes = append(causes, self.contextErr)
	}
	if self.sourceErr != nil {
		causes = append(causes, self.sourceErr)
	}
	return causes
}
