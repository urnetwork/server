package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestLogErrorsSignalSyntheticLogRate(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-api", nil
		}
		return strings.Repeat("channel is full for peers (message is dropped)\n", 11), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "pubsub-drops")
}

func TestLogErrorsSignalSyntheticStructuredProblemClasses(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		class string
	}{
		{"dial timeout", "dial tcp 192.0.2.1:6380: i/o timeout", "dial-io-timeout"},
		{"connection refused", "connect: connection refused", "connection-refused"},
		{"port exhaustion", "connect: cannot assign requested address", "port-exhaustion"},
		{"pool timeout", "redis: connection pool timeout", "pool-timeout"},
		{"cluster down", "CLUSTERDOWN Hash slot not served", "clusterdown"},
		{"oom writes", "OOM command not allowed when used memory > maxmemory", "oom-writes"},
		{"Loki tail backend EOF", `level=error caller=tail.go:230 component=tail-querier org_id=fake msg="Error receiving response from grpc tail client" addr=192.0.2.10:6490 err=EOF`, "loki-tail-backend-eof"},
		{"Loki tail dropped streams", `level=info caller=tailer.go:271 msg="tailer dropped streams is reset" length=100`, "loki-tail-dropped-streams"},
		{"Warpctl direct Loki tail loss", `[warpctl][loki-tail-dropped-entries] service=proxy count=2`, "loki-tail-dropped-entries"},
		{"connection reset", "read: connection reset by peer", "conn-reset"},
		{"redis loading", "LOADING Redis is loading the dataset in memory", "redis-loading"},
		{"required vault", "panic: Resource not found in vault (verify.yml)", "required-vault-resource"},
		{"grafana plugin", "error=\"the result-set has errors: [plugin.notRegistered] plugin not registered\"", "grafana-plugin-unregistered"},
		{"source attribution", "[session]X-UR-Forwarded-For from untrusted peer", "source-attribution"},
		{"HTTP write after hijack", "http: response.WriteHeader on hijacked connection from github.com/urnetwork/server/router.(*Router).ServeHTTP.func1.1 (router.go:104)", "http-hijack-write"},
		{"negative escrow", "[netescrow]negative counter after release", "netescrow-negative"},
		{"escrow mirror write", "[netescrow]mirror write failed after reservation: i/o timeout", "netescrow-mirror-write"},
		{"panic", "panic: synthetic crash frame", "panic"},
		{"payout wallet", "asset amount owned by the wallet is insufficient", "payout-wallet-insufficient"},
		{"invalid payout destination", `Bad status: 400 Bad Request {"code":155219,"message":"Invalid destination address."}`, "payout-invalid-destination"},
		{"payment processor rate limit", `Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`, "payment-processor-rate-limit"},
		{"net escrow ttl", `[redis][ttl]"expireat" key="{escrow_019c640e-f467-4fa7-177f-d7ca43c33b6f}net" ttl 3139393191s-from-now exceeds 9600h0m0s`, "redis-netescrow-ttl"},
		{"redis ttl", "[redis][ttl] suspicious ttl on key", "redis-ttl-suspect"},
		{"taskworker drain", "[taskworker]drain gave up with 2 tasks", "taskworker-drain-gave-up"},
		{"legacy database maintenance", "[db]maintenance reindex[16/22] contract_close", "db-maintenance-legacy-reindex"},
		{"tls identity", "CONTRACT vs FETCHED peer client public key MISMATCH", "tls-key-mitm"},
		{"tls rotation", "peer client public key mismatch with prior commitment", "tls-key-rotate-refused"},
		{"tls publication", "Invalid PEM in certificate chain", "tls-cert-publish-invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			threshold := 0
			for _, class := range logClasses {
				if class.name == tc.class {
					threshold = class.rateThreshold
					break
				}
			}
			if threshold == 0 {
				t.Fatalf("no configured log class %s", tc.class)
			}
			source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
				if len(args) > 1 && args[0] == "ls" {
					return "repo names synthetic-api", nil
				}
				return strings.Repeat(tc.line+"\n", threshold), nil
			}}
			alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, tc.class)
		})
	}
}

