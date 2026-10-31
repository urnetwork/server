package controller

// The verify cohort's census limits stay inside the SN25 validator's signed
// provider census, max_providers 4,096 (sn VERSION2.md:32; a fixed dimension
// of validator/production_capacity.go:85-94). The validator never evicts a
// scored provider, so every provider the server ever admits counts against
// it; a configuration that could exceed it is refused at load.

import (
	"testing"
)

func TestVerifyCohortCensusFitsTheValidatorCensus(t *testing.T) {
	if verifyValidatorMaxProviders != 4096 {
		t.Fatalf("the validator's signed max_providers is %d, not 4,096: recheck sn before changing it", verifyValidatorMaxProviders)
	}
	if !(0 < verifyDefaultCohortSize && verifyDefaultCohortSize <= verifyDefaultCohortLifetimeLimit &&
		verifyDefaultCohortLifetimeLimit <= verifyDefaultCohortCensusLimit && verifyDefaultCohortCensusLimit <= verifyValidatorMaxProviders) {
		t.Fatalf("cohort %d, lifetime %d, census %d must nest inside the validator's census %d",
			verifyDefaultCohortSize, verifyDefaultCohortLifetimeLimit, verifyDefaultCohortCensusLimit, verifyValidatorMaxProviders)
	}
	// headroom for providers the validator lineage scored outside the cohort
	if verifyValidatorMaxProviders-verifyDefaultCohortCensusLimit < 1024 {
		t.Fatalf("census limit %d leaves under 1,024 of the validator's census", verifyDefaultCohortCensusLimit)
	}
	for _, lifetime := range []int{1, 1500, 2000, 3072, 3500, 4096} {
		census := verifyCohortCensusLimit(lifetime)
		if census < lifetime || verifyValidatorMaxProviders < census {
			t.Fatalf("lifetime %d: census limit %d is below it or past the validator's census", lifetime, census)
		}
	}
	value := func(v int) *int { return &v }
	if _, _, err := verifyCohortSettings("mainnet", nil, value(4097)); err == nil {
		t.Fatal("a lifetime limit past the validator's census was accepted")
	}
	if _, _, err := verifyCohortSettings("mainnet", value(4097), nil); err == nil {
		t.Fatal("a cohort size past the validator's census was accepted")
	}
	if size, limit, err := verifyCohortSettings("mainnet", value(1500), value(4096)); err != nil || size != 1500 || limit != 4096 {
		t.Fatal("the validator's census itself is a valid lifetime limit", size, limit, err)
	}
}
