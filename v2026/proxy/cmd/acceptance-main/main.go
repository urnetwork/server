// Command acceptance-main verifies the deployed SOCKS, HTTP, and WireGuard
// proxy paths and emits the root suite's strict result rows.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urnetwork/connect/v2026"
	proxyacceptance "github.com/urnetwork/server/v2026/proxy/acceptance"
)

func main() {
	credentials := flag.String("credentials", "", "mode-0600 file containing user and password lines")
	resultsPath := flag.String("result-file", "", "private TSV result path")
	apiURL := flag.String("api", "https://api.bringyour.com", "API base URL")
	targetURL := flag.String(
		"target",
		proxyacceptance.DefaultTargetURL,
		"HTTPS target loaded through every proxy",
	)
	repeat := flag.Int("repeat", 1, "number of full proxy repetitions")
	probeTimeout := flag.Duration("probe-timeout", 120*time.Second, "readiness and data-plane retry window per protocol")
	soakDuration := flag.Duration("soak-duration", 5*time.Minute, "sustained data-plane duration per protocol")
	soakInterval := flag.Duration("soak-interval", 5*time.Second, "delay between sustained HTTPS requests")
	overlapProtocols := flag.Bool("overlap-protocols", true, "repeat all three sustained campaigns concurrently on one proxy device")
	trackHostedDevice := flag.Bool("track-hosted-device", true, "record redacted hosted-device provider state around failures")
	flag.Parse()
	// The runner reports purpose-built progress and failure timelines. Silence
	// per-RPC SDK info logs so one-second diagnostic polling stays readable.
	connect.SetDefaultLogger(connect.NewNoopLogger())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	results := proxyacceptance.Run(ctx, proxyacceptance.Options{
		APIURL:            *apiURL,
		TargetURL:         *targetURL,
		CredentialsPath:   *credentials,
		Repeat:            *repeat,
		ProbeTimeout:      *probeTimeout,
		SoakDuration:      *soakDuration,
		SoakInterval:      *soakInterval,
		OverlapProtocols:  *overlapProtocols,
		TrackHostedDevice: *trackHostedDevice,
		Progress: func(message string) {
			fmt.Fprintf(os.Stderr, "[proxy acceptance] %s\n", message)
		},
	})
	if err := proxyacceptance.WriteResults(*resultsPath, results); err != nil {
		fmt.Fprintf(os.Stderr, "[proxy acceptance] write results: %v\n", err)
		os.Exit(1)
	}
	for _, result := range results {
		fmt.Fprintf(os.Stderr, "[proxy acceptance] %s: %s - %s\n", result.Case, result.Status, result.Detail)
	}
	if ctx.Err() != nil {
		os.Exit(130)
	}
	if proxyacceptance.Failed(results) {
		os.Exit(1)
	}
}