func TestLogErrorsSignalExplainsLegacyDatabaseMaintenanceReindex(t *testing.T) {
	for _, tableName := range []string{
		"client_reliability",
		"client_reliability_p20260901",
		"contract_close",
		"network_client_location_reliability",
		"network_client_connection",
		"transfer_contract",
		"transfer_escrow",
		"transfer_escrow_sweep",
	} {
		line := "[db]maintenance reindex[1/22] " + tableName
		if !dbMaintenanceLegacyReindexRe.MatchString(line) {
			t.Fatalf("excluded table %s is not recognized in the legacy start format", tableName)
		}
		if got := dbMaintenanceLegacyReindexLogGroup(line); got != "table="+tableName {
			t.Fatalf("excluded table %s has frame %q", tableName, got)
		}
	}

	queryLines := strings.Join([]string{
		// The fixed table/step state machine and an ordinary old-format table
		// are not evidence that an excluded full-table rebuild was selected.
		`[edge-3][taskworker][g2][cid:fixed][I][2026-09-01T11:50:38.900708-05:00][db_maintenance.go:300][db]maintenance table[16/22] cleanup-before transfer_escrow`,
		`[edge-3][taskworker][g2][cid:ordinary][I][2026-09-01T11:50:38.900708-05:00][db_maintenance.go:225][db]maintenance reindex[1/22] st_epoch`,
		// A completion line is not a second launch event.
		`[edge-3][taskworker][g2][cid:legacy][I][2026-09-01T11:55:38.900708-05:00][db_maintenance.go:239][db]maintenance reindex[16/22] contract_close reindex took 300.00s`,
	}, "\n")
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return queryLines, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "db-maintenance-legacy-reindex" {
			t.Fatalf("fixed, ordinary, or completion log was classified as a legacy excluded-table selection:\n%s", alert.Markdown())
		}
	}

	queryLines = `[edge-3][taskworker][g2][cid:602dc13cd6f0][I][2026-09-01T11:50:38.900708-05:00][db_maintenance.go:225][db]maintenance reindex[16/22] contract_close`
	alerts, err = NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "db-maintenance-legacy-reindex")
	if alert.Frame != "table=contract_close" {
		t.Fatalf("legacy maintenance frame = %q, want table=contract_close", alert.Frame)
	}
	for _, detail := range []string{
		"frame=table=contract_close",
		"legacy full-table concurrent-reindex path",
		"7676014f",
		"abfd976b",
		"pg_stat_progress_create_index",
		"does not by itself prove that PostgreSQL began the statement",
		"Do not let a rollout or manual cancellation implicitly interrupt a protected rebuild",
		"cancellation is a database mutation and requires authorization",
		"supported cleanup-only maintenance command",
		"never wildcard-drop _ccnew/_ccold indexes",
		"one complete maintenance epoch emits no legacy start line",
	} {
		if !strings.Contains(alert.Markdown(), detail) {
			t.Fatalf("legacy maintenance alert missing %q:\n%s", detail, alert.Markdown())
		}
	}
}

