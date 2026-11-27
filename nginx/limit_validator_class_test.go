package nginx

import (
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A validator identity is charged at the validator class's burst, where the
// same client without a class token has the identity limit's. Before the
// class, every verified client had the identity limit.
func TestLimitValidatorClassChargesItsOwnBurst(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	// the identity limit: three requests, then one a second; a ceiling far
	// above the class
	limitInterface := newTestIdentityLimitInterface("eth1", 60, 10, 0, 60, 2, 1000)
	addr := netip.MustParseAddr("198.51.100.7")
	validator := newTestLimitIdentity(1)
	validator.Validator = true
	plain := newTestLimitIdentity(1)

	for i := range validatorClassBurst + 1 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, validator)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("validator request %d = %s, want the class's burst", i, outcome)
		}
		release()
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, validator); outcome != limitOutcomeIdentityRefused {
		t.Fatalf("validator past the class's burst = %s, want identity", outcome)
	}
	// the same client without the class has the identity limit's own state
	for i := range 3 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, plain)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("plain request %d = %s", i, outcome)
		}
		release()
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, plain); outcome != limitOutcomeIdentityRefused {
		t.Fatalf("plain past the identity burst = %s, want identity", outcome)
	}
}

// A validator identity is held to the class's requests in flight, not the
// interface's net_connections.
func TestLimitValidatorClassConcurrency(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	// one in flight per identity and address under the identity limit, a
	// ceiling of 1000 at once
	limitInterface := newTestIdentityLimitInterface("eth1", 0, 0, 1, 60, 0, 1000)
	addr := netip.MustParseAddr("198.51.100.7")
	validator := newTestLimitIdentity(1)
	validator.Validator = true
	releases := []func(){}
	for i := range validatorClassNetConnections {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, validator)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("validator request %d in flight = %s, want the class's cap", i, outcome)
		}
		releases = append(releases, release)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, validator); outcome != limitOutcomeIdentityConcurrencyRefused {
		t.Fatalf("validator past the class's cap = %s, want identity_concurrency", outcome)
	}
	for _, release := range releases {
		release()
	}
}

// Through the front: a validator client's requests are charged to the class,
// counted in validator_class_requests_total, and a refusal is counted with
// rate_class validator; an ordinary client keeps the identity limit.
func TestRouteHttpValidatorClassChargesAndCountsValidators(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	// the address: two requests; a client: two, then 60 a minute; the
	// ceiling far above the class
	setTestIdentityRateLimit(t, routeTable, "eth1", 3, 1, 0, 60, 1, 5000)
	identifier := &testIdentifyRequest{}
	settings := testFrontSettings()
	settings.Limit.IdentifyRequest = identifier.identify
	testFront := newTestFront(t, routeTable, settings)
	https := testFront.front.HttpsHandler("eth1")
	send := func(authorization string) int {
		return serveTestRequest(https, newTestAuthorizedRequest("198.51.100.111:1000", "GET", "api.service.example", "/v1", authorization)).Result().StatusCode
	}
	for i := range validatorClassBurst + 1 {
		if status := send("Bearer validator-1"); status != http.StatusOK {
			t.Fatalf("validator request %d = %d, want the class's burst", i, status)
		}
	}
	if status := send("Bearer validator-1"); status != http.StatusTooManyRequests {
		t.Fatalf("validator past the class's burst = %d, want 429", status)
	}
	for i := range 2 {
		if status := send("Bearer client-1"); status != http.StatusOK {
			t.Fatalf("client request %d = %d", i, status)
		}
	}
	if status := send("Bearer client-1"); status != http.StatusTooManyRequests {
		t.Fatalf("an ordinary client past its identity burst = %d, want 429", status)
	}
	admitted := testutil.ToFloat64(testFront.metrics.validatorClassRequests.WithLabelValues("admitted"))
	refused := testutil.ToFloat64(testFront.metrics.validatorClassRequests.WithLabelValues("refused"))
	if admitted != float64(validatorClassBurst+1) || refused != 1 {
		t.Errorf("validator class requests: %v admitted, %v refused, want %d and 1", admitted, refused, validatorClassBurst+1)
	}
	want := map[[3]string]float64{
		{"identity/" + limitRateClassValidator, limitIdentityZone, "api"}: 1,
		{"identity", limitIdentityZone, "api"}:                            1,
	}
	if series := testLimitRefusalSeries(t, testFront.metrics); !maps.Equal(series, want) {
		t.Errorf("refusals %v, want %v", series, want)
	}
	_ = fmt.Sprint
}
