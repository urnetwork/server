package model

// pro.yml has no contract size: every payer, free or Pro, is granted contracts
// up to the controller's one ceiling.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// A pro.yml that still carries the removed per-tier contract cap
// (<tier>.max_contract_transfer_byte_count) parses to the same tiers as one
// without it, so no environment's pro.yml can size free and Pro contracts
// differently.
func TestProTierHasNoContractSize(t *testing.T) {
	parse := func(text string) (free ProTier, pro ProTier) {
		var y proConfigYaml
		if err := yaml.Unmarshal([]byte(text), &y); err != nil {
			t.Fatal(err)
		}
		return parseProTier(y.Free), parseProTier(y.Pro)
	}
	free, pro := parse(`
free:
  data: 30gib
  data_period: 24h
pro:
  data: 300gib
  data_period: 720h
`)
	staleFree, stalePro := parse(`
free:
  data: 30gib
  data_period: 24h
  max_contract_transfer_byte_count: 64mib
pro:
  data: 300gib
  data_period: 720h
  max_contract_transfer_byte_count: 32mib
`)
	if staleFree != free {
		t.Errorf("free tier with the removed key = %+v, want %+v", staleFree, free)
	}
	if stalePro != pro {
		t.Errorf("pro tier with the removed key = %+v, want %+v", stalePro, pro)
	}
}
