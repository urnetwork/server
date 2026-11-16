// Synthetic scalar receipts reach the actual post-work verifier. They prove
// classifier/error ordering only, not TLS, network or product-stall behavior.
package perfvar

import (
	"crypto/tls"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// Reuse the existing owned-request model with five distinct synthetic sessions.
func newC48ReturnFlightVerdictControl(t *testing.T) *nativeBrowsingColdLinkWitness {
	t.Helper()
	witness := newNativeBrowsingColdLinkWitness(&sdkBrowsingScenario{}, &nativeBrowsingPressureObservation{}, true)
	t.Cleanup(witness.close)
	witness.row.Html, witness.row.Assets = 100, 400
	witness.row.ColdEdges, witness.row.VisitedDestinationEdges, witness.row.SameSessionEdges = 1, 98, 98
	witness.row.CssOwnerRetained, witness.row.ColdTupleOwned = true, true
	var sessions [nativeBrowsingOriginCount + 1]tls.Conn
	for page := range witness.requests {
		witness.row.CombinedFirstByteNs[page] = int64(time.Millisecond)
		origin := page % len(sessions)
		for slot := range witness.requests[page] {
			request := nativeBrowsingOwnedColdRequestForTest(witness.requests[page][slot])
			request.clientSession = &sessions[origin]
			request.serverTuple = fmt.Sprintf("192.0.2.1:%d|198.51.100.1:443", 40000+origin)
			request.serverProvider = origin
			witness.requests[page][slot] = request
		}
	}
	return witness
}

// The original first slow error stays first, while the later missing value
// prevents that failure from authorizing another independent measurement arm.
func TestC48ReturnFlightVerdictSlowBeforeMissingIsNotNumeric(t *testing.T) {
	witness := newC48ReturnFlightVerdictControl(t)
	limit := sdk.DefaultDeviceLocalSettings().SendTimeout
	witness.row.CombinedFirstByteNs[2] = int64(limit)
	witness.row.CombinedFirstByteNs[99] = 0
	err, numeric := witness.verdictWithLatencyForTest()
	want := fmt.Sprintf("NATIVE_BROWSING_NAVIGATION_STALL: page=2 combined_tap_to_first_byte=%s", limit)
	if err == nil || err.Error() != want || numeric {
		t.Fatalf("slow-before-missing classification: err=%v numeric=%t", err, numeric)
	}
	if original := witness.verdict(); original == nil || original.Error() != want {
		t.Fatalf("original first error changed: %v", original)
	}
}

// Missing or negative timepoints remain hard failures even with a later stall.
func TestC48ReturnFlightVerdictMissingFirstRemainsHard(t *testing.T) {
	for _, missing := range []int64{0, -1} {
		witness := newC48ReturnFlightVerdictControl(t)
		witness.row.CombinedFirstByteNs[0] = missing
		witness.row.CombinedFirstByteNs[2] = int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
		err, numeric := witness.verdictWithLatencyForTest()
		want := fmt.Sprintf("NATIVE_BROWSING_NAVIGATION_STALL: page=0 combined_tap_to_first_byte=%s", time.Duration(missing))
		if err == nil || err.Error() != want || numeric {
			t.Fatalf("missing-first classification: err=%v numeric=%t", err, numeric)
		}
		if original := witness.verdict(); original == nil || original.Error() != want {
			t.Fatalf("original missing-timepoint error changed: %v", original)
		}
	}
}

// A complete healthy census passes; only a positive stall may be numeric.
func TestC48ReturnFlightVerdictAllPositiveClassifiesOnlyStall(t *testing.T) {
	for _, slow := range []bool{false, true} {
		witness := newC48ReturnFlightVerdictControl(t)
		limit := sdk.DefaultDeviceLocalSettings().SendTimeout
		if slow {
			witness.row.CombinedFirstByteNs[2] = int64(limit)
		}
		err, numeric := witness.verdictWithLatencyForTest()
		if !slow {
			if err != nil || numeric || witness.verdict() != nil {
				t.Fatalf("healthy complete census failed: err=%v numeric=%t", err, numeric)
			}
			continue
		}
		want := fmt.Sprintf("NATIVE_BROWSING_NAVIGATION_STALL: page=2 combined_tap_to_first_byte=%s", limit)
		if err == nil || err.Error() != want || !numeric {
			t.Fatalf("positive-only stall classification: err=%v numeric=%t", err, numeric)
		}
		if original := witness.verdict(); original == nil || original.Error() != want {
			t.Fatalf("original positive-stall error changed: %v", original)
		}
	}
}
