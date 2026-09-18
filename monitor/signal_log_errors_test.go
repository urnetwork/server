package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Mirrors ErrorJson's array schema with deliberately private synthetic material
// before the stack, in arguments, and in source paths that must never be retained.
func panicOwnerTestLine(t *testing.T, errorText string, functions ...string) string {
	t.Helper()
	stack := []string{"goroutine 42 [running]:", "runtime/debug.Stack()", "/synthetic-private-path/stack.go:12345 +0x99"}
	for _, function := range append([]string{
		"github.com/urnetwork/server.HandleError.func1",
		"github.com/urnetwork/server.dbWithPool.func1.1",
		"github.com/urnetwork/server.Raise",
		"github.com/urnetwork/server.RaisePgResult[...]",
	}, functions...) {
		stack = append(stack, function+"(0x12345678, {0x87654321, 0x33})", "/synthetic-private-path/owner.go:12345 +0x99")
	}
	body, err := json.Marshal(map[string]any{"error": errorText, "stack": stack})
	if err != nil {
		t.Fatal(err)
	}
	return "[synthetic-private-host][api][synthetic-generation][cid:synthetic-correlation] metadata={ignored} Unexpected error: " + string(body)
}

// Keeps only actionable panic findings; aggregate and owner views deliberately
// overlap and are keyed by their distinct, stable frames.
func panicOwnerTestAlerts(t *testing.T, tailer *logTailer) map[string]Alert {
	t.Helper()
	alerts := map[string]Alert{}
	for _, finding := range tailer.drainWindow() {
		if finding.class != "panic" || finding.healthy {
			continue
		}
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
		if _, duplicate := alerts[alert.Frame]; duplicate {
			t.Fatal("duplicate panic frame")
		}
		if alert.Severity != SeverityPage || alert.Target != "api" || alert.SignalID != "logs/panic" {
			t.Fatal("panic aggregate/owner changed its existing service, signal or page tier")
		}
		if len(tailer.classSamples) != 0 {
			t.Fatal("drain retained prior window samples")
		}
		for _, private := range []string{
			"synthetic-private-host", "synthetic-generation", "synthetic-correlation", "cid:",
			"synthetic-private-path", "owner.go", "12345", "0x12345678", "0x87654321",
			"synthetic-secret", "synthetic-customer", "192.0.2.88", "2001:db8::88", "@private.example",
		} {
			requireAlertOmits(t, alert, private)
		}
		alerts[alert.Frame] = alert
	}
	return alerts
}

// A long error used to consume the entire sample before the owning stack. Both
// views must now retain the same bounded discriminator without private content.
func TestPanicOwnerLongErrorRetainsPrivateSafeDiscriminator(t *testing.T) {
	tailer := newLogTailer("api", nil)
	line := panicOwnerTestLine(t,
		"*pgconn.PgError=ERROR: "+strings.Repeat("synthetic-secret synthetic-customer 192.0.2.88:443 ", 20)+"(SQLSTATE 25006)",
		"github.com/urnetwork/server/model.StampTopLevelClientContractTime.func1.1",
	)
	for range 5 {
		tailer.classify(line)
	}
	if len(tailer.classSamples["panic"]) > 200 {
		t.Fatal("redacted sample exceeded its existing budget")
	}
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 2 {
		t.Fatal("five owning diagnostics must preserve aggregate plus owner pages")
	}
	for _, frame := range []string{"", "server/model.StampTopLevelClientContractTime"} {
		alert, ok := alerts[frame]
		if !ok || !strings.Contains(alert.Observed, "rate=5/min") {
			t.Fatal("aggregate or owning five-line page disappeared")
		}
		if frame == "" && strings.Contains(alert.Observed, "frame=") {
			t.Fatal("service-wide aggregate rendered an empty owner frame")
		}
		for _, want := range []string{"owner=server/model.StampTopLevelClientContractTime", "error_type=*pgconn.PgError", "sqlstate=25006", "counts overlap"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("bounded panic evidence omitted %q", want)
			}
		}
	}
}

// Independent owner thresholds cannot split the original service-wide page.
func TestPanicOwnerMixedThreePlusTwoPreservesAggregate(t *testing.T) {
	tailer := newLogTailer("api", nil)
	for index, owner := range []string{"SyntheticFirst", "SyntheticSecond"} {
		line := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/server/model."+owner)
		for range 3 - index {
			tailer.classify(line)
		}
	}
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 1 || !strings.Contains(alerts[""].Observed, "rate=5/min") {
		t.Fatal("3+2 owner split hid or duplicated the original aggregate page")
	}
}

// Compiler closure numbers and receiver notation do not create new identities;
// different application owners still retain their own matching samples.
func TestPanicOwnerNormalizationAndSampleSeparation(t *testing.T) {
	for _, test := range []struct{ function, owner string }{
		{function: "server/model.SyntheticWrite.func12.3", owner: "server/model.SyntheticWrite"},
		{function: "connect.(*SyntheticResident).Forward.func2", owner: "connect.SyntheticResident.Forward"},
		{function: "sdk.SyntheticDevice.Load-fm", owner: "sdk.SyntheticDevice.Load"},
		{function: "proxy.SyntheticStart.gowrap2", owner: "proxy.SyntheticStart"},
		{function: "operator-proxy/ingest.(*SyntheticClient).Submit", owner: "operator-proxy/ingest.SyntheticClient.Submit"},
		{function: "userwireguard.SyntheticRun", owner: "userwireguard.SyntheticRun"},
		{function: "warp/services.SyntheticRun.func1.2", owner: "warp/services.SyntheticRun"},
		{function: "server/model.SyntheticWrite[...].func1", owner: "server/model.SyntheticWrite"},
		{function: "connect.(*SyntheticResident[...]).Forward.func2", owner: "connect.SyntheticResident.Forward"},
	} {
		line := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/"+test.function)
		if got := panicLogOwner(line); got != test.owner {
			t.Fatalf("normalized owner = %q, want %q", got, test.owner)
		}
	}
	for _, wrapper := range []string{
		"server.HandleError1[...].func1", "server.HandleError2[...].func2", "server.HandleErrorWithReturn[...]",
		"connect.HandleError1[...].func1", "connect.HandleError2[...].func2",
	} {
		line := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/"+wrapper,
			"github.com/urnetwork/server/model.SyntheticWrite")
		if got := panicLogOwner(line); got != "server/model.SyntheticWrite" {
			t.Fatalf("generic recovery wrapper became owner %q", got)
		}
	}
	tailer := newLogTailer("api", nil)
	for _, owner := range []string{"SyntheticFirst", "SyntheticSecond"} {
		for index := range 5 {
			tailer.classify(panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
				fmt.Sprintf("github.com/urnetwork/server/model.%s.func%d.1", owner, index+1)))
		}
	}
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 3 || !strings.Contains(alerts[""].Observed, "rate=10/min") {
		t.Fatal("separate owners lost the aggregate or their own buckets")
	}
	for _, owner := range []string{"SyntheticFirst", "SyntheticSecond"} {
		frame := "server/model." + owner
		if !strings.Contains(alerts[frame].Evidence, "owner="+frame) || !strings.Contains(alerts[frame].Observed, "rate=5/min") {
			t.Fatal("owner bucket borrowed another owner's sample or rate")
		}
	}
}

