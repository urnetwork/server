// Valid failed-curl observations retain their failure while diagnostics stay private.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const edgePrivacyCanary = "synthetic-private-diagnostic Authorization=Bearer synthetic-edge-token https://diagnostic.example.test/private?signature=synthetic-edge-signature"

// Exercise every public renderer rather than only the owning finding helper.
func edgePrivacyAssertNoDiagnostic(t testing.TB, alerts Alerts) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal("synthetic alert encoding failed")
	}
	var jsonl strings.Builder
	if err := alerts.WriteJSONL(&jsonl); err != nil {
		t.Fatal("synthetic JSONL encoding failed")
	}
	rendered := alerts.Markdown() + string(encoded) + jsonl.String()
	for _, forbidden := range []string{edgePrivacyCanary, "synthetic-private-diagnostic", "synthetic-edge-token", "diagnostic.example.test", "synthetic-edge-signature"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatal("private diagnostic reached a public renderer")
		}
	}
}

func TestEdgeIPv6ValidFailedCurlDiagnosticsStayPrivate(t *testing.T) {
	for _, source := range []string{"public-output", "host-egress", "typed-error", "content-type"} {
		t.Run(source, func(t *testing.T) {
			output := edgeHTTPFixture("000", "28", "", "3.002")
			commandErr := edgeCommandExitError(28)
			if source == "public-output" {
				output = edgePrivacyCanary + "\n" + output
			}
			if source == "typed-error" {
				commandErr = fmt.Errorf("%s: %w", edgePrivacyCanary, commandErr)
			}
			if source == "content-type" {
				output = strings.Replace(output, "monitor_content_type=\n", "monitor_content_type="+edgePrivacyCanary+"\n", 1)
			}
			settings := edgeObservationSettings(output, commandErr, "operstate=up\nconfigured_present=1\nunit_active=active\n", nil)
			transport := settings.Source.(*syntheticSource)
			originalHost := transport.hostFn
			transport.hostFn = func(host HostSettings, command string) (string, error) {
				raw, err := originalHost(host, command)
				if source == "host-egress" && strings.Contains(command, edgeIPv6EgressMarker) {
					raw += "\n" + edgePrivacyCanary + "\n"
				}
				return raw, err
			}
			alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 {
				t.Fatal("valid failed request lost its single product finding")
			}
			alert := requireAlertClass(t, alerts, "edge-ipv6-upstream-drop")
			if alert.Severity != SeverityPage || alert.Sustain != 2 || !strings.Contains(alert.Observed, "http_code=000 curl_exit=28") || !strings.Contains(alert.Observed, "total_seconds=3.002") || !strings.Contains(alert.Observed, "configured_present=1") {
				t.Fatal("privacy projection changed native measurement or failure policy")
			}
			edgePrivacyAssertNoDiagnostic(t, alerts)
		})
	}
}

func TestEdgeIPv6TypedEgressSummaryDoesNotPublishDiagnosticValues(t *testing.T) {
	settings := edgeObservationSettings(edgeHTTPFixture("000", "28", "", "3.002"), edgeCommandExitError(28), "operstate=up\nconfigured_present=1\nunit_active=active\n", nil)
	transport := settings.Source.(*syntheticSource)
	originalHost := transport.hostFn
	transport.hostFn = func(host HostSettings, command string) (string, error) {
		raw, err := originalHost(host, command)
		if strings.Contains(command, edgeIPv6EgressMarker) {
			raw = strings.Replace(raw, "self_time_total=0.080", "self_time_total="+edgePrivacyCanary, 1)
			raw = strings.Replace(raw, "source_egress=2001:db8::41", "source_egress="+edgePrivacyCanary, 1)
		}
		return raw, err
	}
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("typed diagnostic projection failed")
	}
	alert := requireAlertClass(t, alerts, "edge-ipv6-timeout")
	if !strings.Contains(alert.Observed, "self_total_seconds=unobserved") || !strings.Contains(alert.Observed, "source_egress=unobserved") {
		t.Fatal("unvalidated egress text became numeric/address evidence")
	}
	edgePrivacyAssertNoDiagnostic(t, alerts)
}

