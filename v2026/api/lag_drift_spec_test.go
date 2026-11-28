package api

import (
	"testing"

	"github.com/urnetwork/server/v2026/controller"
)

// The drift test's routes (lag.dev/DRIFTTEST.md P1, P8) against the published
// spec (connect/api/bringyour.yml), with the conformance checker of
// spec_conformance_test.go: every property the spec promises exists in the Go
// type with a compatible type.

func lagDriftSpecRegistry() []specEndpoint {
	return []specEndpoint{
		{"POST", "/lag/drift-test", rt(controller.LagDriftTestArgs{}), rt(controller.LagDriftTestResult{})},
		// the id is a query parameter
		{"GET", "/lag/drift-test", nil, rt(controller.LagDriftTestView{})},
		{"POST", "/lag/drift-test/start", rt(controller.LagDriftStartArgs{}), rt(controller.LagDriftStartResult{})},
		{"POST", "/lag/drift-test/finish", rt(controller.LagDriftFinishArgs{}), rt(controller.LagDriftFinishResult{})},
		{"POST", "/lag/drift-test/result", rt(controller.LagDriftResultArgs{}), rt(controller.LagDriftWidgetResult{})},
	}
}

func TestLagDriftSpecConformance(t *testing.T) {
	spec := loadSpec(t)
	checker := &conformanceChecker{t: t, spec: spec}
	implKeys := implRouteKeys()
	for _, ep := range lagDriftSpecRegistry() {
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

// The drift routes' classes (route_authz.go, P1): the customer's two take the
// network credential, an Embed API's, and the widget's three take only the
// token, a credential of its own kind. A lag.dev guest's token is refused on
// all five.
func TestLagDriftRouteClasses(t *testing.T) {
	for key, want := range map[string]routeAccess{
		"POST /lag/drift-test":        routeAccessNetwork,
		"GET /lag/drift-test":         routeAccessNetwork,
		"POST /lag/drift-test/start":  routeAccessPublic,
		"POST /lag/drift-test/finish": routeAccessPublic,
		"POST /lag/drift-test/result": routeAccessPublic,
	} {
		if got := routeAccessByRoute[key]; got != want {
			t.Errorf("%s access = %d, want %d", key, got, want)
		}
		if lagGuestRoutes[key] {
			t.Errorf("%s accepts a lag.dev guest's token", key)
		}
	}
}
