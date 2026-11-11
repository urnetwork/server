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

// The configured URL is public catalog data, retained as the exact target of
// a security recheck. A finding whose destination leaves the served pool is
// retired by the server's TLS judge and no longer rechecked. No raw response
// is kept.
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
	// The HTTP/3 leg of a destination that declares it (Destination.H3),
	// present once the HTTPS leg above passed. The outcome is a success only
	// when both legs are; everything above is the HTTPS leg's. Absent for
	// every other destination, so a server that predates it sees the same
	// document it always did.
	H3 *UrlProbeH3Evidence `json:"h3,omitempty"`
	// The URL declares h3 and the provider's country disables UDP probes
	// (country_settings udp_probes: false): no HTTP/3 leg was made, so UDP is
	// not measured, never a failure or a pass, and the outcome is judged on
	// its HTTPS leg alone. With no leg, it adds no place evidence and gets no
	// place verdict (model.JudgeProviderEgressH3).
	UdpProbesDisabled bool `json:"udp_probes_disabled,omitempty"`
}

// The HTTP/3 (QUIC over UDP) leg of one URL outcome: the same request and
// contract as its HTTPS leg, through the same provider. It is bounded
// diagnostic data like the rest of the evidence: no body, no address. Its
// certificate outcome is recorded here and never as URL security evidence:
// TLS findings and their judge stay about the HTTPS identity.
//
// Udp is what the leg says about the provider's UDP path (UrlProbeH3Udp*):
// passed, answered (the site answered over QUIC and refused or failed the
// request: UDP works), silent (nothing came back: the only evidence against
// the provider), or not_measured. Verdict is the server's place judgement of
// a silent leg (model.JudgeProviderEgressH3): provider, where established
// networks in the same country reach the URL over QUIC, so the silence counts
// against the provider; or place, where they do not, so the outcome counts
// as its HTTPS leg. A prober never sets it.
type UrlProbeH3Evidence struct {
	Udp                       string  `json:"udp"`
	Verdict                   string  `json:"verdict,omitempty"`
	ContentClassification     string  `json:"content_classification"`
	PerformanceClassification string  `json:"performance_classification"`
	FailureStage              string  `json:"failure_stage"`
	NotMeasured               bool    `json:"not_measured"`
	TlsAuthenticated          bool    `json:"tls_authenticated"`
	TlsFailure                bool    `json:"tls_failure"`
	StatusCode                int     `json:"status_code"`
	RedirectCount             int     `json:"redirect_count"`
	ByteCount                 int64   `json:"byte_count"`
	WireByteCount             int64   `json:"wire_byte_count"`
	WireSampleByteCount       int64   `json:"wire_sample_byte_count"`
	BodyComplete              bool    `json:"body_complete"`
	BodySampled               bool    `json:"body_sampled"`
	RequestWritten            bool    `json:"request_written"`
	FirstByteReceived         bool    `json:"first_byte_received"`
	DnsMillis                 float64 `json:"dns_ms"`
	HandshakeMillis           float64 `json:"handshake_ms"`
	TtfbMillis                float64 `json:"ttfb_ms"`
	BodyMillis                float64 `json:"body_ms"`
	BodyFirstByteWaitMillis   float64 `json:"body_first_byte_wait_ms"`
	BodyBitsPerSecond         float64 `json:"body_bits_per_second"`
}

