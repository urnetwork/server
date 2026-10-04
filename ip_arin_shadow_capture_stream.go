package server

import (
	"context"
	"slices"
	"time"
)

const ArinShadowCapturePopulationLimit = 2000000

// This private input must come from one independently qualified current
// provider/native-membership census. Capture never invents that authority.
type ArinShadowCaptureProvider struct {
	ClientId                                           Id       `json:"-"`
	ExpectedConnections                                int      `json:"-"`
	Buckets                                            []string `json:"-"`
	BaseQuality, BaseSpeed, ActiveQuality, ActiveSpeed bool     `json:"-"`
	// Missing native-source membership must not manufacture false baseline
	// flags or turn a missing join into a measured policy exclusion.
	MembershipUnavailable bool `json:"-"`
}

type ArinShadowCaptureReport struct {
	ArinShadowReport
	StartedAt                           time.Time                     `json:"capture_started_at"`
	CapturedConnections                 int64                         `json:"captured_connections"`
	DeclaredConnections                 int64                         `json:"declared_connections"`
	Providers                           int64                         `json:"providers"`
	EarliestLookupAt                    time.Time                     `json:"earliest_immutable_lookup_at"`
	LatestLookupAt                      time.Time                     `json:"latest_immutable_lookup_at"`
	EarliestDurableReadAt               time.Time                     `json:"earliest_durable_read_at"`
	LatestDurableReadAt                 time.Time                     `json:"latest_durable_read_at"`
	Reasons                             map[string]int64              `json:"connection_reasons"`
	NativeMembershipComplete            bool                          `json:"native_membership_complete"`
	NativeMembershipUnknown             int64                         `json:"native_membership_unknown_providers"`
	CohortGenerationSHA256              string                        `json:"cohort_generation_sha256,omitempty"`
	CohortObservedAt                    time.Time                     `json:"cohort_observed_at,omitzero"`
	NativeSourceStartedAt               time.Time                     `json:"native_source_started_at,omitzero"`
	NativeSourceCompletedAt             time.Time                     `json:"native_source_completed_at,omitzero"`
	NativePublishedAt                   time.Time                     `json:"native_published_at,omitzero"`
	NativeGenerationAtEnd               string                        `json:"native_generation_at_end,omitempty"`
	NativeGenerationChanged             bool                          `json:"native_generation_changed"`
	RegistrationCounts                  []ArinShadowRegistrationCount `json:"registration_connection_counts"`
	RegistrationUnattributedConnections int64                         `json:"registration_unattributed_connections"`
	RegistrationOverflowConnections     int64                         `json:"registration_overflow_connections"`
	OriginCounts                        []ArinShadowOriginCount       `json:"origin_connection_counts"`
	OriginUnattributedConnections       int64                         `json:"origin_unattributed_connections"`
	OriginOverflowConnections           int64                         `json:"origin_overflow_connections"`
}

type arinShadowCaptureProviderState struct {
	input                                     ArinShadowCaptureProvider
	lastConnection                            Id
	count                                     int
	complete, verified                        bool
	risk, proxy, excluded, unknown, ambiguous bool
}

// One sequential owner drives this reducer. It retains one provider and a
// finite country matrix, not the population of addresses or connection IDs.
// A provider may span any number of bounded batches and is counted only once.
type ArinShadowCaptureStream struct {
	recorder                    *ArinShadowRecorder
	ctx                         context.Context
	started                     time.Time
	lastProvider                Id
	current                     *arinShadowCaptureProviderState
	buckets                     map[string]*ArinShadowBucket
	registrations               map[ArinShadowRegistration]*ArinShadowRegistrationCount
	origins                     map[string]*ArinShadowOriginCount
	report                      ArinShadowCaptureReport
	complete, finished, invalid bool
}

func (r *ArinShadowRecorder) NewCurrentCaptureStream(ctx context.Context, required []string) (*ArinShadowCaptureStream, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || len(required) == 0 || len(required) > 677 {
		return nil, ErrArinShadowInput
	}
	r.mu.RLock()
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return nil, ErrArinShadowInput
	}
	now := r.clock()
	s := &ArinShadowCaptureStream{recorder: r, ctx: ctx, started: now, buckets: make(map[string]*ArinShadowBucket, len(required)), registrations: make(map[ArinShadowRegistration]*ArinShadowRegistrationCount), origins: make(map[string]*ArinShadowOriginCount), complete: true}
	s.report = ArinShadowCaptureReport{StartedAt: now, NativeMembershipComplete: true, Reasons: make(map[string]int64, len(arinShadowCaptureReasons)+1)}
	for _, reason := range arinShadowCaptureReasons {
		s.report.Reasons[reason] = 0
	}
	s.report.Reasons["not_captured"] = 0
	for _, bucket := range required {
		if !validShadowBucket(bucket) || s.buckets[bucket] != nil {
			return nil, ErrArinShadowInput
		}
		s.buckets[bucket] = &ArinShadowBucket{Bucket: bucket}
	}
	return s, nil
}

