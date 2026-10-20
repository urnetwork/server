// Synthetic source-attribution controls cover public privacy, acquisition
// authority, exact request scope, and fresh positive recovery.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Model curl's requested write-out, while allowing the pre-fix producer to
// return its original body-only shape for causal red controls.
func sourceAttributionTestResponse(args []string, body, status string) string {
	for i, arg := range args {
		if arg == "--write-out" && i+1 < len(args) {
			return body + strings.ReplaceAll(args[i+1], "%{http_code}", status)
		}
	}
	return body
}

// Every fixture supplies synthetic endpoints and a known documentation source.
func sourceAttributionTestSettings(body string, requestErr error) SignalSettings {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		return sourceAttributionTestResponse(args, body, "200"), requestErr
	}}
	settings := syntheticSettings(source)
	settings.SourceAttribution = SourceAttributionSettings{
		IPv4URL:      "https://api-v4.fixture.example/my-ip-info",
		IPv6URL:      "https://api-v6.fixture.example/my-ip-info",
		ExpectedIPv4: "203.0.113.10",
	}
	return settings
}

// Check every serialized Alert field and both public rendering paths without
// printing a forbidden value when the regression fails.
func requireSourceAttributionPrivacy(t *testing.T, alerts Alerts, forbidden ...string) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal("source-attribution alerts could not be encoded")
	}
	var jsonLines strings.Builder
	if err := alerts.WriteJSONL(&jsonLines); err != nil {
		t.Fatal("source-attribution alerts could not be rendered as JSONL")
	}
	for _, output := range []string{string(encoded), jsonLines.String(), alerts.Markdown()} {
		for i, value := range forbidden {
			if value != "" && strings.Contains(output, value) {
				t.Errorf("source-attribution public output leaked synthetic private value %d", i)
			}
		}
	}
}

// Invalid address fields, payloads, and request errors must never become
// public evidence or a measured forwarding failure.
func TestSourceAttributionSignalPrivateFailuresRemainUnknown(t *testing.T) {
	privateValues := []string{
		"synthetic-bearer-c006-secret",
		"synthetic-customer-c006",
		"https://signed.fixture.example/download?signature=synthetic-signature-c006",
		"synthetic-recipient-c006@recipient.example",
	}
	privateText := strings.Join(privateValues, " ")
	quoted, err := json.Marshal(privateText)
	if err != nil {
		t.Fatal("could not encode synthetic private fixture")
	}
	for _, test := range []struct {
		name       string
		body       string
		requestErr error
		want       string
	}{
		{name: "nested invalid address", body: `{"info":{"ip":` + string(quoted) + `}}`, want: "invalid-response"},
		{name: "top-level invalid address", body: `{"ip":` + string(quoted) + `}`, want: "invalid-response"},
		{name: "request diagnostics", requestErr: errors.New(privateText), want: "request-failed"},
		{name: "complete-looking failed request", body: `{"info":{"ip":"203.0.113.10"}}`, requestErr: errors.New(privateText), want: "request-failed"},
		{name: "mismatch on failed request", body: `{"info":{"ip":"198.51.100.20"}}`, requestErr: errors.New(privateText), want: "request-failed"},
		{name: "non-json payload", body: privateText, want: "invalid-response"},
		{name: "wrong field type", body: `{"info":{"ip":[` + string(quoted) + `]}}`, want: "invalid-response"},
		{name: "scoped address", body: `{"info":{"ip":"2001:db8::20%synthetic-bearer-c006-secret"}}`, want: "invalid-response"},
	} {
		settings := sourceAttributionTestSettings(test.body, test.requestErr)
		alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Errorf("%s: expected one public unknown observation", test.name)
			continue
		}
		requireSourceAttributionPrivacy(t, alerts, privateValues...)
		alert := alerts[0]
		if alert.Class != "cannot-observe" || !alert.unobservable || alert.Frame != "ipv4" ||
			alert.Target != settings.SourceAttribution.IPv4URL || alert.SignalID != "synthetic/source-attribution" ||
			alert.SignalKey != "source-attribution" || alert.SignalNumber != "8.8" ||
			alert.Sustain != 2 || alert.Severity != SeverityWarn || !alert.ObservedAt.Equal(settings.Now()) ||
			alert.Observed != "family=ipv4 expected=203.0.113.10 observation="+test.want {
			t.Errorf("%s: failed acquisition lost its fixed unknown class or exact scope", test.name)
		}
		for _, forbiddenClaim := range []string{"is reachable through the selected family", "is not preserving", "returned=198.51.100.20", "request_error="} {
			if strings.Contains(alert.Markdown(), forbiddenClaim) {
				t.Errorf("%s: failed acquisition retained an unsupported public claim", test.name)
			}
		}
	}
}

