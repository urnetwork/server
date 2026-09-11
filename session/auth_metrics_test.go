package session

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

func TestRequestAuthMethodIsBounded(t *testing.T) {
	request := httptest.NewRequest("GET", "https://fixture.example/api", nil)
	if got := requestAuthMethod(request); got != "none" {
		t.Fatalf("missing credential method = %q, want none", got)
	}
	request.Header.Set("Authorization", "Synthetic fixture")
	if got := requestAuthMethod(request); got != "other" {
		t.Fatalf("non-bearer credential method = %q, want other", got)
	}
	request.Header.Set("Authorization", "Bearer synthetic.jwt.fixture")
	if got := requestAuthMethod(request); got != "jwt" {
		t.Fatalf("bearer method = %q, want jwt", got)
	}
	request.Header.Set("Authorization", "Bearer urn_synthetic_fixture")
	if got := requestAuthMethod(request); got != "api_key" {
		t.Fatalf("API key method = %q, want api_key", got)
	}
}

func TestRecordSessionAuthCountsOutcomeWithoutIdentityLabel(t *testing.T) {
	byJwt := &jwt.ByJwt{NetworkId: server.NewId()}
	succeededBefore := testutil.ToFloat64(sessionAuthAttemptsTotal.WithLabelValues("jwt", "succeeded"))
	rejectedBefore := testutil.ToFloat64(sessionAuthAttemptsTotal.WithLabelValues("jwt", "rejected"))
	recordSessionAuth("jwt", byJwt, nil)
	recordSessionAuth("jwt", nil, errors.New("synthetic rejection"))
	if got := testutil.ToFloat64(sessionAuthAttemptsTotal.WithLabelValues("jwt", "succeeded")) - succeededBefore; got != 1 {
		t.Fatalf("successful auth delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(sessionAuthAttemptsTotal.WithLabelValues("jwt", "rejected")) - rejectedBefore; got != 1 {
		t.Fatalf("rejected auth delta = %v, want 1", got)
	}
	if got := sessionAuthAttemptsTotal.WithLabelValues("jwt", "succeeded").Desc().String(); got == "" {
		t.Fatal("auth metric descriptor is empty")
	}
}

func TestSessionActiveAuthCollectorDeduplicatesAndExpiresDigests(t *testing.T) {
	collector := newSessionActiveAuthCollector()
	now := time.Unix(1_800_000_000, 0)
	collector.now = func() time.Time { return now }
	byJwt := &jwt.ByJwt{NetworkId: server.NewId()}
	collector.observe(byJwt)
	collector.observe(byJwt)
	if got := len(collector.lastSeen); got != 1 {
		t.Fatalf("deduplicated active principals = %d, want 1", got)
	}

	now = now.Add(25 * time.Hour)
	metricChannel := make(chan prometheus.Metric, 3)
	collector.Collect(metricChannel)
	close(metricChannel)
	for range metricChannel {
	}
	if got := len(collector.lastSeen); got != 0 {
		t.Fatalf("expired principal digests retained = %d, want 0", got)
	}
}
