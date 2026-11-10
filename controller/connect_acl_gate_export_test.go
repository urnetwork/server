package controller

// What controller_test's inline path reaches of the contract gate, a bridge kept in a _test.go file so that no
// production name is exported for it.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar"
)

// CreateContract's gate step on a warm state, installed until the test ends: the step to run inline.
func AclContractGateWarmStepForTest(tb testing.TB) func(ctx context.Context) sidecar.AclContractVerdict {
	return newAclGateWarmStep(tb, true).run
}
