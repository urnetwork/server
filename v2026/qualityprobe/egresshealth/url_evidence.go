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
	// The IPv6 legs of a load whose provider advertises IPv6 and whose turn
	// has a URL that serves it (Destination.Ipv6, or the stand-in in
	// Ipv6.Destination), present once the IPv4 HTTPS leg above passed: the
	// HTTPS leg over that URL's AAAA record, and its HTTP/3 leg. Everything
	// above, the HTTP/3 leg in H3 included, is IPv4's. Absent for every other
	// load.
	Ipv6 *UrlProbeIpv6Evidence `json:"ipv6,omitempty"`
	// The families the provider advertised when it was claimed, when they
	// include IPv6 (dualstack); absent for a provider that advertises IPv4
	// alone, as every outcome before families were probed reads.
	IpFamily IpFamily `json:"ip_family,omitempty"`
	// A URL of the outcome declares h3, its own or the stand-in its IPv6 legs
	// loaded, and the provider's country disables UDP probes (country_settings
	// udp_probes: false): no HTTP/3 leg was made in any family, and the
	// outcome is judged on its HTTPS legs alone.
	UdpProbesDisabled bool `json:"udp_probes_disabled,omitempty"`
	// The end-to-end session a post-quantum turn opened with the provider
	// (url_e2e.go): its post-quantum outcome, group and window reading.
	// Absent for a turn that opened none, so a server that predates it sees
	// the same document it always did.
	E2e *UrlProbeE2eEvidence `json:"e2e,omitempty"`
}

// The bits of FailedLaterLegs: each is a later leg of a failed outcome that
// the server's place judgement held against the provider (FP2-PROTOCOL.md
// §3.2, §4.1). They are stored values of failed_later_legs.
const (
	// the IPv4 HTTP/3 leg was silent, judged the provider's
	UrlProbeFailedLaterLegIpv4Udp int16 = 1
	// the IPv6 HTTPS leg failed, judged the provider's
	UrlProbeFailedLaterLegIpv6Tcp int16 = 2
	// the IPv6 HTTP/3 leg was silent after a passed IPv6 HTTPS leg, judged
	// the provider's
	UrlProbeFailedLaterLegIpv6Udp int16 = 4
)

// The later legs of a failed measured outcome, of ok successes in total
// loads, that the server judged the provider's (UrlProbeFailedLaterLeg*), or 0.
// Later legs are made only after the IPv4 HTTPS leg passed (ValidateOutcome),
// so a non-zero mask names an outcome that failed only on them; and the judge
// stores an outcome as a pass when every failed later leg was the place's, so
// a stored failure with a non-zero mask has one leg judged the provider's.
// A leg the judge did not rule on (no verdict) is no evidence and sets no bit:
// its outcome counts as a plain failure, the safe direction. The masks that
// occur are 1, 2, 4, 1|2 and 1|4: an IPv6 HTTP/3 leg follows only a passed
// IPv6 HTTPS leg, so 2|4 cannot.
func (self *UrlProbeEvidence) FailedLaterLegs(ok int, total int) int16 {
	if self == nil || total != 1 || ok != 0 {
		return 0
	}
	mask := int16(0)
	if leg := self.H3; leg != nil && leg.Udp == UrlProbeH3UdpSilent && leg.Verdict == UrlProbeH3VerdictProvider {
		mask |= UrlProbeFailedLaterLegIpv4Udp
	}
	if leg := self.Ipv6; leg != nil {
		switch leg.Tcp {
		case UrlProbeIpv6TcpFailed:
			if leg.Verdict == UrlProbeH3VerdictProvider {
				mask |= UrlProbeFailedLaterLegIpv6Tcp
			}
		case UrlProbeIpv6TcpPassed:
			if leg.H3 != nil && leg.H3.Udp == UrlProbeH3UdpSilent && leg.H3.Verdict == UrlProbeH3VerdictProvider {
				mask |= UrlProbeFailedLaterLegIpv6Udp
			}
		}
	}
	return mask
}

