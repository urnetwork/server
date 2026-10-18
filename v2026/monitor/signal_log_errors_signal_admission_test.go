// SIGNALS.md §4 and §14.6: exact, privacy-safe signaling admission evidence
// must coexist with legacy diagnostics without hiding a mixed refusal burst.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Executes the ordinary synthetic log probe with fixed source attribution.
func signalAdmissionSyntheticAlerts(t *testing.T, lines string) []Alert {
	t.Helper()
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return lines, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Samples and group identity retain only the bounded gate, kind and reset
// flag; no unrelated prefix or transport identity enters the sample renderer.
func TestLogErrorsSignalPreservesAdmissionDiscriminators(t *testing.T) {
	for _, boundary := range []string{"pack-admission", "queue-handoff", "resend-capacity", "loopback", "unknown"} {
		for _, kind := range []string{"offer", "answer", "candidate", "waiting", "none", "mixed", "unknown"} {
			suffix := fmt.Sprintf("mode=receive-reply reason=not-admitted boundary=%s kind=%s reset=false", boundary, kind)
			line := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-18T12:00:00Z]" +
				"[transport_p2p_webrtc.go:220][signal]send failed " + suffix
			alerts := signalAdmissionSyntheticAlerts(t, strings.Repeat(line+"\n", novelRateThreshold))
			alert := requireAlertClass(t, alerts, "signal-send-not-admitted")
			if !strings.Contains(alert.Markdown(), suffix) {
				t.Fatalf("%s/%s omitted exact bounded evidence: %s", boundary, kind, alert.Markdown())
			}
			wantFrame := "frame=receive-reply/boundary=" + boundary + "/kind=" + kind + "/reset=false"
			foundFrame := false
			for _, candidate := range alerts {
				if candidate.Class == "signal-send-not-admitted" && strings.Contains(candidate.Observed, wantFrame) {
					foundFrame = true
				}
				if candidate.Class == "novel" {
					t.Fatalf("valid diagnostic became novel: %+v", candidate)
				}
			}
			if !foundFrame {
				t.Fatalf("missing bounded admission group %q", wantFrame)
			}
			if sample := signalSendStructuredLogSample(line); sample != "[signal]send failed "+suffix {
				t.Fatalf("sample retained prefix or omitted discriminator: %q", sample)
			}
		}
	}
}

// Adding gate/kind groups cannot turn two sub-threshold refusal populations
// into healthy service evidence when their aggregate reaches the old threshold.
func TestLogErrorsSignalAdmissionMixedGatesRetainAggregate(t *testing.T) {
	prefix := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-18T12:00:00Z]" +
		"[transport_p2p_webrtc.go:220][signal]send failed mode=receive-reply reason=not-admitted "
	first := prefix + "boundary=pack-admission kind=answer reset=false\n"
	second := prefix + "boundary=queue-handoff kind=candidate reset=false\n"
	lines := strings.Repeat(first, novelRateThreshold/2) + strings.Repeat(second, novelRateThreshold-novelRateThreshold/2)
	alerts := signalAdmissionSyntheticAlerts(t, lines)
	alert := requireAlertClass(t, alerts, "signal-send-not-admitted")
	if !strings.Contains(alert.Observed, fmt.Sprintf("rate=%d/min", novelRateThreshold)) ||
		strings.Contains(alert.Observed, "frame=") {
		t.Fatalf("mixed refusal aggregate was lost or attributed to one gate: %+v", alert)
	}
}

// Gate diagnostics do not collapse typed encryption, lifecycle and unexpected
// errors into an admission failure. Their unknown gate is explicit.
func TestLogErrorsSignalNonAdmissionReasonsRetainDiagnostics(t *testing.T) {
	for _, reason := range []string{"encryption-not-ready", "canceled-or-closed", "other"} {
		suffix := "mode=sender reason=" + reason + " boundary=unknown kind=offer reset=true"
		line := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-18T12:00:00Z]" +
			"[transport_p2p_webrtc.go:220][signal]send failed " + suffix
		alerts := signalAdmissionSyntheticAlerts(t, strings.Repeat(line+"\n", novelRateThreshold))
		alert := requireAlertClass(t, alerts, "signal-send-"+reason)
		if !strings.Contains(alert.Markdown(), suffix) {
			t.Fatalf("%s diagnostic omitted bounded signal context: %s", reason, alert.Markdown())
		}
		for _, candidate := range alerts {
			if candidate.Class == "novel" || candidate.Class == "signal-send-not-admitted" {
				t.Fatalf("non-admission diagnostic acquired the wrong cause: %+v", candidate)
			}
		}
	}
}

// Old structured logs prove refusal but cannot identify its gate or payload.
func TestLogErrorsSignalLegacyAdmissionBoundaryRemainsUnobserved(t *testing.T) {
	line := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-18T12:00:00Z]" +
		"[transport_p2p_webrtc.go:164][signal]send failed mode=receive-reply reason=not-admitted"
	if got := signalSendLogAdmission(line); got != "receive-reply/boundary=unobserved" {
		t.Fatalf("legacy diagnostic inferred an admission gate: %q", got)
	}
	alerts := signalAdmissionSyntheticAlerts(t, strings.Repeat(line+"\n", novelRateThreshold))
	markdown := requireAlertClass(t, alerts, "signal-send-not-admitted").Markdown()
	for _, want := range []string{
		"Old mode/reason-only lines leave the boundary unobserved",
		"fairness also reserves capacity",
		"zero-buffer rendezvous",
		"refused answer is not replayed merely by a duplicate offer",
		"Quiet or accepted sends alone do not prove remote delivery",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("admission alert omitted qualifier %q", want)
		}
	}
}

// Unknown/misordered/partial fields, trailing payloads, and an admission gate
// attached to a non-admission reason remain visible as generic novelty.
func TestLogErrorsSignalRejectsMalformedAdmissionDiscriminators(t *testing.T) {
	prefix := "[fixture-edge][taskworker][g2][cid:fixture][I][2026-09-18T12:00:00Z]" +
		"[transport_p2p_webrtc.go:220][signal]send failed mode=receive-reply "
	for _, fields := range []string{
		"reason=not-admitted boundary=peer-down kind=answer reset=false",
		"reason=not-admitted boundary=pack-admission kind=unexpected reset=false",
		"reason=not-admitted boundary=pack-admission kind=answer reset=sometimes",
		"reason=not-admitted kind=answer boundary=pack-admission reset=false",
		"reason=not-admitted boundary=pack-admission kind=answer",
		"reason=not-admitted boundary=pack-admission kind=answer reset=false detail=synthetic-private-payload",
		"reason=encryption-not-ready boundary=pack-admission kind=answer reset=false",
	} {
		alerts := signalAdmissionSyntheticAlerts(t, strings.Repeat(prefix+fields+"\n", novelRateThreshold))
		requireAlertClass(t, alerts, "novel")
		for _, alert := range alerts {
			if strings.HasPrefix(alert.Class, "signal-send-") {
				t.Fatalf("unsupported diagnostic entered a typed class: %+v", alert)
			}
		}
	}
}
