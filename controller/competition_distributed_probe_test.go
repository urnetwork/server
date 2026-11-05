// Native qualification metadata remains explicit and cannot be mistaken for
// a new live namespace probe merely because a signed checker was reachable.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompetitionPrivateHostProbeRetainsHistoricalNativeBoundary(t *testing.T) {
	check := HostSelfCheck{NamespaceProbeSource: "retained_native_containment", PrivateRuntimeBoundarySha256: strings.Repeat("a", 64)}
	content, err := json.Marshal(check)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeHostSelfCheck(content)
	if err != nil || validateCompetitionPrivateHostProbe(decoded) != nil || decoded.PrivateRuntimeBoundarySha256 != check.PrivateRuntimeBoundarySha256 {
		t.Fatal("exact retained native boundary metadata was lost", err)
	}
	for _, change := range []func(*HostSelfCheck){
		func(value *HostSelfCheck) { value.NamespaceProbeSource = "" },
		func(value *HostSelfCheck) { value.NamespaceProbeSource = "live_probe" },
		func(value *HostSelfCheck) { value.NamespaceProbeFresh = true },
		func(value *HostSelfCheck) { value.PrivateRuntimeBoundarySha256 = "" },
	} {
		changed := check
		change(&changed)
		if validateCompetitionPrivateHostProbe(changed) == nil {
			t.Fatal("missing or misleading live-map metadata was admitted")
		}
	}
}
