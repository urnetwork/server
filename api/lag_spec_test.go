package api

import (
	"testing"

	"github.com/urnetwork/server/controller"
)

// The lag.dev routes (lag.dev/PLAN.md A1, A4, A6, A7, A11) against the
// published spec (connect/api/bringyour.yml), with the conformance checker of
// spec_conformance_test.go: every property the spec promises exists in the Go
// type with a compatible type. my-ip-info's `asn` (A3) is checked there, by
// the registry's MyIpInfoResult.

func lagSpecRegistry() []specEndpoint {
	return []specEndpoint{
		// the body is `{}` and not read
		{"POST", "/lag/session", nil, rt(controller.LagSessionResult{})},
		{"GET", "/lag/extenders", nil, rt(controller.LagExtendersResult{})},
		{"POST", "/lag/track", rt(controller.LagTrackArgs{}), rt(controller.LagTrackResult{})},
		{"POST", "/network/extender-lag-report", rt(controller.ExtenderLagReportArgs{}), rt(controller.ExtenderLagReportResult{})},
	}
}

func TestLagSpecConformance(t *testing.T) {
	spec := loadSpec(t)
	checker := &conformanceChecker{t: t, spec: spec}
	implKeys := implRouteKeys()
	for _, ep := range lagSpecRegistry() {
		endpoint := ep.method + " " + ep.path
		if !spec.hasOperation(ep.method, ep.path) {
			t.Errorf("[%s] is not in the spec", endpoint)
			continue
		}
		if !implKeys[ep.method+" "+normalizePath(ep.path)] {
			t.Errorf("[%s] has no route", endpoint)
		}
		req, resp := spec.operation(ep.method, ep.path)
		if ep.argType != nil {
			if req == nil {
				t.Errorf("[%s] documents no request body", endpoint)
			} else {
				checker.check(endpoint+" (request)", "", req, ep.argType, 0)
			}
		}
		if resp == nil {
			t.Errorf("[%s] documents no response body", endpoint)
		} else {
			checker.check(endpoint+" (response)", "", resp, ep.resultType, 0)
		}
	}
}

// The lag routes' classes (route_authz.go): the guest session needs no
// credential, since it is how a browser gets one; the sample, the tickets and
// the reports act for the caller's own client, the reports for the extender
// the caller activated.
func TestLagRouteClasses(t *testing.T) {
	for key, want := range map[string]routeAccess{
		"POST /lag/session":                 routeAccessPublic,
		"GET /lag/extenders":                routeAccessClient,
		"POST /lag/track":                   routeAccessClient,
		"POST /network/extender-lag-report": routeAccessClient,
	} {
		if got := routeAccessByRoute[key]; got != want {
			t.Errorf("%s access = %d, want %d", key, got, want)
		}
	}
}
