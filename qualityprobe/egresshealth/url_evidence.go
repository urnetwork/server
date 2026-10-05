// URL diagnostics contain bounded outcome data, never response bodies or exit IPs.
package egresshealth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// A verified HTTPS response (including ordinary content errors) authenticates
// this exact URL. A certificate failure is separate and wins timestamp ties.
type UrlProbeSecurityEvent struct {
	Destination      Destination `json:"destination"`
	MeasuredAt       time.Time   `json:"measured_at"`
	TlsFailure       bool        `json:"tls_failure"`
	TlsAuthenticated bool        `json:"tls_authenticated"`
}

// The configured URL is public catalog data, retained so its exact security
// exception remains recheckable after a catalog edit. No raw response is kept.
type UrlProbeEvidence struct {
	PolicyVersion             int                     `json:"policy_version"`
	Policy                    UrlProbePolicy          `json:"policy"`
	Destination               Destination             `json:"destination"`
	MeasuredAt                time.Time               `json:"measured_at"`
	Security                  []UrlProbeSecurityEvent `json:"security"`
	ContentClassification     string                  `json:"content_classification"`
	PerformanceClassification string                  `json:"performance_classification"`
	FailureStage              string                  `json:"failure_stage"`
	ContentMatcherVersion     int                     `json:"content_matcher_version"`
	StatusCode                int                     `json:"status_code"`
	RedirectCount             int                     `json:"redirect_count"`
	ByteCount                 int64                   `json:"byte_count"`
	WireByteCount             int64                   `json:"wire_byte_count"`
	WireSampleByteCount       int64                   `json:"wire_sample_byte_count"`
	BodyComplete              bool                    `json:"body_complete"`
	BodySampled               bool                    `json:"body_sampled"`
	RequestWritten            bool                    `json:"request_written"`
	FirstByteReceived         bool                    `json:"first_byte_received"`
	DnsMillis                 float64                 `json:"dns_ms"`
	TcpMillis                 float64                 `json:"tcp_ms"`
	TlsMillis                 float64                 `json:"tls_ms"`
	TtfbMillis                float64                 `json:"ttfb_ms"`
	BodyMillis                float64                 `json:"body_ms"`
	BodyFirstByteWaitMillis   float64                 `json:"body_first_byte_wait_ms"`
	BodyBitsPerSecond         float64                 `json:"body_bits_per_second"`
}

// Normalization changes only origin spelling and the irrelevant fragment;
// path/query are not reordered because their precise meaning belongs to the site.
func UrlProbeDestinationKey(destination Destination) string {
	target, err := url.Parse(destination.Url)
	if err != nil || validateUrlProbeTarget(target) != nil {
		return ""
	}
	target.Scheme = "https"
	target.Host = strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if strings.Contains(target.Host, ":") {
		target.Host = "[" + target.Host + "]"
	}
	target.Fragment = ""
	if target.Path == "" {
		target.Path = "/"
	}
	key := sha256.Sum256([]byte(target.String()))
	return hex.EncodeToString(key[:])
}

