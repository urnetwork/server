package controller_test

// §22.5's inline gate for the contract gate, budgets.yml's acl-contract-gate-inline row (I-12): CreateContract's gate
// step on a warm state runs with every PostgreSQL acquisition in the process refused and every Redis call counted, and
// makes neither. The relationship's one query before it is CreateContract's, not the gate's.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/sidecart3"
)

func TestAclContractGateInline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	step := controller.AclContractGateWarmStepForTest(t)
	var verdict sidecar.AclContractVerdict
	result := sidecart3.InlinePath(t, "acl.contract-gate", sidecartest.NewFakeRedis(ctx, nil, nil, nil, nil), func(redis sidecar.Redis) {
		verdict = step(ctx)
	})
	if verdict.Outcome != sidecar.AclContractAllow || result.RedisCallCount != 0 || result.PostgresAttempts != 0 {
		t.Errorf("%s: %+v", result, verdict)
	}
}
