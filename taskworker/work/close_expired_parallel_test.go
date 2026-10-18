// contract_close.yml can only lower the expiry sweep's concurrent closes, and
// can turn its owner batches off.
package work

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestCloseExpiredContractsParallelLimitKeepsTheCompiledLimitWithoutConfig(t *testing.T) {
	if limit := closeExpiredContractsParallelLimit(); limit != closeExpiredContractsParallel {
		t.Fatalf("limit without config = %d, want the compiled %d", limit, closeExpiredContractsParallel)
	}
}

func TestCloseExpiredContractsParallelLimitOnlyLowersTheCompiledLimit(t *testing.T) {
	cases := []struct {
		config string
		want   int
	}{
		{config: "expiry_parallel: 24\n", want: 24},
		{config: "expiry_parallel: 1\n", want: 1},
		{config: "deadline_close_parallel: 2\nexpiry_parallel: 46\n", want: 46},
		{config: "deadline_close_parallel: 2\n", want: closeExpiredContractsParallel},
		{config: "expiry_parallel: 0\n", want: closeExpiredContractsParallel},
		{config: "expiry_parallel: -3\n", want: closeExpiredContractsParallel},
		{config: "expiry_parallel: 93\n", want: closeExpiredContractsParallel},
		{config: "expiry_parallel: many\n", want: closeExpiredContractsParallel},
		{config: "expiry_parallel: [2\n", want: closeExpiredContractsParallel},
		{config: "expiry_owner_batch_parallel: 0\n", want: closeExpiredContractsParallel},
		{config: "expiry_parallel: 24\nexpiry_owner_batch_parallel: off\n", want: 24},
	}
	for _, c := range cases {
		func() {
			defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte(c.config))()
			if limit := closeExpiredContractsParallelLimit(); limit != c.want {
				t.Errorf("config %q: limit = %d, want %d", c.config, limit, c.want)
			}
		}()
	}
}

func TestCloseExpiredContractsOwnerBatchLimitKeepsTheCompiledLimitWithoutConfig(t *testing.T) {
	if model.ForceCloseOwnerBatchParallel != 4 {
		t.Fatalf("compiled owner batch limit = %d, want 4", model.ForceCloseOwnerBatchParallel)
	}
	if limit := closeExpiredContractsOwnerBatchLimit(); limit != model.ForceCloseOwnerBatchParallel {
		t.Fatalf("owner batch limit without config = %d, want the compiled %d", limit, model.ForceCloseOwnerBatchParallel)
	}
}

// The owner batch limit only lowers its compiled 4, and 0 turns batching off.
func TestCloseExpiredContractsOwnerBatchLimitOnlyLowersOrTurnsOff(t *testing.T) {
	cases := []struct {
		config string
		want   int
	}{
		{config: "expiry_owner_batch_parallel: 3\n", want: 3},
		{config: "expiry_owner_batch_parallel: 1\n", want: 1},
		{config: "expiry_owner_batch_parallel: 0\n", want: 0},
		{config: "expiry_parallel: 24\nexpiry_owner_batch_parallel: 2\n", want: 2},
		{config: "expiry_parallel: 24\n", want: model.ForceCloseOwnerBatchParallel},
		{config: "expiry_owner_batch_parallel: -1\n", want: model.ForceCloseOwnerBatchParallel},
		{config: "expiry_owner_batch_parallel: 5\n", want: model.ForceCloseOwnerBatchParallel},
		{config: "expiry_owner_batch_parallel: off\n", want: model.ForceCloseOwnerBatchParallel},
		{config: "expiry_owner_batch_parallel: [0\n", want: model.ForceCloseOwnerBatchParallel},
		{config: "expiry_parallel: many\nexpiry_owner_batch_parallel: 1\n", want: 1},
	}
	for _, c := range cases {
		func() {
			defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte(c.config))()
			if limit := closeExpiredContractsOwnerBatchLimit(); limit != c.want {
				t.Errorf("config %q: owner batch limit = %d, want %d", c.config, limit, c.want)
			}
		}()
	}
}
