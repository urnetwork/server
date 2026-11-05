// Historical operator recovery grants cover two explicitly reviewed singleton
// incidents. They do not authorize a new distributed execution cycle or clock.
package controller

import "fmt"

// This check applies only to new grants after the existing immutable-receipt
// replay path. A future configured policy alone cannot revoke epoch-one recovery.
func validateCompetitionReevaluationRound(round *roundRecord) error {
	if round == nil {
		return ErrConflict
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil {
		return ErrConflict
	}
	if policy.DistributedEvaluationPolicy != nil {
		return fmt.Errorf("distributed terminal-incident recovery requires separately reviewed authority: %w", ErrConflict)
	}
	return nil
}
