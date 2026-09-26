//go:build acklineagetrace

package perfvar

import (
	"strconv"
	"testing"
)

func TestDohTransferRunDomainMatchesIdentityAndLaunch(t *testing.T) {
	scenario, err := dohTransferScenarioNamed("established-rtt3s-request-prefix")
	if err != nil {
		t.Fatal(err)
	}
	// Identity preflight and the real replay selector must share the same
	// bounded domain. Otherwise an apparently sealed cohort can fail before
	// constructing its first workload, as the historical run-31 cell did.
	for run := -1; run <= 65; run++ {
		t.Run(strconv.Itoa(run), func(t *testing.T) {
			_, identityErr := dohTransferTrace(scenario, run)
			selected, launchErr := parseDohTransferRun(strconv.Itoa(run))
			wantValid := 1 <= run && run <= 64
			if (identityErr == nil) != wantValid || (launchErr == nil) != wantValid {
				t.Fatalf("run=%d wantValid=%t identityErr=%v launchErr=%v", run, wantValid, identityErr, launchErr)
			}
			if wantValid && selected != run {
				t.Fatalf("launch changed declared run: got %d want %d", selected, run)
			}
		})
	}
}

func TestDohTransferRunDomainPinsHistoricalCohort31And32(t *testing.T) {
	for _, profile := range []string{"established-rtt3s-request-prefix", "established-rtt3s-request-prefix-burst-1"} {
		scenario, err := dohTransferScenarioNamed(profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range []int{31, 32} {
			t.Run(profile+"/"+strconv.Itoa(run), func(t *testing.T) {
				declared, err := dohTransferTrace(scenario, run)
				if err != nil {
					t.Fatal(err)
				}
				selected, err := parseDohTransferRun(strconv.Itoa(run))
				if err != nil {
					t.Fatalf("identity preflight accepted run %d but actual selector rejected it: %v", run, err)
				}
				launched, err := dohTransferTrace(scenario, selected)
				if err != nil || launched != declared {
					t.Fatalf("launch changed the declared identity: %v", err)
				}
			})
		}
	}
}

func TestDohTransferRunDomainRejectsInvalidSelectors(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "65", "1.5", "1e1", "1x", " 1", "1 ", "0x1", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			if run, err := parseDohTransferRun(value); err == nil || run != 0 {
				t.Fatalf("invalid selector %q produced run=%d err=%v", value, run, err)
			}
		})
	}
	// An integer overflow cannot enter via the string selector; the identity
	// helper must independently reject every representable out-of-range int.
	scenario, err := dohTransferScenarioNamed("established-rtt3s-request-prefix")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []int{int(^uint(0) >> 1), -int(^uint(0)>>1) - 1} {
		if _, err := dohTransferTrace(scenario, run); err == nil {
			t.Fatalf("out-of-range identity run %d was accepted", run)
		}
	}
}
