// contract_close.yml can only lower the expiry sweep's concurrent closes.
package work

import (
	"testing"

	"github.com/urnetwork/server"
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
