package session

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// Every unanswered recheck is counted and its time since the last successful
// authoritative check is observed, so operators can see how long admitted
// sessions run unverified.
func TestAuthorizationLeaseCountsUnansweredRechecks(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		check := &scriptedLeaseCheck{
			// unanswered at 60s, 66.25s and 78.75s, then verified at 103.75s
			results:   []error{nil, ErrAuthUnavailable, ErrAuthUnavailable, ErrSessionStoreUnavailable},
			otherwise: nil,
		}
		rechecksBefore := testutil.ToFloat64(sessionLeaseUnavailableRechecks)
		countBefore, sumBefore := unverifiedObservations(t)
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(3 * time.Minute)
		synctest.Wait()

		select {
		case retired := <-closes:
			t.Fatalf("unanswered rechecks retired the transport with %d", retired.code)
		default:
		}
		if got := testutil.ToFloat64(sessionLeaseUnavailableRechecks) - rechecksBefore; got != 3 {
			t.Fatalf("counted %v unanswered rechecks, want 3", got)
		}
		count, sum := unverifiedObservations(t)
		if count-countBefore != 3 || sum-sumBefore != 205 {
			t.Fatalf("observed %d unverified durations summing to %vs, want 3 summing to 205s", count-countBefore, sum-sumBefore)
		}
	})
}

func unverifiedObservations(t *testing.T) (uint64, float64) {
	var metric dto.Metric
	if err := sessionLeaseUnverifiedSeconds.Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}