func TestLogErrorsSignalExplainsHTTPHijackWrite(t *testing.T) {
	line := `[edge-3][connect][g4][cid:test][2026-08-31T22:23:26.584101Z]2026/08/31 22:23:26 http: response.WriteHeader on hijacked connection from github.com/urnetwork/server/router.(*Router).ServeHTTP.func1.1 (router.go:104)`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-connect", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "http-hijack-write").Markdown()
	for _, detail := range []string{
		"transferred ownership of the H1 connection through Hijack",
		"Connect's GET / route hands its socket to Gorilla",
		"fell through to http.Error",
		"131 canonical WriteHeader warnings",
		"zero paired [h]unhandled route errors",
		"does not itself prove a failed handshake or active transport",
		"returns immediately for server.IsDoneError",
		"Do not suppress net/http's error logger globally",
		"zero http-hijack-write lines for 10 minutes",
		"Done panic performs no write after Hijack",
		"router.(*Router).ServeHTTP.func1.1 (router.go:104)",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("HTTP hijack-write alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestLogErrorsSignalExplainsLokiTailDroppedStreams(t *testing.T) {
	line := `[edge-0][grafana][g1][cid:test][2026-08-31T21:09:30.100000Z]level=info caller=tailer.go:271 msg="tailer dropped streams is reset" length=100`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "loki-tail-dropped-streams").Markdown()
	for _, detail := range []string{
		"observation service grafana emitted 1/min",
		"affected live-tail selector is unknown",
		"observation_service=grafana affected_selector=unknown",
		"ingester-side Loki live tail",
		"100-stream processing queue",
		"five-stream send queue",
		"omitted records before they reached the querier",
		"more traffic arrives over 15 seconds after blockedAt",
		"five-second ticker",
		"35,909 Grafana records",
		"11,583 resets",
		"5,511 Mimir query-frontend statistics",
		"5,511 evaluator statistics",
		"4,596 Loki `get or create table` records",
		"182 exact backend EOFs",
		"11,022 avoidable per-query records",
		"not a proven sole cause",
		"affirmative internal live-tail loss",
		"pushTailResponseFromIngester",
		"discards resp.DroppedStreams",
		"Grafana is the observation service",
		"all six active Grafana nodes were healthy",
		"removes missing LAN identity as the current prerequisite",
		"capped service-wide query is not a total",
		"same absolute window for each configured block",
		"do not redeploy already-current blocks",
		"Warp commit 1e95aef",
		"server Proxy commit e055c98c",
		"Warp commits 42168fe and bca37cf",
		"disables only Mimir's per-query statistics stream",
		"retaining query execution, metrics, errors, and alert cadence",
		"preserves the backend address on any residual EOF",
		"Do not raise Loki's fixed queues",
		"one aggregate summary per reconciling instance",
		"query-frontend/evaluator statistics fall to zero",
		"loki-tail-dropped-streams plus loki-tail-backend-eof remain zero for 10 minutes",
		"residual EOF is framed by backend address",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("Loki dropped-stream alert missing %q:\n%s", detail, markdown)
		}
	}
	for _, staleAction := range []string{
		"Deploy a Grafana image containing Warp commit 1e95aef",
		"deploy server Proxy commit e055c98c or later",
		"missing Fireside LAN identity",
		"node recovery remains a prerequisite",
	} {
		if strings.Contains(markdown, staleAction) {
			t.Fatalf("Loki dropped-stream alert retained unconditional stale action %q:\n%s", staleAction, markdown)
		}
	}
}

