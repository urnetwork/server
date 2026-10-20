// Original request closure must be reachable from the actual API route owner.
package api

import "testing"

// The route table creates a bounded handler per API lifecycle.
func TestVerifyRequestClosureProductionRouteIsReachable(t *testing.T) {
	count := 0
	for _, route := range Routes() {
		if route.String() == "POST ^/verify/original/close$" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("actual API lacks unique original request closure route", count)
	}
}
