// SIGNALS.md §1.3b: prepared diagnostics never expose the original error to
// generic alert routing. Returned markers describe local facts, not a backend.
package monitor

import (
	"errors"
	"fmt"
	"strings"
)

const poolRetentionUnavailableText = "PostgreSQL pool-retention observation unavailable"

// Owning return-branch identity; never remote progress.
type poolRetentionOrigin uint8

const (
	poolRetentionOriginUnknown poolRetentionOrigin = iota
	poolRetentionOriginReadError
	poolRetentionOriginResponseRejected
)

// Return the immutable existing local-phase vocabulary.
func poolRetentionAdmissionNames() [3]string {
	return [3]string{"acquire", "release", "unknown"}
}

// Return the immutable existing execution-boundary vocabulary.
func poolRetentionBoundaryNames() [9]string {
	return [9]string{
		"unknown/unknown", "global-admission/not-attempted", "transport-preflight/not-attempted",
		"host-admission/not-attempted", "native-guard/not-attempted", "native-start/failed-no-child",
		"native-wait/started-joined", "native-wait/started-join-unknown", "mimir-host-admission/not-attempted",
	}
}

// Copied finite metadata; no source error or rows retained.
type poolRetentionPreparedFields struct {
	origin      poolRetentionOrigin
	sourceClass string
	admission   [3]bool
	boundaries  [9]bool
}

// There is deliberately no Unwrap, As or Is method. Only a copied legacy
// normalized error string and finite value metadata survive this boundary.
type poolRetentionPreparedError struct {
	legacyText string
	fields     poolRetentionPreparedFields
}

// Preserve controlled legacy text and reject malformed carrier text.
func (self *poolRetentionPreparedError) Error() string {
	if self == nil {
		return poolRetentionUnavailableText
	}
	return poolRetentionLegacyText(self.legacyText)
}

// All parser errors are locally generated. Preserve their exact prior text,
// including controlled shape counts, without permitting arbitrary source text.
func poolRetentionLegacyText(text string) string {
	if text == poolRetentionUnavailableText || text == "inconsistent PostgreSQL pool-retention summary" {
		return text
	}
	for _, field := range []string{"normal ceiling", "total clients", "loopback clients", "loopback idle", "loopback active", "loopback idle-in-transaction", "aged loopback idle", "oldest loopback idle"} {
		if text == "invalid PostgreSQL pool-retention "+field {
			return text
		}
	}
	for _, shape := range []string{"PostgreSQL pool-retention query returned %d rows, want 1", "PostgreSQL pool-retention query returned %d columns, want 8"} {
		var count int
		if n, err := fmt.Sscanf(text, shape, &count); err == nil && n == 1 && count >= 0 && fmt.Sprintf(shape, count) == text {
			return text
		}
	}
	return poolRetentionUnavailableText
}

// Admit existing classes only; forged values stay unclassified.
func poolRetentionSafeSourceClass(class string) string {
	switch class {
	case observationErrorClassTimeout, observationErrorClassCanceled, observationErrorClassUnreachable,
		observationErrorClassCounterReset, observationErrorClassContractMismatch, observationErrorClassStateUnavailable,
		observationErrorClassAdmissionUnavailable, observationErrorClassBoundExceeded, observationErrorClassInvalidResponse,
		observationErrorClassMetricUnavailable, observationErrorClassAccessDenied, observationErrorClassCommandFailed,
		observationErrorClassSSHExit255, observationErrorClassUnclassified,
		observationErrorClassLogHttp429, observationErrorClassLogHttp500, observationErrorClassLogHttp502,
		observationErrorClassLogHttp503, observationErrorClassLogHttp504, observationErrorClassLogConfigSchema:
		return class
	default:
		return observationErrorClassUnclassified
	}
}

// Snapshot returned local facts before discarding the source tree.
func preparePoolRetentionReadError(source error) error {
	fields := poolRetentionPreparedFields{origin: poolRetentionOriginReadError, sourceClass: poolRetentionSafeSourceClass(classifyObservationError(source))}
	if phases := sshAdmissionFailurePhases(source); phases != "" {
		for _, phase := range strings.Split(phases, ",") {
			found := false
			for index, name := range poolRetentionAdmissionNames() {
				if phase == name {
					fields.admission[index] = true
					found = true
				}
			}
			if !found {
				fields.admission[2] = true
			}
		}
	}
	if boundaries := observationBoundaryDetails(source); boundaries != "" {
		const prefix = " execution_boundaries="
		if !strings.HasPrefix(boundaries, prefix) {
			fields.boundaries[0] = true
		} else {
			for _, boundary := range strings.Split(strings.TrimPrefix(boundaries, prefix), ",") {
				found := false
				for index, name := range poolRetentionBoundaryNames() {
					if boundary == name {
						fields.boundaries[index] = true
						found = true
					}
				}
				if !found {
					fields.boundaries[0] = true
				}
			}
		}
	}
	return &poolRetentionPreparedError{legacyText: poolRetentionUnavailableText, fields: fields}
}

// Keep the legacy parser error without borrowing read-failure facts.
func preparePoolRetentionRejectedError(legacy error) error {
	text := poolRetentionUnavailableText
	if legacy != nil {
		text = poolRetentionLegacyText(legacy.Error())
	}
	return &poolRetentionPreparedError{legacyText: text, fields: poolRetentionPreparedFields{origin: poolRetentionOriginResponseRejected}}
}

// Only direct owner carriers are eligible. In particular, never search a
// signalContextError or a joined tree for metadata after its owner has ended.
func splitPoolRetentionPreparedError(signal Signal, err error) (error, *poolRetentionPreparedFields) {
	prepared, ok := err.(*poolRetentionPreparedError)
	if !ok {
		return err, nil
	}
	legacy := errors.New(prepared.Error())
	if prepared == nil || signal.Key() != "pool-retention" || signal.ID() != "pg/pool-retention" || signal.Number() != "1.3b" {
		return legacy, nil
	}
	fields := prepared.fields
	return legacy, &fields
}

// Emit finite pool fields; unknown and rejected origins cannot borrow source markers.
func (self poolRetentionPreparedFields) details() string {
	origin := "unknown"
	switch self.origin {
	case poolRetentionOriginReadError:
		origin = "read_error"
	case poolRetentionOriginResponseRejected:
		origin = "response_rejected"
	}
	details := "pool_retention_origin=" + origin
	if self.origin != poolRetentionOriginReadError {
		return details
	}
	details += " pool_retention_source_error_class=" + poolRetentionSafeSourceClass(self.sourceClass)
	var phases []string
	for index, found := range self.admission {
		if found {
			phases = append(phases, poolRetentionAdmissionNames()[index])
		}
	}
	if len(phases) != 0 {
		details += " ssh_admission_failures=" + strings.Join(phases, ",")
	}
	var boundaries []string
	for index, found := range self.boundaries {
		if found {
			boundaries = append(boundaries, poolRetentionBoundaryNames()[index])
		}
	}
	if len(boundaries) != 0 {
		details += " execution_boundaries=" + strings.Join(boundaries, ",")
	}
	return details
}