// The URL the outcome's IPv6 legs loaded: their stand-in when they loaded one
// (UrlProbeIpv6Evidence.Destination), else the outcome's own. The IPv6 place
// evidence and verdicts are that URL's.
func (self *UrlProbeEvidence) Ipv6Destination() Destination {
	if self.Ipv6 != nil && self.Ipv6.Destination != nil {
		return *self.Ipv6.Destination
	}
	return self.Destination
}

// The IPv6 legs of one URL outcome (url_family.go): the same request and
// contract as the IPv4 legs, through the same provider, forced to the URL's
// AAAA record with no fallback to IPv4. Bounded diagnostic data like the rest:
// no body, no address. Its TLS outcome stays here and is never URL security
// evidence: TLS findings and their judge stay about the IPv4 HTTPS identity.
//
// Tcp is what the HTTPS leg says about the provider's IPv6 path
// (UrlProbeIpv6Tcp*): passed, failed (the only evidence against the provider)
// or not_measured. Verdict is the server's place judgement of a failed leg
// (model.JudgeProviderEgressLegs): provider, where established networks in the
// same country reach the URL over IPv6, so the failure counts; place, where
// they do not, so the outcome counts as its IPv4 legs. H3 is the HTTP/3 leg
// over IPv6, judged the same way when silent against networks that reach the
// URL over QUIC over IPv6. A prober never sets a verdict.
type UrlProbeIpv6Evidence struct {
	// The URL the legs loaded when it is not the outcome's own: a stand-in the
	// turn drew from the URLs of its lane that serve IPv6, because the
	// outcome's URL serves none (url_family.go). Absent when the legs loaded
	// the outcome's URL.
	Destination *Destination `json:"destination,omitempty"`
	Tcp         string       `json:"tcp"`
	Verdict     string       `json:"verdict,omitempty"`
	// The leg was not measured because its URL, or a redirect hop of it, had
	// no AAAA record through the provider's resolver: the URL's outcome, not
	// the provider's (urlProbeAddressRecordMissing).
	NoAaaa                    bool                `json:"no_aaaa,omitempty"`
	ContentClassification     string              `json:"content_classification"`
	PerformanceClassification string              `json:"performance_classification"`
	FailureStage              string              `json:"failure_stage"`
	NotMeasured               bool                `json:"not_measured"`
	TlsAuthenticated          bool                `json:"tls_authenticated"`
	TlsFailure                bool                `json:"tls_failure"`
	StatusCode                int                 `json:"status_code"`
	RedirectCount             int                 `json:"redirect_count"`
	ByteCount                 int64               `json:"byte_count"`
	WireByteCount             int64               `json:"wire_byte_count"`
	WireSampleByteCount       int64               `json:"wire_sample_byte_count"`
	BodyComplete              bool                `json:"body_complete"`
	BodySampled               bool                `json:"body_sampled"`
	RequestWritten            bool                `json:"request_written"`
	FirstByteReceived         bool                `json:"first_byte_received"`
	DnsMillis                 float64             `json:"dns_ms"`
	TcpMillis                 float64             `json:"tcp_ms"`
	TlsMillis                 float64             `json:"tls_ms"`
	TtfbMillis                float64             `json:"ttfb_ms"`
	BodyMillis                float64             `json:"body_ms"`
	BodyFirstByteWaitMillis   float64             `json:"body_first_byte_wait_ms"`
	BodyBitsPerSecond         float64             `json:"body_bits_per_second"`
	H3                        *UrlProbeH3Evidence `json:"h3,omitempty"`
}

