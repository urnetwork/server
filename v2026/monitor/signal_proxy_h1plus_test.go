package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestProxyH1PlusCurrentSlotsHealthyXLControl(t *testing.T) {
	now := h1PlusTestNow()
	alerts, err := NewProxyH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "proxy", h1PlusTestFleet(now)))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("active current XL: alerts=%+v err=%v", alerts, err)
	}
}

func TestProxyH1PlusCurrentSlotsMissingProtocolIsNotZero(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	for _, p := range fleet {
		for key := range p.values {
			if !strings.HasSuffix(key, "/start") {
				delete(p.values, key)
				delete(p.times, key)
			}
		}
	}
	alerts, err := NewProxyH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "proxy", fleet))
	if err != nil || len(alerts) != 2 {
		t.Fatalf("missing XL visibility: alerts=%+v err=%v", alerts, err)
	}
	for _, alert := range alerts {
		if alert.Class != "h1plus-telemetry-missing" || !strings.Contains(alert.Markdown(), "urnetwork-framerxl/1") || !strings.Contains(alert.Markdown(), "SIGNALS.md §23.2") {
			t.Errorf("lost distinct XL ownership: %s", alert.Markdown())
		}
	}
}

func TestProxyH1PlusQueryCannotUseCompactCounter(t *testing.T) {
	query := h1PlusQuery("synthetic", h1PlusProbe{service: "proxy", protocol: "urnetwork-framerxl/1"})
	if !strings.Contains(query, `protocol="urnetwork-framerxl/1"`) || strings.Contains(query, `protocol="urnetwork-framer/1"`) ||
		!strings.Contains(query, "urnetwork_proxy_h1plus_bytes_total") || strings.Contains(query, "urnetwork_connect_h1plus") {
		t.Fatal("XL query borrowed compact protocol or service authority")
	}
}

func TestProxyH1PlusLongLivedPayloadAndBlockVisibility(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	for _, p := range fleet {
		p.values["now/accepted"] = p.values["prior/accepted"]
		p.values["now/attempts"] = p.values["prior/attempts"]
	}
	delete(fleet[0].times, "now/bytes")
	alerts, err := NewProxyH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "proxy", fleet))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "h1plus-telemetry-incomplete" || alerts[0].Target != "proxy/blue" {
		t.Fatalf("XL current-slot/long-lived boundary: alerts=%+v err=%v", alerts, err)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{"expected_slots=2", "complete_slots=1", "payload_active_slots=1", "incomplete_slots=1", "same stable process start"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("XL finding lacks %q", want)
		}
	}
	for _, private := range []string{"alpha.example.test", "beta.example.test", "/one"} {
		if strings.Contains(markdown, private) {
			t.Error("finding rendered a private process identity")
		}
	}
}