// Malformed/unknown/oversized records keep aggregate visibility and fixed safe
// samples. An error message cannot inject ownership in place of a stack.
func TestPanicOwnerMalformedAndOversizedStayAggregateOnly(t *testing.T) {
	valid := panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/server/model.SyntheticWrite")
	for _, line := range []string{
		"panic: synthetic-secret 192.0.2.88:443",
		"Unexpected error: {",
		valid[:len(valid)-1],
		strings.Repeat("x", panicLogMaxBytes) + valid,
		`Unexpected error: {"error":"synthetic-secret","stack":"synthetic-private-path"}`,
		`Unexpected error: {"error":"synthetic-secret","stack":[]}`,
		`Unexpected error: {"error":"synthetic-secret","stack":["github.com/urnetwork/server/model.SyntheticWrite(0x12345678)"]}`,
		strings.Replace(valid, `"error":`, `"error":"synthetic-secret","error":`, 1),
		strings.Replace(valid, `"error":"*errors.errorString=synthetic-secret"`, `"error":null`, 1),
		strings.Replace(valid, `"stack":[`, `"stack":[null,`, 1),
		strings.Replace(valid, `"stack":[`, `"stack":[{},`, 1),
		panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/unlisted.SyntheticWrite"),
		panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/server/model.SyntheticWrite[unrecognized]",
			"github.com/urnetwork/server/model.SyntheticCaller"),
		panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/server/model."+strings.Repeat("Synthetic", 30)),
		`Unexpected error: {"error":"*errors.errorString=github.com/urnetwork/server/model.SyntheticWrite(0x12345678)","stack":["goroutine 42 [running]"]}`,
	} {
		tailer := newLogTailer("api", nil)
		for range 5 {
			tailer.classify(line)
		}
		alerts := panicOwnerTestAlerts(t, tailer)
		if len(alerts) != 1 || !strings.Contains(alerts[""].Observed, "rate=5/min") {
			t.Fatal("unrecognized stack lost aggregate visibility or invented an owner")
		}
	}
	stack := make([]string, panicLogMaxStackLines+1)
	for index := range stack {
		stack[index] = "runtime.synthetic()"
	}
	stack[0] = "github.com/urnetwork/server/model.SyntheticWrite(0x12345678)"
	stack[1] = "/synthetic-private-path/owner.go:12345 +0x99"
	body, err := json.Marshal(map[string]any{"error": "*errors.errorString=synthetic-secret", "stack": stack})
	if err != nil {
		t.Fatal(err)
	}
	if got := panicLogOwner("Unexpected error: " + string(body)); got != "" {
		t.Fatal("oversized stack array gained an owner")
	}
}

// Unknown first samples must not leak endpoint attribution; raw Go type text and
// SQLSTATE-like message substrings cannot escape their allowlists.
func TestPanicOwnerUnknownFirstSampleAndTypeGuards(t *testing.T) {
	tailer := newLogTailer("api", nil)
	for range 4 {
		tailer.classify("panic: synthetic-secret 192.0.2.88:443")
	}
	tailer.classify(panicOwnerTestLine(t, "*errors.errorString=synthetic-secret", "github.com/urnetwork/server/model.SyntheticWrite"))
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 1 || !strings.Contains(alerts[""].Evidence, "owner=unknown error_type=unknown sqlstate=unknown") {
		t.Fatal("unknown first sample was replaced with invented ownership or raw text")
	}
	for _, errorText := range []string{
		"*synthetic-secret.Error=synthetic-secret (SQLSTATE 25006)",
		"*errors.errorString=synthetic-secret (SQLSTATE 25006)",
		"*pgconn.PgError=synthetic-secret (SQLSTATE 25006) trailing",
		"*pgconn.PgError=synthetic-secret (SQLSTATE secret)",
	} {
		observation := parsePanicLogObservation(panicOwnerTestLine(t, errorText, "github.com/urnetwork/server/model.SyntheticWrite"))
		if observation.sqlstate != "unknown" || strings.Contains(observation.errorType, "synthetic-secret") {
			t.Fatal("unvalidated Go error type or SQLSTATE escaped")
		}
	}
}

// Overflow is aggregate-only, existing owners remain admitted, and the next
// minute starts with an empty owner budget rather than permanent suppression.
func TestPanicOwnerCardinalityCapResetsWithoutSuppressingAggregate(t *testing.T) {
	tailer := newLogTailer("api", nil)
	lineForOwner := func(index int) string {
		return panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
			fmt.Sprintf("github.com/urnetwork/server/model.SyntheticOwner%d", index))
	}
	for index := range logClassMaxOwnerGroups {
		tailer.classify(lineForOwner(index))
	}
	for range 4 {
		tailer.classify(lineForOwner(0))
	}
	for range 5 {
		tailer.classify(lineForOwner(logClassMaxOwnerGroups))
	}
	if tailer.classGroupCounts["panic"] != logClassMaxOwnerGroups || len(tailer.classCounts) != logClassMaxOwnerGroups+1 {
		t.Fatal("owner state exceeded the bounded per-window cardinality")
	}
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 2 || !strings.Contains(alerts[""].Observed, fmt.Sprintf("rate=%d/min", logClassMaxOwnerGroups+9)) ||
		!strings.Contains(alerts["server/model.SyntheticOwner0"].Observed, "rate=5/min") {
		t.Fatal("overflow suppressed aggregate events or an already-known owner")
	}
	if len(tailer.classGroupCounts) != 0 {
		t.Fatal("drain retained an exhausted owner budget")
	}
	for range 5 {
		tailer.classify(lineForOwner(logClassMaxOwnerGroups))
	}
	if len(panicOwnerTestAlerts(t, tailer)) != 2 {
		t.Fatal("previous overflow owner was not admitted in a fresh window")
	}
	if len(panicOwnerTestAlerts(t, tailer)) != 0 {
		t.Fatal("empty window retained a prior panic page")
	}
}

// A known typed failure still wins before generic panic accounting, even when
// its structured stack could otherwise produce an owner bucket.
func TestPanicOwnerPreservesTypedClassPrecedence(t *testing.T) {
	tailer := newLogTailer("api", nil)
	line := panicOwnerTestLine(t,
		"*pgconn.PgError=FATAL: server login has been failing, cached error: sorry, too many clients already (server_login_retry) (SQLSTATE 08P01)",
		"github.com/urnetwork/server/model.SyntheticWrite",
	)
	for range 5 {
		tailer.classify(line)
	}
	findings := tailer.drainWindow()
	if finding := findingByClass(t, findings, "pg-client-capacity"); finding.healthy {
		t.Fatal("typed database-capacity class lost precedence")
	}
	for _, finding := range findings {
		if finding.class == "panic" && !finding.healthy {
			t.Fatal("typed error also entered generic aggregate or owner buckets")
		}
	}
}

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

