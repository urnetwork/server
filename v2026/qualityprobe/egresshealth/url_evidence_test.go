package egresshealth

import (
	"testing"
	"time"
)

func urlEvidenceTestSuccess() UrlProbeEvidence {
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	destination := Destination{Name: "synthetic-document", Class: ClassSite, Url: "https://content.example/"}
	return UrlProbeEvidence{
		PolicyVersion: UrlProbePolicyVersion, Policy: DefaultUrlProbePolicy(), Destination: destination, MeasuredAt: now,
		Security:              []UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: now, TlsAuthenticated: true}},
		ContentMatcherVersion: 1, ContentClassification: "content", PerformanceClassification: "insufficient_sample",
		StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true, RequestWritten: true, FirstByteReceived: true,
		TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
	}
}

// Versions are exact contracts; version zero and a future version never inherit
// today's meaning by omission or a greater-than comparison.
func TestUrlProbeEvidenceRequiresSupportedVersionAndRawSuccess(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*UrlProbeEvidence)
		pass bool
	}{
		{"version one", func(*UrlProbeEvidence) {}, true},
		{"legacy zero", func(e *UrlProbeEvidence) { e.PolicyVersion = 0; e.Policy.Version = 0 }, false},
		{"future two", func(e *UrlProbeEvidence) { e.PolicyVersion = 2; e.Policy.Version = 2 }, false},
		{"mismatched policy", func(e *UrlProbeEvidence) { e.Policy.Version = 2 }, false},
		{"no write clock", func(e *UrlProbeEvidence) { e.RequestWritten = false }, false},
		{"no first byte", func(e *UrlProbeEvidence) { e.FirstByteReceived = false }, false},
		{"empty decoded body", func(e *UrlProbeEvidence) { e.ByteCount = 0 }, false},
		{"empty wire body", func(e *UrlProbeEvidence) { e.WireByteCount = 0 }, false},
		{"partial tiny body", func(e *UrlProbeEvidence) { e.BodyComplete = false }, false},
		{"partial response", func(e *UrlProbeEvidence) { e.StatusCode = 206 }, false},
		{"missing TLS authentication", func(e *UrlProbeEvidence) { e.Security = nil }, false},
		{"ambiguous TLS evidence", func(e *UrlProbeEvidence) { e.Security[0].TlsAuthenticated = false }, false},
		{"challenge content", func(e *UrlProbeEvidence) { e.ContentClassification = "captcha" }, false},
		{"bodyless legacy status", func(e *UrlProbeEvidence) { e.StatusCode = 204; e.ByteCount = 0 }, false},
		{"slow final TTFB", func(e *UrlProbeEvidence) { e.TtfbMillis = 2000.001 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := urlEvidenceTestSuccess()
			test.edit(&evidence)
			if err := evidence.ValidateOutcome(1, 1, false); (err == nil) != test.pass {
				t.Fatalf("accepted=%t want=%t: %v", err == nil, test.pass, err)
			}
		})
	}
}

// Larger samples must satisfy the wire count/clock equation and exact limits.
func TestUrlProbeEvidenceRejectsInflatedThroughput(t *testing.T) {
	evidence := urlEvidenceTestSuccess()
	evidence.ByteCount, evidence.WireByteCount, evidence.WireSampleByteCount = 25001, 25001, 25000
	evidence.BodyMillis, evidence.BodyBitsPerSecond = 2000, 100000
	evidence.TtfbMillis, evidence.PerformanceClassification = 2000, "passed"
	if err := evidence.ValidateOutcome(1, 1, false); err != nil {
		t.Fatalf("inclusive threshold rejected: %v", err)
	}
	evidence.BodyBitsPerSecond = 100001
	if evidence.ValidateOutcome(1, 1, false) == nil {
		t.Fatal("a claimed bandwidth replaced the raw wire byte/clock calculation")
	}
	evidence.WireByteCount, evidence.WireSampleByteCount, evidence.BodyBitsPerSecond = 20001, 20000, 100000
	if evidence.ValidateOutcome(1, 1, false) == nil {
		t.Fatal("decoded body size inflated physical transfer speed")
	}
}

// Authenticated security recovery remains valid even when the URL is an error.
func TestUrlProbeEvidenceRecoveryDoesNotRequireQualitySuccess(t *testing.T) {
	evidence := urlEvidenceTestSuccess()
	evidence.ContentClassification, evidence.FailureStage = "captcha", "response_content"
	if err := evidence.ValidateOutcome(0, 1, false); err != nil {
		t.Fatal(err)
	}
	evidence.Security[0].TlsFailure = true
	evidence.Security[0].TlsAuthenticated = false
	if err := evidence.ValidateOutcome(0, 1, true); err != nil {
		t.Fatal(err)
	}
	if evidence.ValidateOutcome(1, 1, true) == nil {
		t.Fatal("TLS failure became a URL success")
	}
}
