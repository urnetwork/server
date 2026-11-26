package monitor

// SIGNALS.md §32.5 (CSRV-M12-06): the enclaves' family reports only whether its series is current.

import (
	"testing"
)

// Live, the family reports nothing, §14.7 naming no enclave page; with no current series it reads blind.
func TestComputeEnclavesReportOnlyTheirLiveness(t *testing.T) {
	settings := computeTestSettings("enclaves")
	if findings := computeFamilyJudge(computeEnclavesFamily(), settings, []*sidecarStorageSample{computeTestLive()}); len(findings) != 0 {
		t.Errorf("a live family reports %+v", findings)
	}
	findings := computeFamilyJudge(computeEnclavesFamily(), settings, nil)
	if len(findings) != 1 || findings[0].class != "compute-enclaves-unobservable" || !findings[0].isUnobservable() {
		t.Errorf("a family with no series reports %+v", findings)
	}
}
