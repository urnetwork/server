// The running route table must expose the signed SDK transport on every API
// lifecycle; helper-only or unpublished custody paths cannot complete a window.
package api

import (
	"testing"
)

// Enumerate the exact production route table rather than an alternate test mux.
func TestProviderWorkProductionRoutesAreReachable(t *testing.T) {
	want := map[string]bool{"GET ^/provider-work/v1/requests$": false, "POST ^/provider-work/v1/requests$": false, "GET ^/provider-work/v1/requests/([^/]+)$": false, "POST ^/provider-work/v1/cuts$": false, "GET ^/provider-work/v1/cuts/([^/]+)$": false, "POST ^/provider-work/v1/authorities$": false, "GET ^/provider-work/v1/windows$": false}
	for _, route := range Routes() {
		if _, ok := want[route.String()]; ok {
			if want[route.String()] {
				t.Fatal("duplicate work route", route.String())
			}
			want[route.String()] = true
		}
	}
	for route, present := range want {
		if !present {
			t.Fatal("actual API lacks SDK work route", route)
		}
	}
}