// Resolver-leg diagnostics retain the page guard without claiming a lookup
// failed or copying the endpoint, domain, or correlation identity.
func TestLogErrorsSignalDohDialTimeoutIsPrivateAttemptEvidence(t *testing.T) {
	testCases := []struct {
		name string
		line string
	}{
		{name: "family IPv4", line: "[family]dial tag=doh net=tcp family=? policy=force4 demoted=none err=dial tcp 192.0.2.53:443: i/o timeout"},
		{name: "family IPv6", line: "[family]dial tag=doh net=tcp6 family=? policy=force6 demoted=none err=dial tcp6 [2001:db8::53]:443: i/o timeout"},
		{name: "endpoint-free family", line: "[family]dial tag=doh net=tcp family=? policy=force4 demoted=none err=i/o timeout"},
		{name: "bound egress", line: "[egress]dial tag=doh tcp 192.0.2.53:443 if=4:2/6:0 bound=yes err=dial tcp 192.0.2.53:443: i/o timeout (2 suppressed)"},
	}
	for _, testCase := range testCases {
		line := "[synthetic-proxy.example][proxy][synthetic-block][cid:synthetic-private-correlation]" +
			"[I][2026-01-01T00:00:00Z][egress_dial.go:306]" + testCase.line + " domain=private-query.example"
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-proxy", nil
			}
			return strings.Repeat(line+"\n", 10), nil
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		alert := requireAlertClass(t, alerts, "doh-dial-timeout")
		if alert.Severity != SeverityPage || alert.Frame != "doh-attempt" {
			t.Fatalf("%s: resolver attempt lost its page or fixed frame", testCase.name)
		}
		markdown := alert.Markdown()
		for _, want := range []string{
			"rate=10/min", "logical DNS outcome is unknown",
			"count neither unique attempts nor failed logical DNS queries",
			"success-only DoH result callback", "Do not restart Redis",
			"independent final-query or end-to-end outcome", "10 minutes",
		} {
			if !strings.Contains(markdown, want) {
				t.Errorf("%s: resolver attempt guidance omitted %q", testCase.name, want)
			}
		}
		for _, private := range []string{
			"synthetic-proxy.example", "synthetic-block", "synthetic-private-correlation",
			"cid:", "private-query.example", "192.0.2.53", "2001:db8::53", "if=4:2",
			"node accept path starving", "event loop wedged",
		} {
			if strings.Contains(markdown, private) {
				t.Errorf("%s: alert retained private data or the old wedge claim", testCase.name)
			}
		}
		for _, other := range alerts {
			if other.Class == "dial-io-timeout" || other.Class == "novel" {
				t.Errorf("%s: resolver attempt fell through to %s", testCase.name, other.Class)
			}
		}
	}
}

// The retained threshold counts diagnostic lines across resolver legs. Empty
// windows and process replacement must not carry the prior minute's rate.
func TestDohDialTimeoutThresholdAndWindowReset(t *testing.T) {
	tailer := newLogTailer("proxy", nil)
	line := "[family]dial tag=doh net=tcp family=? policy=force4 demoted=none err=i/o timeout"
	for range 9 {
		tailer.classify(line)
	}
	if finding := findingByClass(t, tailer.drainWindow(), "doh-dial-timeout"); !finding.healthy {
		t.Fatal("nine diagnostic lines crossed the unchanged ten-line threshold")
	}
	for index := range 10 {
		tailer.classify(fmt.Sprintf("[family]dial tag=doh net=tcp family=? policy=force4 demoted=none err=dial tcp 192.0.2.%d:443: i/o timeout", index+1))
	}
	finding := findingByClass(t, tailer.drainWindow(), "doh-dial-timeout")
	if finding.healthy || finding.tier != tierPage || finding.frame != "doh-attempt" || finding.target != "proxy" {
		t.Fatal("mixed resolver endpoints split or weakened the diagnostic page")
	}
	if finding := findingByClass(t, tailer.drainWindow(), "doh-dial-timeout"); !finding.healthy {
		t.Fatal("an empty window retained the prior diagnostic page")
	}
	replacement := newLogTailer("proxy", nil)
	if finding := findingByClass(t, replacement.drainWindow(), "doh-dial-timeout"); !finding.healthy {
		t.Fatal("a replacement collector inherited diagnostic counts")
	}
}

// Only the exact DoH producer shape gains attempt attribution; unrelated
// targets retain the generic timeout page without a Redis-cause assertion.
func TestDohDialTimeoutDoesNotCaptureOtherDialOwners(t *testing.T) {
	for _, line := range []string{
		"dial tcp 192.0.2.80:6380: i/o timeout",
		"[family]dial tag=api net=tcp err=dial tcp 192.0.2.80:443: i/o timeout",
		"[family]dial tag=doh-other net=tcp err=dial tcp 192.0.2.80:443: i/o timeout",
		"[other]dial tag=doh net=tcp err=dial tcp 192.0.2.80:443: i/o timeout",
	} {
		tailer := newLogTailer("proxy", nil)
		for range 10 {
			tailer.classify("[synthetic-host.example][proxy][synthetic-block][cid:synthetic-private-correlation] " + line)
		}
		findings := tailer.drainWindow()
		generic := findingByClass(t, findings, "dial-io-timeout")
		if generic.healthy || generic.tier != tierPage || generic.frame == "" {
			t.Fatal("generic endpoint timeout lost its page or endpoint frame")
		}
		if !strings.Contains(generic.mechanism, "remain alternatives") ||
			!strings.Contains(generic.action, "corroborating local PING hang") ||
			strings.Contains(generic.evidence, "synthetic-private-correlation") ||
			strings.Contains(generic.evidence, "event loop wedged") {
			t.Fatal("generic timeout retained unsupported attribution or private correlation data")
		}
		if doh := findingByClass(t, findings, "doh-dial-timeout"); !doh.healthy {
			t.Fatal("an unrelated dial owner was classified as DoH")
		}
	}
	for _, line := range []string{
		"[family]dial tag=doh net=tcp family=4 policy=force4 demoted=none local=192.0.2.80:12345",
		"[family]dial tag=doh net=tcp err=context canceled",
		"[family]dial tag=doh net=tcp err=dial tcp 192.0.2.80:443: connect: connection refused",
		"err=i/o timeout",
	} {
		tailer := newLogTailer("proxy", nil)
		for range 10 {
			tailer.classify(line)
		}
		if finding := findingByClass(t, tailer.drainWindow(), "doh-dial-timeout"); !finding.healthy {
			t.Fatal("a non-timeout or ownerless line gained DoH timeout attribution")
		}
	}
}

