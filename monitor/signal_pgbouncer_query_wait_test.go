package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPgBouncerQueryWaitClassifiesFailureWithoutAssigningSaturation(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
	}{
		{"plain", "FATAL: query_wait_timeout"},
		{"pgconn", "*pgconn.PgError=FATAL: query_wait_timeout (SQLSTATE 08P01)"},
		{"trace JSON", `Unexpected error: {"error":"*pgconn.PgError=FATAL: query_wait_timeout (SQLSTATE 08P01)","stack":"goroutine 12 [running]","private":"synthetic-private-queue-marker","endpoint":"192.0.2.9:6432"}`},
		{"route JSON", `[h]unhandled error from route GET ^/$: {"error":"*pgconn.PgError=FATAL: query_wait_timeout (SQLSTATE 08P01)","stack":"goroutine 13 [running]","private":"synthetic-private-queue-marker"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, count := range []int{1, 4, 5} {
				tailer := newLogTailer("connect", nil)
				for range count {
					tailer.classify(test.line)
				}
				findings := tailer.drainWindow()
				observed := findingByClass(t, findings, "pgbouncer-query-wait")
				wantTier := tierWarn
				if count >= 5 {
					wantTier = tierPage
				}
				if observed.healthy || observed.tier != wantTier || !strings.Contains(observed.observed, fmt.Sprintf("rate=%d/min", count)) {
					t.Fatalf("queue failure lost severity or line count: count=%d healthy=%t tier=%s", count, observed.healthy, observed.tier)
				}
				for _, class := range []string{"panic", "pg-client-capacity"} {
					if !findingByClass(t, findings, class).healthy {
						t.Fatalf("queue failure was also attributed to %s", class)
					}
				}
				retained := observed.evidence + " " + observed.observed + " " + observed.frame
				if strings.Contains(retained, "synthetic-private-queue-marker") || strings.Contains(retained, "goroutine") || strings.Contains(retained, "192.0.2.9") {
					t.Fatal("queue diagnostic retained raw private payload or stack")
				}
				guidance := observed.mechanism + " " + observed.context + " " + observed.action
				for _, required := range []string{"does not establish", "sv_login", "SHOW POOLS", "same backend role", "TLS mode"} {
					if !strings.Contains(guidance, required) {
						t.Fatalf("queue failure omitted causal boundary %q", required)
					}
				}
				if strings.Contains(guidance, "server sessions all busy") {
					t.Fatal("queue failure still claims established-server saturation")
				}
			}
		})
	}
}

func TestPgBouncerQueryWaitDoesNotConsumeOtherFailureBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		line      string
		wantClass string
	}{
		{"configuration", `query_wait_timeout = 120`, ""},
		{"configuration JSON", `{"query_wait_timeout":120,"status":"healthy"}`, ""},
		{"longer token", `Unexpected error: FATAL: query_wait_timeout_extra`, "panic"},
		{"wrong severity", `Unexpected error: ERROR: query_wait_timeout`, "panic"},
		{"bare initial ping timeout", `Unexpected error: {"error":"*pgconn.errTimeout=timeout: context deadline exceeded","stack":"goroutine 12 [running]"}`, "panic"},
		{"server rejection", `Unexpected error: FATAL: server login has been failing, cached error: sorry, too many clients already (server_login_retry)`, "pg-client-capacity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tailer := newLogTailer("connect", nil)
			for range 5 {
				tailer.classify(test.line)
			}
			findings := tailer.drainWindow()
			if !findingByClass(t, findings, "pgbouncer-query-wait").healthy {
				t.Fatal("a different boundary gained queue-timeout attribution")
			}
			if test.wantClass != "" && findingByClass(t, findings, test.wantClass).healthy {
				t.Fatalf("queue classification hid %s", test.wantClass)
			}
		})
	}
}

func TestPgBouncerQueryWaitStandingReplayAndStaleArrival(t *testing.T) {
	now := time.Date(2026, 10, 4, 22, 0, 0, 0, time.UTC)
	lineAt := func(at time.Time) string {
		return "[fixture-edge][connect][g1][cid:fixture][I][" + at.Format(time.RFC3339Nano) + "][trace.go:51]Unexpected error: FATAL: query_wait_timeout"
	}
	tailer := newLogTailer("connect", nil)
	tailer.clock = func() time.Time { return now }
	current := lineAt(now)
	for range 5 {
		tailer.ingestStanding(current, true, true)
	}
	observed := findingByClass(t, tailer.drainWindow(), "pgbouncer-query-wait")
	if observed.healthy || observed.tier != tierWarn || !strings.Contains(observed.observed, "rate=1/min") {
		t.Fatal("one replayed queue error inflated the failure rate")
	}
	stale := lineAt(now.Add(-logReconcileLookback - time.Minute))
	for range 5 {
		tailer.ingestStanding(stale, true, true)
	}
	if tailer.staleArrivalCount != 1 {
		t.Fatal("stale source visibility was lost or inflated")
	}
	if !findingByClass(t, tailer.drainWindow(), "pgbouncer-query-wait").healthy {
		t.Fatal("stale queue errors became a current failure rate")
	}
}
