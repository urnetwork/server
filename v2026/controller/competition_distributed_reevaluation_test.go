// New distributed grants remain fail closed; historical singleton authority
// and stored policy bytes remain unchanged by a staged future executor policy.
package controller

import (
	"bytes"
	"errors"
	"testing"
)

// Frozen policy, not mutable API rollout configuration, determines the boundary.
func TestCompetitionDistributedReevaluationRequiresSeparateIncidentAuthority(t *testing.T) {
	settings := automationApiSettings()
	legacy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	staged, err := policySnapshot(settings)
	if err != nil || !bytes.Equal(legacy, staged) {
		t.Fatal("staged executor policy changed historical recovery authority", err)
	}
	for _, epoch := range []int{1, 2, 6} {
		if err := validateCompetitionReevaluationRound(&roundRecord{RoundResult: RoundResult{Epoch: epoch}, PolicyJson: legacy}); err != nil {
			t.Fatal("singleton recovery was narrowed by an inactive future policy", epoch, err)
		}
	}
	for _, epoch := range []int{2, 6} {
		future, err := settings.ForCompetitionEpoch(epoch)
		if err != nil {
			t.Fatal(err)
		}
		frozen, err := policySnapshot(future)
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), frozen...)
		if err := validateCompetitionReevaluationRound(&roundRecord{RoundResult: RoundResult{Epoch: epoch}, PolicyJson: frozen}); !errors.Is(err, ErrConflict) || !bytes.Equal(before, frozen) {
			t.Fatal("legacy incident authority admitted or rewrote a distributed cycle", epoch, err)
		}
	}
	for _, round := range []*roundRecord{nil, {}, {PolicyJson: []byte(`{"distributed_evaluation_policy":{}}`)}} {
		if err := validateCompetitionReevaluationRound(round); !errors.Is(err, ErrConflict) {
			t.Fatal("missing or invalid frozen policy granted incident authority", err)
		}
	}
}
