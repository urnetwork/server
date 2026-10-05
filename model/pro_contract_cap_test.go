package model

// The per-tier contract size cap read from pro.yml.

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
)

// The contract cap is optional in pro.yml (free.max_contract_transfer_byte_count
// for the free tier). A tier that does not set it is not capped, and neither is
// one that sets zero or less, so the key can ship unset without changing any
// grant.
func TestProMaxContractTransferByteCount(t *testing.T) {
	for _, test := range []struct {
		name string
		yaml string
		want ByteCount
	}{
		{
			name: "unset",
			yaml: `
free:
  data: 30gib
  data_period: 24h
`,
			want: 0,
		},
		{
			name: "zero",
			yaml: `
free:
  data: 30gib
  data_period: 24h
  max_contract_transfer_byte_count: 0
`,
			want: 0,
		},
		{
			name: "negative",
			yaml: `
free:
  data: 30gib
  data_period: 24h
  max_contract_transfer_byte_count: -1
`,
			want: 0,
		},
		{
			name: "64 MiB",
			yaml: `
free:
  data: 30gib
  data_period: 24h
  max_contract_transfer_byte_count: 64mib
`,
			want: 64 * Mib,
		},
	} {
		var y proConfigYaml
		if err := yaml.Unmarshal([]byte(test.yaml), &y); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		c := &ProConfig{Free: parseProTier(y.Free)}
		if got := c.MaxContractTransferByteCount(false); got != test.want {
			t.Errorf("%s: free cap = %d, want %d", test.name, got, test.want)
		}
		// the pro tier has its own key and stays uncapped
		if got := c.MaxContractTransferByteCount(true); got != 0 {
			t.Errorf("%s: pro cap = %d, want 0", test.name, got)
		}
	}

	// an absent pro.yml caps nothing
	absent := &ProConfig{}
	connect.AssertEqual(t, ByteCount(0), absent.MaxContractTransferByteCount(false))
	connect.AssertEqual(t, ByteCount(0), absent.MaxContractTransferByteCount(true))

	// the test override reaches the process config and restores it
	freeBefore := Pro().MaxContractTransferByteCount(false)
	proBefore := Pro().MaxContractTransferByteCount(true)
	restore := Testing_SetMaxContractTransferByteCount(64*Mib, 32*Mib)
	connect.AssertEqual(t, 64*Mib, Pro().MaxContractTransferByteCount(false))
	connect.AssertEqual(t, 32*Mib, Pro().MaxContractTransferByteCount(true))
	restore()
	connect.AssertEqual(t, freeBefore, Pro().MaxContractTransferByteCount(false))
	connect.AssertEqual(t, proBefore, Pro().MaxContractTransferByteCount(true))
}