// The evidence of a load's IPv6 legs, or nil when it made none.
func urlProbeIpv6EvidenceOf(check *CheckResult) *UrlProbeIpv6Evidence {
	if check == nil {
		return nil
	}
	millis := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	// a passed HTTPS leg whose HTTP/3 leg went silent carries the HTTP/3
	// failure as its own; the HTTPS leg itself met the contract
	stage := check.FailureStage
	if check.H3 != nil && stage == FailureStageH3 {
		stage = ""
	}
	return &UrlProbeIpv6Evidence{
		Tcp:                   UrlProbeIpv6Tcp(check),
		NoAaaa:                check.AddressRecordMissing && UrlProbeIpv6Tcp(check) == UrlProbeIpv6TcpNotMeasured,
		ContentClassification: check.ContentClassification, PerformanceClassification: check.PerformanceClassification,
		FailureStage: stage, NotMeasured: check.NotMeasured,
		TlsAuthenticated: 0 < check.StatusCode && !check.TlsAuthenticationFailure, TlsFailure: check.TlsAuthenticationFailure,
		StatusCode: check.StatusCode, RedirectCount: check.RedirectCount,
		ByteCount: check.ByteCount, WireByteCount: check.WireByteCount, WireSampleByteCount: check.WireSampleByteCount,
		BodyComplete: check.BodyComplete, BodySampled: check.BodySampled,
		RequestWritten: check.RequestWritten, FirstByteReceived: check.FirstByteReceived,
		DnsMillis: millis(check.DnsLookupLatency), TcpMillis: millis(check.TcpConnectLatency), TlsMillis: millis(check.TlsHandshakeLatency),
		TtfbMillis: millis(check.RequestTimeToFirstByte), BodyMillis: millis(check.BodyDuration),
		BodyFirstByteWaitMillis: millis(check.BodyFirstByteWait), BodyBitsPerSecond: check.BodyBytesPerSecond * 8,
		H3: urlProbeH3EvidenceOf(check.H3),
	}
}

// Whether the IPv6 HTTPS leg's final response meets the contract.
func (self *UrlProbeIpv6Evidence) succeeded(policy UrlProbePolicy) error {
	return (urlProbeLegEvidence{
		authenticated:         self.TlsAuthenticated && !self.TlsFailure,
		contentClassification: self.ContentClassification, performanceClassification: self.PerformanceClassification,
		failureStage: self.FailureStage, statusCode: self.StatusCode, byteCount: self.ByteCount, wireByteCount: self.WireByteCount,
		wireSampleByteCount: self.WireSampleByteCount, bodyComplete: self.BodyComplete, bodySampled: self.BodySampled,
		requestWritten: self.RequestWritten, firstByteReceived: self.FirstByteReceived,
		ttfbMillis: self.TtfbMillis, bodyMillis: self.BodyMillis, bodyBitsPerSecond: self.BodyBitsPerSecond,
	}).succeeded(policy, "URL IPv6")
}

// Whether the IPv6 legs fail their outcome before the server's judgement: the
// HTTPS leg failed, or it passed and its HTTP/3 leg was silent.
func (self *UrlProbeIpv6Evidence) failed() bool {
	switch self.Tcp {
	case UrlProbeIpv6TcpFailed:
		return true
	case UrlProbeIpv6TcpPassed:
		return self.H3 != nil && self.H3.Udp == UrlProbeH3UdpSilent
	}
	return false
}