// The evidence of one HTTP/3 leg, or nil when the load made none.
func urlProbeH3EvidenceOf(check *CheckResult) *UrlProbeH3Evidence {
	if check == nil {
		return nil
	}
	millis := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return &UrlProbeH3Evidence{
		Udp:                   UrlProbeH3Udp(check),
		ContentClassification: check.ContentClassification, PerformanceClassification: check.PerformanceClassification,
		FailureStage: check.FailureStage, NotMeasured: check.NotMeasured,
		TlsAuthenticated: 0 < check.StatusCode && !check.TlsAuthenticationFailure, TlsFailure: check.TlsAuthenticationFailure,
		StatusCode: check.StatusCode, RedirectCount: check.RedirectCount,
		ByteCount: check.ByteCount, WireByteCount: check.WireByteCount, WireSampleByteCount: check.WireSampleByteCount,
		BodyComplete: check.BodyComplete, BodySampled: check.BodySampled,
		RequestWritten: check.RequestWritten, FirstByteReceived: check.FirstByteReceived,
		DnsMillis: millis(check.DnsLookupLatency), HandshakeMillis: millis(check.TlsHandshakeLatency),
		TtfbMillis: millis(check.RequestTimeToFirstByte), BodyMillis: millis(check.BodyDuration),
		BodyFirstByteWaitMillis: millis(check.BodyFirstByteWait), BodyBitsPerSecond: check.BodyBytesPerSecond * 8,
	}
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
	if self.ContentMatcherVersion != 1 && self.ContentMatcherVersion != 2 && self.ContentMatcherVersion != UrlProbeContentMatcherVersion {
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
	if leg := self.H3; leg != nil {
		if !self.Destination.H3 {
			return fmt.Errorf("URL evidence carries an HTTP/3 leg for a destination that declares none")
		}
		if leg.RedirectCount < 0 || leg.RedirectCount > self.Policy.MaxRedirects || leg.ByteCount < 0 || leg.ByteCount > int64(self.Policy.MaxBodyBytes) || leg.WireByteCount < 0 || leg.WireByteCount > int64(self.Policy.MaxBodyBytes) {
			return fmt.Errorf("URL HTTP/3 evidence exceeds its bounds")
		}
		if leg.WireSampleByteCount != max(0, leg.WireByteCount-1) {
			return fmt.Errorf("URL HTTP/3 wire sample must exclude the first byte that starts its clock")
		}
		for _, value := range []float64{leg.DnsMillis, leg.HandshakeMillis, leg.TtfbMillis, leg.BodyMillis, leg.BodyFirstByteWaitMillis, leg.BodyBitsPerSecond} {
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("URL HTTP/3 timing evidence must be finite and nonnegative")
			}
		}
		if len(leg.ContentClassification) > 64 || len(leg.PerformanceClassification) > 64 || len(leg.FailureStage) > 64 {
			return fmt.Errorf("URL HTTP/3 outcome classification is too long")
		}
		if leg.TlsAuthenticated && leg.TlsFailure {
			return fmt.Errorf("URL HTTP/3 evidence cannot both authenticate and fail TLS")
		}
		// the UDP classification follows from the leg's own record
		// (UrlProbeH3Udp), and only a silent leg can carry a place verdict
		switch leg.Udp {
		case UrlProbeH3UdpPassed:
			if leg.NotMeasured || leg.FailureStage != "" || leg.succeeded(self.Policy) != nil {
				return fmt.Errorf("URL HTTP/3 evidence passed without success evidence")
			}
		case UrlProbeH3UdpAnswered:
			if leg.NotMeasured || leg.FailureStage == "" || leg.FailureStage == FailureStageDialQuic {
				return fmt.Errorf("URL HTTP/3 evidence answered without a failure the site gave")
			}
		case UrlProbeH3UdpSilent:
			if leg.NotMeasured || leg.FailureStage != FailureStageDialQuic || leg.StatusCode != 0 || leg.FirstByteReceived || leg.TlsAuthenticated || leg.TlsFailure {
				return fmt.Errorf("URL HTTP/3 evidence is silent although the site answered")
			}
		case UrlProbeH3UdpNotMeasured:
			if !leg.NotMeasured && leg.FailureStage == FailureStageDialQuic {
				return fmt.Errorf("URL HTTP/3 evidence calls a silent leg unmeasured")
			}
		default:
			return fmt.Errorf("URL HTTP/3 evidence has no UDP classification")
		}
		switch leg.Verdict {
		case "":
		case UrlProbeH3VerdictProvider, UrlProbeH3VerdictPlace:
			if leg.Udp != UrlProbeH3UdpSilent {
				return fmt.Errorf("URL HTTP/3 place verdict on a leg that was not silent")
			}
		default:
			return fmt.Errorf("URL HTTP/3 verdict %q is unknown", leg.Verdict)
		}
	}
	if self.UdpProbesDisabled && (!self.Destination.H3 || self.H3 != nil) {
		return fmt.Errorf("URL evidence disables UDP probes for a destination without h3, or carries an HTTP/3 leg beside it")
	}
	return nil
}

// The server's place judgement of a silent HTTP/3 leg (UrlProbeH3Evidence).
const (
	UrlProbeH3VerdictProvider = "provider"
	UrlProbeH3VerdictPlace    = "place"
)

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
	https := func(failureStage string) urlProbeLegEvidence {
		return urlProbeLegEvidence{
			authenticated:         !tlsFailure && len(self.Security) == self.RedirectCount+1,
			contentClassification: self.ContentClassification, performanceClassification: self.PerformanceClassification,
			failureStage: failureStage, statusCode: self.StatusCode, byteCount: self.ByteCount, wireByteCount: self.WireByteCount,
			wireSampleByteCount: self.WireSampleByteCount, bodyComplete: self.BodyComplete, bodySampled: self.BodySampled,
			requestWritten: self.RequestWritten, firstByteReceived: self.FirstByteReceived,
			ttfbMillis: self.TtfbMillis, bodyMillis: self.BodyMillis, bodyBitsPerSecond: self.BodyBitsPerSecond,
		}
	}
	if leg := self.H3; leg != nil {
		// An HTTP/3 leg is made only after the HTTPS leg passed, so its
		// presence is itself a claim the HTTPS evidence must bear out, whatever
		// the outcome counts: a stored HTTP/3 leg always means the provider
		// carried the site over TCP. The outcome's stage names the leg's
		// verdict: none when the leg proved the provider's UDP path, h3 when
		// it was silent or not measured.
		if self.FailureStage != "" && self.FailureStage != FailureStageH3 {
			return fmt.Errorf("URL HTTP/3 evidence follows an HTTPS leg that failed")
		}
		if err := https("").succeeded(self.Policy, "URL"); err != nil {
			return fmt.Errorf("URL HTTP/3 evidence follows an HTTPS leg without success evidence: %w", err)
		}
		udpProven := leg.Udp == UrlProbeH3UdpPassed || leg.Udp == UrlProbeH3UdpAnswered
		if udpProven != (self.FailureStage == "") {
			return fmt.Errorf("URL failure stage disagrees with its HTTP/3 leg")
		}
	}
	if ok == 0 {
		return nil
	}
	leg := self.H3
	// a silent leg the server judged the place's counts as its HTTPS leg
	placeJudged := leg != nil && leg.Udp == UrlProbeH3UdpSilent && leg.Verdict == UrlProbeH3VerdictPlace
	stage := self.FailureStage
	if placeJudged {
		stage = ""
	}
	if err := https(stage).succeeded(self.Policy, "URL"); err != nil {
		return err
	}
	if !self.Destination.H3 || self.UdpProbesDisabled {
		// a URL without h3, or with UDP probes off in the provider's country,
		// is judged on its HTTPS leg alone
		return nil
	}
	// A destination that declares HTTP/3 passes only with the HTTPS leg above
	// and an HTTP/3 leg that proved the provider's UDP path, or one the
	// server judged silent for its place.
	if leg == nil || !(leg.Udp == UrlProbeH3UdpPassed || leg.Udp == UrlProbeH3UdpAnswered || placeJudged) {
		return fmt.Errorf("URL success of an HTTP/3 destination is missing an HTTP/3 leg that reached the site")
	}
	return nil
}