func TestEdgeIPv6DiagnosticProjectionKeepsHealthyUnknownAndCancellation(t *testing.T) {
	identity := "operstate=up\nconfigured_present=1\nunit_active=active\n"
	healthy := edgeObservationSettings(edgePrivacyCanary+"\n"+edgeHTTPFixture("200", "0", "2001:db8::41", "0.080"), nil, identity, nil)
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), healthy)
	if err != nil || len(alerts) != 0 {
		t.Fatal("healthy measurement became a diagnostic incident")
	}
	unknown := edgeObservationSettings(edgePrivacyCanary, errors.New(edgePrivacyCanary), identity, nil)
	alerts, err = NewEdgeIPv6Signal().Run(context.Background(), unknown)
	if err != nil || len(alerts) != 1 {
		t.Fatal("incomplete native observation gained product certainty")
	}
	requireAlertClass(t, alerts, "cannot-observe")
	edgePrivacyAssertNoDiagnostic(t, alerts)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	canceled := edgeObservationSettings("", nil, identity, nil)
	transport := canceled.Source.(*syntheticSource)
	transport.localFn = func(string, ...string) (string, error) { calls++; return "", nil }
	transport.hostFn = func(HostSettings, string) (string, error) { calls++; return "", nil }
	alerts, err = NewEdgeIPv6Signal().Run(ctx, canceled)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 0 {
		t.Fatal("ended parent gained diagnostic findings or source calls")
	}
}

// Valid wire syntax is not authority to publish a management address or name.
func TestEdgeIPv6ValidWireEgressIdentityStaysPrivate(t *testing.T) {
	for _, sample := range []struct {
		name      string
		field     string
		value     string
		wantClass string
	}{
		{name: "ULA route", field: "route_source", value: "fd00:feed:beef::85", wantClass: "edge-ipv6-policy-route"},
		{name: "link-local route", field: "route_source", value: "fe80::85", wantClass: "edge-ipv6-policy-route"},
		{name: "other public route", field: "route_source", value: "2001:db8:feed::85", wantClass: "edge-ipv6-policy-route"},
		{name: "ULA egress", field: "source_egress", value: "fd00:face:beef::85", wantClass: "edge-ipv6-timeout"},
		{name: "management interface", field: "route_device", value: "mgmt-canary85", wantClass: "edge-ipv6-policy-route"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			settings := edgeObservationSettings(edgeHTTPFixture("000", "28", "", "3.002"), edgeCommandExitError(28), "operstate=up\nconfigured_present=1\nunit_active=active\n", nil)
			transport := settings.Source.(*syntheticSource)
			originalHost := transport.hostFn
			transport.hostFn = func(host HostSettings, command string) (string, error) {
				raw, err := originalHost(host, command)
				if strings.Contains(command, edgeIPv6EgressMarker) {
					original := "2001:db8::41"
					if sample.field == "route_device" {
						original = "synthetic-if-a"
					}
					raw = strings.Replace(raw, sample.field+"="+original+"\n", sample.field+"="+sample.value+"\n", 1)
				}
				return raw, err
			}
			alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 {
				t.Fatal("valid wire diagnostic lost its single measured failure")
			}
			alert := requireAlertClass(t, alerts, sample.wantClass)
			encoded, err := json.Marshal(alerts)
			if err != nil {
				t.Fatal("synthetic alert encoding failed")
			}
			var jsonl strings.Builder
			if err := alerts.WriteJSONL(&jsonl); err != nil {
				t.Fatal("synthetic JSONL encoding failed")
			}
			if strings.Contains(alerts.Markdown()+string(encoded)+jsonl.String(), sample.value) {
				t.Fatal("valid diagnostic identity escaped into public evidence")
			}
			if !strings.Contains(alert.Observed, sample.field+"=differs-configured") || !strings.Contains(alert.Observed, "http_code=000 curl_exit=28") || !strings.Contains(alert.Observed, "total_seconds=3.002") {
				t.Fatal("identity privacy changed the mismatch or native measurement")
			}
		})
	}
	settings := edgeObservationSettings(edgeHTTPFixture("000", "28", "", "3.002"), edgeCommandExitError(28), "operstate=up\nconfigured_present=1\nunit_active=active\n", nil)
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("matching identity control failed")
	}
	alert := requireAlertClass(t, alerts, "edge-ipv6-upstream-drop")
	for _, field := range []string{"route_device", "route_source", "source_egress"} {
		if !strings.Contains(alert.Observed, field+"=matches-configured") {
			t.Fatal("authoritative configured identity lost its match evidence")
		}
	}
}