func TestLogErrorsSignalSyntheticStructuredProblemClasses(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		class string
	}{
		{"dial timeout", "dial tcp 192.0.2.1:6380: i/o timeout", "dial-io-timeout"},
		{"connection refused", "connect: connection refused", "connection-refused"},
		{"Grafana local Mimir refusal", `Stats push error (Post "http://synthetic-mimir.invalid:14818/api/v1/push": dial tcp synthetic-mimir.invalid:14818: connect: connection refused)`, "grafana-mimir-push-refused"},
		{"Connect TLS disabled", "[c]Could not initialize tls config. Disabling transport. = synthetic loader failure", "connect-tls-disabled"},
		{"port exhaustion", "connect: cannot assign requested address", "port-exhaustion"},
		{"pool timeout", "redis: connection pool timeout", "pool-timeout"},
		{"cluster down", "CLUSTERDOWN Hash slot not served", "clusterdown"},
		{"oom writes", "OOM command not allowed when used memory > maxmemory", "oom-writes"},
		{"Mimir series admission", "Stats push rejected (400): per-user series limit of 75000 exceeded", "mimir-series-limit"},
		{"Mimir structured rate admission", "Stats push rejected status=429 reason=rate-limit job=api metric_families=2 time_series=3 family_classes=go:1,process:1 family_classes_truncated=false", "mimir-ingestion-rate-limit"},
		{"Mimir structured other rejection", "Stats push rejected status=503 reason=server job=other metric_families=1 time_series=1 family_classes=other:1 family_classes_truncated=false", "mimir-push-rejected"},
		{"Loki tail backend EOF", `level=error caller=tail.go:230 component=tail-querier org_id=fake msg="Error receiving response from grpc tail client" addr=192.0.2.10:6490 err=EOF`, "loki-tail-backend-eof"},
		{"Loki tail dropped streams", `level=info caller=tailer.go:271 msg="tailer dropped streams is reset" length=100`, "loki-tail-dropped-streams"},
		{"Warpctl direct Loki tail loss", `[warpctl][loki-tail-dropped-entries] service=proxy count=2`, "loki-tail-dropped-entries"},
		{"connection reset", "read: connection reset by peer", "conn-reset"},
		{"WebSocket abnormal EOF", "websocket: close 1006 (abnormal closure): unexpected EOF", "conn-reset"},
		{"redis loading", "LOADING Redis is loading the dataset in memory", "redis-loading"},
		{"required vault", "panic: Resource not found in vault (verify.yml)", "required-vault-resource"},
		{"grafana plugin", "error=\"the result-set has errors: [plugin.notRegistered] plugin not registered\"", "grafana-plugin-unregistered"},
		{"source attribution", "[session]X-UR-Forwarded-For from untrusted peer", "source-attribution"},
		{"onboarding post-primary canceled", "[onboarding]campaign enrollment failed for network synthetic-network: Done", "onboarding-post-primary-canceled"},
		{"onboarding app-open attribution", "[onboarding]app open attribution failed for network synthetic-private-network: ERROR: inconsistent types deduced for parameter $4 (SQLSTATE 42P08)", "onboarding-app-open-attribution"},
		{"onboarding connect-day write", "[onboarding]connect.day write failed for client private-client.fixture.example: ERROR: inconsistent types deduced for parameter $3 (SQLSTATE 42P08)", "onboarding-connect-day-write"},
		{"HTTP write after hijack", "http: response.WriteHeader on hijacked connection from github.com/urnetwork/server/router.(*Router).ServeHTTP.func1.1 (router.go:104)", "http-hijack-write"},
		{"negative escrow", "[netescrow]negative counter after release", "netescrow-negative"},
		{"escrow mirror write", "[netescrow]mirror write failed after reservation: i/o timeout", "netescrow-mirror-write"},
		{"PostgreSQL client slots", "Unexpected error: FATAL: server login has been failing, cached error: sorry, too many clients already (server_login_retry)", "pg-client-capacity"},
		{"panic", "panic: synthetic crash frame", "panic"},
		{"payout wallet", "asset amount owned by the wallet is insufficient", "payout-wallet-insufficient"},
		{"invalid payout destination", `Bad status: 400 Bad Request {"code":155219,"message":"Invalid destination address."}`, "payout-invalid-destination"},
		{"payment processor rate limit", `Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`, "payment-processor-rate-limit"},
		{"net escrow ttl", `[redis][ttl]"expireat" key="{escrow_019c640e-f467-4fa7-177f-d7ca43c33b6f}net" ttl 3139393191s-from-now exceeds 9600h0m0s`, "redis-netescrow-ttl"},
		{"redis ttl", "[redis][ttl] suspicious ttl on key", "redis-ttl-suspect"},
		{"HTTP drain hard cut", "[http]drain deadline after 1m10.25s: 2 connection(s) cut", "http-drain-cut"},
		{"taskworker drain", "[taskworker]drain gave up with 2 tasks", "taskworker-drain-gave-up"},
		{"legacy database maintenance", "[db]maintenance reindex[16/22] contract_close", "db-maintenance-legacy-reindex"},
		{"legacy signal send", "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z][transport_p2p_webrtc.go:149][signal]send failed ->11111111-1111-1111-1111-111111111111", "signal-send-unclassified"},
		{"signal admission", "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z][transport_p2p_webrtc.go:164][signal]send failed mode=receive-reply reason=not-admitted", "signal-send-not-admitted"},
		{"signal encryption", "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z][transport_p2p_webrtc.go:164][signal]send failed mode=sender reason=encryption-not-ready", "signal-send-encryption-not-ready"},
		{"signal closure", "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z][transport_p2p_webrtc.go:164][signal]send failed mode=sender reason=canceled-or-closed", "signal-send-canceled-or-closed"},
		{"other signal error", "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z][transport_p2p_webrtc.go:164][signal]send failed mode=sender reason=other", "signal-send-other"},
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

func TestLogErrorsSignalKeepsLegacySignalSendCausallyUnclassifiedAndPrivate(t *testing.T) {
	const privateDestination = "11111111-1111-1111-1111-111111111111"
	line := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z]" +
		"[transport_p2p_webrtc.go:149][signal]send failed ->" + privateDestination
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return strings.Repeat(line+"\n", novelRateThreshold), nil
	}}

	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "signal-send-unclassified").Markdown()
	for _, want := range []string{
		"legacy destination omitted; result unavailable",
		"boolean-only caller discarded",
		"does not prove send capacity healthy",
		"corrected from Taskworker to Connect",
		"Do not infer pressure, lifecycle, transport failure",
		"structured sender diagnostic",
		"below 20/min for ten minutes",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("legacy signal-send alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, privateDestination) {
		t.Fatalf("legacy signal-send alert retained destination id:\n%s", markdown)
	}
	for _, alert := range alerts {
		if alert.Class == "novel" {
			t.Fatalf("legacy signal-send line remained novel: %+v", alert)
		}
	}
}

func TestLogErrorsSignalClassifiesStructuredSignalSendReasons(t *testing.T) {
	tests := []struct {
		mode   string
		reason string
		class  string
	}{
		{mode: "receive-reply", reason: "not-admitted", class: "signal-send-not-admitted"},
		{mode: "sender", reason: "encryption-not-ready", class: "signal-send-encryption-not-ready"},
		{mode: "sender", reason: "canceled-or-closed", class: "signal-send-canceled-or-closed"},
		{mode: "sender", reason: "other", class: "signal-send-other"},
	}
	for _, test := range tests {
		line := fmt.Sprintf(
			"[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z]"+
				"[transport_p2p_webrtc.go:164][signal]send failed mode=%s reason=%s",
			test.mode,
			test.reason,
		)
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-taskworker", nil
			}
			return strings.Repeat(line+"\n", novelRateThreshold), nil
		}}

		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, test.class)
		if !strings.Contains(alert.Observed, "frame="+test.mode) {
			t.Errorf("%s alert omitted bounded mode: %+v", test.class, alert)
		}
		if !strings.Contains(alert.Markdown(), "mode="+test.mode+" reason="+test.reason) {
			t.Errorf("%s alert omitted bounded sample: %s", test.class, alert.Markdown())
		}
		for _, other := range alerts {
			if other.Class == "novel" {
				t.Errorf("structured %s signal-send line remained novel: %+v", test.reason, other)
			}
		}
	}
}

func TestLogErrorsSignalLeavesMalformedSignalSendNovel(t *testing.T) {
	line := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-11T22:04:45Z]" +
		"[transport_p2p_webrtc.go:164][signal]send failed mode=receive-reply reason=network-error"
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return strings.Repeat(line+"\n", novelRateThreshold), nil
	}}

	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "novel")
	for _, class := range []string{
		"signal-send-unclassified",
		"signal-send-not-admitted",
		"signal-send-encryption-not-ready",
		"signal-send-canceled-or-closed",
		"signal-send-other",
	} {
		for _, alert := range alerts {
			if alert.Class == class {
				t.Fatalf("malformed signal-send line entered %s: %+v", class, alert)
			}
		}
	}
}