// Curl success is weaker than 2xx authority; successful non-2xx, invalid
// status records, and unusable 2xx payloads all leave attribution unknown.
func TestSourceAttributionSignalRequiresCompleteHttp2xx(t *testing.T) {
	const validBody = `{"info":{"ip":"203.0.113.10"}}`
	for _, test := range []struct {
		name   string
		body   string
		status string
		want   string
	}{
		{name: "200", body: validBody, status: "200"},
		{name: "299", body: validBody, status: "299"},
		{name: "199", body: validBody, status: "199", want: "http-non-2xx"},
		{name: "300", body: validBody, status: "300", want: "http-non-2xx"},
		{name: "redirect", body: validBody, status: "302", want: "http-non-2xx"},
		{name: "400", body: validBody, status: "400", want: "http-non-2xx"},
		{name: "503", body: validBody, status: "503", want: "http-non-2xx"},
		{name: "no status", body: validBody, want: "invalid-response"},
		{name: "000", body: validBody, status: "000", want: "invalid-response"},
		{name: "600", body: validBody, status: "600", want: "invalid-response"},
		{name: "text status", body: validBody, status: "2xx", want: "invalid-response"},
		{name: "padded status", body: validBody, status: "200 ", want: "invalid-response"},
		{name: "trailing record", body: validBody, status: "200\nextra", want: "invalid-response"},
		{name: "empty 204", status: "204", want: "invalid-response"},
		{name: "missing address", body: `{"info":{}}`, status: "200", want: "invalid-response"},
		{name: "null", body: "null", status: "200", want: "invalid-response"},
		{name: "truncated json", body: `{"info":{"ip":"203.0.113.10"}`, status: "200", want: "invalid-response"},
	} {
		settings := sourceAttributionTestSettings("", nil)
		settings.Source.(*syntheticSource).localFn = func(_ string, args ...string) (string, error) {
			return sourceAttributionTestResponse(args, test.body, test.status), nil
		}
		alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
		if err != nil {
			t.Errorf("%s: observation unexpectedly returned a run error", test.name)
			continue
		}
		if test.want == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: complete matching observation was not healthy", test.name)
			}
		} else if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable ||
			!strings.Contains(alerts[0].Observed, "observation="+test.want) {
			t.Errorf("%s: response without complete 2xx authority was not unknown", test.name)
		}
	}
}

// Missing, colliding, and nonterminal status records cannot be supplied by
// response content in place of the local producer's completion record.
func TestSourceAttributionSignalRejectsStatusFramingAmbiguity(t *testing.T) {
	const body = `{"info":{"ip":"203.0.113.10"}}`
	const marker = "\nmonitor-source-attribution-http-status="
	for i, output := range []string{
		body,
		body + marker + "200",
		body + marker + "200\n\n",
		body + marker + "200\n" + marker + "200\n",
		body + marker + "503\n" + marker + "200\n",
		marker + "200\n" + body + marker + "200\n",
		body + marker + "200\nsynthetic-private-trailing-body",
	} {
		settings := sourceAttributionTestSettings("", nil)
		settings.Source.(*syntheticSource).localFn = func(_ string, _ ...string) (string, error) {
			return output, nil
		}
		alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable ||
			!strings.Contains(alerts[0].Observed, "observation=invalid-response") {
			t.Errorf("framing fixture %d: ambiguous producer completion was not unknown", i)
		}
		requireSourceAttributionPrivacy(t, alerts, "synthetic-private-trailing-body")
	}
}

