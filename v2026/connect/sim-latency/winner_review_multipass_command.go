// The executable provider entrypoint accepts exactly one bounded contained call.
// It has no API mutation, operator token, model key, candidate execution or retry.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"

	"github.com/docopt/docopt-go"
)

// Outer containment qualification authenticates these leaf-only mounts, running
// cgroup, one-shot host claim and egress fence before any stdin is released.
func runWinnerMultipassBroker(opts docopt.Opts) {
	limits := defaultWinnerMultipassProviderLimits()
	if os.Getuid() != 65532 || optString(opts, "--review-provider-settings", "") != "/run/review/provider.json" || optString(opts, "--review-state-directory", "") != "/run/review/state" {
		fatalf("multi-pass provider requires the qualified non-root contained profile")
	}
	content, err := io.ReadAll(io.LimitReader(os.Stdin, int64(limits.MaximumEnvelopeBytes)+1))
	if err != nil || len(content) > limits.MaximumEnvelopeBytes {
		fatalf("multi-pass envelope exceeds its bound")
	}
	var envelope winnerMultipassBrokerEnvelope
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &envelope, "multi-pass contained envelope") != nil {
		fatalf("multi-pass envelope is malformed")
	}
	provider, err := loadWinnerSnowProviderSettings(optString(opts, "--review-provider-settings", ""), optString(opts, "--review-provider-settings-sha256", ""))
	if err != nil {
		fatalf("multi-pass fixed Snow provider settings are unavailable")
	}
	ctx, cancel := signalContext()
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	result, err := executeWinnerMultipassBroker(ctx, &http.Client{Transport: transport}, envelope, provider, optString(opts, "--review-state-directory", ""), limits)
	if result != nil {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
			fatalf("multi-pass result cannot be retained")
		}
	}
	if err != nil {
		fatalf("multi-pass call failed; preserve the original result and do not retry: %s", err)
	}
}