func TestLogErrorsSignalRedactsOnboardingAppOpenAttribution(t *testing.T) {
	const privateNetwork = "synthetic-private-network"
	line := "[onboarding]app open attribution failed for network " + privateNetwork + ": ERROR: inconsistent types deduced for parameter $4 (SQLSTATE 42P08)"
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-api", nil
		}
		return line, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "onboarding-app-open-attribution")
	if alert.Severity != SeverityWarn {
		t.Fatalf("onboarding attribution severity = %q, want warn", alert.Severity)
	}
	markdown := alert.Markdown()
	for _, want := range []string{"SQLSTATE 42P08", "network identifier omitted", "0aac4806", "exact flow_step"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("onboarding attribution alert lacks %q: %s", want, markdown)
		}
	}
	if strings.Contains(markdown, privateNetwork) {
		t.Fatalf("onboarding attribution alert retained network evidence: %s", markdown)
	}
	for _, other := range alerts {
		if other.Class == "novel" {
			t.Fatalf("known onboarding attribution failure remained novel: %+v", other)
		}
	}
}

func TestLogErrorsSignalClassifiesOnboardingPostPrimaryCancellationByStage(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		stage      string
		identifier string
	}{
		{
			name:       "app open",
			line:       "[onboarding]app open attribution failed for network synthetic-network-token: Done",
			stage:      "app-open",
			identifier: "synthetic-network-token",
		},
		{
			name:       "campaign enrollment",
			line:       "[onboarding]campaign enrollment failed for network synthetic-enrollment-token: Done",
			stage:      "campaign-enrollment",
			identifier: "synthetic-enrollment-token",
		},
		{
			name:       "client context",
			line:       "[onboarding]client context failed for network synthetic-context-token: Done",
			stage:      "client-context",
			identifier: "synthetic-context-token",
		},
		{
			name:       "connect day",
			line:       "[onboarding]connect.day write failed for client synthetic-client-token: Done",
			stage:      "connect-day",
			identifier: "synthetic-client-token",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
				if len(args) > 1 && args[0] == "ls" {
					return "repo names synthetic-service", nil
				}
				return test.line, nil
			}}
			alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "onboarding-post-primary-canceled")
			markdown := alert.Markdown()
			for _, want := range []string{
				"frame=stage=" + test.stage,
				"stage=" + test.stage,
				"identifier omitted",
				"primary",
				"ten-second",
				"API and Connect",
			} {
				if !strings.Contains(markdown, want) {
					t.Errorf("post-primary cancellation alert lacks %q: %s", want, markdown)
				}
			}
			if strings.Contains(markdown, test.identifier) {
				t.Fatalf("post-primary cancellation alert retained identifier: %s", markdown)
			}
			for _, other := range alerts {
				if other.Class == "novel" {
					t.Fatalf("known post-primary cancellation remained novel: %+v", other)
				}
			}
		})
	}
}

func TestLogErrorsSignalRedactsOnboardingConnectDayWrite(t *testing.T) {
	const privateClient = "private-client.fixture.example"
	line := "[onboarding]connect.day write failed for client " + privateClient + ": ERROR: inconsistent types deduced for parameter $3 (SQLSTATE 42P08)"
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-connect", nil
		}
		return line, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "onboarding-connect-day-write")
	if alert.Severity != SeverityWarn {
		t.Fatalf("onboarding connect-day severity = %q, want warn", alert.Severity)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"SQLSTATE 42P08",
		"client identifier omitted",
		"already committed",
		"next connection retries",
		"deploy Connect",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("onboarding connect-day alert lacks %q: %s", want, markdown)
		}
	}
	if strings.Contains(markdown, privateClient) {
		t.Fatalf("onboarding connect-day alert retained client evidence: %s", markdown)
	}
	for _, other := range alerts {
		if other.Class == "novel" {
			t.Fatalf("known onboarding connect-day failure remained novel: %+v", other)
		}
	}
}

func TestLogErrorsSignalAttributesGrafanaMimirPushRefusalWithoutEndpoint(t *testing.T) {
	line := `[synthetic-edge.invalid][grafana][g1][cid:synthetic-container][2026-09-10T11:20:08Z] Stats push error (Post "http://synthetic-mimir.invalid:14818/api/v1/push": dial tcp synthetic-mimir.invalid:14818: connect: connection refused)`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return line, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "grafana-mimir-push-refused")
	if alert.Severity != SeverityPage {
		t.Fatalf("Grafana/Mimir refusal severity = %q, want PAGE", alert.Severity)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"frame=local-mimir-push",
		"co-located Mimir listener was unavailable",
		"SO_REUSEPORT",
		"not Redis §5.2",
		"child shutdown/SIGTERM boundary",
		"two successive generations on one block",
		"counts rejected pushes, not failed parents or incidents",
		"drain-before-child-stop",
		"6544fe1",
		"Do not restart Redis",
		"closed at the bounded deadline",
		"full rollout plus 10 steady minutes",
		"§11.20 continuity gap",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("Grafana/Mimir refusal alert omitted %q:\n%s", want, markdown)
		}
	}
	for _, private := range []string{
		"synthetic-edge.invalid",
		"synthetic-container",
		"synthetic-mimir.invalid",
		"14818",
	} {
		if strings.Contains(markdown, private) {
			t.Errorf("Grafana/Mimir refusal retained endpoint detail %q:\n%s", private, markdown)
		}
	}
	for _, other := range alerts {
		if other.Class == "connection-refused" || other.Class == "novel" {
			t.Errorf("specific Grafana/Mimir refusal escaped into %s", other.Class)
		}
	}
}

func TestLogErrorsSignalGenericConnectionRefusalDoesNotAssumeRedis(t *testing.T) {
	line := `synthetic request to service.invalid failed: connect: connection refused`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-service", nil
		}
		return strings.Repeat(line+"\n", 10), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "connection-refused").Markdown()
	for _, want := range []string{
		"otherwise-unclassified TCP target",
		"does not identify the target service",
		"Do not assume this is Redis",
		"same network namespace",
		"Do not restart an inferred service",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("generic refusal alert omitted %q:\n%s", want, markdown)
		}
	}
	for _, stale := range []string{
		"SIGNALS.md 5.2",
		"after manual restart",
		"restart with correct conf",
	} {
		if strings.Contains(markdown, stale) {
			t.Errorf("generic refusal retained stale attribution %q:\n%s", stale, markdown)
		}
	}
}

func TestLogErrorsSignalMimirSeriesLimitIsSpecificAndSanitized(t *testing.T) {
	line := `Stats push rejected (400): per-user series limit of 75000 exceeded; series metric{customer="private-customer",instance="private-instance",address="192.0.2.19:9494",detail="connect: connection refused"}`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-grafana", nil
		}
		return line, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-series-limit")
	markdown := alert.Markdown()
	for _, want := range []string{
		"tenant-series-admission", "series details omitted", "per-user-series admission discards",
		"candidate contributors", "Accepted per-service", "Legacy unstructured events",
		"preserve random instance identity", "two-hour",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("Mimir admission alert omitted %q", want)
		}
	}
	for _, secret := range []string{"private-customer", "private-instance", "192.0.2.19", "metric{", "connect: connection refused"} {
		if strings.Contains(markdown, secret) {
			t.Errorf("Mimir admission alert retained series detail %q", secret)
		}
	}
	for _, other := range alerts {
		if other.Class == "connection-refused" || other.Class == "novel" {
			t.Errorf("Mimir rejection body escaped into generic class %s", other.Class)
		}
	}

	for _, control := range []string{
		`Stats push rejected (400): sample timestamp too old`,
		`Stats push rejected (503): per-user series limit unavailable`,
		`level=info configured per-user series limit=75000`,
		`Stats push accepted (202): per-user series limit=75000`,
	} {
		source.localFn = func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-grafana", nil
			}
			return control, nil
		}
		controls, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		for _, controlAlert := range controls {
			if controlAlert.Class == "mimir-series-limit" {
				t.Errorf("adjacent control falsely classified: %s", control)
			}
		}
	}
}