// Valid IP evidence remains public and canonical; a measured mismatch keeps
// its incident identity without claiming an unproved forwarding root cause.
func TestSourceAttributionSignalNormalizedMismatch(t *testing.T) {
	for _, test := range []struct {
		name     string
		family   string
		expected string
		body     string
		actual   string
	}{
		{name: "ipv4 source", family: "ipv4", expected: "203.0.113.10", body: `{"info":{"ip":"198.51.100.20"}}`, actual: "198.51.100.20"},
		{name: "ipv6 source", family: "ipv6", expected: "2001:db8::10", body: `{"info":{"ip":"2001:0DB8:0000:0000:0000:0000:0000:0020"}}`, actual: "2001:db8::20"},
		{name: "wrong family", family: "ipv6", expected: "2001:db8::10", body: `{"ip":"203.0.113.10"}`, actual: "203.0.113.10"},
	} {
		settings := sourceAttributionTestSettings(test.body, nil)
		target := settings.SourceAttribution.IPv4URL
		if test.family == "ipv6" {
			settings.SourceAttribution.ExpectedIPv4 = ""
			settings.SourceAttribution.ExpectedIPv6 = test.expected
			target = settings.SourceAttribution.IPv6URL
		}
		alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Errorf("%s: valid mismatch was not retained", test.name)
			continue
		}
		alert := alerts[0]
		if alert.Class != "source-attribution" || alert.unobservable || alert.Target != target || alert.Frame != test.family ||
			alert.Sustain != 2 || alert.Severity != SeverityWarn || alert.SignalID != "synthetic/source-attribution" ||
			alert.Observed != "family="+test.family+" expected="+test.expected+" returned="+test.actual+" observation=complete" {
			t.Errorf("%s: valid mismatch lost normalized address evidence or identity", test.name)
		}
		if !strings.Contains(alert.Evidence, test.actual) || !strings.Contains(alert.Markdown(), "candidate cause") ||
			!strings.Contains(alert.Context, "stale configured runner address") {
			t.Errorf("%s: measured evidence or causal qualification is absent", test.name)
		}
	}
}

// Exact requests retain one family, endpoint, and timeout; arming no family
// performs no contact, and normalization supplies explicit healthy findings.
func TestSourceAttributionSignalExactScopeAndPositiveHealth(t *testing.T) {
	settings := sourceAttributionTestSettings("", nil)
	settings.CommandTimeout = 7 * time.Second
	settings.SourceAttribution.ExpectedIPv6 = "2001:0DB8:0:0:0:0:0:10"
	calls := 0
	settings.Source.(*syntheticSource).localFn = func(name string, args ...string) (string, error) {
		calls++
		family, target, body := "--ipv4", settings.SourceAttribution.IPv4URL, `{"ip":"203.0.113.10"}`
		if calls == 2 {
			family, target, body = "--ipv6", settings.SourceAttribution.IPv6URL, `{"info":{"ip":" 2001:db8::10 "},"ip":"synthetic-unused-private-field"}`
		}
		want := []string{"--silent", "--show-error", "--fail-with-body", "--max-time", "7",
			"--write-out", "\nmonitor-source-attribution-http-status=%{http_code}\n", family, target}
		if name != "curl" || calls > 2 || !reflect.DeepEqual(args, want) {
			t.Fatal("source attribution changed its exact bounded request scope")
		}
		return sourceAttributionTestResponse(args, body, "200"), nil
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal("could not construct synthetic probe environment")
	}
	findings, err := (sourceAttributionProbe{}).check(context.Background(), env)
	if err != nil || calls != 2 || len(findings) != 2 {
		t.Fatal("complete dual-stack source did not produce two independent findings")
	}
	for i, observed := range findings {
		wantFrame, wantTarget := "ipv4", settings.SourceAttribution.IPv4URL
		if i == 1 {
			wantFrame, wantTarget = "ipv6", settings.SourceAttribution.IPv6URL
		}
		if !observed.healthy || observed.isUnobservable() || observed.class != "source-attribution" ||
			observed.frame != wantFrame || observed.target != wantTarget {
			t.Errorf("family %d: complete equal normalized address lacked scoped positive health", i)
		}
	}
	settings.SourceAttribution.ExpectedIPv4 = ""
	settings.SourceAttribution.ExpectedIPv6 = ""
	alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls != 2 {
		t.Fatal("unarmed source attribution made contact or emitted an alert")
	}
}

