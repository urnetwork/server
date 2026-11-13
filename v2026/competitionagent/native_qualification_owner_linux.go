// A prospective collection has one bounded ordinary owner and a distinct,
// joined cleanup owner. Observation records never satisfy native admission.
package competitionagent

import (
	"context"
	"errors"
	"time"
)

// All tunable lifetimes/evidence bounds belong to one collection instance.
type nativeQualificationOwnerSettings struct {
	TotalTimeout         time.Duration
	TrialTimeout         time.Duration
	CleanupTimeout       time.Duration
	ArtifactBytes        int64
	EvidenceBytes        int64
	MaximumCorePipeLimit int
}

// These are prospective collection ceilings, not scoring or admission limits.
func defaultNativeQualificationOwnerSettings() nativeQualificationOwnerSettings {
	return nativeQualificationOwnerSettings{TotalTimeout: 2 * time.Hour, TrialTimeout: 10 * time.Minute, CleanupTimeout: 2 * time.Minute, ArtifactBytes: 16 * 1024 * 1024, EvidenceBytes: 128 * 1024 * 1024, MaximumCorePipeLimit: DefaultRuntimeResourceBoundarySettings().MaximumCorePipeLimit}
}

// The concrete handler must retain its bounded immutable manifest itself;
// runner injection remains private to this package's deterministic controls.
type nativeQualificationObservation struct {
	Name                  string                      `json:"name"`
	RuntimeIdentitySha256 string                      `json:"runtime_identity_sha256"`
	Kernel                RuntimeNativeKernelIdentity `json:"kernel"`
	Outcome               string                      `json:"outcome"`
	ArtifactManifest      Pin                         `json:"artifact_manifest"`
	ArtifactBytes         int64                       `json:"artifact_bytes"`
}

// A distinct kind and explicit false authority cannot be decoded as the
// existing native qualification/acceptance schemas, even after all12 observed.
type nativeQualificationCollectionObservation struct {
	Schema                int                              `json:"schema"`
	Kind                  string                           `json:"kind"`
	RuntimeIdentitySha256 string                           `json:"runtime_identity_sha256"`
	Kernel                RuntimeNativeKernelIdentity      `json:"kernel"`
	StartedAt             time.Time                        `json:"started_at"`
	OriginalDeadline      time.Time                        `json:"original_deadline"`
	CompletedAt           time.Time                        `json:"completed_at"`
	Trials                []nativeQualificationObservation `json:"trials"`
	AllTrialsObserved     bool                             `json:"all_trials_observed"`
	OrdinaryJoined        bool                             `json:"ordinary_joined"`
	CleanupJoined         bool                             `json:"cleanup_joined"`
	StoppedTrial          string                           `json:"stopped_trial,omitempty"`
	StopOutcome           string                           `json:"stop_outcome,omitempty"`
	Accepted              bool                             `json:"accepted"`
	RuntimeQualified      bool                             `json:"runtime_qualified"`
}

