// Cold-start allowance belongs to sampled requests, not a special warm-up URL.
package egresshealth

import (
	"context"
	"math/rand"
	"net/http"
	"testing"
	"time"
)

// A cold tunnel has one shared deadline; neither new sites nor retries renew it.
func TestSampleColdStartWindowIsSharedAndFinite(t *testing.T) {
	clock := newFakeClock()
	opts := clockedOptions(clock)
	opts.PerRequestTimeout = 10 * time.Second
	opts.ColdStartTimeout = time.Minute
	client := &http.Client{}
	run := newRun(staticPath{client: client}, opts, 2, time.Hour, rand.New(rand.NewSource(7)))
	if got := run.requestTimeout(client); got != time.Minute {
		t.Fatalf("initial allowance=%s", got)
	}
	_ = clock.Sleep(context.Background(), 20*time.Second)
	if got := run.requestTimeout(client); got != 40*time.Second {
		t.Fatalf("sibling renewed allowance=%s", got)
	}
	_ = clock.Sleep(context.Background(), time.Minute)
	if got := run.requestTimeout(client); got != 10*time.Second {
		t.Fatalf("expired allowance=%s", got)
	}
	replacement := &http.Client{}
	if got := run.requestTimeout(replacement); got != time.Minute {
		t.Fatalf("new generation allowance=%s", got)
	}
	run.established(replacement)
	if got := run.requestTimeout(replacement); got != 10*time.Second {
		t.Fatalf("established path still cold=%s", got)
	}
}

// Default scheduling reserves each permitted cold generation without an echo.
func TestSampleBudgetIncludesColdTunnelGenerations(t *testing.T) {
	opts := Options{PerRequestTimeout: 10 * time.Second, ColdStartTimeout: time.Minute, LoadAttempts: 1, Concurrency: 1}
	if got := opts.RunBudget(1); got != 160*time.Second {
		t.Fatalf("static cold budget=%s", got)
	}
	opts.Path = staticPath{client: &http.Client{}}
	opts.TunnelRecreateAttempts = 2
	if got := opts.RunBudget(1); got != 160*time.Second {
		t.Fatalf("recreated cold budget=%s", got)
	}
}