func TestLogErrorsSignalExplainsDirectLokiTailDroppedEntries(t *testing.T) {
	line := `[warpctl][loki-tail-dropped-entries] service=proxy count=2`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-proxy", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "loki-tail-dropped-entries").Markdown()
	for _, detail := range []string{
		"querier-to-WebSocket response channel overflowed",
		"buffers ten tail responses",
		"decoded dropped_entries but silently discarded it",
		"Warp commit 26089b2",
		"exact affected-service attribution",
		"privacy-safe",
		"independent of the earlier ingester",
		"two consecutive overlap reconciliations complete",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("direct Loki dropped-entry alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestLogErrorsSignalExplainsLokiTailBackendEOF(t *testing.T) {
	legacyEOFLine := `[edge-0][grafana][g1][cid:test][2026-08-31T19:20:51.247763Z]level=error ts=2026-08-31T19:20:51.244318653Z caller=tail.go:230 component=tail-querier org_id=fake msg="Error receiving response from grpc tail client" err=EOF`
	attributedEOFLine := `[edge-0][grafana][g1][cid:test][2026-09-01T05:00:17.247763Z]level=error ts=2026-09-01T05:00:17.244318653Z caller=tail.go:232 component=tail-querier org_id=fake msg="Error receiving response from grpc tail client" addr=192.0.2.10:6490 err=EOF`
	canceledLine := `[edge-1][grafana][g1][cid:test][2026-08-31T19:17:37.333774Z]level=error ts=2026-08-31T19:17:37.270771087Z caller=tail.go:230 component=tail-querier org_id=fake msg="Error receiving response from grpc tail client" err="rpc error: code = Canceled desc = context canceled"`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		// Four EOFs are below the threshold. The expected watcher-retirement
		// variant must not be counted as the fifth internal backend loss.
		return strings.Repeat(attributedEOFLine+"\n", 4) + strings.Repeat(canceledLine+"\n", 20), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "loki-tail-backend-eof" {
			t.Fatalf("four EOFs plus client cancellations crossed the EOF threshold:\n%s", alert.Markdown())
		}
	}

	source.localFn = func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return strings.Repeat(attributedEOFLine+"\n", 5), nil
	}
	alerts, err = NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "loki-tail-backend-eof").Markdown()
	for _, detail := range []string{
		"internal gRPC tail backend",
		"frame=backend=192.0.2.10:6490",
		"59-61-second recurrence",
		"60-second application read deadline",
		"100-stream processing queue",
		"five-stream send queue",
		"more than 15 seconds later closes",
		"Recv observes EOF",
		"five-second connection ticker reconnects",
		"backend process exit",
		"ring loss",
		"long-lived HTTP/2 and gRPC connections",
		"Canceled/context-canceled",
		"182 exact EOFs",
		"11,583 dropped-stream resets",
		"48 quoted cancellations",
		"2026.8.31+1034210530",
		"emitting host moved from edge-4 to edge-0",
		"log host is not the failed backend identity",
		"Warp commit bca37cf adds that address",
		"restored all six active ring nodes",
		"missing LAN identity is no longer a current prerequisite",
		"Bounded log reconciliation remains required",
		"Warp commit 1e95aef",
		"only to older Grafana blocks",
		"Warp commits 42168fe and bca37cf",
		"Do not claim the instrumentation itself fixes loss",
		"named backend's tailer, process, ring, and network state",
		"Do not raise Loki's fixed queues",
		"every configured active ring member owns its LAN identity",
		"loki-tail-dropped-streams plus loki-tail-backend-eof remain zero for 10 minutes",
		"residual EOF must carry a backend frame",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("Loki tail backend EOF alert missing %q:\n%s", detail, markdown)
		}
	}
	for _, staleAction := range []string{
		"Publish and deploy a Grafana image containing Warp commit 1e95aef",
		"restore any missing ring member or LAN identity",
		"Fireside was absent from the active Loki ring",
	} {
		if strings.Contains(markdown, staleAction) {
			t.Fatalf("post-fix EOF alert retained stale guidance %q:\n%s", staleAction, markdown)
		}
	}

	// Rolling deployment remains observable: a pre-instrumentation line has no
	// backend frame, but must still classify instead of falling into novel.
	source.localFn = func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return strings.Repeat(legacyEOFLine+"\n", 5), nil
	}
	alerts, err = NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	legacyMarkdown := requireAlertClass(t, alerts, "loki-tail-backend-eof").Markdown()
	if !strings.Contains(legacyMarkdown, "frame=") || strings.Contains(legacyMarkdown, "frame=backend=") {
		t.Fatalf("legacy Loki EOF should remain un-attributed during rollout:\n%s", legacyMarkdown)
	}
}

func TestLogErrorsSignalExplainsInvalidPayoutDestination(t *testing.T) {
	paymentID := "019f77ae-de17-db98-b22d-2642f6f67594"
	taskID := "01a0088a-3260-06db-9376-227cbd7c4691"
	response := `Bad status: 400 Bad Request {"code":155219,"message":"Invalid destination address."}`
	providerLine := `[edge-3][taskworker][g2][cid:test][I][2026-08-31T16:51:58.100000Z][circle_client_controller.go:142][circlec]error sending payment ` + paymentID + `: ` + response
	evaluatorLine := `[edge-3][taskworker][g2][cid:test][I][2026-08-31T16:51:58.200000Z][task.go:1930][` + taskID + `]eval error = AdvancePayment({"payment_id":"` + paymentID + `"}) = ` + response
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		// Exact replay of the evaluator line remains diagnostically visible but
		// must not manufacture a second logical processor event.
		return providerLine + "\n" + evaluatorLine + "\n" + evaluatorLine + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "payout-invalid-destination").Markdown()
	for _, detail := range []string{
		"invalid for its declared chain",
		"safely releases only that pre-chain submit attempt",
		"selects the same invalid payout_wallet configuration",
		"Historical bounded controls on a pre-dispersion taskworker",
		"same six payments recurring at the same minute",
		"not a statement about the currently deployed artifact",
		"does not infer runtime provenance from a historical version",
		"Retry dispersion cannot repair wallet data",
		"separate payout-retry-microburst finding",
		"supported account API",
		"account-owner/operations action only",
		"do not redeploy taskworker solely from this invalid-destination alert",
		"Do not edit account_payment or pending_task rows",
		"invalid_destination_events=1",
		"diagnostic_lines=3",
		"canonical_source=exact-replay-deduplicated-task-evaluator",
		"logical event count: 1 exact-replay-deduplicated task evaluator line(s) from 3 diagnostic line(s)",
		"within 90 minutes plus log-ingestion delay",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("invalid-destination alert missing %q:\n%s", detail, markdown)
		}
	}
	for _, staleDeploymentClaim := range []string{
		"2026.8.31-outerwerld+1033655820",
		"source 1d8f01e5",
		"deploy taskworker commit 70b0d269",
	} {
		if strings.Contains(markdown, staleDeploymentClaim) {
			t.Fatalf("invalid-destination alert retained stale deployment claim %q:\n%s", staleDeploymentClaim, markdown)
		}
	}
	for _, id := range []string{paymentID, taskID} {
		if strings.Contains(markdown, id) {
			t.Fatalf("invalid-destination alert leaked id %s:\n%s", id, markdown)
		}
	}
}

