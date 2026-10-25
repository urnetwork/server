// Bounded, attempt-private diagnostics for the existing shared queue. Timings
// include local descheduling; recovery contains any recovery update subphases.
package monitor

import (
	"context"
	"errors"
)

// Aggregate repeated queue passes rather than persisting owners, tokens, hosts
// or an unbounded event list. Null spans mean unentered, not zero elapsed work.
type pgSampleSharedPhase struct {
	Calls          int    `json:"calls"`
	Errors         int    `json:"errors"`
	FirstEnteredNs *int64 `json:"first_entered_ns"`
	LastReturnedNs *int64 `json:"last_returned_ns"`
	TotalNs        *int64 `json:"total_ns"`
}

// Decisions count state.grant evaluations, not claims or commands started.
// Publication includes encoding/no-op comparison and durable replacement;
// validation includes state reads, owner checks, pruning and queue selection.
type pgSampleSharedAdmissionDetail struct {
	Schema               int                  `json:"schema"`
	Outcome              string               `json:"outcome"`
	ClaimDisposition     string               `json:"claim_disposition"`
	Decisions            int                  `json:"decisions"`
	Grants               int                  `json:"grants"`
	PublicationUnchanged int                  `json:"publication_unchanged"`
	PublicationDurable   int                  `json:"publication_durable"`
	Recovery             *pgSampleSharedPhase `json:"recovery"`
	Flock                *pgSampleSharedPhase `json:"flock"`
	Validation           *pgSampleSharedPhase `json:"validation"`
	EligibleWait         *pgSampleSharedPhase `json:"eligible_wait"`
	Publication          *pgSampleSharedPhase `json:"publication"`
	Claim                *pgSampleSharedPhase `json:"claim"`
}

type pgSharedPhase uint8

const (
	pgSharedRecovery pgSharedPhase = iota
	pgSharedFlock
	pgSharedValidation
	pgSharedEligibleWait
	pgSharedPublication
	pgSharedClaim
)

// Only the actual store acquisition creates this projection. Injected backends
// and legacy observations retain null, so unavailable detail is not healthy zero.
func beginPgSharedAdmission(ctx context.Context) *pgSampleSharedAdmissionDetail {
	observation := pgSampleTransportFromContext(ctx)
	if observation == nil || observation.receipt.LocalPhaseTiming == nil {
		return nil
	}
	detail := &pgSampleSharedAdmissionDetail{Schema: 1, Outcome: "entered", ClaimDisposition: "untracked"}
	observation.receipt.SharedAdmissionDetail = detail
	return detail
}

// Fixed labels only; the existing parent timing clock owns validity for offsets.
func beginPgSharedPhase(ctx context.Context, phase pgSharedPhase) func(error) {
	observation := pgSampleTransportFromContext(ctx)
	if observation == nil || observation.receipt.SharedAdmissionDetail == nil {
		return func(error) {}
	}
	detail := observation.receipt.SharedAdmissionDetail
	var target **pgSampleSharedPhase
	switch phase {
	case pgSharedRecovery:
		target = &detail.Recovery
	case pgSharedFlock:
		target = &detail.Flock
	case pgSharedValidation:
		target = &detail.Validation
	case pgSharedEligibleWait:
		target = &detail.EligibleWait
	case pgSharedPublication:
		target = &detail.Publication
	case pgSharedClaim:
		target = &detail.Claim
	default:
		return func(error) {}
	}
	if *target == nil {
		*target = &pgSampleSharedPhase{}
	}
	span := *target
	entered := observation.localOffset(observation.localNow())
	span.Calls++
	if span.Calls == 1 {
		span.FirstEnteredNs = entered
	}
	finished := false
	return func(err error) {
		if finished {
			return
		}
		finished = true
		returned := observation.localOffset(observation.localNow())
		span.LastReturnedNs = returned
		if err != nil {
			span.Errors++
		}
		if entered == nil || returned == nil {
			span.TotalNs = nil
			return
		}
		if span.Calls == 1 {
			total := int64(0)
			span.TotalNs = &total
		}
		if span.TotalNs != nil {
			*span.TotalNs += *returned - *entered
		}
	}
}

// An acquire failure does not identify a particular resource or external cause.
func (self *pgSampleSharedAdmissionDetail) complete(err error) {
	if self == nil {
		return
	}
	self.Outcome = "returned_grant"
	if err != nil {
		self.Outcome = "error"
		if errors.Is(err, context.Canceled) {
			self.Outcome = "canceled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			self.Outcome = "deadline"
		}
	}
}

// Cleanup outcome supplies publication return authority, not remote execution.
func (self *pgSampleSharedAdmissionDetail) claimCleanup(err error) {
	if self == nil {
		return
	}
	self.ClaimDisposition = "cleanup_succeeded"
	if err != nil {
		self.ClaimDisposition = "cleanup_error"
	}
}

// Durable means rename and directory fsync returned successfully; a later
// acknowledgement failure remains visible independently in phase errors.
func recordPgSharedPublication(ctx context.Context, durable bool) {
	observation := pgSampleTransportFromContext(ctx)
	if observation == nil || observation.receipt.SharedAdmissionDetail == nil {
		return
	}
	detail := observation.receipt.SharedAdmissionDetail
	if durable {
		detail.PublicationDurable++
	} else {
		detail.PublicationUnchanged++
	}
}