func TestLogErrorsSignalDoesNotAttributeConnectionResetToServer(t *testing.T) {
	line := `[fireside][proxy][g8][cid:test][I][2026-09-03T04:56:58.402388-05:00][device_rpc_transport.go:339][mux]read done = websocket: close 1006 (abnormal closure): unexpected EOF`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-proxy", nil
		}
		return strings.Repeat(line+"\n", 50), nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "conn-reset").Markdown()
	for _, want := range []string{
		"abrupt transport termination without an orderly close",
		"Connection reset by peer is peer-relative",
		"WebSocket close 1006 with unexpected EOF",
		"without a WebSocket close frame",
		"NAT, load balancer, firewall, or other intermediary",
		"cannot distinguish peer lifecycle, path failure, remote process replacement, or resource pressure",
		"proxy-memory, proxy-pool, proxy-runtime, and proxy-cache controls were all healthy",
		"contemporaneous controls reject Proxy resource pressure as the demonstrated cause",
		"1,176 attach/EOF/detach triplets from one logical hosted device",
		"1,075 of 1,175 measured sessions lasted under one second",
		"zero write, keepalive, receive-budget, receive-queue, version-rejection, instance-rejection, or SyncReverse markers",
		"Proxy accepts this /device-rpc WebSocket and the remote DeviceRemote initiates it",
		"exact 500-millisecond pace matches failed-sync retry pacing",
		"cohort predates the bounded endpoint markers now in source",
		"[drpc-attempt] endpoint=remote stage=<dial|sync|sync-reverse|active> result=<bounded>",
		"[drpc-session] endpoint=proxy stage=<transport|request|response> result=<bounded>",
		"stage=transport ingress=absent",
		"stage=request egress=absent",
		"no ids, addresses, frame bytes, counts, or raw errors",
		"rate=50/min class=conn-reset target=proxy",
		"Do not call the event a server close",
		"remains below 50/min for 10 minutes",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("connection-reset alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "server closed the conn") {
		t.Fatalf("connection-reset alert made an unsupported server attribution:\n%s", markdown)
	}
}

func TestLogErrorsSignalExplainsConnectTLSDisabled(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-connect", nil
		}
		return "[c]Could not initialize tls config. Disabling transport. = synthetic invalid certificate\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "connect-tls-disabled").Markdown()
	for _, want := range []string{
		"substituted an empty TLS configuration",
		"rejecting every QUIC ClientHello",
		"64366fb5",
		"before any listener goroutine begins",
		"not generic provider unresponsiveness",
		"does not explain that day's provider degradation without a matching line",
		"Do not restart the same artifact repeatedly",
		"completes a real QUIC handshake on every enabled carrier",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Connect TLS-disabled alert missing %q:\n%s", want, markdown)
		}
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
	if alert.Severity != SeverityPage {
		t.Fatalf("legacy maintenance severity = %q, want PAGE", alert.Severity)
	}
	for _, detail := range []string{
		"frame=table=contract_close",
		"legacy full-table concurrent-reindex path",
		"908a8b2c",
		"d8392c83",
		"patch-identical",
		"pg_stat_progress_create_index",
		"does not by itself prove that PostgreSQL began the statement",
		"exhausted PostgreSQL admission",
		"reclaimed the same durable task into a new retry",
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

func TestLogErrorsSignalSeparatesPostgresClientExhaustionFromPanic(t *testing.T) {
	lines := strings.Join([]string{
		`[edge-3][connect][g4][cid:trace][I][2026-09-01T12:09:30.949276-05:00][trace.go:51]Unexpected error: {"error":"*pgconn.PgError=FATAL: server login has been failing, cached error: sorry, too many clients already (server_login_retry) (SQLSTATE 08P01)","stack":"goroutine 12 [running]"}`,
		`[edge-3][connect][g4][cid:route][I][2026-09-01T12:09:31.048153-05:00][router.go:99][h]unhandled error from route GET ^/$: {"error":"*pgconn.PgError=FATAL: server login has been failing, cached error: sorry, too many clients already (server_login_retry) (SQLSTATE 08P01)","stack":"goroutine 13 [running]"}`,
	}, "\n")
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-connect", nil
		}
		return lines, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityPage {
		t.Fatalf("PostgreSQL client-capacity severity = %q, want PAGE", alert.Severity)
	}
	for _, candidate := range alerts {
		if candidate.Class == "panic" {
			t.Fatalf("typed PostgreSQL capacity lines also leaked into panic: %+v", alerts)
		}
	}
	for _, detail := range []string{
		"rate=2/min class=pg-client-capacity",
		"server_login_retry",
		"32 independent PgBouncer processes",
		"diagnostic amplification, not unique rejected PostgreSQL sessions",
		"takes precedence over panic",
		"distinct from query_wait_timeout",
		"direct 5432",
		"60-66-second COMMITs",
		"replacement connection churn",
		"908a8b2c",
		"d8392c83",
		"Do not raise max_connections first",
		"large work_mem",
		"headroom stays above 25%",
	} {
		if !strings.Contains(alert.Markdown(), detail) {
			t.Fatalf("PostgreSQL client-capacity alert missing %q:\n%s", detail, alert.Markdown())
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
		"nonblocking send",
		"even while the 100-slot input queue had capacity",
		"blocking cancellation-aware select",
		"larger than five but smaller than the processing bound drains without loss",
		"omitted records before they reached the querier",
		"Sustained blockage beyond the existing 100-slot bound",
		"still drops streams",
		"five-second ticker",
		"affirmative internal live-tail loss",
		"pushTailResponseFromIngester",
		"Warp 5927527, contained by 35453fd",
		"without enlarging either queue",
		"Grafana is the observation service",
		"All six active Grafana nodes were healthy",
		"removes missing LAN identity as the current prerequisite",
		"capped service-wide query is not a total",
		"same absolute window for each configured block",
		"do not redeploy already-current blocks",
		"Warp commit 1e95aef",
		"server Proxy commit e055c98c",
		"Warp 35453fd",
		"contains 13fcd05's producer reductions",
		"5927527's descriptor forwarding",
		"cancellation-aware five-slot handoff fix",
		"service-attributed loki-tail-dropped-entries",
		"Do not raise Loki's fixed queues",
		"suppress genuine sustained-backpressure evidence",
		"one aggregate summary per reconciling instance",
		"Mimir query-frontend/evaluator statistics and Loki table lookup info records remain zero",
		"bucket-index version gaps converge below two minutes",
		"loki-tail-dropped-streams, service-attributed loki-tail-dropped-entries, plus loki-tail-backend-eof remain zero for 10 minutes",
		"greater-than-100 sustained burst may still produce bounded descriptors",
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
		"declared live-tail loss for this service's standing tail",
		"Two bounded Loki stages",
		"buffers ten tail responses",
		"up to ten DroppedStreams descriptors",
		"Warp 5927527 forwards those descriptors",
		"without enlarging a queue",
		"decoded dropped_entries but silently discarded it",
		"Warp commit 26089b2",
		"exact affected-service attribution",
		"privacy-safe",
		"does not distinguish the two stages",
		"same-window ingester reset corroborates ingester-side loss",
		"cannot assign the reset to this exact service summary",
		"rate counts summary lines",
		"one response's bounded descriptor count",
		"understate loss magnitude",
		"Warp 35453fd contains that forwarding",
		"prevents sub-processing-bound bursts",
		"genuine sustained blockage still produces bounded descriptors",
		"running Grafana artifact contains Warp 35453fd",
		"Warpctl artifact contains 26089b2",
		"two consecutive overlap reconciliations complete",
		"no service-attributed loki-tail-dropped-entries summary",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("direct Loki dropped-entry alert missing %q:\n%s", detail, markdown)
		}
	}
}

