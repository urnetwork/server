// Catalog fixtures are explicit and synthetic; no compiled production URLs exist.
package egresshealth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

var testDestinations = testCatalogDestinations()

// Missing configuration must fail before any request, including legacy callers.
func TestMissingCatalogNeverFallsBackToCompiledUrls(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("synthetic transport has no network")
	})}
	result, err := Check(context.Background(), client, Options{LoadAttempts: 1, Concurrency: 1, Budget: time.Second})
	if !errors.Is(err, ErrNoDestinations) || result != nil || requests != 0 {
		t.Fatalf("missing catalog result=%v error=%v requests=%d", result != nil, err, requests)
	}
}

// Enough synthetic contracts to exercise sampling across the legacy class shapes.
func testCatalogDestinations() []Destination {
	result := []Destination{}
	for _, class := range Classes {
		for index := range sampleSizes[class] + 7 {
			name := fmt.Sprintf("synthetic-%s-%02d", class, index)
			destination := Destination{Name: name, Class: class, Url: "https://" + name + ".example/", MaxBytes: maxAssetBytes}
			switch class {
			case ClassDns:
				destination.Url += "dns-query?name=example.com&type=A"
				destination.Headers = map[string]string{"Accept": acceptDnsJson}
				destination.MaxBytes = maxDnsBytes
				destination.Verify = BodyCheck{Kind: BodyCheckDnsJson}
			case ClassConnectivity:
				destination.MaxBytes = maxConnectivityBytes
				if index%2 == 0 {
					destination.Expect = ExpectStatus
					destination.Status = http.StatusNoContent
				} else {
					destination.Verify = BodyCheck{Kind: BodyCheckContains, Text: "synthetic-success"}
				}
			}
			result = append(result, destination)
		}
	}
	return result
}

// Each caller owns its fixture rather than sharing mutable pool state.
func testCatalogPool() *Pool {
	return &Pool{Version: 1, GeneratedAt: time.Now().UTC(), Destinations: testCatalogDestinations(), Profile: DefaultRequestProfile()}
}
