// SIGNALS.md §1.7: retain only code-owned local execution boundaries. These
// facts do not attribute network failures or prove any remote command ran.
package monitor

import "strings"

type observationBoundary uint8

const (
	observationBoundaryUnknown observationBoundary = iota
	observationBoundaryGlobal
	observationBoundaryPreflight
	observationBoundaryHost
	observationBoundaryNativeGuard
	observationBoundaryNativeStart
	observationBoundaryNativeWait
	observationBoundaryNativeWaitUnknown
	observationBoundaryCount
)

type observationBoundaryError struct {
	err      error
	boundary observationBoundary
}

func (self *observationBoundaryError) Error() string { return self.err.Error() }
func (self *observationBoundaryError) Unwrap() error { return self.err }

// A native error can be normalized into an existing timeout/cancellation class
// without losing its fixed boundary. No proof is synthesized for callbacks.
func carryObservationBoundary(source, result error) error {
	if proof, ok := source.(*observationBoundaryError); ok {
		return &observationBoundaryError{err: result, boundary: proof.boundary}
	}
	return result
}

// A bounded set describes independent returned error branches, never their
// count, ordering, shared identity or one continuous execution. Absence of a
// marker means unknown. Native Start/Wait are the only native proof sites.
// This bounds only this projection's child inspections/visits. Existing error
// classification and admission-phase traversal have no new work guarantee;
// neither arbitrary Error/Unwrap implementations nor those walkers are capped.
func observationBoundaryDetails(err error) string {
	var present [observationBoundaryCount]bool
	var pending [64]error
	pending[0] = err
	length := 1
	inspected := 1 // includes the root; nil child slots also consume work
	for visited := 0; length > 0; visited++ {
		if visited == len(pending) {
			present[observationBoundaryUnknown] = true
			break
		}
		length--
		current := pending[length]
		pending[length] = nil
		if current == nil {
			continue
		}
		switch proof := current.(type) {
		case *runSlotAdmissionError:
			present[observationBoundaryGlobal] = true
		case *observationBoundaryError:
			if proof.boundary < observationBoundaryCount {
				present[proof.boundary] = true
			} else {
				present[observationBoundaryUnknown] = true
			}
		}
		push := func(child error) bool {
			if inspected == len(pending) {
				present[observationBoundaryUnknown] = true
				return false
			}
			inspected++
			if child == nil {
				return true
			}
			pending[length] = child
			length++
			return true
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			remaining := len(pending) - inspected
			if len(children) > remaining {
				present[observationBoundaryUnknown] = true
				children = children[:remaining]
			}
			for _, child := range children {
				if !push(child) {
					break
				}
			}
		case interface{ Unwrap() error }:
			if inspected == len(pending) {
				present[observationBoundaryUnknown] = true
			} else {
				push(wrapped.Unwrap())
			}
		}
	}
	names := [...]string{
		"unknown/unknown",
		"global-admission/not-attempted",
		"transport-preflight/not-attempted",
		"host-admission/not-attempted",
		"native-guard/not-attempted",
		"native-start/failed-no-child",
		"native-wait/started-joined",
		"native-wait/started-join-unknown",
	}
	var selected []string
	for boundary, found := range present {
		if found {
			selected = append(selected, names[boundary])
		}
	}
	if len(selected) == 0 {
		return ""
	}
	return " execution_boundaries=" + strings.Join(selected, ",")
}