// One closed handler sequence is synchronous: a canceled operation must unwind
// before this function returns. Cleanup dispatch is independent and can force
// actual owned process/container teardown while that unwind is still pending.
// The eventual public entrypoint binds only concrete native operations here.
func collectNativeQualificationObservations(parent context.Context, identity string, kernel RuntimeNativeKernelIdentity, boundary RuntimeResourceBoundary, settings nativeQualificationOwnerSettings, run func(context.Context, string) (nativeQualificationObservation, error), cleanup func(context.Context) error) (observation nativeQualificationCollectionObservation, resultErr error) {
	observation = nativeQualificationCollectionObservation{Schema: Schema, Kind: "runtime-native-class-collection-observation/v1", RuntimeIdentitySha256: identity, Kernel: kernel, StartedAt: time.Now().UTC()}
	if parent == nil || !digestPattern.MatchString(identity) || !validRuntimeNativeKernel(kernel, settings.MaximumCorePipeLimit) || settings.TotalTimeout <= 0 || settings.TrialTimeout <= 0 || settings.CleanupTimeout <= 0 || settings.ArtifactBytes <= 0 || settings.EvidenceBytes <= 0 || run == nil || cleanup == nil {
		return observation, errors.New("prospective collection requires finite independent owners and complete identity")
	}
	boundIdentity, identityErr := prospectiveNativeQualificationIdentity(boundary)
	if identityErr != nil || boundIdentity != identity || boundary.nativeClass != nil || boundary.NativeClass.Receipt != (Pin{}) {
		return observation, errors.New("prospective collection is not bound to its pending unadmitted boundary")
	}
	if err := parent.Err(); err != nil {
		return observation, err
	}
	owner, cancel := context.WithTimeout(parent, settings.TotalTimeout)
	defer cancel()
	observation.OriginalDeadline, _ = owner.Deadline()
	cleanupDone := make(chan error, 1)
	// A single cleanup lifetime is allocated only once. It is never refreshed
	// per trial and preserves parent values without inheriting cancellation.
	go func() {
		<-owner.Done()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(parent), settings.CleanupTimeout)
		defer cleanupCancel()
		err := cleanup(cleanupCtx)
		cleanupDone <- errors.Join(err, cleanupCtx.Err())
	}()
	defer func() {
		// All ordinary calls have returned before the deferred boundary. A
		// successful cleanup never substitutes for an unjoined ordinary call.
		observation.OrdinaryJoined = true
		cancel()
		cleanupErr := <-cleanupDone
		observation.CleanupJoined = true
		resultErr = errors.Join(resultErr, cleanupErr, parent.Err())
		if resultErr != nil {
			observation.AllTrialsObserved = false
			observation.StopOutcome = "failed"
			if errors.Is(resultErr, errNativeQualificationUnsupported) {
				observation.StopOutcome = "unsupported"
			} else if errors.Is(resultErr, context.DeadlineExceeded) {
				observation.StopOutcome = "owner-expired"
			} else if errors.Is(resultErr, context.Canceled) {
				observation.StopOutcome = "canceled"
			}
		}
		observation.CompletedAt = time.Now().UTC()
	}()
	var evidenceBytes int64
	for _, name := range runtimeNativeTrialNames() {
		observation.StoppedTrial = name
		if err := owner.Err(); err != nil {
			return observation, err
		}
		trial, trialCancel := context.WithTimeout(owner, settings.TrialTimeout)
		trialTerminal := make(chan struct{})
		stopTrialCleanup := context.AfterFunc(trial, func() { cancel(); close(trialTerminal) })
		value, err := run(trial, name)
		if !stopTrialCleanup() {
			<-trialTerminal
		}
		trialErr := trial.Err()
		trialCancel()
		if value.Name != name || value.RuntimeIdentitySha256 != identity || value.Kernel != kernel || (value.Outcome != "observed" && value.Outcome != "failed" && value.Outcome != "unsupported") || !runtimeNativePin(value.ArtifactManifest, boundary) || value.ArtifactBytes <= 0 || value.ArtifactBytes > settings.ArtifactBytes || value.ArtifactBytes > settings.EvidenceBytes-evidenceBytes {
			return observation, errors.Join(err, trialErr, errors.New("native trial did not retain its exact bounded observation"))
		}
		observation.Trials = append(observation.Trials, value)
		evidenceBytes += value.ArtifactBytes
		if value.Outcome == "unsupported" {
			err = errors.Join(err, errNativeQualificationUnsupported)
		} else if value.Outcome != "observed" {
			err = errors.Join(err, errors.New("native trial retained a failed observation"))
		}
		if err = errors.Join(err, trialErr, owner.Err()); err != nil {
			return observation, err
		}
	}
	// This is an explicit observation boundary, not an impossible atomic
	// promise against a cancellation arriving later during joined cleanup.
	if err := owner.Err(); err != nil {
		return observation, err
	}
	observation.AllTrialsObserved = true
	observation.StoppedTrial = ""
	return observation, nil
}