func (self *UrlProbeEvidence) Validate() error {
	if self == nil {
		return nil
	}
	if self.PolicyVersion != UrlProbePolicyVersion || self.PolicyVersion != self.Policy.Version {
		return fmt.Errorf("URL result policy version %d is unsupported or inconsistent", self.PolicyVersion)
	}
	if err := self.Policy.Validate(); err != nil {
		return err
	}
	if self.ContentMatcherVersion != 1 && self.ContentMatcherVersion != UrlProbeContentMatcherVersion {
		return fmt.Errorf("URL content matcher version %d is unsupported", self.ContentMatcherVersion)
	}
	if UrlProbeDestinationKey(self.Destination) == "" || len(self.Destination.Name) == 0 || len(self.Destination.Name) > 256 || len(self.Destination.Url) > 4096 || self.MeasuredAt.IsZero() || len(self.Security) > 6 {
		return fmt.Errorf("invalid URL evidence identity, timestamp, or redirect count")
	}
	if self.RedirectCount < 0 || self.RedirectCount > self.Policy.MaxRedirects || self.ByteCount < 0 || self.ByteCount > int64(self.Policy.MaxBodyBytes) || self.WireByteCount < 0 || self.WireByteCount > int64(self.Policy.MaxBodyBytes) {
		return fmt.Errorf("URL evidence exceeds its bounds")
	}
	if self.WireSampleByteCount != max(0, self.WireByteCount-1) {
		return fmt.Errorf("URL wire sample must exclude the first byte that starts its clock")
	}
	for _, value := range []float64{self.DnsMillis, self.TcpMillis, self.TlsMillis, self.TtfbMillis, self.BodyMillis, self.BodyFirstByteWaitMillis, self.BodyBitsPerSecond} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("URL timing evidence must be finite and nonnegative")
		}
	}
	if len(self.ContentClassification) > 64 || len(self.PerformanceClassification) > 64 || len(self.FailureStage) > 64 {
		return fmt.Errorf("URL outcome classification is too long")
	}
	for _, event := range self.Security {
		if UrlProbeDestinationKey(event.Destination) == "" || len(event.Destination.Name) == 0 || len(event.Destination.Name) > 256 || len(event.Destination.Url) > 4096 || event.MeasuredAt.IsZero() || event.MeasuredAt.After(self.MeasuredAt) {
			return fmt.Errorf("invalid per-URL security evidence")
		}
		if event.TlsFailure == event.TlsAuthenticated {
			return fmt.Errorf("URL security evidence requires exactly one authentication outcome")
		}
	}
	return nil
}

// The operator report must carry the actual versioned success evidence. Legacy
// nil evidence can be stored for audit but never becomes a URL-policy receipt.
func (self *UrlProbeEvidence) ValidateOutcome(ok, total int, tlsFailure bool) error {
	if err := self.Validate(); err != nil || self == nil {
		return err
	}
	if total < 0 || total > 1 || ok < 0 || ok > total {
		return fmt.Errorf("URL evidence must describe at most one measured outcome")
	}
	hasTlsFailure := false
	for _, event := range self.Security {
		hasTlsFailure = hasTlsFailure || event.TlsFailure
	}
	if hasTlsFailure != tlsFailure {
		return fmt.Errorf("URL security evidence disagrees with its aggregate TLS flag")
	}
	if ok == 0 {
		return nil
	}
	if tlsFailure || self.ContentClassification != "content" || self.FailureStage != "" ||
		self.StatusCode < 200 || self.StatusCode >= 300 || self.ByteCount == 0 || self.WireByteCount == 0 ||
		!self.RequestWritten || !self.FirstByteReceived || len(self.Security) != self.RedirectCount+1 {
		return fmt.Errorf("URL success is missing authenticated real-content or final-request timing evidence")
	}
	if self.TtfbMillis > float64(self.Policy.MaxTtfbMillis) {
		return fmt.Errorf("URL success exceeded its final-request TTFB policy")
	}
	if self.WireSampleByteCount < int64(self.Policy.MinThroughputBytes) {
		if !self.BodyComplete || self.StatusCode == 206 || self.PerformanceClassification != "insufficient_sample" {
			return fmt.Errorf("a small URL success requires a confirmed complete body and insufficient-sample classification")
		}
		return nil
	}
	if self.BodyMillis <= 0 {
		return fmt.Errorf("URL success is missing its body transfer clock")
	}
	if !self.BodyComplete && !self.BodySampled {
		return fmt.Errorf("URL success is neither complete nor an intentional bounded sample")
	}
	bitsPerSecond := float64(self.WireSampleByteCount) * 8 * 1000 / self.BodyMillis
	if self.PerformanceClassification != "passed" || float64(self.WireSampleByteCount)*8*1000 < float64(self.Policy.MinThroughputBps)*self.BodyMillis || math.Abs(bitsPerSecond-self.BodyBitsPerSecond) > max(1, bitsPerSecond)*1e-6 {
		return fmt.Errorf("URL success did not meet its measured wire-throughput policy")
	}
	return nil
}