func TestLogErrorsSignalHidesMalformedDirectLokiTailDroppedEntrySuffix(t *testing.T) {
	line := `[warpctl][loki-tail-dropped-entries] service=fixture-service count=2 error=fixture-sensitive-suffix`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names fixture-service", nil
		}
		return line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	visibility := requireAlertClass(t, alerts, "loki-tail-dropped-entries-unobservable")
	if strings.Contains(visibility.Markdown(), "fixture-sensitive-suffix") {
		t.Fatal("malformed summary suffix reached Markdown")
	}
	for _, alert := range alerts {
		if alert.Class == "loki-tail-dropped-entries" {
			t.Fatal("malformed summary became affirmative loss evidence")
		}
	}
}

func TestLokiShortBurstHandoffDocumentationContract(t *testing.T) {
	catalog, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalogText := strings.Join(strings.Fields(string(catalog)), " ")
	for _, want := range []string{
		"Warp commit `35453fd` is the subsequent correctness boundary",
		"100-stream processing queue",
		"five-stream channel",
		"more than five but fewer than 100",
		"pressure beyond 100 remains bounded and emits drop metadata",
		"all 8,118 source journal rows matched durable Loki",
		"1,099 ingester reset markers and 28 backend EOFs",
		"Deploy a Grafana image containing `35453fd`",
		"zero raw reset, service-attributed dropped-entry, plus EOF classes for ten minutes",
	} {
		if !strings.Contains(catalogText, want) {
			t.Errorf("Loki short-burst catalog missing %q", want)
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
		"2026.9.1-outerwerld+1035004200",
		"clean Warp source 71731e4",
		"rules out a stale rollout and a single failed backend",
		"already contained 42168fe and bca37cf",
		"restored all six active ring nodes",
		"missing LAN identity is no longer a current prerequisite",
		"Bounded log reconciliation remains required",
		"Warp commit 1e95aef",
		"only to older Grafana blocks",
		"Warp commits 1e95aef and bca37cf",
		"Warp commit 35453fd",
		"contains 5927527's producer reduction and bounded descriptor forwarding",
		"five-slot ingester handoff",
		"existing 100-slot processing queue can absorb a short burst",
		"residual service-attributed drop",
		"named backend's tailer, process, ring, and network state",
		"Do not raise Loki's fixed queues",
		"2,825 unconditional Mimir `evaluation stats`",
		"1,084 Loki table lookup records",
		"942 bucket-index warnings",
		"1,229 reset records",
		"22 attributed EOFs split across every one of the six backend addresses",
		"every configured active ring member owns its LAN identity",
		"loki-tail-dropped-streams, service-attributed loki-tail-dropped-entries, plus loki-tail-backend-eof remain zero for 10 minutes",
		"Genuine sustained blockage remains bounded and observable",
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
	legacyAlert := requireAlertClass(t, alerts, "loki-tail-backend-eof")
	legacyMarkdown := legacyAlert.Markdown()
	if legacyAlert.Frame != "" || strings.Contains(legacyAlert.Observed, "frame=") ||
		!strings.Contains(legacyAlert.Observed, "target=grafana") {
		t.Fatalf("legacy Loki EOF should remain service-attributed without an invented backend frame:\n%s", legacyMarkdown)
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
		"when its guarded reset succeeds",
		"select the same invalid payout_wallet configuration",
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
	if requireAlertClassCount(alerts, "payout-invalid-destination-reset-failed") != 0 {
		t.Fatal("ordinary invalid-destination control produced a reset-failure alert")
	}
}

func TestLogErrorsSignalSeparatesInvalidDestinationResetFailure(t *testing.T) {
	const paymentId = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const taskId = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	line := `[synthetic-host][taskworker][synthetic-block][cid:fixture][I][2026-09-01T12:00:00Z][task.go:1930][` + taskId + `]eval error = AdvancePayment({"payment_id":"` + paymentId + `"}) = [` + paymentId + `]Payment create transaction error = Bad status: 400 Bad Request {"code":155219,"message":"Invalid destination address."}; invalid destination reset error = private.fixture.example dial tcp 192.0.2.83:5432 i/o timeout wallet=2001:db8::83 goroutine 123 [synthetic.Stack]`
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return line + "\n" + line + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "payout-invalid-destination-reset-failed")
	for _, want := range []string{
		"failed guarded attempt reset", "Concurrent, terminal, or on-chain payment state",
		"Preserve the existing idempotency key", "Do not infer unchanged wallet configuration",
		"invalid_destination_reset_failed_events=1", "diagnostic_lines=2",
		"frame=phase=guarded-reset", "without duplicate transfers",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("reset-failure log alert missing %q", want)
		}
	}
	requireAlertOmits(t, alert, paymentId, taskId, "private.fixture.example", "192.0.2.83", "2001:db8::83", "synthetic.Stack", "selects the same invalid", "account-owner/operations action only")
	for _, class := range []string{"payout-invalid-destination", "dial-io-timeout", "panic"} {
		if requireAlertClassCount(alerts, class) != 0 {
			t.Errorf("reset failure was also attributed to %q", class)
		}
	}
}

func TestInvalidDestinationResetLogRequiresExactComposedMarker(t *testing.T) {
	var resetClass *logClass
	for i := range logClasses {
		if logClasses[i].name == "payout-invalid-destination-reset-failed" {
			resetClass = &logClasses[i]
			break
		}
	}
	if resetClass == nil {
		t.Fatal("missing reset-failure log class")
	}
	for _, line := range []string{
		"Bad status: 400 Bad Request Invalid destination address.",
		"Payment create transaction error = Invalid destination address.; invalid destination reset error: synthetic",
		"Payment create transaction error = Invalid destination address.; invalid destination reset error = ",
		"unrelated failure; invalid destination reset error = synthetic",
	} {
		if resetClass.re.MatchString(line) {
			t.Errorf("non-composed control matched reset-failure class: %q", line)
		}
	}
	for _, line := range []string{
		"Payment create transaction error = Invalid destination address.; invalid destination reset error = Invalid payment.",
		"Payment create transaction error = processor code 155219; invalid destination reset error = context canceled",
	} {
		if !resetClass.re.MatchString(line) {
			t.Errorf("owned reset-failure marker was not classified: %q", line)
		}
	}
}

func TestLogErrorsSignalHealthyWindowHasNoResetFailure(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return "", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "payout-invalid-destination-reset-failed") != 0 {
		t.Fatal("healthy log window produced a reset-failure alert")
	}
}

