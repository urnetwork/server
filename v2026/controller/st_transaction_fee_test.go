// Exercises fee preparation through the same HTTP fixture as durable account
// reconciliation, including the pre-London gas-price fallback.
package controller

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Initial execution, cancellation, and legacy replacement all use batched fee
// preparation; none depends on a database or a receipt polling interval.
func TestStTransactionReconcileFeePreparationReadsBatchedLegacyFees(t *testing.T) {
	fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100}
	_, rpcClient := newStTransactionReconcileClient(t, fixture, stTransactionReconcileConfig())
	from := common.HexToAddress("0x1000000000000000000000000000000000000001")
	to := common.HexToAddress("0x2000000000000000000000000000000000000002")
	for _, c := range []struct {
		name        string
		estimate    bool
		forceLegacy bool
		wantGas     uint64
	}{
		{name: "execution", estimate: true, wantGas: 50_000},
		{name: "cancellation"},
		{name: "replacement", forceLegacy: true},
	} {
		prepared, err := readStTransactionFeePreparation(context.Background(), rpcClient, from, to, []byte{1, 2, 3}, c.estimate, c.forceLegacy)
		if err != nil {
			t.Fatalf("%s fee preparation: %v", c.name, err)
		}
		if !prepared.legacy || prepared.fee.Cmp(big.NewInt(100)) != 0 || prepared.estimatedGas != c.wantGas || prepared.header.Number.Uint64() != 100 {
			t.Fatalf("%s legacy fee preparation = %+v", c.name, prepared)
		}
	}
}
