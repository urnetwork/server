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
		{"connection reset", "read: connection reset by peer", "conn-reset"},
		{"redis loading", "LOADING Redis is loading the dataset in memory", "redis-loading"},
		{"required vault", "panic: Resource not found in vault (verify.yml)", "required-vault-resource"},
		{"grafana plugin", "error=\"the result-set has errors: [plugin.notRegistered] plugin not registered\"", "grafana-plugin-unregistered"},
		{"source attribution", "[session]X-UR-Forwarded-For from untrusted peer", "source-attribution"},
		{"negative escrow", "[netescrow]negative counter after release", "netescrow-negative"},
		{"panic", "panic: synthetic crash frame", "panic"},
		{"payout wallet", "asset amount owned by the wallet is insufficient", "payout-wallet-insufficient"},
		{"invalid payout destination", `Bad status: 400 Bad Request {"code":155219,"message":"Invalid destination address."}`, "payout-invalid-destination"},
		{"payment processor rate limit", `Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`, "payment-processor-rate-limit"},
		{"net escrow ttl", `[redis][ttl]"expireat" key="{escrow_019c640e-f467-4fa7-177f-d7ca43c33b6f}net" ttl 3139393191s-from-now exceeds 9600h0m0s`, "redis-netescrow-ttl"},
		{"redis ttl", "[redis][ttl] suspicious ttl on key", "redis-ttl-suspect"},
		{"taskworker drain", "[taskworker]drain gave up with 2 tasks", "taskworker-drain-gave-up"},
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
		"six canonical evaluator events per UTC hour",
		"operational wallet-configuration evidence",
		"supported account API",
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
		"First verify every taskworker block's embedded source revision",
		"Deploy the proportional-jitter taskworker only to blocks older than commit 70b0d269",
		"do not redeploy from this alert",
		"allow up to 90 minutes plus log-ingestion delay",
		"no duplicate Circle transfers",
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
	markdown := requireAlertClass(t, alerts, "payment-processor-rate-limit").Markdown()
	for _, detail := range []string{
		"short-window request limit",
		"diagnostic line rate is not a unique-submit rate",
		"five distinct wallet-insufficient attempts landed in one second",
		"second-scale microbursts",
		"ambiguous submit outcome",
		"idempotency key must be retained",
		"not a general Circle outage",
		"Do not manually retry",
		"deploy commit 70b0d269 or later only to older blocks",
		"do not redeploy from this alert",
		"one full 90-minute drain window",
		"account's authoritative quota",
		"durable processor-rate-limit count does not increase",
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

func TestLogErrorsSignalExplainsGrafanaPrometheusPluginFailure(t *testing.T) {
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
		"Grafana 13 extracted the formerly core Prometheus datasource",
		"provisioned warp-mimir row",
		"direct Mimir query",
		"exact Grafana generation and image",
		"pinned Prometheus plugin and catalog SHA-256",
		"Prometheus plugin and provisioned-alert interval tests",
		"Do not recreate the datasource",
		"vector(1) through Grafana /api/ds/query",
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
		"current page-local additive path",
		"PostgreSQL statement fixes its page snapshot",
		"later Redis GET sees the newer mirror",
		"not evidence that the site independently created",
		"log-ingestion delay",
		"reservation statement shape and timing",
		"migration 601",
		"unsettled-partial query",
		"durable per-balance fencing/versioning",
		"committed settlement before its delayed Redis release",
		"atomic release Lua",
		"clamped_to=0",
		"contained commit/post ordering window",
		"unsettled-partial pages below 1 second",
		"below 120 seconds",
		"below 256GiB",
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