func (s *ArinShadowCaptureStream) live() bool {
	now := s.recorder.clock()
	return !s.finished && !s.invalid && s.ctx.Err() == nil && !now.Before(s.started) && now.Sub(s.started) <= ArinShadowCaptureMaxAge
}

func (s *ArinShadowCaptureStream) reject() error { s.invalid = true; return ErrArinShadowInput }

func (s *ArinShadowCaptureStream) BeginProvider(input ArinShadowCaptureProvider) error {
	if !s.live() || s.current != nil || input.ClientId == (Id{}) || !s.lastProvider.Less(input.ClientId) ||
		input.ExpectedConnections < 1 || int64(input.ExpectedConnections) > ArinShadowCapturePopulationLimit-s.report.DeclaredConnections ||
		s.report.Providers >= ArinShadowCapturePopulationLimit || len(input.Buckets) == 0 || len(input.Buckets) > len(s.buckets) ||
		!input.MembershipUnavailable && (input.ActiveQuality && !input.BaseQuality || input.ActiveSpeed && !input.BaseSpeed) {
		return s.reject()
	}
	seen := make(map[string]bool, len(input.Buckets))
	for _, bucket := range input.Buckets {
		if s.buckets[bucket] == nil || seen[bucket] {
			return s.reject()
		}
		seen[bucket] = true
	}
	input.Buckets = slices.Clone(input.Buckets)
	s.current = &arinShadowCaptureProviderState{input: input, complete: true, verified: true}
	s.lastProvider = input.ClientId
	s.report.DeclaredConnections += int64(input.ExpectedConnections)
	return nil
}

// CaptureBatch consumes only opaque rows produced by this recorder. Targets
// must be in durable connection-key order; missing owners are kept as rows.
func (s *ArinShadowCaptureStream) CaptureBatch(targets []ArinShadowCaptureTarget, read ArinShadowCaptureFactReader) error {
	if !s.live() || s.current == nil || len(targets) == 0 || len(targets) > ArinShadowCaptureBatchLimit || len(targets) > s.current.input.ExpectedConnections-s.current.count {
		return s.reject()
	}
	last := s.current.lastConnection
	for _, target := range targets {
		if !last.Less(target.ConnectionId) {
			return s.reject()
		}
		last = target.ConnectionId
	}
	// One owner bounds all batches; the per-reader deadline never extends it.
	remaining := ArinShadowCaptureMaxAge - s.recorder.clock().Sub(s.started)
	ctx, cancel := context.WithTimeout(s.ctx, remaining)
	defer cancel()
	rows, err := s.recorder.CaptureCurrent(ctx, targets, read)
	if err != nil {
		// A failed exact read cannot be treated as an empty provider or retried
		// without accounting. Count this batch as explicitly indeterminate.
		rows = make([]arinShadowCapturedConnection, len(targets))
		for i, target := range targets {
			rows[i] = arinShadowCapturedConnection{recorder: s.recorder, capturedAt: s.recorder.clock(), connectionId: target.ConnectionId, reason: "facts_unavailable"}
		}
	}
	if addErr := s.addCaptured(rows); addErr != nil {
		return addErr
	}
	return err
}

// The collector can read a page crossing several providers once, then feed
// each contiguous group here. Rows are sealed to this recorder and capture
// window; no caller can recycle an old result or fabricate a zero default.
func (s *ArinShadowCaptureStream) addCaptured(rows []arinShadowCapturedConnection) error {
	if !s.live() || s.current == nil || len(rows) == 0 || len(rows) > ArinShadowCaptureBatchLimit || len(rows) > s.current.input.ExpectedConnections-s.current.count {
		return s.reject()
	}
	last := s.current.lastConnection
	for _, row := range rows {
		if row.recorder != s.recorder || row.capturedAt.Before(s.started) || row.capturedAt.After(s.recorder.clock().Add(arinShadowCaptureClockSkew)) || !last.Less(row.connectionId) || !slices.Contains(arinShadowCaptureReasons[:], row.reason) {
			return s.reject()
		}
		last = row.connectionId
	}
	for _, row := range rows {
		s.addConnection(row)
	}
	return nil
}