// Whether the HTTP/3 leg's final response meets the contract its HTTPS leg's
// does.
func (self *UrlProbeH3Evidence) succeeded(policy UrlProbePolicy) error {
	return (urlProbeLegEvidence{
		authenticated:         self.TlsAuthenticated && !self.TlsFailure,
		contentClassification: self.ContentClassification, performanceClassification: self.PerformanceClassification,
		failureStage: self.FailureStage, statusCode: self.StatusCode, byteCount: self.ByteCount, wireByteCount: self.WireByteCount,
		wireSampleByteCount: self.WireSampleByteCount, bodyComplete: self.BodyComplete, bodySampled: self.BodySampled,
		requestWritten: self.RequestWritten, firstByteReceived: self.FirstByteReceived,
		ttfbMillis: self.TtfbMillis, bodyMillis: self.BodyMillis, bodyBitsPerSecond: self.BodyBitsPerSecond,
	}).succeeded(policy, "URL HTTP/3")
}

// Whether the HTTPS leg of the outcome passed, which is what a site's own
// health is judged by: a success, or an outcome whose HTTP/3 leg was made,
// which needs a passed HTTPS leg (ValidateOutcome). Only meaningful for
// evidence that validated.
func (self *UrlProbeEvidence) HttpsOk(ok int) bool {
	return 0 < ok || (self != nil && self.H3 != nil)
}

// One leg's final response, as the success contract reads it.
type urlProbeLegEvidence struct {
	authenticated                                    bool
	contentClassification, performanceClassification string
	failureStage                                     string
	statusCode                                       int
	byteCount, wireByteCount, wireSampleByteCount    int64
	bodyComplete, bodySampled                        bool
	requestWritten, firstByteReceived                bool
	ttfbMillis, bodyMillis, bodyBitsPerSecond        float64
}

// The contract every leg's success meets, HTTPS or HTTP/3: authenticated real
// content, the final request's TTFB under the policy, and either a confirmed
// complete small body or a measured wire throughput over the policy's bar.
func (self urlProbeLegEvidence) succeeded(policy UrlProbePolicy, noun string) error {
	if !self.authenticated || self.contentClassification != "content" || self.failureStage != "" ||
		self.statusCode < 200 || self.statusCode >= 300 || self.byteCount == 0 || self.wireByteCount == 0 ||
		!self.requestWritten || !self.firstByteReceived {
		return fmt.Errorf("%s success is missing authenticated real-content or final-request timing evidence", noun)
	}
	if self.ttfbMillis > float64(policy.MaxTtfbMillis) {
		return fmt.Errorf("%s success exceeded its final-request TTFB policy", noun)
	}
	if self.wireSampleByteCount < int64(policy.MinThroughputBytes) {
		if !self.bodyComplete || self.statusCode == 206 || self.performanceClassification != "insufficient_sample" {
			return fmt.Errorf("a small %s success requires a confirmed complete body and insufficient-sample classification", noun)
		}
		return nil
	}
	if self.bodyMillis <= 0 {
		return fmt.Errorf("%s success is missing its body transfer clock", noun)
	}
	if !self.bodyComplete && !self.bodySampled {
		return fmt.Errorf("%s success is neither complete nor an intentional bounded sample", noun)
	}
	bitsPerSecond := float64(self.wireSampleByteCount) * 8 * 1000 / self.bodyMillis
	if self.performanceClassification != "passed" || float64(self.wireSampleByteCount)*8*1000 < float64(policy.MinThroughputBps)*self.bodyMillis || math.Abs(bitsPerSecond-self.bodyBitsPerSecond) > max(1, bitsPerSecond)*1e-6 {
		return fmt.Errorf("%s success did not meet its measured wire-throughput policy", noun)
	}
	return nil
}
