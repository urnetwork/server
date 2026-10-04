package monitor

// These reasons are assigned only before the sampler calls its host transport.
// They describe local prerequisites, never database execution or backend health.
type pgSamplePreflightReason uint8

const (
	pgSamplePreflightContext pgSamplePreflightReason = iota + 1
	pgSamplePreflightPrimaryMissing
	pgSamplePreflightPrimaryDisabled
	pgSamplePreflightGenerationStale
	pgSamplePreflightGenerationUnobservable
	pgSamplePreflightDirectory
	pgSamplePreflightCadenceLock
	pgSamplePreflightCadenceState
	pgSamplePreflightMarker
)

type pgSamplePreflightError struct {
	reason pgSamplePreflightReason
	err    error
}

func (e *pgSamplePreflightError) Error() string {
	if e != nil {
		switch e.reason {
		case pgSamplePreflightPrimaryMissing, pgSamplePreflightPrimaryDisabled:
			return "monitor: bounded PG sample primary unavailable"
		case pgSamplePreflightGenerationStale:
			return "monitor: bounded PG sample settings generation stale"
		case pgSamplePreflightGenerationUnobservable:
			return "monitor: bounded PG sample settings generation unobservable"
		case pgSamplePreflightDirectory:
			return "monitor: bounded PG sample state unavailable"
		case pgSamplePreflightCadenceLock:
			return "monitor: bounded PG sample cadence lock unavailable"
		case pgSamplePreflightCadenceState:
			return "monitor: bounded PG sample cadence state unavailable"
		case pgSamplePreflightMarker:
			return "monitor: bounded PG sample durable marker unavailable"
		}
	}
	return "monitor: bounded PG sample local prerequisite unavailable"
}

func (e *pgSamplePreflightError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *pgSamplePreflightError) projection() (phase, cause string, valid bool) {
	if e == nil {
		return "", "", false
	}
	switch e.reason {
	case pgSamplePreflightContext:
		return "local-preflight", "context-ended", true
	case pgSamplePreflightPrimaryMissing:
		return "inventory-preflight", "primary-unavailable", true
	case pgSamplePreflightPrimaryDisabled:
		return "inventory-preflight", "primary-disabled", true
	case pgSamplePreflightGenerationStale:
		return "settings-generation", "stale", true
	case pgSamplePreflightGenerationUnobservable:
		return "settings-generation", "unobservable", true
	case pgSamplePreflightDirectory:
		return "local-state", "directory-unavailable", true
	case pgSamplePreflightCadenceLock:
		return "cadence-admission", "lock-unavailable", true
	case pgSamplePreflightCadenceState:
		return "cadence-admission", "state-unavailable", true
	case pgSamplePreflightMarker:
		return "one-shot-admission", "marker-unavailable", true
	default:
		return "", "", false
	}
}
