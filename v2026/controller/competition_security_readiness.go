// API host checks observe a separately authenticated execution identity while
// source, workload and historical control retain their frozen epoch pins.
package controller

import "context"

// The API-purpose grant already binds its exact round. Load heartbeat
// authority before the outer readiness snapshot, not only the later lifecycle
// projection, or the public result can remain false after grading starts.
// No caller-owned connection or transaction spans these independent reads.
func (self PostgresStore) competitionReadinessExecutionSettings(ctx context.Context, settings *Settings) (*Settings, error) {
	if settings.intakeFirstActivation == nil || settings.DistributedEvaluationPolicy == nil {
		return settings, nil
	}
	derived := *settings
	derived.readinessSecurityToolchainException = nil
	derived.automationRoundId = settings.intakeFirstActivation.TargetRoundId
	round, err := self.GetRound(ctx, &derived, derived.automationRoundId)
	if err != nil {
		return nil, err
	}
	baseline, err := self.distributedRoundBaseline(ctx, &derived, round.RoundId)
	if err != nil {
		return nil, err
	}
	if baseline == nil {
		// A newly opened intake round legitimately has no grading control yet.
		return &derived, nil
	}
	if err := self.loadExecutorControlAmendment(ctx, &derived, round); err != nil {
		return nil, err
	}
	if err := self.loadCompetitionReadinessSecurityToolchain(ctx, &derived, round, baseline); err != nil {
		return nil, err
	}
	return &derived, nil
}