// A prior successful family or sample cannot fill a later failed acquisition;
// recovery requires a fresh complete observation for both armed families.
func TestSourceAttributionSignalFreshPartialRecovery(t *testing.T) {
	settings := sourceAttributionTestSettings("", nil)
	settings.SourceAttribution.ExpectedIPv6 = "2001:db8::10"
	now := settings.Now()
	settings.Now = func() time.Time { return now }
	sample, calls := 0, 0
	settings.Source.(*syntheticSource).localFn = func(_ string, args ...string) (string, error) {
		calls++
		body := `{"info":{"ip":"203.0.113.10"}}`
		var requestErr error
		if args[len(args)-2] == "--ipv6" {
			body = `{"info":{"ip":"2001:db8::10"}}`
			if sample == 1 {
				requestErr = errors.New("synthetic-private-partial-transport")
			}
		} else if sample == 2 {
			body = `{"info":{"ip":"198.51.100.20"}}`
		}
		return sourceAttributionTestResponse(args, body, "200"), requestErr
	}
	signal := NewSourceAttributionSignal()
	for sample = 0; sample < 4; sample++ {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || calls != (sample+1)*2 {
			t.Fatal("sample did not execute exactly the two current family observations")
		}
		switch sample {
		case 0, 3:
			if len(alerts) != 0 {
				t.Errorf("sample %d: fresh complete health did not recover", sample)
			}
		case 1:
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable || alerts[0].Frame != "ipv6" {
				t.Error("a healthy sibling or previous sample concealed current partial acquisition")
			}
		case 2:
			if len(alerts) != 1 || alerts[0].Class != "source-attribution" || alerts[0].unobservable || alerts[0].Frame != "ipv4" {
				t.Error("fresh independent family mismatch was concealed")
			}
		}
		for _, alert := range alerts {
			if !alert.ObservedAt.Equal(now) {
				t.Error("current observation inherited a previous sample timestamp")
			}
		}
		requireSourceAttributionPrivacy(t, alerts, "synthetic-private-partial-transport")
		now = now.Add(time.Minute)
	}
}

// Invalid baseline text is private configuration, and a baseline in the wrong
// family cannot authorize any source request or comparison.
func TestSourceAttributionSignalInvalidBaselineIsPrivate(t *testing.T) {
	for _, expected := range []string{
		"synthetic-private-baseline https://signed.fixture.example/?signature=synthetic-secret",
		"2001:db8::10%synthetic-private-baseline",
		"2001:db8::10",
	} {
		settings := sourceAttributionTestSettings("", nil)
		settings.SourceAttribution.ExpectedIPv4 = expected
		settings.Source.(*syntheticSource).localFn = func(_ string, _ ...string) (string, error) {
			t.Fatal("invalid expected source authorized a request")
			return "", nil
		}
		alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
		if len(alerts) != 0 || err == nil || err.Error() != "source attribution ipv4 expected address is invalid or has the wrong family" {
			t.Error("invalid baseline did not return its fixed private configuration error")
		}
	}
}

