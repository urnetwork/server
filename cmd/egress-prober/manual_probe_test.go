package main

// Manual, operator-driven probe of one named provider. Not part of the
// automated suite: it is skipped unless MANUAL_PROBE_PROVIDER is set, because
// it needs a live provider, a real network client jwt, and real egress.
//
// Build a runnable binary for a VPS with:
//
//	go test -c -o manualprobe ./cmd/egress-prober
//
// then run it there with MANUAL_PROBE_PROVIDER / UR_PROBER_BY_JWT set:
//
//	./manualprobe -test.run TestManualProbeOneProvider -test.v
//
// It runs one egress-health run through a tunnel to the provider -- the /ip
// warm-up first, then the pass's pool (or the built-in table), every load with
// its spaced retries -- and prints the exit address the echo saw alongside
// every destination's outcome. With the default five-minute spacing a
// provider with a failing site takes ten to fifteen minutes; set
// MANUAL_PROBE_RETRY_MEAN (a duration) to shorten it for a quick look.

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/providertunnel"
)

// Probes the provider MANUAL_PROBE_PROVIDER names through a real tunnel and
// logs every outcome; see the file header for how to run it.
func TestManualProbeOneProvider(t *testing.T) {
	providerStr := os.Getenv("MANUAL_PROBE_PROVIDER")
	if providerStr == "" {
		t.Skip("set MANUAL_PROBE_PROVIDER to the provider client id to probe")
	}
	byJwt := os.Getenv("UR_PROBER_BY_JWT")
	apiUrl := os.Getenv("MANUAL_PROBE_API_URL")
	platformUrl := os.Getenv("MANUAL_PROBE_PLATFORM_URL")
	// The operator secret is required: this probe fetches the same pins and
	// pool the CLI does, from the same endpoints, with the same validation, so
	// it keeps exercising the shipped path rather than a permissive one of its
	// own.
	operatorSecret := os.Getenv("UR_OPERATOR_SECRET")
	if byJwt == "" || apiUrl == "" || platformUrl == "" || operatorSecret == "" {
		t.Fatal("UR_PROBER_BY_JWT, UR_OPERATOR_SECRET, MANUAL_PROBE_API_URL and MANUAL_PROBE_PLATFORM_URL are all required")
	}
	retryMean := egresshealth.DefaultLoadRetryMeanInterval
	if raw := os.Getenv("MANUAL_PROBE_RETRY_MEAN"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("MANUAL_PROBE_RETRY_MEAN %q: %s", raw, err)
		}
		retryMean = parsed
	}

	providerId, err := connect.ParseId(providerStr)
	if err != nil {
		t.Fatalf("parse provider client id %q: %s", providerStr, err)
	}
	selfId, err := parseByJwtClientId(byJwt)
	if err != nil {
		t.Fatalf("parse by-jwt client id: %s", err)
	}

	// Long enough for the worst of the retry schedule at the default spacing.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	operator := &ingest.Client{
		ServerUrl:      apiUrl,
		OperatorSecret: operatorSecret,
		Http:           &http.Client{Timeout: 30 * time.Second},
	}
	pins, err := fetchPins(ctx, operator)
	if err != nil {
		t.Fatalf("fetch the certificate pins from %s: %s", apiUrl, err)
	}
	pool, err := fleetprobe.LoadPool(ctx, operator.Http, fleetprobe.PoolUrl(apiUrl), operatorSecret)
	if err != nil {
		t.Logf("pool               built-in table (%s)", err)
	} else {
		t.Logf("pool               version %d, %d destination(s)", pool.Version, len(pool.Destinations))
	}
	echoUrl := fleetprobe.IpEchoUrl(apiUrl)

	tun, err := providertunnel.Open(ctx, providertunnel.Config{
		ApiUrl:            apiUrl,
		PlatformUrl:       platformUrl,
		ByJwt:             byJwt,
		ClientId:          selfId,
		Pins:              pins,
		DeviceDescription: "manual egress probe",
		DeviceSpec:        "egress-prober",
		Version:           "0.0.0",
	}, providerId)
	if err != nil {
		t.Fatalf("open tunnel to provider %s: %s", providerId, err)
	}
	defer tun.Close()

	// The one client the CLI builds: every pool host and the echo host
	// allowed, pinned where the server serves a pin, WebPKI otherwise.
	hosts := append(egresshealth.HostsOf(pool.Destinations), egresshealth.HostsOf([]egresshealth.Destination{{Url: echoUrl}})...)
	client := tun.HttpClientForHosts(90*time.Second, hosts)

	health, err := egresshealth.Check(ctx, client, egresshealth.Options{
		PerRequestTimeout:     45 * time.Second,
		IpEchoUrl:             echoUrl,
		IpEchoTimeout:         90 * time.Second,
		LoadRetryMeanInterval: retryMean,
		Destinations:          pool.Destinations,
		Profile:               &pool.Profile,
	})
	if err != nil {
		t.Fatalf("egress health through provider %s did not run: %s", providerId, err)
	}
	t.Logf("provider           %s", providerId)
	t.Logf("exit               %s (echo error: %q)", health.ExitIp, health.IpEchoErr)
	t.Logf("egress-health      %s", health.Summary())
	for _, c := range health.Checks {
		status := "FAIL"
		switch {
		case c.Ok:
			status = "ok  "
		case c.NotMeasured:
			status = "n/m "
		}
		t.Logf("  %-4s %-26s %-12s attempts=%d status=%-4d bytes=%-5d %-8s %s",
			status, c.Name, c.Class, c.Attempts, c.StatusCode, c.ByteCount, c.Latency.Round(time.Millisecond), c.Err)
	}
}