func TestLogErrorsSignalExplainsPayoutWalletInsufficiency(t *testing.T) {
	line := `[edge-3][taskworker][g2][cid:test][I][2026-08-31T09:00:48-05:00][circle_client_controller.go:142][circlec]error sending payment: wallet 019f77ae-de17-db98-b22d-2642f6f67594: asset amount owned by the wallet is insufficient`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return strings.Repeat(line+"\n", 5), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "payout-wallet-insufficient").Markdown()
	for _, detail := range []string{
		"source wallet lacks enough token balance",
		"one-hour nominal cap",
		"disperses saturated retries across 30–90 minutes",
		"older code used only two seconds of jitter",
		"N parked rows still produce roughly N retry lines per hour on average",
		"operational liquidity boundary",
		"software release cannot fund",
		"Do not delete or manually replay pending_task rows",
		"First use §8.12 to verify every taskworker block's immutable source",
		"artifact containing server commit b8718420",
		"§2.14 to prove complete admission metrics",
		"fewer than four canonical attempts/second",
		"allow the same window plus ingestion delay",
		"duplicate Circle transfers",
		"wallet <id>",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("payout-wallet alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "019f77ae-de17-db98-b22d-2642f6f67594") {
		t.Fatalf("payout-wallet alert leaked the wallet id:\n%s", markdown)
	}
	if strings.Contains(markdown, "The observed value is outside the SIGNALS.md healthy band") ||
		strings.Contains(markdown, "Follow SIGNALS.md §4") {
		t.Fatalf("payout-wallet alert retained generic guidance:\n%s", markdown)
	}
}

func TestLogErrorsSignalExplainsPaymentProcessorRateLimit(t *testing.T) {
	line := `[edge-3][taskworker][g2][cid:test][I][2026-08-31T15:46:23Z][task.go:1930][019f77ae-de17-db98-b22d-2642f6f67594]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`
	lines := []string{}
	for attempt := 300; attempt < 305; attempt++ {
		_, evaluatorLine := payoutAttemptLogLines("2026-08-31T15:46:23", attempt)
		lines = append(lines, evaluatorLine)
	}
	lines = append(lines, line)
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return strings.Join(lines, "\n") + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "payment-processor-rate-limit").Markdown()
	for _, detail := range []string{
		"short-window request limit",
		"diagnostic line rate is not a unique-submit rate",
		"post-jitter recurrence at 07:12:48Z",
		"four of those five rejections were on exact executables already proven to contain proportional jitter",
		"Independent random retry times",
		"Circle documents a default five POST requests/second",
		"ambiguous submit outcome",
		"idempotency key must be retained",
		"joins exact-replay-deduplicated evaluator records by normalized source second",
		"latest error",
		"not a general Circle outage",
		"correlated_source_seconds=1",
		"correlated_cohort_seconds=1",
		"coincident_wallet_attempts=5",
		"peak_coincident_wallet_attempts_per_second=5",
		"source-second correlation: 1/1 payment-processor-rate-limit source second(s)",
		"Do not manually retry",
		"commit b8718420",
		"fleet-wide Redis-time transfer gate",
		"conservative three-per-second ceiling",
		"all §2.14 admission metrics",
		"full 90-minute retry window",
		"account's authoritative quota",
		"processor-rate-limit events stay zero",
		"[<id>]eval error",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("payment-processor-rate-limit alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "019f77ae-de17-db98-b22d-2642f6f67594") {
		t.Fatalf("payment-processor-rate-limit alert leaked the payment id:\n%s", markdown)
	}
}