func TestLogErrorsSignalExplainsPayoutWalletInsufficiency(t *testing.T) {
	lines := []string{}
	for attempt := 100; attempt < 105; attempt++ {
		processorLine, evaluatorLine := payoutAttemptLogLines("2026-08-31T14:00:48", attempt)
		lines = append(lines, processorLine, evaluatorLine)
	}
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
	markdown := requireAlertClass(t, alerts, "payout-wallet-insufficient").Markdown()
	for _, detail := range []string{
		"source wallet lacks enough token balance",
		"one-hour nominal cap",
		"disperses saturated retries across 30–90 minutes",
		"older code used only two seconds of jitter",
		"N parked rows still produce roughly N canonical task attempts per hour on average",
		"both a Circle-client diagnostic and a task-evaluator line",
		"line rate measures diagnostic amplification",
		"wallet_insufficient_events=5",
		"diagnostic_lines=10",
		"logical event count: 5 exact-replay-deduplicated task evaluator line(s) from 10 diagnostic line(s)",
		"operational liquidity boundary",
		"software release cannot fund",
		"Do not delete or manually replay pending_task rows",
		"First use §8.12 to verify every taskworker block's source/digest identity",
		"intentional local checkout containing current-main marker-capable server commit 928abfca",
		"earlier 66525afc baseline alone does not provide the gauge or marker",
		"§2.14 to prove complete admission-observable and activity metrics",
		"fewer than four exact pre-POST admission markers/second",
		"allow the same window plus ingestion delay",
		"duplicate Circle transfers",
		"<id>",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("payout-wallet alert missing %q:\n%s", detail, markdown)
		}
	}
	for attempt := 100; attempt < 105; attempt++ {
		id := fmt.Sprintf("019f77ae-de17-db98-b22d-%012x", attempt)
		if strings.Contains(markdown, id) {
			t.Fatalf("payout-wallet alert leaked wallet/task id %s:\n%s", id, markdown)
		}
	}
	if strings.Contains(markdown, "The observed value is outside the SIGNALS.md healthy band") ||
		strings.Contains(markdown, "Follow SIGNALS.md §4") {
		t.Fatalf("payout-wallet alert retained generic guidance:\n%s", markdown)
	}
}

func TestLogErrorsSignalRendersOnlyBoundedAdmissionBurstEvidence(t *testing.T) {
	lines := []string{}
	for admission := 0; admission < 4; admission++ {
		lines = append(lines, payoutAdmissionLogLine("2026-09-12T19:13:00", admission))
	}
	// This resembles the fixed prefix but appends provider-controlled text.
	// The strict grammar must neither count it nor retain it in evidence.
	lines = append(lines,
		payoutAdmissionLogLine("2026-09-12T19:13:00", 4)+" arbitrary_private_field=synthetic-value",
	)
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
	markdown := requireAlertClass(t, alerts, "payout-retry-microburst").Markdown()
	for _, want := range []string{
		"peak_admitted_submissions_per_second=4",
		"admitted_submissions=4",
		"exact-replay-deduplicated pre-POST admission markers",
		"absent markers are unknown rather than zero",
		"§2.14 admission-observable capability",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("admission-burst Markdown lacks %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{
		"worker-a.invalid",
		"cid:synthetic",
		"arbitrary_private_field",
		"synthetic-value",
	} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("admission-burst Markdown retained %q:\n%s", forbidden, markdown)
		}
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
		"marker-capable server commit 928abfca",
		"earlier 66525afc baseline can expose the activity/error collectors without the capability gauge or exact marker",
		"fleet-wide Redis-time transfer gate",
		"conservative three-per-second ceiling",
		"§2.14 admission-observable capability plus all five activity families",
		"exact pre-POST admission markers stay below four/second",
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

func TestLogErrorsSignalDiscriminatesGrafanaPluginRequestFailure(t *testing.T) {
	line := `logger=context userId=1 orgId=1 uname=admin padding=` + strings.Repeat("x", 240) + ` method=POST path=/api/ds/query status=404 remote_addr=192.0.2.1 referer="https://grafana.example/d/urnetwork-connect/urnetwork-connect?refresh=30s" handler=/api/ds/query status_source=server errorReason=NotFound errorMessageID=plugin.notRegistered error="plugin not registered"`
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
		"request-level failure",
		"Prometheus and Loki datasource implementations as standalone native plugins",
		"stale browser/dashboard payload",
		"Logs Drilldown app is a frontend",
		"dedicated grafana-datasources probe",
		"same generation",
		"pinned native plugin and catalog SHA-256",
		"If both controls succeed",
		"discard or reload the stale originating browser/dashboard state",
		"queries[].datasource.type",
		"queries[].datasource.uid",
		"do not add central request-body logging",
		"Do not recreate a healthy datasource",
		"count_over_time query through warp-loki via Grafana /api/ds/query",
		"var-ds=warp-loki",
		"every active exact-edge generation",
		"method=POST path=/api/ds/query status=404",
		"urnetwork-connect",
		"errorMessageID=plugin.notRegistered",
		"zero new grafana-plugin-unregistered lines for 10 minutes",
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

// Keep causal caveats in the owning section, not an unrelated catalog mention.
func TestLogErrorsWindowObservationNegativeControlsDocumented(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	sectionStart := strings.Index(catalog, "\n### 14.6 ")
	if sectionStart < 0 {
		t.Fatal("catalog is missing the owning section 14.6")
	}
	section := catalog[sectionStart:]
	sectionEnd := strings.Index(section, "\n### 14.7 ")
	if sectionEnd < 0 {
		t.Fatal("catalog is missing the section 14.7 boundary")
	}
	section = section[:sectionEnd]
	paragraphFor := func(anchor string) string {
		anchorIndex := strings.Index(section, anchor)
		if anchorIndex < 0 {
			return ""
		}
		paragraphStart := strings.LastIndex(section[:anchorIndex], "\n\n")
		if paragraphStart < 0 {
			paragraphStart = 0
		} else {
			paragraphStart += len("\n\n")
		}
		paragraphEnd := len(section)
		if nextParagraph := strings.Index(section[anchorIndex:], "\n\n"); nextParagraph >= 0 {
			paragraphEnd = anchorIndex + nextParagraph
		}
		paragraph := strings.ReplaceAll(section[paragraphStart:paragraphEnd], "`", "")
		return strings.Join(strings.Fields(paragraph), " ")
	}
	checks := []struct {
		name    string
		anchor  string
		clauses []string
	}{
		{
			name:    "session settings are not evaluation outcomes",
			anchor:  "event=session",
			clauses: []string{"settings", "configuration evidence", "not evaluation outcomes"},
		},
		{
			name:    "generic timeout does not establish authentication or lifecycle cause",
			anchor:  "Timeout.",
			clauses: []string{"not proof of credential rejection", "request deadline", "caller/strategy cancellation"},
		},
		{
			name:    "window events retain task attribution limits",
			anchor:  "identity-free lines",
			clauses: []string{"cannot be joined one-to-one to those tasks", "do not identify customer sessions"},
		},
	}
	for _, check := range checks {
		paragraph := paragraphFor(check.anchor)
		missingClauses := []string{}
		for _, clause := range check.clauses {
			if !strings.Contains(paragraph, clause) {
				missingClauses = append(missingClauses, clause)
			}
		}
		if len(missingClauses) > 0 {
			t.Errorf("section 14.6 %s is missing %q", check.name, missingClauses)
		}
	}
}
