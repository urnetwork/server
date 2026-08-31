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
		"full-fleet reconciler",
		"absolute SET or DEL",
		"not evidence that the site independently created",
		"log-ingestion delay",
		"page-local additive reconciler",
		"transfer_escrow(balance_id, contract_id) index",
		"Retain them where present",
		"roll them out only where version or code evidence says they are absent",
		"PostgreSQL settlement commits before its Redis release",
		"atomic release Lua",
		"clamped_to=0",
		"contained commit/post ordering window",
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