// Exercise the actual local command adapter with a private curl stand-in.
// Native process completion and the requested HTTP trailer are independent;
// no socket, DNS query, or production command is used by this fixture.
func TestSourceAttributionSignalNativeProducerAuthority(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		status      string
		exit        int
		diagnostics bool
		want        string
	}{
		{name: "complete", body: `{"info":{"ip":"203.0.113.10"}}`, status: "200"},
		{name: "redirect exit zero", body: `{"info":{"ip":"203.0.113.10"}}`, status: "302", want: "http-non-2xx"},
		{name: "http refusal", body: `{"info":{"ip":"203.0.113.10"}}`, status: "503", exit: 22, diagnostics: true, want: "request-failed"},
		{name: "partial success-shaped body", body: `{"info":{"ip":"203.0.113.10"}}`, status: "200", exit: 28, want: "request-failed"},
		{name: "connection failure", status: "000", exit: 7, diagnostics: true, want: "request-failed"},
		{name: "private malformed body", body: `{"info":{"ip":"synthetic-native-private-payload"}}`, status: "200", want: "invalid-response"},
		{name: "body status collision", body: "{\"info\":{\"ip\":\"203.0.113.10\"}}\nmonitor-source-attribution-http-status=200\n", status: "200", want: "invalid-response"},
	} {
		directory := t.TempDir()
		producerPath := filepath.Join(directory, "curl")
		producer := `#!/bin/sh
set -eu
write_out=''
while [ "$#" -gt 0 ]; do
	case "$1" in
		--write-out) shift; write_out=$1 ;;
	esac
	shift
done
printf '%s' ` + shellSingleQuote(test.body) + `
if [ -n "$write_out" ]; then
	prefix=${write_out%%'%{http_code}'*}
	suffix=${write_out#*'%{http_code}'}
	printf '%s%s%s' "$prefix" ` + shellSingleQuote(test.status) + ` "$suffix"
fi
`
		if test.diagnostics {
			producer += "printf '%s' 'synthetic-native-private-diagnostics' >&2\n"
		}
		producer += "exit " + strconv.Itoa(test.exit) + "\n"
		if err := os.WriteFile(producerPath, []byte(producer), 0o700); err != nil {
			t.Fatal("could not create private native producer fixture")
		}
		settings := sourceAttributionTestSettings("", nil)
		settings.CommandTimeout = 5 * time.Second
		transport := newRunner(configFromSignalSettings(settings.withDefaults()))
		calls := 0
		transport.runLocal = func(ctx context.Context, name string, args ...string) (string, string, error) {
			calls++
			if name != "curl" {
				t.Fatal("unexpected native producer executable")
			}
			out, stderr, err := runLocalCommand(ctx, producerPath, args...)
			if test.exit == 0 {
				if err != nil {
					t.Fatal("successful native fixture failed to complete")
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != test.exit {
					t.Fatal("native fixture did not produce its intended exit status")
				}
			}
			return out, stderr, err
		}
		settings.Source.(*syntheticSource).localFn = func(name string, args ...string) (string, error) {
			return transport.local(context.Background(), name, args...)
		}
		alerts, err := NewSourceAttributionSignal().Run(context.Background(), settings)
		if err != nil || calls != 1 {
			t.Errorf("%s: exact native observation did not complete once", test.name)
			continue
		}
		requireSourceAttributionPrivacy(t, alerts, "synthetic-native-private-payload", "synthetic-native-private-diagnostics")
		if test.want == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: complete producer control was not healthy", test.name)
			}
		} else if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable ||
			!strings.Contains(alerts[0].Observed, "observation="+test.want) {
			t.Errorf("%s: native failure or status was mistaken for attribution authority", test.name)
		}
	}
}