// Whether the IPv6 legs' failure, if any, was judged the place's: the failed
// leg carries the place verdict.
func (self *UrlProbeIpv6Evidence) placeJudged() bool {
	switch self.Tcp {
	case UrlProbeIpv6TcpFailed:
		return self.Verdict == UrlProbeH3VerdictPlace
	case UrlProbeIpv6TcpPassed:
		return self.H3 != nil && self.H3.Udp == UrlProbeH3UdpSilent && self.H3.Verdict == UrlProbeH3VerdictPlace
	}
	return false
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
		if err := leg.validate(self.Policy, "URL HTTP/3"); err != nil {
			return err
		}
	}
	if !self.Destination.ServesIpv4() {
		return fmt.Errorf("URL evidence's destination serves no IPv4; it can only stand in for an IPv6 leg")
	}
	ipv6Destination := self.Ipv6Destination()
	if leg := self.Ipv6; leg != nil {
		if standIn := leg.Destination; standIn != nil {
			if UrlProbeDestinationKey(*standIn) == "" || len(standIn.Name) == 0 || len(standIn.Name) > 256 || len(standIn.Url) > 4096 || standIn.Name == self.Destination.Name {
				return fmt.Errorf("invalid URL IPv6 stand-in identity")
			}
			if self.Destination.ServesIpv6() {
				return fmt.Errorf("URL evidence carries an IPv6 stand-in for a destination that serves IPv6 itself")
			}
		}
		if !ipv6Destination.ServesIpv6() {
			return fmt.Errorf("URL evidence carries an IPv6 leg for a destination that declares none")
		}
		if err := leg.validate(self.Policy); err != nil {
			return err
		}
		if leg.H3 != nil && !ipv6Destination.H3 {
			return fmt.Errorf("URL evidence carries an IPv6 HTTP/3 leg for a destination that declares none")
		}
	}
	if self.UdpProbesDisabled && (!(self.Destination.H3 || ipv6Destination.H3) || self.H3 != nil || (self.Ipv6 != nil && self.Ipv6.H3 != nil)) {
		return fmt.Errorf("URL evidence disables UDP probes for a destination without h3, or carries an HTTP/3 leg beside it")
	}
	if self.IpFamily != "" && (self.IpFamily.Normalize() != self.IpFamily || !self.IpFamily.Ipv6() || !self.IpFamily.Ipv4()) {
		return fmt.Errorf("URL evidence names an IP family %q a load cannot have", self.IpFamily)
	}
	if self.Ipv6 != nil && !self.IpFamily.Ipv6() {
		return fmt.Errorf("URL evidence carries an IPv6 leg for a provider that advertises no IPv6")
	}
	if self.E2e != nil {
		if err := self.E2e.validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validates one HTTP/3 leg's record, in either family.
func (self *UrlProbeH3Evidence) validate(policy UrlProbePolicy, noun string) error {
	leg := self
	if leg.RedirectCount < 0 || leg.RedirectCount > policy.MaxRedirects || leg.ByteCount < 0 || leg.ByteCount > int64(policy.MaxBodyBytes) || leg.WireByteCount < 0 || leg.WireByteCount > int64(policy.MaxBodyBytes) {
		return fmt.Errorf("%s evidence exceeds its bounds", noun)
	}
	if leg.WireSampleByteCount != max(0, leg.WireByteCount-1) {
		return fmt.Errorf("%s wire sample must exclude the first byte that starts its clock", noun)
	}
	for _, value := range []float64{leg.DnsMillis, leg.HandshakeMillis, leg.TtfbMillis, leg.BodyMillis, leg.BodyFirstByteWaitMillis, leg.BodyBitsPerSecond} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s timing evidence must be finite and nonnegative", noun)
		}
	}
	if len(leg.ContentClassification) > 64 || len(leg.PerformanceClassification) > 64 || len(leg.FailureStage) > 64 {
		return fmt.Errorf("%s outcome classification is too long", noun)
	}
	if leg.TlsAuthenticated && leg.TlsFailure {
		return fmt.Errorf("%s evidence cannot both authenticate and fail TLS", noun)
	}
	// the UDP classification follows from the leg's own record
	// (UrlProbeH3Udp), and only a silent leg can carry a place verdict
	switch leg.Udp {
	case UrlProbeH3UdpPassed:
		if leg.NotMeasured || leg.FailureStage != "" || leg.succeeded(policy) != nil {
			return fmt.Errorf("%s evidence passed without success evidence", noun)
		}
	case UrlProbeH3UdpAnswered:
		if leg.NotMeasured || leg.FailureStage == "" || leg.FailureStage == FailureStageDialQuic {
			return fmt.Errorf("%s evidence answered without a failure the site gave", noun)
		}
	case UrlProbeH3UdpSilent:
		if leg.NotMeasured || leg.FailureStage != FailureStageDialQuic || leg.StatusCode != 0 || leg.FirstByteReceived || leg.TlsAuthenticated || leg.TlsFailure {
			return fmt.Errorf("%s evidence is silent although the site answered", noun)
		}
	case UrlProbeH3UdpNotMeasured:
		if !leg.NotMeasured && leg.FailureStage == FailureStageDialQuic {
			return fmt.Errorf("%s evidence calls a silent leg unmeasured", noun)
		}
	default:
		return fmt.Errorf("%s evidence has no UDP classification", noun)
	}
	switch leg.Verdict {
	case "":
	case UrlProbeH3VerdictProvider, UrlProbeH3VerdictPlace:
		if leg.Udp != UrlProbeH3UdpSilent {
			return fmt.Errorf("%s place verdict on a leg that was not silent", noun)
		}
	default:
		return fmt.Errorf("%s verdict %q is unknown", noun, leg.Verdict)
	}
	return nil
}

