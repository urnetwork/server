// Exit evidence can ride an already-sampled IP-text response, never a new URL.
package egresshealth

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// Runs only the supplied randomized sample against synthetic HTTPS responses.
func sampledExitRun(t *testing.T, bodies map[string]string) *Result {
	t.Helper()
	dests := []Destination{}
	for _, name := range []string{"one", "two"} {
		if _, present := bodies[name]; present {
			dests = append(dests, Destination{Name: name, Class: ClassConnectivity, Url: "https://" + name + ".example/address", Verify: BodyCheck{Kind: BodyCheckIpText}})
		}
	}
	requests := 0
	client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests++
		body, found := bodies[strings.TrimSuffix(req.URL.Hostname(), ".example")]
		if !found {
			t.Error("unselected host was requested")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	opts := fastOptions()
	opts.exitAddressAllowed = func(ip netip.Addr) bool {
		return netip.MustParsePrefix("192.0.2.0/24").Contains(ip) || netip.MustParsePrefix("198.51.100.0/24").Contains(ip)
	}
	opts.Concurrency = 1
	result, err := check(context.Background(), client, dests, opts)
	if err != nil {
		t.Fatal(err)
	}
	if requests != len(dests) {
		t.Fatalf("extra request outside sample: got=%d want=%d", requests, len(dests))
	}
	return result
}

// One authenticated sampled endpoint can provide location without a warm-up.
func TestSampledIpTextProvidesIndependentExitEvidence(t *testing.T) {
	result := sampledExitRun(t, map[string]string{"one": "192.0.2.7"})
	if result.OkCount != 1 || result.ExitIp != "192.0.2.7" || result.ExitObservedAt.IsZero() {
		t.Fatalf("sampled independent observation lost: %+v", result)
	}
}

// Disagreement is not consensus and must never invent a location.
func TestConflictingSampledIpsDoNotPublishExit(t *testing.T) {
	result := sampledExitRun(t, map[string]string{"one": "192.0.2.7", "two": "198.51.100.8"})
	if result.OkCount != 2 || result.ExitIp != "" || !result.ExitObservedAt.IsZero() {
		t.Fatalf("conflicting evidence placed provider: %+v", result)
	}
}

// Non-public observations cannot place a provider even when content verified.
func TestSampledNonGlobalIpDoesNotPublishExit(t *testing.T) {
	result := sampledExitRun(t, map[string]string{"one": "0.0.0.0"})
	if result.ExitIp != "" || !result.ExitObservedAt.IsZero() {
		t.Fatalf("invalid location evidence accepted: %+v", result)
	}
}

// Test fixtures never become production public-exit exceptions.
func TestSampledExitRejectsSpecialPurposeSpace(t *testing.T) {
	for _, value := range []string{"192.0.2.7", "198.51.100.8", "203.0.113.9", "2001:db8::7", "::ffff:192.0.2.7", "0.0.0.0"} {
		if publicSampledExit(value) != "" {
			t.Fatalf("special-purpose exit accepted: %s", value)
		}
	}
	for _, prefix := range nonExitPrefixes {
		if publicExitAddress(prefix.Addr()) {
			t.Fatalf("reserved prefix accepted: %s", prefix)
		}
	}
}