func (s *ArinShadowCaptureStream) addConnection(row arinShadowCapturedConnection) {
	p := s.current
	p.count++
	p.lastConnection = row.connectionId
	s.report.CapturedConnections++
	if row.reason == "qualified" && row.clientId != p.input.ClientId {
		row.reason = "binding_mismatch"
	}
	s.report.Reasons[row.reason]++
	if row.reason != "qualified" {
		p.complete = false
		p.verified = false
		p.unknown = true
		s.report.FailedObservations++
		return
	}
	f := row.facts
	s.addRegistration(f)
	s.addOrigin(f)
	p.verified = p.verified && f.verified
	p.risk = p.risk || f.risk
	p.proxy = p.proxy || f.proxyRisk
	p.excluded = p.excluded || f.state == "excluded"
	p.unknown = p.unknown || f.state == "unknown"
	p.ambiguous = p.ambiguous || f.state == "ambiguous"
	includeCaptureClock(&s.report.EarliestLookupAt, &s.report.LatestLookupAt, row.actualAt)
	includeCaptureClock(&s.report.EarliestDurableReadAt, &s.report.LatestDurableReadAt, row.observedAt)
}

func includeCaptureClock(first, last *time.Time, at time.Time) {
	if first.IsZero() || at.Before(*first) {
		*first = at
	}
	if last.IsZero() || at.After(*last) {
		*last = at
	}
}

func (s *ArinShadowCaptureStream) EndProvider() error {
	if !s.live() || s.current == nil {
		return s.reject()
	}
	p := s.current
	missing := p.input.ExpectedConnections - p.count
	if missing != 0 {
		p.complete = false
		p.verified = false
		p.unknown = true
		s.report.Reasons["not_captured"] += int64(missing)
		s.report.FailedObservations += int64(missing)
	}
	s.complete = s.complete && p.complete
	knownMembership := !p.input.MembershipUnavailable
	if !knownMembership {
		s.report.NativeMembershipComplete = false
		s.report.NativeMembershipUnknown++
	}
	candidateQuality := knownMembership && p.complete && p.input.BaseQuality && p.verified && !p.risk
	candidateSpeed := knownMembership && p.complete && p.input.BaseSpeed && !p.risk
	for _, bucket := range p.input.Buckets {
		row := s.buckets[bucket]
		row.Providers++
		if p.complete {
			row.CompleteLookups++
		} else {
			row.MissingOrStale++
		}
		switch {
		case p.ambiguous:
			row.Ambiguous++
		case p.excluded:
			row.Excluded++
		case p.unknown || !p.verified:
			row.Unknown++
		default:
			row.VerifiedSubscriber++
		}
		if p.risk {
			row.Risk++
		}
		if p.proxy {
			row.ProxyRisk++
		}
		if knownMembership && p.input.ActiveQuality {
			row.ActiveQuality++
		}
		if knownMembership && p.input.ActiveSpeed {
			row.ActiveSpeed++
		}
		if candidateQuality {
			row.CandidateQuality++
		}
		if candidateSpeed {
			row.CandidateSpeed++
		}
		if !p.complete || !knownMembership {
			row.QualityIndeterminate++
			row.SpeedIndeterminate++
		}
		if knownMembership && p.complete && p.input.ActiveQuality && !candidateQuality {
			row.QualityRemoved++
		}
		if !p.input.ActiveQuality && candidateQuality {
			row.QualityAdded++
		}
	}
	s.report.Providers++
	s.current = nil
	return nil
}

// Finish requires the source's end marker and independently counted exact
// totals. Matching totals still are not proof of Main identity or policy GO.
func (s *ArinShadowCaptureStream) Finish(sourceComplete bool, expectedProviders, expectedConnections int64) (ArinShadowCaptureReport, error) {
	live := s.live()
	s.finished = true
	s.report.At = s.recorder.clock()
	s.report.CensusComplete = live && s.current == nil && sourceComplete && expectedProviders >= 0 && expectedConnections >= 0 &&
		expectedProviders == s.report.Providers && expectedConnections == s.report.DeclaredConnections
	s.report.ObservationComplete = s.report.CensusComplete && s.complete && s.report.CapturedConnections == s.report.DeclaredConnections
	s.finishRegistrations()
	s.finishOrigins()
	keys := make([]string, 0, len(s.buckets))
	for key := range s.buckets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		s.report.Buckets = append(s.report.Buckets, *s.buckets[key])
	}
	if !live || s.current != nil {
		return s.report, ErrArinShadowInput
	}
	return s.report, nil
}