// Validates the IPv6 legs' record: bounds, a TCP classification that
// follows from the leg's own record (UrlProbeIpv6Tcp), an HTTP/3 leg only
// after a passed HTTPS leg, and a verdict only on a failed HTTPS leg.
func (self *UrlProbeIpv6Evidence) validate(policy UrlProbePolicy) error {
	leg := self
	if leg.RedirectCount < 0 || leg.RedirectCount > policy.MaxRedirects || leg.ByteCount < 0 || leg.ByteCount > int64(policy.MaxBodyBytes) || leg.WireByteCount < 0 || leg.WireByteCount > int64(policy.MaxBodyBytes) {
		return fmt.Errorf("URL IPv6 evidence exceeds its bounds")
	}
	if leg.WireSampleByteCount != max(0, leg.WireByteCount-1) {
		return fmt.Errorf("URL IPv6 wire sample must exclude the first byte that starts its clock")
	}
	for _, value := range []float64{leg.DnsMillis, leg.TcpMillis, leg.TlsMillis, leg.TtfbMillis, leg.BodyMillis, leg.BodyFirstByteWaitMillis, leg.BodyBitsPerSecond} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("URL IPv6 timing evidence must be finite and nonnegative")
		}
	}
	if len(leg.ContentClassification) > 64 || len(leg.PerformanceClassification) > 64 || len(leg.FailureStage) > 64 {
		return fmt.Errorf("URL IPv6 outcome classification is too long")
	}
	if leg.TlsAuthenticated && leg.TlsFailure {
		return fmt.Errorf("URL IPv6 evidence cannot both authenticate and fail TLS")
	}
	switch leg.Tcp {
	case UrlProbeIpv6TcpPassed:
		if leg.NotMeasured || leg.FailureStage != "" || leg.succeeded(policy) != nil {
			return fmt.Errorf("URL IPv6 evidence passed without success evidence")
		}
	case UrlProbeIpv6TcpFailed:
		if leg.NotMeasured || leg.FailureStage == "" {
			return fmt.Errorf("URL IPv6 evidence failed without a measured failure")
		}
	case UrlProbeIpv6TcpNotMeasured:
	default:
		return fmt.Errorf("URL IPv6 evidence has no TCP classification")
	}
	if leg.NoAaaa && (leg.Tcp != UrlProbeIpv6TcpNotMeasured || leg.FailureStage != "dial_dns") {
		return fmt.Errorf("URL IPv6 evidence says the URL had no AAAA record although the leg resolved it")
	}
	if leg.H3 != nil {
		if leg.Tcp != UrlProbeIpv6TcpPassed {
			return fmt.Errorf("URL IPv6 HTTP/3 evidence follows an IPv6 HTTPS leg that did not pass")
		}
		if err := leg.H3.validate(policy, "URL IPv6 HTTP/3"); err != nil {
			return err
		}
	}
	switch leg.Verdict {
	case "":
	case UrlProbeH3VerdictProvider, UrlProbeH3VerdictPlace:
		if leg.Tcp != UrlProbeIpv6TcpFailed {
			return fmt.Errorf("URL IPv6 place verdict on a leg that did not fail")
		}
	default:
		return fmt.Errorf("URL IPv6 verdict %q is unknown", leg.Verdict)
	}
	return nil
}