func TestLogErrorsSignalExplainsCircleAdmissionFailClosed(t *testing.T) {
	line := `[edge-1][taskworker][g2][cid:test][I][2026-09-01T08:10:00Z][circle_transfer_limiter.go:225][circlec][transfer-admission] failed closed after 2 deferral(s), wait=1.5s: redis: i/o timeout`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "circle-transfer-admission-failed").Markdown()
	for _, want := range []string{
		"deliberately returned before the financial POST",
		"Fail-closed behavior prevents an uncounted, ambiguous transfer submit",
		"deploy drain can cancel a waiter once",
		"durable Circle idempotency key is retained",
		"§2.14 admission-error metrics",
		"do not manually replay the payment",
		"three-per-second ceiling",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Circle admission failure alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestLogErrorsSignalExplainsLongLivedNetEscrowMirror(t *testing.T) {
	line := `[redis][ttl]"expireat" key="{escrow_019c640e-f467-4fa7-177f-d7ca43c33b6f}net" ttl 3139393191s-from-now exceeds 9600h0m0s`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-api", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "redis-netescrow-ttl")
	markdown := alert.Markdown()
	for _, detail := range []string{
		"derived reservation mirror",
		"rolling 90-day horizon",
		"durable long-lived balance remains unchanged",
		"independent of the legacy stream",
		"{escrow_<id>}net",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("net-escrow TTL alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "019c640e-f467-4fa7-177f-d7ca43c33b6f") {
		t.Fatalf("net-escrow TTL alert leaked the balance id:\n%s", markdown)
	}
}

func TestLogErrorsSignalExplainsGrafanaDatasourcePluginFailure(t *testing.T) {
	line := `logger=ngalert.scheduler rule_uid=redis-node-down error="the result-set has errors that can be retried: [plugin.notRegistered] plugin not registered"`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "grafana-plugin-unregistered").Markdown()
	for _, detail := range []string{
		"Prometheus and Loki datasource implementations as standalone native plugins",
		"warp-mimir and warp-loki rows",
		"Logs Drilldown app is a frontend",
		"direct Mimir or Loki query",
		"exact Grafana generation and image",
		"pinned Prometheus and Loki plugins and catalog SHA-256",
		"datasource-plugin packaging, Logs Drilldown provisioning",
		"Do not recreate either datasource",
		"count_over_time query through warp-loki via Grafana /api/ds/query",
		"var-ds=warp-loki",
		"every active exact-edge generation",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("Grafana plugin alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "The observed value is outside the SIGNALS.md healthy band") ||
		strings.Contains(markdown, "Follow SIGNALS.md 11.15") {
		t.Fatalf("Grafana plugin alert retained generic guidance:\n%s", markdown)
	}
}

func TestLogErrorsSignalExplainsNegativeNetEscrowAftermath(t *testing.T) {
	line := "[netescrow]negative counter after release: site=release balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-21434368"
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-api", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "netescrow-negative")
	markdown := alert.Markdown()
	for _, detail := range []string{
		"legacy full-fleet reconciler",
		"old absolute SET or DEL snapshot",
		"page-local additive path",
		"PostgreSQL statement fixes its page snapshot",
		"visible before the later Redis GET",
		"not evidence that the site independently created",
		"log-ingestion delay",
		"425 contracts on 20 balances",
		"6,937,052,501 bytes",
		"586,862,592 bytes",
		"52 clamped negative results",
		"No Redis restart, failover, eviction",
		"wave-start shortfall",
		"replayed release",
		"expired-balance reconciliation blind spot",
		"immutable artifact provenance",
		"reservation statement shape and timing",
		"checked mirror-pipeline results",
		"single-attempt client",
		"migration 601",
		"unsettled-partial query",
		"non-current-open reconciliation fix",
		"durable per-balance fencing/versioning",
		"committed settlement before its delayed Redis release",
		"atomic release Lua",
		"clamped_to=0",
		"unsettled-partial pages below 1 second",
		"non-current balance with outcome-NULL escrow",
		"below 120 seconds",
		"below 256GiB",
		"next balance-expiry and close interval",
		"taskworker, API, and Connect",
		"balance=<id> contract=<id>",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("negative net-escrow alert missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(markdown, "01a04ff7-83b0-1970-2353-4b9ccf6e461d") ||
		strings.Contains(markdown, "01a05086-db24-dde0-dd4b-cbd20ace42ca") {
		t.Fatalf("negative net-escrow alert leaked an entity id:\n%s", markdown)
	}
	if strings.Contains(markdown, "The observed value is outside the SIGNALS.md healthy band") {
		t.Fatalf("negative net-escrow alert fell back to the generic mechanism:\n%s", markdown)
	}
	if strings.Contains(markdown, "Install the transfer_escrow(balance_id, contract_id) index first, then roll out") {
		t.Fatalf("negative net-escrow alert retained a stale unconditional rollout diagnosis:\n%s", markdown)
	}
	if strings.Contains(markdown, "The dominant production cause is the pre-fix full-fleet reconciler") {
		t.Fatalf("negative net-escrow alert overclaimed the retired writer after the current additive reproduction:\n%s", markdown)
	}
}

func TestLogErrorsSignalExplainsNetEscrowMirrorWriteFailure(t *testing.T) {
	line := "[netescrow]mirror write failed after reservation: i/o timeout"
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-connect", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "netescrow-mirror-write").Markdown()
	for _, detail := range []string{
		"PostgreSQL escrow state change committed",
		"Redis mirror pipeline returned an error",
		"old paths discarded some pipeline results",
		"retry-capable client",
		"stops after one command attempt",
		"CloseExpiredContracts intentionally leaves",
		"not proof that every command in the pipeline failed",
		"Never replay INCRBY, DECRBY, or additive correction blindly",
		"non-current balances with open escrow",
		"do not manually change the key",
		"outcome-NULL escrow",
		"zero netescrow-negative lines",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("net-escrow mirror-write alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestLogErrorsSignalGroupsRequiredVaultFailuresByRouteAndGeneration(t *testing.T) {
	lines := strings.Join([]string{
		`[edge-1][api][g3][cid:a1][I][2026-08-30T13:20:04Z][router.go:95][h]unhandled error from route GET ^/verify/keys$: {"error":"Resource not found in vault (verify.yml)"}`,
		`[edge-1][api][g3][cid:a2][I][2026-08-30T13:20:05Z][router.go:95][h]unhandled error from route GET ^/verify/stats$: {"error":"Resource not found in vault (verify.yml)"}`,
	}, "\n")
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-api", nil
		}
		return lines, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("required-vault alerts = %d, want one per route: %+v", len(alerts), alerts)
	}
	wantFrames := map[string]bool{
		"resource=verify.yml route=GET /verify/keys generation=g3":  false,
		"resource=verify.yml route=GET /verify/stats generation=g3": false,
	}
	for _, alert := range alerts {
		if _, ok := wantFrames[alert.Frame]; !ok {
			t.Fatalf("unexpected required-vault frame %q", alert.Frame)
		}
		wantFrames[alert.Frame] = true
		markdown := alert.Markdown()
		for _, detail := range []string{
			"resolved lazily",
			"process and /hello can stay green",
			"documented 503 and Retry-After",
			"Do not invent or commit signing material",
			"every active generation",
			alert.Frame,
		} {
			if !strings.Contains(markdown, detail) {
				t.Fatalf("required-vault alert missing %q:\n%s", detail, markdown)
			}
		}
	}
	for frame, seen := range wantFrames {
		if !seen {
			t.Fatalf("missing required-vault frame %q: %+v", frame, alerts)
		}
	}
}
