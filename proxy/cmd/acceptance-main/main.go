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

	proxyacceptance "github.com/urnetwork/server/proxy/acceptance"
)

func main() {
	credentials := flag.String("credentials", "", "mode-0600 file containing user and password lines")
	resultsPath := flag.String("result-file", "", "private TSV result path")
	apiURL := flag.String("api", "https://api.bringyour.com", "API base URL")
	targetURL := flag.String("target", "https://api.bringyour.com/hello", "HTTPS target loaded through every proxy")
	repeat := flag.Int("repeat", 1, "number of full proxy repetitions")
	probeTimeout := flag.Duration("probe-timeout", 120*time.Second, "readiness and data-plane retry window per protocol")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	results := proxyacceptance.Run(ctx, proxyacceptance.Options{
		APIURL:          *apiURL,
		TargetURL:       *targetURL,
		CredentialsPath: *credentials,
		Repeat:          *repeat,
		ProbeTimeout:    *probeTimeout,
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