// Whether any leg of the outcome carries a place verdict, which only the
// server's judgement sets (model.JudgeProviderEgressLegs).
func (self *UrlProbeEvidence) HasLegVerdict() bool {
	if self == nil {
		return false
	}
	if self.H3 != nil && self.H3.Verdict != "" {
		return true
	}
	if leg := self.Ipv6; leg != nil && (leg.Verdict != "" || (leg.H3 != nil && leg.H3.Verdict != "")) {
		return true
	}
	return false
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
	// a positive post-quantum outcome is one whose IPv4 HTTPS leg passed
	// through the sealed session, so it needs that leg's measured pass
	if self.E2e != nil && self.E2e.Pq == UrlProbePqPositive && (total != 1 || !self.HttpsOk(ok)) {
		return fmt.Errorf("URL end-to-end evidence is positive without a passed IPv4 HTTPS leg")
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
	leg := self.H3
	if leg != nil || self.Ipv6 != nil {
		// An HTTP/3 leg, and an IPv6 leg, are made only after the IPv4 HTTPS
		// leg passed, so their presence is itself a claim the HTTPS evidence
		// must bear out, whatever the outcome counts: a stored later leg
		// always means the provider carried the site over TCP and IPv4. The
		// outcome's stage names its first failed leg: none when every later
		// leg proved its path or was not measured in IPv6, h3 when the IPv4
		// HTTP/3 leg was silent or not measured, and ipv6 when the IPv4 legs
		// stood and an IPv6 leg failed.
		if self.FailureStage != "" && self.FailureStage != FailureStageH3 && self.FailureStage != FailureStageIpv6 {
			return fmt.Errorf("URL HTTP/3 or IPv6 evidence follows an HTTPS leg that failed")
		}
		if err := https("").succeeded(self.Policy, "URL"); err != nil {
			return fmt.Errorf("URL HTTP/3 or IPv6 evidence follows an HTTPS leg without success evidence: %w", err)
		}
		udpProven := leg == nil || leg.Udp == UrlProbeH3UdpPassed || leg.Udp == UrlProbeH3UdpAnswered
		ipv6Stands := self.Ipv6 == nil || !self.Ipv6.failed()
		switch {
		case !udpProven:
			if self.FailureStage != FailureStageH3 {
				return fmt.Errorf("URL failure stage disagrees with its HTTP/3 leg")
			}
		case !ipv6Stands:
			if self.FailureStage != FailureStageIpv6 {
				return fmt.Errorf("URL failure stage disagrees with its IPv6 leg")
			}
		case self.FailureStage != "":
			return fmt.Errorf("URL failure stage disagrees with its HTTP/3 and IPv6 legs")
		}
	}
	if ok == 0 {
		return nil
	}
	// A failed leg the server judged the place's counts as the legs before it:
	// a silent IPv4 HTTP/3 leg as its HTTPS leg, a failed IPv6 leg as the IPv4
	// legs. The outcome passes only when every failed leg was so judged.
	h3PlaceJudged := leg != nil && leg.Udp == UrlProbeH3UdpSilent && leg.Verdict == UrlProbeH3VerdictPlace
	if leg != nil && leg.Udp == UrlProbeH3UdpSilent && !h3PlaceJudged {
		return fmt.Errorf("URL success with a silent HTTP/3 leg the server did not judge the place's")
	}
	if self.Ipv6 != nil && self.Ipv6.failed() && !self.Ipv6.placeJudged() {
		return fmt.Errorf("URL success with a failed IPv6 leg the server did not judge the place's")
	}
	stage := self.FailureStage
	if stage == FailureStageH3 || stage == FailureStageIpv6 {
		stage = ""
	}
	if err := https(stage).succeeded(self.Policy, "URL"); err != nil {
		return err
	}
	if !self.Destination.H3 || self.UdpProbesDisabled {
		// a URL without h3, or with UDP probes off in the provider's country,
		// is judged on its HTTPS legs alone
		return nil
	}
	// A destination that declares HTTP/3 passes only with the HTTPS leg above
	// and an HTTP/3 leg that proved the provider's UDP path, or one the
	// server judged silent for its place.
	if leg == nil || !(leg.Udp == UrlProbeH3UdpPassed || leg.Udp == UrlProbeH3UdpAnswered || h3PlaceJudged) {
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

// Whether the IPv4 HTTPS leg of the outcome passed, which is what a site's own
// health is judged by: a success, or an outcome whose HTTP/3 leg or IPv6 leg
// was made, which needs a passed IPv4 HTTPS leg (ValidateOutcome). Only
// meaningful for evidence that validated.
func (self *UrlProbeEvidence) HttpsOk(ok int) bool {
	return 0 < ok || (self != nil && (self.H3 != nil || self.Ipv6 != nil))
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
