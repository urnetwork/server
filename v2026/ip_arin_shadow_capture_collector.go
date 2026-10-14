package server

import (
	"context"
	"encoding/hex"
	"time"
)

// A generation pin is independently supplied by the current native/source
// owner. It is never inferred from an MMDB file, collector clock or row count.
type ArinShadowCaptureCohort struct {
	GenerationSHA256       string    `json:"-"`
	ObservedAt             time.Time `json:"-"`
	Providers, Connections int64     `json:"-"`
}

// Each provider header occurs exactly once, on its first connection. The
// complete source drives order, including unknown/missing transport owners.
type ArinShadowCaptureRecord struct {
	Provider                *ArinShadowCaptureProvider `json:"-"`
	ConnectionId, HandlerId Id                         `json:"-"`
}

type ArinShadowCapturePage struct {
	GenerationSHA256 string                    `json:"-"`
	Sequence         uint64                    `json:"-"`
	Records          []ArinShadowCaptureRecord `json:"-"`
}

type ArinShadowCaptureOwnerReader func(context.Context, []Id) ([]ArinShadowCaptureTarget, error)

// ArinShadowCaptureCollector is a single sequential owner. It retains at most
// one 256-row page, one provider and fixed country counts. There is no retry,
// detached worker, all-population result map, or serving-policy mutation.
type ArinShadowCaptureCollector struct {
	stream   *ArinShadowCaptureStream
	cohort   ArinShadowCaptureCohort
	sequence uint64
}

func (r *ArinShadowRecorder) NewCurrentCaptureCollector(ctx context.Context, cohort ArinShadowCaptureCohort, required []string) (*ArinShadowCaptureCollector, error) {
	if r == nil {
		return nil, ErrArinShadowInput
	}
	pin, err := hex.DecodeString(cohort.GenerationSHA256)
	now := r.clock()
	if err != nil || len(pin) != 32 || cohort.Providers < 0 || cohort.Providers > ArinShadowCapturePopulationLimit ||
		cohort.Connections < cohort.Providers || cohort.Connections > ArinShadowCapturePopulationLimit ||
		cohort.ObservedAt.IsZero() || cohort.ObservedAt.Before(now.Add(-ArinShadowCaptureMaxAge)) || cohort.ObservedAt.After(now.Add(arinShadowCaptureClockSkew)) {
		return nil, ErrArinShadowInput
	}
	stream, err := r.NewCurrentCaptureStream(ctx, required)
	if err != nil {
		return nil, err
	}
	stream.report.CohortGenerationSHA256 = cohort.GenerationSHA256
	stream.report.CohortObservedAt = cohort.ObservedAt
	return &ArinShadowCaptureCollector{stream: stream, cohort: cohort}, nil
}

func (c *ArinShadowCaptureCollector) Consume(page ArinShadowCapturePage, owners ArinShadowCaptureOwnerReader, facts ArinShadowCaptureFactReader) error {
	s := c.stream
	if !s.live() || owners == nil || facts == nil || page.GenerationSHA256 != c.cohort.GenerationSHA256 || page.Sequence != c.sequence ||
		len(page.Records) == 0 || len(page.Records) > ArinShadowCaptureBatchLimit || int64(len(page.Records)) > c.cohort.Connections-s.report.CapturedConnections {
		return s.reject()
	}
	ids := make([]Id, len(page.Records))
	for i, record := range page.Records {
		if record.HandlerId == (Id{}) {
			return s.reject()
		}
		ids[i] = record.ConnectionId
	}
	remaining := ArinShadowCaptureMaxAge - s.recorder.clock().Sub(s.started)
	ctx, cancel := context.WithTimeout(s.ctx, remaining)
	defer cancel()
	targets, err := owners(ctx, ids)
	if err != nil || len(targets) != len(ids) {
		return s.reject()
	}
	for i := range ids {
		if targets[i].ConnectionId != ids[i] {
			return s.reject()
		}
	}
	rows, err := s.recorder.CaptureCurrent(ctx, targets, facts)
	if err != nil {
		return s.reject()
	}
	for i, record := range page.Records {
		if record.Provider != nil {
			if s.report.Providers >= c.cohort.Providers || s.BeginProvider(*record.Provider) != nil {
				return s.reject()
			}
		}
		if rows[i].reason == "qualified" && rows[i].handlerId != record.HandlerId {
			rows[i].reason = "binding_mismatch"
		}
		if s.addCaptured(rows[i:i+1]) != nil {
			return s.reject()
		}
		if s.current.count == s.current.input.ExpectedConnections {
			if s.EndProvider() != nil {
				return s.reject()
			}
		}
	}
	if ctx.Err() != nil || !s.live() {
		return s.reject()
	}
	c.sequence++
	return nil
}

// A distinct end marker must bind the same source generation and exact totals.
// Running out of pages or reaching a row/time cap cannot manufacture it.
func (c *ArinShadowCaptureCollector) Finish(end ArinShadowCaptureCohort, sourceComplete bool) (ArinShadowCaptureReport, error) {
	coherent := sourceComplete && end.GenerationSHA256 == c.cohort.GenerationSHA256 && end.ObservedAt.Equal(c.cohort.ObservedAt) &&
		end.Providers == c.cohort.Providers && end.Connections == c.cohort.Connections
	return c.stream.Finish(coherent, c.cohort.Providers, c.cohort.Connections)
}
