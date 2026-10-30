// Package egresshealth records accepted URL outcomes through an injected client.
// Production callers supply a validated configuration-owned catalog and probe
// one randomized URL at a time. Missing catalog data fails before network work.
// Legacy batch helpers retain historical wire compatibility, never a URL fallback.
package egresshealth

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Groups destinations so a partial failure is diagnosable: a provider
// that serves DNS but fails every CDN is the datacenter-IP-blocking case,
// which is a different fault from a total blackhole.
type Class string

const (
	ClassDns Class = "dns"
	// The purpose-built captive-portal-detection endpoints
	// every operating system already ships with, plus the echo services that
	// answer with the exit address. They are unauthenticated, carry no anti-bot
	// machinery, and answer in tens of bytes -- the cheapest useful signal in
	// the table, and the class least likely to fail for a reason that has
	// nothing to do with the provider.
	ClassConnectivity Class = "connectivity"
	// CDN edges, distribution mirrors and bulk-download hosts: the
	// operators whose business is serving static bytes. This is the class that
	// fails when a provider's egress range is on a CDN blocklist.
	ClassCdn Class = "cdn"
	// Ordinary web properties -- search, social, video, commerce,
	// news, reference, developer infrastructure, and the regional properties
	// that carry the same traffic outside the US and EU -- including the large
	// sites that refuse addresses they take for datacenters, which used to sit
	// in an unscored class of their own.
	//
	// The regional entries (qq.com, taobao, vk, naver, globo, ...) are
	// deliberately not a class of their own, even though the source list groups
	// them that way. A "regional" class would fail on healthy providers for
	// geographic reasons -- an exit in one country reaching another country's
	// properties slowly or not at all -- and a class-level verdict would then
	// be about geography rather than the exit. Mixed into site, they widen the
	// operator spread without inventing a verdict the data cannot support.
	ClassSite Class = "site"
)

// The declared order the classes are reported in. Result.ByClass
// is a map, so iterating it directly would render a different summary line on
// every pass; every ordered rendering goes through this slice instead. It is
// also the closed set a pooled destination's class must come from.
var Classes = []Class{ClassDns, ClassConnectivity, ClassCdn, ClassSite}

// Legacy batch sample targets, independent of the configured URL catalog.
// Scheduled URL probes use one randomized destination per turn.
var sampleSizes = map[Class]int{
	ClassDns:          6,
	ClassConnectivity: 8,
	ClassCdn:          10,
	ClassSite:         26,
}

// Declares what "success" means for one destination. The default,
// ExpectBody, is the original rule and the reason this check catches
// blackholes; ExpectStatus exists for the endpoints where an empty body is the
// correct answer.
//
// On the wire (a pooled Destination) it is a word, not the number: "body",
// "status" or "reachable". A stored integer would silently change meaning the
// first time these constants were reordered, and an unknown word is refused
// when the pool is decoded rather than judged as something else.
type Expect int

const (
	// Requires a 2xx and a non-empty body. This is the default (the
	// zero value) and must stay that way: a status line with no data behind it
	// is exactly what a blackholing provider produces, so a 200 with an empty
	// body is a failure. TestEmptyBodyIs200Failure guards it.
	ExpectBody Expect = iota
	// Requires exactly Destination.Status and permits an empty
	// body. It exists because 21 of the 143 endpoints measured from a real
	// datacenter host are reachable while legitimately returning zero bytes --
	// every generate_204 connectivity check, and many 3xx redirects -- and the
	// ExpectBody rule would score all of them as failures.
	//
	// Note that this is stricter than ExpectBody about the status, not looser:
	// the status must match exactly. A provider that synthesizes a bare 200 does
	// not pass an ExpectStatus 204 destination. Relaxing this to "any 2xx" would
	// turn the class into a hole in the blackhole rule.
	//
	// It is still the weaker contract in one respect that matters, and no entry
	// should take it without cause: a bare status line proves only that
	// something answered, while a body proves bytes crossed. That is why a 4xx
	// is never declared here (a refusal must stay a failure), and why a
	// destination that could be pointed at a url returning a real body is
	// pointed there instead of being declared.
	ExpectStatus
	// Accepts any 2xx or 3xx, with or without a body. It exists
	// for consumer sites that redirect based on the client's geography, which
	// this probe sees from a different exit country on every provider.
	//
	// Measured: microsoft.com answers 206/1024B to a datacenter host in the US
	// and in Germany, but 302/0B through 40 of 40 provider tunnels -- a locale
	// redirect keyed on the exit ip. Declaring a fixed status for such a
	// destination is impossible (the status is a function of where the provider
	// exits) and pointing it at robots.txt only moves the problem, because that
	// redirects too.
	//
	// This is the weakest contract and must stay confined to ClassSite, where
	// the question being asked is "did the provider carry my request to this
	// host and bring back a valid HTTP response" -- which is exactly the
	// "basic functionality" bar. It does not weaken the blackhole rule: a
	// blackholing provider returns no response at all, so it fails this too.
	// A 4xx/5xx is still a failure here, so a refusal stays a refusal.
	ExpectReachable
)

// Expect's wire spelling, in declaration order.
var expectWords = map[Expect]string{
	ExpectBody:      "body",
	ExpectStatus:    "status",
	ExpectReachable: "reachable",
}

// Implements fmt.Stringer: the wire spelling, or Expect(n) for a value that
// has none.
func (self Expect) String() string {
	if word, ok := expectWords[self]; ok {
		return word
	}
	return fmt.Sprintf("Expect(%d)", int(self))
}

// Refuses a value with no wire spelling rather than inventing one,
// so a corrupted table cannot be served as a pool.
func (self Expect) MarshalText() ([]byte, error) {
	if word, ok := expectWords[self]; ok {
		return []byte(word), nil
	}
	return nil, fmt.Errorf("egresshealth: expect %d has no wire spelling", int(self))
}

// Accepts exactly the wire spellings. An unknown word fails the
// decode, and with it the whole pool (see FetchPool): judging a contract the
// prober does not know as some contract it does would be a guess about a
// provider's traffic.
func (self *Expect) UnmarshalText(text []byte) error {
	for value, word := range expectWords {
		if string(text) == word {
			*self = value
			return nil
		}
	}
	return fmt.Errorf("egresshealth: unknown expect %q; want body, status or reachable", text)
}

// Names a check that proves a body is what its destination
// serves.
type BodyCheckKind string

const (
	// Requires a DoH JSON answer section with data (see
	// verifyDnsJson).
	BodyCheckDnsJson BodyCheckKind = "dns_json"
	// Requires the body to be one ip address (see verifyIpText).
	BodyCheckIpText BodyCheckKind = "ip_text"
	// Requires the body to contain BodyCheck.Text (see
	// verifyContains).
	BodyCheckContains BodyCheckKind = "contains"
)

// The answer to a class of failure a status code cannot see: a
// captive portal or an interception box happily returns 200 with a body. Only
// parsing the body proves the request was served by whom it was addressed to.
// It runs after the status and body rules, on the capped body.
//
// It is data rather than a function so a pooled destination carries the same
// proof a built-in one does; a pool whose dns entries arrived without their
// check would pass every portal. The zero value is "no check".
type BodyCheck struct {
	Kind BodyCheckKind `json:"kind"`
	// What BodyCheckContains looks for; unused by the others.
	Text string `json:"text,omitempty"`
}

// Reports whether the check can be run as declared. An unknown kind is
// an error, never "no check": treating a check the prober cannot run as
// passing would be the portal hole BodyCheck exists to close.
func (self BodyCheck) valid() error {
	switch self.Kind {
	case "", BodyCheckDnsJson, BodyCheckIpText:
		return nil
	case BodyCheckContains:
		if self.Text == "" {
			return errors.New("body check \"contains\" names no text; it would accept any body")
		}
		return nil
	default:
		return fmt.Errorf("unknown body check %q", self.Kind)
	}
}

// Runs the declared check on body. The zero value accepts.
func (self BodyCheck) check(body []byte) error {
	switch self.Kind {
	case "":
		return nil
	case BodyCheckDnsJson:
		return verifyDnsJson(body)
	case BodyCheckIpText:
		return verifyIpText(body)
	case BodyCheckContains:
		return verifyContains(body, self.Text)
	default:
		// valid() refuses this at the pool boundary; failing here too keeps a
		// hand-built Destination from passing a check nobody can run.
		return fmt.Errorf("unknown body check %q", self.Kind)
	}
}

// One endpoint to check. The json tags are the pool's wire
// format (see Pool): the server stores and serves destinations in exactly this
// shape.
type Destination struct {
	Name  string `json:"name"`
	Class Class  `json:"class"`
	Url   string `json:"url"`
	// Sent with the request, over the RequestProfile. They exist because
	// some destinations cannot be checked without them:
	// cloudflare-dns.com answers 400 to the DoH JSON GET form without an
	// Accept header. Range and Accept-Encoding are dropped whatever sets them
	// (see neverSent).
	Headers map[string]string `json:"headers,omitempty"`
	// Declares the success contract. The zero value is ExpectBody.
	Expect Expect `json:"expect"`
	// The required status code, read only when Expect is
	// ExpectStatus. Leaving it zero there is a misconfiguration and is reported
	// as a failed check rather than silently accepting anything.
	Status int `json:"status,omitempty"`
	// Caps the body read for this destination. Zero means MaxBodyBytes,
	// and a value above MaxBodyBytes is clamped to it -- the package-level cap is
	// the one documented in the byte budget and cannot be raised per entry.
	MaxBytes int `json:"max_bytes,omitempty"`
	// Optionally proves the body is what the destination is supposed to
	// serve. See BodyCheck.
	Verify BodyCheck `json:"verify,omitzero"`
	// Where the destination is known not to work from: a
	// provider published in one of these places is never asked to load it, so
	// a site blocked in a country never counts against that country's exits
	// (GEOMAP §11.3). The server learns and maintains the list (§11.4).
	Incompatible []Place `json:"incompatible,omitempty"`
	// Set by the server on a destination incompatible with some place, asks
	// for it to be loaded from there anyway -- unscored, reported apart
	// (CheckResult.Canary) -- so the server can tell when the site works there
	// again. Where the destination is compatible it changes nothing.
	Canary bool `json:"canary,omitempty"`
}

// The ceiling on any single body read, and the clamp on
// Destination.MaxBytes. It is a cap on the read, applied with io.LimitReader,
// after which the body is closed: a hostile or merely huge response cannot
// blow the byte budget documented on the package -- www.canva.com answers a
// 403 with 1.4 MB and apnews.com a 200 with 2.3 MB.
//
// It is 1024 bytes, down from 4096. Nothing in the table needs more: the point
// of a read is that bytes arrived and, where a body check is declared, that
// they parse, and every check in this package works on far less than a
// kilobyte. Hitting the cap is not by itself a signal that anything is wrong --
// most destinations serve far more than this when healthy.
const MaxBodyBytes = 1024

// The Accept header for the DoH JSON GET form. Cloudflare
// answers 400 without it. The others serve JSON with no header at all; sending
// it is harmless and keeps the seven entries identical in shape. It replaces
// the navigation Accept of the RequestProfile for these entries.
const acceptDnsJson = "application/dns-json"

// The query every DoH destination asks. example.com is an IANA
// reserved name that will not disappear, and its A record is short.
const dnsQuery = "?name=example.com&type=A"

// Per-class read caps. They are sized just above the largest response measured
// from each class, so the byte budget on the package is real arithmetic rather
// than a hope.
const (
	maxDnsBytes          = 768  // largest measured: doh.dns.sb, 579 B (it returns DNSSEC records)
	maxConnectivityBytes = 256  // largest measured that must parse: captive.apple.com, 69 B
	maxAssetBytes        = 1024 // = MaxBodyBytes; everything else
)

// Proves a DoH response actually resolved the name, which a
// status code cannot: a captive portal, a transparent proxy or an interception
// box all answer 200 with a body. Only an answer section proves resolution.
//
// It decodes only the Answer field, on purpose. The seven operators disagree
// about the rest of the document -- dns.alidns.com returns Question as an
// object where every other operator returns an array, so a struct that declared
// Question would fail to unmarshal AliDNS's perfectly good answer and score a
// working resolver as a captive portal.
//
// Status is not required to be 0 either. dns.adguard-dns.com omits the field
// entirely, and Go would leave it at 0 -- which happens to equal NOERROR, so a
// check on it would be passing by accident rather than by evidence. A non-empty
// answer with non-empty data is the evidence.
func verifyDnsJson(body []byte) error {
	var doc struct {
		Answer []struct {
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("the response is not dns json (%w); a 200 with a body is not proof a name was resolved", err)
	}
	if len(doc.Answer) == 0 {
		return errors.New("the dns response carries no answer section; the request was answered by something that did not resolve the name")
	}
	for _, a := range doc.Answer {
		if strings.TrimSpace(a.Data) != "" {
			return nil
		}
	}
	return errors.New("every record in the dns answer section is empty")
}

// Proves an echo endpoint returned an address rather than a
// portal's html. TrimSpace first: checkip.amazonaws.com terminates with a
// newline.
func verifyIpText(body []byte) error {
	text := strings.TrimSpace(string(body))
	if net.ParseIP(text) == nil {
		return fmt.Errorf("%q is not an ip address; this endpoint returns one, so something else answered", truncate(text, 64))
	}
	return nil
}

// Proves a fixed-text endpoint returned its fixed text.
//
// Substring, after TrimSpace, not equality: detectportal.firefox.com serves
// "success\n" -- eight bytes for a seven-character word -- so an equality check
// would fail for every provider forever, which is noise dressed as signal.
//
// Every want in the table is measured to appear within the first
// maxConnectivityBytes of its endpoint's body, which is what makes a substring
// test on a capped read meaningful: cloudflare's trace document puts ip= in its
// first 40 bytes, and example.com's title in its first 60.
func verifyContains(body []byte, want string) error {
	text := strings.TrimSpace(string(body))
	if !strings.Contains(text, want) {
		return fmt.Errorf("the body does not contain %q (got %q); a captive portal answers 200 with a page", want, truncate(text, 64))
	}
	return nil
}

// Keeps an unexpected body from filling the log with someone's login
// page.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// One destination's outcome, after its retries. It is recorded
// for failures as fully as for successes -- especially ByteCount, which is what
// separates "the destination refused us with a page of explanation" (bytes
// flowed; the tunnel works, the content provider said no) from "nothing came
// back at all" (the blackhole signature). Losing that distinction would defeat
// the point of grouping by class.
//
// StatusCode, ByteCount, Latency and Err describe the last attempt made;
// Attempts and LastFailure say how it got there.
type CheckResult struct {
	Name          string
	Class         Class
	Ok            bool
	StatusCode    int   // 0 when the request never produced a response
	ByteCount     int64 // bytes of body actually read, capped; set on failures too
	WireByteCount int64 // URL-mode encoded response-body bytes; never inferred from decoded size
	// Total bounded fetch duration, preserving the historical metric contract.
	Latency time.Duration
	// Time to the first response byte, including DNS and connection setup.
	// Unset on a no-byte failure; FirstByteReceived distinguishes that case.
	TimeToFirstByte         time.Duration
	DnsLookupLatency        time.Duration
	DnsMeasured             bool
	ConnectFirstByteLatency time.Duration
	FirstByteReceived       bool
	// Capped sampled-body diagnostics, not a calibrated bulk bandwidth sample.
	BodyDuration        time.Duration
	BodyBytesPerSecond  float64
	WireSampleByteCount int64
	BodyFirstByteWait   time.Duration
	// URL-mode diagnostics describe only the final request. Redirect time is
	// separate, and a content/challenge verdict is never a TLS finding.
	RequestTimeToFirstByte    time.Duration
	RequestWritten            bool
	TcpConnectLatency         time.Duration
	TlsHandshakeLatency       time.Duration
	RedirectCount             int
	ContentClassification     string
	PerformanceClassification string
	BodyComplete              bool
	BodySampled               bool
	UrlProbeSecurity          []UrlProbeSecurityEvent
	// Only an already-sampled successful HTTPS IP-text connectivity response
	// may carry these; DNS answers and provider claims are never exit evidence.
	ObservedExitIp string    `json:"-"`
	ExitObservedAt time.Time `json:"-"`
	Err            string    // "" when OK
	// Local-only, bounded diagnostic for the last attempt. Never sent to the
	// ingestion API: raw Err may contain a URL, while this is a fixed stage.
	FailureStage             string `json:"-"`
	TlsAuthenticationFailure bool   // the peer's certificate could not authenticate the requested host
	// How many times the destination was fetched: 1 when the
	// first attempt passed, Options.LoadAttempts when every attempt failed,
	// fewer after a terminal TLS failure or when the run ran out, 0 when the
	// run ended before the first attempt could start.
	Attempts int
	// The most recent failed attempt's error: on a failure it
	// equals Err, and on a load that passed after a retry it is why the retry
	// was needed. "" when the first attempt passed.
	LastFailure string
	// Set on a load whose tunnel was not there for its last
	// attempt: the tunnel died, could not be re-created in time (see Path),
	// or its own control registration failed before any client existed.
	// It is neither a pass nor a failure -- an unavailable instrument
	// says nothing about the site -- and it is left out
	// of every tally and reported on its own, so the server can leave it out
	// of total_count. Attempts and LastFailure still say what happened.
	NotMeasured bool
	// Set on a destination loaded from a place it is incompatible with, on
	// the server's request (Destination.Canary): unscored, and reported apart
	// so the server keeps it out of the counts.
	Canary bool
}

// The ok/total tally for one class.
type ClassSummary struct {
	Ok    int
	Total int
}

// One full run.
type Result struct {
	// Stable report identity and the server-issued durable cycle token.
	RunId          string
	CycleStartedAt time.Time
	// Scheduled URL source, including an explicit country-coverage gap.
	UrlSource        string
	UrlProbeEvidence *UrlProbeEvidence
	// Every sampled destination's outcome after retries, in table
	// order.
	Checks []CheckResult
	// Optional independent evidence from already-sampled successful HTTPS
	// IP-text connectivity responses. Empty if none were sampled/answered,
	// an address was not public unicast, or the valid observations disagree.
	ExitIp string
	// Actual response observation time on the Options clock, never DNS time.
	ExitObservedAt time.Time
	// Deprecated compatibility field. No predictable echo request is made.
	IpEchoErr string
	// True when any sampled HTTPS peer failed
	// certificate-chain or hostname verification. It is intentionally separate
	// from the score: one forged identity is a hard integrity failure and must
	// not be diluted by unrelated successful destinations.
	TlsAuthenticationFailure bool
	// The loads that passed on some attempt. With Total it covers every
	// class, over the destinations this run sampled and measured, after every
	// load's retries: Total - OkCount is the loads that failed every attempt,
	// which is what the index and the one-in-ten rule count. Loads that were
	// not measured, and canaries, are in neither.
	OkCount int
	// The loads this run sampled and measured; see OkCount.
	Total int
	// How many loads the run could not measure because their
	// tunnel was gone and could not be re-created in time (see
	// CheckResult.NotMeasured). The run is still a measurement of the loads it
	// did measure.
	NotMeasured int
	// Lists, in Classes order, the classes that had fewer
	// destinations compatible with the provider's place than their sample
	// size: the run took all of them rather than pad from sites known not to
	// work there. It is the pool's signal (a place too thin for a class), not
	// the provider's.
	ShortClasses []Class
	// The per-class tally, over the sample.
	ByClass map[Class]ClassSummary
	// The size of the table (or pool) the sample was drawn from,
	// rendered as table=N so nobody reads "dns=6/6" as "the table holds
	// six resolvers". Zero when the run was not sampled (a test driving check()
	// directly), and then omitted from Summary.
	TableTotal int
}

// Tunes a single Check or Blackhole call. The zero value is valid and
// uses the defaults below.
type Options struct {
	// Scheduled mode records one random URL outcome. The server owns retry
	// pacing and the ten-success cycle; no per-URL retry hides an error.
	UrlProbe bool
	// Per-attempt content, redirect and timing contract served with the catalog.
	UrlProbePolicy *UrlProbePolicy
	// Unresolved exact-URL TLS targets are sampled with equal probability to
	// the normal pool. Their presence never disables normal URL observations.
	SecurityDestinations []Destination
	// Accepted durable outcome count is diagnostic progress, not source order.
	// Normal selection independently draws general/country with equal probability;
	// missing country coverage uses general URLs without an admission gate.
	OutcomeCount        int
	CountryDestinations []Destination
	// Bounds each attempt of each load. Zero or negative
	// uses DefaultPerRequestTimeout.
	PerRequestTimeout time.Duration
	// Bounds the whole run, so a provider that swallows every request
	// cannot hold the tunnel open indefinitely. Zero or negative uses
	// RunBudget, the longest the retry schedule can take -- see there for why
	// the default is derived rather than fixed.
	Budget time.Duration
	// Caps simultaneous fetches over the one tunnel. A load
	// waiting out its retry spacing does not count against it. Zero or
	// negative uses DefaultConcurrency.
	Concurrency int
	// Draws this run's sample and its retry spacing. Nil -- which is
	// what production passes -- means a fresh generator seeded from
	// crypto/rand for every run, which is the whole anti-gaming property: the
	// provider cannot know what it will be asked for, or when it will be asked
	// again. Tests set it to make a run reproducible.
	//
	// It must not be shared between concurrent runs: math/rand.Rand is stateful
	// and not goroutine-safe, and the prober probes several providers at once.
	// Within one run it is drawn from behind a lock. Leaving it nil is what
	// makes the cross-run case safe, so production leaves it nil.
	Rand *rand.Rand

	// Runs every destination in the table instead of drawing a
	// sample. It exists for operator-run diagnostics -- "show me the complete
	// picture for this fleet right now" -- and is deliberately not the default.
	//
	// Two things change when it is set, and the caller owns both:
	//
	//   1. Cost. The sample is bounded by construction; the full table is not.
	//      At the current table it is ~2.8x the bytes and ~2.8x the requests.
	//   2. The anti-gaming property is lost. A fixed, fully-enumerable
	//      destination list is exactly what a provider can whitelist. Random
	//      sampling is what makes that impractical, so scheduled production
	//      passes must keep sampling; this is for on-demand inspection.
	//
	// Concurrency should be raised to AllConcurrency to match; RunBudget grows
	// with the table on its own.
	AllDestinations bool

	// The configured catalog. A missing catalog fails before network work;
	// there is no compiled-in fallback.
	Destinations []Destination
	// The browser shape every request carries. Nil -- or one with
	// no user agent -- means DefaultRequestProfile. See RequestProfile.
	Profile *RequestProfile

	// How many times a load is tried before it counts as
	// failed. Zero or negative uses DefaultLoadAttempts; 1 disables retries.
	LoadAttempts int
	// The mean of the random spacing between a
	// failed attempt and the next (exponential, capped at three times the
	// mean). Zero or negative uses DefaultLoadRetryMeanInterval.
	LoadRetryMeanInterval time.Duration
	// Called immediately before and after a retry wait. Concurrent loads may
	// call it concurrently; observers must not block or retain provider data.
	// It describes waiting load attempts, not occupied worker count.
	ObserveRetryWait func(waiting bool)

	// Deprecated compatibility input. Never requested: only sampled URLs
	// traverse the provider. Website quality does not imply exit-location evidence.
	IpEchoUrl string
	// Deprecated spelling of ColdStartTimeout, accepted for existing callers.
	IpEchoTimeout time.Duration
	// One shared establishment window per tunnel generation, starting with
	// its first sampled request. Success ends the allowance for later requests;
	// failures never renew it. Zero leaves the ordinary request timeout in force.
	ColdStartTimeout time.Duration

	// When set, the tunnel the run rides and a way to re-create it when it
	// dies part-way (see Path); the client passed to Check or
	// Blackhole is then ignored. Nil means that client, as it is, for the
	// whole run.
	Path Path
	// Caps how many times one run or check may
	// re-create its tunnel. Every re-creation is a new proxy device and a new
	// contract with the provider, so a provider that cannot hold a tunnel up
	// must not be able to cost one per load; past the cap the pending loads
	// are not measured. Zero or negative uses DefaultTunnelRecreateAttempts.
	TunnelRecreateAttempts int
	// Where the provider is published. Destinations
	// incompatible with it are left out of the sample, and canaries for it
	// are loaded besides (see Destination.Incompatible, Destination.Canary).
	// The zero Place excludes nothing.
	ProviderPlace Place

	// The run's clock, with Sleep. Nil uses the wall clock and a sleep that
	// selects on time.After and the context. Tests inject both so a run whose
	// loads wait minutes between attempts finishes in microseconds, and so the
	// spacing a run asked for can be read back.
	Now func() time.Time
	// The run's wait, with Now. It must return the context's error, and
	// return early, when the context ends first.
	Sleep func(ctx context.Context, d time.Duration) error
	// Package-private synthetic address seam; production always uses the
	// special-purpose-aware public exit policy.
	exitAddressAllowed func(netip.Addr) bool
}

// Defaults for Options. They are vars so tests can lower them.
var (
	// Bounds one attempt on an established path, including in-tunnel DNS,
	// TCP, TLS and the capped response body. The first sampled requests share
	// a finite cold-start allowance instead of using a separate warm-up. Treat it as
	// a floor rather than a preference: a load that times out is retried, but
	// only minutes later.
	DefaultPerRequestTimeout = 10 * time.Second
	// Caps simultaneous fetches over one tunnel. It is 6
	// because simultaneous TLS handshakes over one gvisor tunnel with
	// keep-alives disabled contend with each other and inflate every latency.
	// It is not raised with the sample: waiting loads hold no slot, so the
	// first round of 50 takes nine slots-worth of fetches and the retries,
	// spread over minutes, rarely contend at all.
	//
	// The field signal that this is set too high is specific: first-attempt
	// timeouts spread evenly across all classes after a sampled URL passed.
	// That calls for measuring contention, not declaring every provider bad.
	DefaultConcurrency = 6
	// How many times a load is tried (GEOMAP §11.3).
	// Three independent failures of a destination that answers 93 % of the
	// time in the field are rare enough to mean something; one was not.
	DefaultLoadAttempts = 3
	// The mean spacing between a failed
	// attempt and the next (GEOMAP §11.3). Five minutes is long enough for a
	// rate limit or a flapping path to clear, and makes the requests to any
	// one site look like a person coming back to it.
	DefaultLoadRetryMeanInterval = 5 * time.Minute
	// How many times a run or a check may
	// re-create a tunnel that died under it. Two covers a provider that
	// reconnects once or twice in a fifteen-minute run; one that needs more is
	// not holding a path up, and what it leaves unmeasured is honest.
	DefaultTunnelRecreateAttempts = 2
	// Legacy name for the default shared cold-start window --
	// the provider window, the contract, the in-tunnel DNS resolution and the
	// TLS handshake of a tunnel whose open returned before any path existed --
	// and is sized like the per-source geolocation deadline that used to
	// absorb the same cost, not like a load's.
	DefaultIpEchoTimeout = 60 * time.Second
)

// Returned when client is nil. Checks run in spawned
// goroutines, where a nil-client panic could not be recovered by the caller, so
// it is rejected up front.
var ErrNilClient = errors.New("egresshealth: client must not be nil")

// Returned when the destination table is empty, which
// would make a run vacuous: 0/0 checks passed is not evidence of anything.
var ErrNoDestinations = errors.New("egresshealth: at least one destination is required")

// Returned when the context is already done on entry. This is a
// structural failure and must not be reported as a run, because a run started
// on a dead context returns 0/N -- identical to a total blackhole. The caller
// (see prober) is expected to check for it and log "skipped" rather than a
// verdict.
var ErrNoBudget = errors.New("egresshealth: the context was already done before any check ran")

// Returned when the caller's context ended while the run was
// still going. It is ErrNoBudget's twin, and matters more now that a run spans
// minutes: the loads in flight at that moment fail on the dead context and the
// loads waiting to retry never retry, so the partial outcome is the prober's
// own deadline or shutdown, not anything the provider did. Reporting it as a
// run would charge a working provider with failed sites. The run's own budget
// (Options.Budget) ending is different: that bound is part of the
// measurement, and its outcome is a result.
var ErrInterrupted = errors.New("egresshealth: the caller's context ended before the run finished")

// Reports that the server does not implement the egress-health
// ingest endpoint (it answers 404). It lives here rather than in ingest for the
// same reason bandwidth.ErrUnsupported lives in bandwidth: the prober
// classifies the outcome, and it must be able to tell "this deployment has not
// shipped the endpoint yet" from a real submission failure without importing
// the submitter implementation its interfaces exist to keep out.
//
// An unsupported endpoint leaves quality unacknowledged. The prober logs the
// measurement, but must not count the attempt as accepted health coverage.
var ErrUnsupported = errors.New("egresshealth: the server does not implement the provider egress health endpoint")

// Runs a random sample of the destination table through client and
// returns the full pattern of what worked, after every load's tries.
//
// The table is the explicit Options.Destinations catalog. URL mode selects
// one destination; the legacy batch sample is drawn per call and per class
// (see sampleDestinations and sampleSizes): the same provider probed twice is
// asked for different destinations, and no provider can know in advance which
// ones. Coverage of the table accumulates across runs and across the fleet
// rather than being paid on every run.
//
// One destination failing never aborts the run: the pattern of failures is the
// value, so every sampled destination is attempted and every outcome recorded.
// An error is returned only when something structural stopped the run from
// happening, or from finishing (see ErrNilClient, ErrNoDestinations,
// ErrNoBudget, ErrInterrupted) -- so `err == nil && OkCount == 0` is a real,
// trustworthy total-blackhole reading, distinguishable from a run that never
// took place.
//
// In production client egresses through one provider's tunnel, so what this
// measures is that provider's willingness and ability to carry ordinary
// traffic.
func Check(ctx context.Context, client *http.Client, opts Options) (*Result, error) {
	if opts.UrlProbe {
		return checkUrlProbe(ctx, client, opts)
	}
	table := opts.table()
	// One generator for the whole run, drawn first for the sample and then
	// for the retry spacing, so a caller-supplied seed reproduces both.
	opts.Rand = opts.rng()
	compatible, canaries := forPlace(table, opts.ProviderPlace)
	var chosen []Destination
	var shortClasses []Class
	if opts.AllDestinations {
		chosen = compatible
	} else {
		chosen = sampleDestinations(compatible, sampleSizes, opts.Rand)
		// The classes whose compatible pool is smaller than their declared
		// sample size -- the classes a run for this place could not fill -- in
		// Classes order.
		compatibleClassCounts := map[Class]int{}
		for _, d := range compatible {
			compatibleClassCounts[d.Class]++
		}
		for _, c := range Classes {
			if size, declared := sampleSizes[c]; declared && compatibleClassCounts[c] < size {
				shortClasses = append(shortClasses, c)
			}
		}
	}
	if 0 < len(canaries) {
		// Recovery evidence accumulates across providers and passes. A country
		// with many incompatible sites must not add an unbounded fixed workload.
		permutation := opts.Rand.Perm(len(canaries))
		selectedCanaries := make([]Destination, 0, min(len(canaries), MaxSampledCanaries))
		for _, index := range permutation[:min(len(canaries), MaxSampledCanaries)] {
			selectedCanaries = append(selectedCanaries, canaries[index])
		}
		// The sample and the canaries in table order: Checks, FailedNames and
		// the log line read in the order the table declares whatever was
		// drawn, canaries included.
		pickedNames := map[string]bool{}
		for _, d := range chosen {
			pickedNames[d.Name] = true
		}
		for _, d := range selectedCanaries {
			pickedNames[d.Name] = true
		}
		chosen = make([]Destination, 0, len(pickedNames))
		for _, d := range table {
			if pickedNames[d.Name] {
				chosen = append(chosen, d)
			}
		}
	}
	res, err := check(ctx, client, chosen, opts)
	if res != nil {
		res.TableTotal = len(table)
		res.ShortClasses = shortClasses
	}
	return res, err
}

// The concurrency an AllDestinations run uses. It is raised
// above DefaultConcurrency because otherwise the round count -- and with it
// the wall clock -- grows linearly with the table. It is not raised further:
// every request rides the same provider tunnel, so concurrency here is load on
// the provider under test, and a diagnostic that overloads its subject
// measures the overload.
const AllConcurrency = 10

// Unscored place-recovery controls are drawn independently each run. The small
// fixed cap bounds extra requests and is included in outer admission budgets.
const MaxSampledCanaries = 2

// The legacy batch budget target, independent of any configured URL catalog.
func SampleTargetPerRun() int {
	count := 0
	for _, size := range sampleSizes {
		count += size
	}
	return count
}

// Like SamplePerRun, for any table, such as a pool.
func SamplePerRunOf(dests []Destination) int {
	n := 0
	for _, c := range tableClasses(dests) {
		n += sampleCount(dests, c, sampleSizes)
	}
	return n
}

// The longest a run of loads loads can take under these options,
// and what Budget defaults to: every load's every attempt in
// rounds of Concurrency -- as if every retry landed at the same moment, the
// worst the random spacing can produce -- and the longest spacing between
// attempts, plus one finite cold-start allowance per possible tunnel generation.
// At the defaults with a 60s cold window and a 50-load sample that is
// 3 x 9 x 10s + 2 x 15m + 3 x (60s - 10s), 37 minutes; a run that passes everything
// takes its first round, and one with a site that keeps failing about ten to
// fifteen minutes.
//
// It is derived rather than fixed because the bound has to move with the
// settings it bounds. A fixed budget shorter than the schedule would cut the
// last attempt off every chain that drew long delays -- a load failed for the
// prober's arithmetic, not the provider's traffic -- and one far longer would
// hold a swallowing provider's tunnel open for nothing.
func (self Options) RunBudget(loads int) time.Duration {
	if loads < 1 {
		loads = 1
	}
	concurrency := self.concurrency()
	rounds := (loads + concurrency - 1) / concurrency
	attempts := self.loadAttempts()
	budget := time.Duration(attempts*rounds) * self.perRequestTimeout()
	budget += time.Duration(attempts-1) * retryDelayCapFactor * self.loadRetryMeanInterval()
	generations := 1 + self.tunnelRecreateAttempts()
	budget += time.Duration(generations) * max(0, self.coldStartTimeout()-self.perRequestTimeout())
	return budget
}

// Retains the configured establishment allowance without making an echo request.
func (self Options) coldStartTimeout() time.Duration {
	if 0 < self.ColdStartTimeout {
		return self.ColdStartTimeout
	}
	if 0 < self.IpEchoTimeout {
		return self.IpEchoTimeout
	}
	if self.IpEchoUrl != "" {
		return DefaultIpEchoTimeout
	}
	return self.perRequestTimeout()
}

// Returns the per-attempt timeout, or DefaultPerRequestTimeout when unset.
func (self Options) perRequestTimeout() time.Duration {
	if 0 < self.PerRequestTimeout {
		return self.PerRequestTimeout
	}
	return DefaultPerRequestTimeout
}

// Returns the whole run's bound, or RunBudget(loads) when unset.
func (self Options) budget(loads int) time.Duration {
	if 0 < self.Budget {
		return self.Budget
	}
	return self.RunBudget(loads)
}

// Returns the fetch concurrency, or DefaultConcurrency when unset.
func (self Options) concurrency() int {
	if 0 < self.Concurrency {
		return self.Concurrency
	}
	return DefaultConcurrency
}

// Returns the attempts per load, or DefaultLoadAttempts when unset.
func (self Options) loadAttempts() int {
	if 0 < self.LoadAttempts {
		return self.LoadAttempts
	}
	return DefaultLoadAttempts
}

// Returns the mean retry spacing, or DefaultLoadRetryMeanInterval when unset.
func (self Options) loadRetryMeanInterval() time.Duration {
	if 0 < self.LoadRetryMeanInterval {
		return self.LoadRetryMeanInterval
	}
	return DefaultLoadRetryMeanInterval
}

// Returns the tunnel re-creation cap, or DefaultTunnelRecreateAttempts when
// unset.
func (self Options) tunnelRecreateAttempts() int {
	if 0 < self.TunnelRecreateAttempts {
		return self.TunnelRecreateAttempts
	}
	return DefaultTunnelRecreateAttempts
}

// Returns the warm-up's timeout, or DefaultIpEchoTimeout when unset.
func (self Options) ipEchoTimeout() time.Duration {
	if 0 < self.IpEchoTimeout {
		return self.IpEchoTimeout
	}
	return DefaultIpEchoTimeout
}

// Every caller must supply its configuration-owned catalog.
func (self Options) table() []Destination {
	return self.Destinations
}

// Returns the profile every request carries: Profile, or the default profile
// when there is none or it has no user agent.
func (self Options) profile() RequestProfile {
	if self.Profile != nil {
		return self.Profile.orDefault()
	}
	return DefaultRequestProfile()
}

// Returns the time on the run's clock: Now, or the wall clock.
func (self Options) now() time.Time {
	if self.Now != nil {
		return self.Now()
	}
	return time.Now()
}

// Waits d on the run's clock, or until ctx ends, whichever comes first, and
// says which: Sleep, or the production wait. The production wait selects on
// time.After rather than a timer the caller must stop: it is always bounded by
// d, and nothing here needs to reset or reuse it.
func (self Options) sleep(ctx context.Context, d time.Duration) error {
	if self.Sleep != nil {
		return self.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// The randomness one run's sample and spacing are drawn from.
//
// A caller-supplied generator is used as given, which is what makes a test
// reproducible. Otherwise a fresh one is built per run and seeded from
// crypto/rand -- not from the clock, and never from the global generator. The
// anti-gaming property is that a provider cannot predict which destinations it
// will be asked for, and a clock seed is predictable to anyone who knows
// roughly when the pass started. The global generator is avoided for a duller
// reason: it is shared process-wide state that any other package can reseed.
func (self Options) rng() *rand.Rand {
	if self.Rand != nil {
		return self.Rand
	}
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice. If it ever does, a clock seed
		// still samples: a weaker unpredictability is worth having, a skipped
		// run is not.
		return rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(b[:]))))
}

// The body read cap for this destination: its own, clamped to the
// package ceiling so no single entry can raise the documented budget.
func (self Destination) maxBytes() int64 {
	if 0 < self.MaxBytes && self.MaxBytes < MaxBodyBytes {
		return int64(self.MaxBytes)
	}
	return MaxBodyBytes
}

// Lists the classes present in dests, in table order and without
// repeats.
//
// Table order, never map iteration order, and that is load-bearing rather than
// tidy: sampleDestinations draws from the generator once per class, so a class
// order that varied between runs would make the same seed produce a different
// sample. Reproducibility is what the tests turn on.
func tableClasses(dests []Destination) []Class {
	seen := map[Class]bool{}
	var out []Class
	for _, d := range dests {
		if seen[d.Class] {
			continue
		}
		seen[d.Class] = true
		out = append(out, d.Class)
	}
	return out
}

// How many destinations of class c a run draws: the declared
// size, or the whole class when it is smaller than that or has no declared size
// at all. See sampleSizes for why "no declared size" means "probe it all".
func sampleCount(dests []Destination, c Class, sizes map[Class]int) int {
	total := 0
	for _, d := range dests {
		if d.Class == c {
			total++
		}
	}
	n, declared := sizes[c]
	if !declared || n <= 0 || total < n {
		return total
	}
	return n
}

// Draws each class's sample for one run.
//
// It is a partial Fisher-Yates over each class's indices, so every subset of a
// class is equally likely and no destination is drawn twice. The result is
// returned in table order (the indices are sorted afterwards) so that Checks,
// FailedNames and the log line keep reading in the order the table declares,
// whatever the draw was -- a summary that reordered itself per run would be
// undiffable.
//
// It takes the table, the sizes and the generator as arguments rather than
// reaching for package state, which is the same seam check() uses: a test can
// drive it with a stub table and a fixed seed and assert exactly what came out.
func sampleDestinations(dests []Destination, sizes map[Class]int, r *rand.Rand) []Destination {
	byClass := map[Class][]int{}
	for i, d := range dests {
		byClass[d.Class] = append(byClass[d.Class], i)
	}

	var picked []int
	for _, c := range tableClasses(dests) {
		idx := byClass[c]
		n := sampleCount(dests, c, sizes)
		if len(idx) <= n {
			picked = append(picked, idx...)
			continue
		}
		shuffled := append([]int(nil), idx...)
		for i := 0; i < n; i++ {
			j := i + r.Intn(len(shuffled)-i)
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		}
		picked = append(picked, shuffled[:n]...)
	}
	sort.Ints(picked)

	out := make([]Destination, 0, len(picked))
	for _, i := range picked {
		out = append(out, dests[i])
	}
	return out
}

// The seam Check is built on, taking the destinations explicitly so
// tests can drive httptest servers instead of the real internet.
func check(ctx context.Context, client *http.Client, dests []Destination, opts Options) (*Result, error) {
	path := opts.path(client)
	if current, _ := path.Current(); current == nil {
		return nil, ErrNilClient
	}
	if len(dests) == 0 {
		return nil, ErrNoDestinations
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoBudget, err)
	}

	parent := ctx
	budget := opts.budget(len(dests))
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	res := &Result{}
	r := newRun(path, opts, opts.concurrency(), budget, opts.rng())

	res.Checks = r.loadAll(ctx, dests)
	if err := parent.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	collectSampledExit(res)

	// ByClass is seeded from the sample, not from the results that happened to
	// come back, so a class in which every single check failed still appears
	// as 0/n. Accumulating only from successes would make the datacenter-IP
	// case -- the whole reason the class dimension exists -- vanish from the
	// map at exactly the moment it matters. A load that was not measured, or a
	// canary, is in no tally.
	res.ByClass = map[Class]ClassSummary{}
	for i := range res.Checks {
		c := &res.Checks[i]
		c.Canary = dests[i].Canary && dests[i].IncompatibleWith(opts.ProviderPlace)
		if c.TlsAuthenticationFailure {
			res.TlsAuthenticationFailure = true
		}
		switch {
		case c.Canary:
			continue
		case c.NotMeasured:
			res.NotMeasured++
			continue
		}
		s := res.ByClass[c.Class]
		s.Total++
		res.Total++
		if c.Ok {
			s.Ok++
			res.OkCount++
		}
		res.ByClass[c.Class] = s
	}
	return res, nil
}

// The Path a run rides: the caller's, or its one client as it is.
func (self Options) path(client *http.Client) Path {
	if self.Path != nil {
		return self.Path
	}
	return staticPath{client: client}
}

// Performs one attempt of one destination's check. It never returns an
// error: a failed attempt is a result, and the run keeps going.
func fetch(ctx context.Context, client *http.Client, d Destination, timeout time.Duration, profile RequestProfile, now func() time.Time) CheckResult {
	return fetchWithExitPolicy(ctx, client, d, timeout, profile, now, nil)
}

// The test-only address predicate never changes request, TLS or scoring policy.
func fetchWithExitPolicy(ctx context.Context, client *http.Client, d Destination, timeout time.Duration, profile RequestProfile, now func() time.Time, exitAllowed func(netip.Addr) bool) CheckResult {
	r := CheckResult{Name: d.Name, Class: d.Class}
	start := now()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, progress := traceRequestProgressAt(ctx, now)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Url, nil)
	if err != nil {
		progress.recordTiming(&r, start, now())
		r.Err = err.Error()
		r.FailureStage = "request_build"
		return r
	}
	applyHeaders(req, profile, d.Headers)

	resp, err := client.Do(req)
	if err != nil {
		progress.recordTiming(&r, start, now())
		r.Err = err.Error()
		r.FailureStage = echoRequestStage(err)
		if r.FailureStage == "request_timeout" {
			r.FailureStage = progress.timeoutStage()
		}
		r.TlsAuthenticationFailure = isTlsAuthenticationFailure(err)
		// An exact local-control/no-contact proof is not a peer verdict.
		// Response/TCP/TLS evidence and confinement failures still stand.
		if resp == nil && !r.TlsAuthenticationFailure && r.FailureStage != "policy" &&
			progress.phase.Load() < requestPhaseAfterDial {
			if unavailable, ok := client.Transport.(interface{ ProviderMeasurementUnavailable() bool }); ok && unavailable.ProviderMeasurementUnavailable() {
				r.NotMeasured = true
				r.FailureStage = "local_control_registration"
			} else if unavailable, ok := client.Transport.(interface{ ProviderContractAcquisitionUnavailable() bool }); ok && unavailable.ProviderContractAcquisitionUnavailable() {
				r.NotMeasured = true
				r.FailureStage = "local_contract_acquisition"
			} else if unavailable, ok := client.Transport.(interface{ ProviderLocalWriteUnavailable() bool }); ok && unavailable.ProviderLocalWriteUnavailable() {
				r.NotMeasured = true
				r.FailureStage = "local_transport_admission"
			}
		}
		return r
	}
	// Closed after the capped read, whatever is left on the wire: the first
	// kilobyte is the whole question (see MaxBodyBytes and neverSent).
	defer resp.Body.Close()
	r.StatusCode = resp.StatusCode
	// Standard transports emit GotFirstResponseByte. A custom transport that
	// does not trace still proves first byte no later than response headers.
	progress.recordFirstByte(now())

	// The body is read even on an error status, and ByteCount is recorded
	// either way: a 403 with a page of explanation proves the tunnel carried
	// bytes in both directions and the content provider refused, which is a
	// different fault from nothing coming back at all.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, d.maxBytes()))
	r.ByteCount = int64(len(body))
	progress.recordTiming(&r, start, now())

	if readErr != nil {
		r.Err = readErr.Error()
		r.FailureStage = "response_body"
		return r
	}
	if err := d.judge(resp.StatusCode, body); err != nil {
		r.Err = err.Error()
		r.FailureStage = "response_judgment"
		return r
	}
	r.Ok = true
	if d.Class == ClassConnectivity && d.Verify.Kind == BodyCheckIpText && req.URL.Scheme == "https" {
		if ip := sampledExitWithPolicy(string(body), exitAllowed); ip != "" {
			r.ObservedExitIp = ip
			r.ExitObservedAt = now()
		}
	}
	return r
}

// Identifies certificate-chain and hostname
// verification failures without parsing their human-readable text. These are
// qualitatively different from timeouts, refused connections, HTTP statuses,
// and content mismatches: a provider returned a TLS identity that does not
// authenticate the destination the client requested.
//
// CertificateVerificationError is the normal crypto/tls wrapper. The x509
// fallbacks cover transports with a custom VerifyPeerCertificate callback,
// including the provider tunnel's pinning path, which can return the underlying
// verification error directly.
func isTlsAuthenticationFailure(err error) bool {
	var explicit interface{ TLSAuthenticationFailure() bool }
	if errors.As(err, &explicit) && explicit.TLSAuthenticationFailure() {
		return true
	}
	var verificationErr *tls.CertificateVerificationError
	if errors.As(err, &verificationErr) {
		return true
	}
	var unknownAuthorityErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthorityErr) {
		return true
	}
	var certificateInvalidErr x509.CertificateInvalidError
	if errors.As(err, &certificateInvalidErr) {
		return true
	}
	var hostnameErr x509.HostnameError
	return errors.As(err, &hostnameErr)
}

// Applies the destination's success contract to what came back. It is
// separated from the transport so the rule can be read -- and reasoned about --
// without an http server in the way.
func (self Destination) judge(status int, body []byte) error {
	switch self.Expect {
	case ExpectStatus:
		if self.Status <= 0 {
			// A misconfigured entry must fail loudly rather than accept anything:
			// "expect exactly nothing in particular" is how ExpectStatus would
			// become a hole in the blackhole rule.
			return fmt.Errorf("destination declares ExpectStatus with no Status; it cannot be judged")
		}
		if status != self.Status {
			// Exact, not "any 2xx". A provider that synthesizes a bare 200 must
			// not pass a destination that is supposed to answer 204.
			return fmt.Errorf("status %d, want exactly %d", status, self.Status)
		}
	case ExpectReachable:
		// Any 2xx or 3xx: the host answered through the provider. A 4xx/5xx is
		// still a refusal and still fails, and a blackholing provider produces
		// no response at all, so this remains a real liveness check.
		if status < 200 || 400 <= status {
			return fmt.Errorf("status %d, want any 2xx or 3xx", status)
		}
	case ExpectBody:
		if status < 200 || 300 <= status {
			// 3xx counts as a failure, not a redirect to follow. The production
			// client refuses redirects outright (providertunnel's CheckRedirect),
			// so a 3xx here is a destination that did not serve what was asked
			// for. A destination that legitimately answers 3xx should declare
			// ExpectStatus rather than be chased.
			return fmt.Errorf("status %d", status)
		}
		if len(body) == 0 {
			// The blackhole signature, and the reason this rule is spelled out: a
			// status line with no body is exactly what a provider that terminates
			// the connection itself, or one whose upstream returns a stub,
			// produces. Counting it as success would let the failure this package
			// exists to catch pass the check. This stays the default contract; an
			// endpoint where an empty body is correct must say so with
			// ExpectStatus.
			return fmt.Errorf("status %d with an empty body", status)
		}
	default:
		// Only a hand-built Destination can get here; the pool decoder
		// refuses an unknown contract. Accepting it would be a guess.
		return fmt.Errorf("destination declares %s, which cannot be judged", self.Expect)
	}
	if err := self.Verify.check(body); err != nil {
		return fmt.Errorf("status %d but the body did not verify: %w", status, err)
	}
	return nil
}

// Returns the host of every destination in dests, de-duplicated and in
// order. It is the tunnel allowlist a run over dests needs.
//
// A URL that does not parse, or carries no host, is skipped rather than
// panicking. Catalog validation rejects malformed URLs before production use.
func HostsOf(dests []Destination) []string {
	seen := map[string]bool{}
	hosts := make([]string, 0, len(dests))
	for _, d := range dests {
		u, err := url.Parse(d.Url)
		if err != nil {
			continue
		}
		host := u.Hostname()
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

// Counts the loads that passed only after a failed attempt: the
// flakes the retries absorbed, which a single-attempt run would have counted
// as failed sites.
func (self *Result) Retried() int {
	if self == nil {
		return 0
	}
	n := 0
	for _, c := range self.Checks {
		if c.Ok && 1 < c.Attempts && !c.Canary {
			n++
		}
	}
	return n
}

// Renders the one-line, per-pass form:
//
//	ok=48/50 dns=6/6 connectivity=8/8 cdn=9/10 site=25/26 table=139 retried=3
//
// Every figure except table= is over the destinations this run sampled and
// measured, after every load's retries, which is why the table size is on the
// line: dns=6/6 is six of seven asked and answered, not a six-entry class. The
// rest appear only when there is something to say: retried= counts the loads
// that passed only on a later attempt; not_measured= the loads whose tunnel
// was gone for their last attempt and could not be re-created in time (they
// are in no tally); short= the classes too thin, for the provider's place, to
// fill their sample; canary=passed/loaded the unscored canaries; and
// exit=unobserved is retained only for legacy results carrying IpEchoErr; the
// sampled-only path does not treat optional exit absence as failure. The address itself
// is never on the line: the server does not store it either, and a log is a
// worse place for it.
//
// Class order is Classes, never map iteration order, so successive passes are
// diffable. Classes absent from the sample are omitted; a class present in the
// sample but absent from Classes is appended in sorted order rather than
// silently dropped.
func (self *Result) Summary() string {
	if self == nil {
		return "ok=0/0"
	}
	// The classes present in ByClass: the declared ones first, in Classes
	// order, then any others sorted, so a class added to the table but not to
	// Classes still shows up.
	orderedClasses := make([]Class, 0, len(self.ByClass))
	declaredClasses := map[Class]bool{}
	for _, c := range Classes {
		declaredClasses[c] = true
		if _, ok := self.ByClass[c]; ok {
			orderedClasses = append(orderedClasses, c)
		}
	}
	var extraClassNames []string
	for c := range self.ByClass {
		if !declaredClasses[c] {
			extraClassNames = append(extraClassNames, string(c))
		}
	}
	sort.Strings(extraClassNames)
	for _, name := range extraClassNames {
		orderedClasses = append(orderedClasses, Class(name))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ok=%d/%d", self.OkCount, self.Total)
	for _, c := range orderedClasses {
		s := self.ByClass[c]
		fmt.Fprintf(&b, " %s=%d/%d", c, s.Ok, s.Total)
	}
	if 0 < self.TableTotal {
		fmt.Fprintf(&b, " table=%d", self.TableTotal)
	}
	if retried := self.Retried(); 0 < retried {
		fmt.Fprintf(&b, " retried=%d", retried)
	}
	if 0 < self.NotMeasured {
		fmt.Fprintf(&b, " not_measured=%d", self.NotMeasured)
	}
	if 0 < len(self.ShortClasses) {
		short := make([]string, 0, len(self.ShortClasses))
		for _, c := range self.ShortClasses {
			short = append(short, string(c))
		}
		fmt.Fprintf(&b, " short=%s", strings.Join(short, ","))
	}
	if canaries := len(self.CanaryPassedNames()) + len(self.CanaryFailedNames()); 0 < canaries {
		fmt.Fprintf(&b, " canary=%d/%d", len(self.CanaryPassedNames()), canaries)
	}
	if self.IpEchoErr != "" {
		b.WriteString(" exit=unobserved")
	}
	if self.TlsAuthenticationFailure {
		b.WriteString(" tls_authentication_failure=true")
	}
	return b.String()
}

// Lists the measured, scored destinations that failed every
// attempt, in table order. The summary line says how many failed; this says
// which, which is what turns a log line into something actionable -- and with
// a sampled table it is the only way to know which destinations a run actually
// asked for. It is also what the server's per-site failure share is computed
// from (GEOMAP §11.4), which is why a load that was not measured, or a canary,
// is never in it.
func (self *Result) FailedNames() []string {
	return self.names(func(c CheckResult) bool { return !c.Ok && !c.NotMeasured && !c.Canary })
}

// FailureStageSummary is a finite, identity-free diagnostic of the final
// attempt for each failed load. It is logged before the fleet batch guard, so
// a guarded 0/N run still distinguishes a dial failure from a response or
// policy failure. Unknown or malformed stages are not copied into logs.
func (self *Result) FailureStageSummary() string {
	if self == nil {
		return ""
	}
	return failureStageSummary(self.Checks)
}

func failureStageSummary(checks []CheckResult) string {
	order := []string{"dial_dns", "dial_tcp", "dial_dns_or_socket", "tls", "policy", "request_build", "request_dns_timeout", "request_dial_timeout", "request_tls_timeout", "request_connect_timeout", "request_write_timeout", "request_response_timeout", "request_timeout", "request_canceled", "request_eof", "request_unknown", "response_body", "response_judgment", "tunnel_unavailable", "local_control_registration", "local_contract_acquisition", "local_transport_admission", "run_ended", "unknown"}
	counts := map[string]int{}
	for _, check := range checks {
		if check.Ok {
			continue
		}
		stage := check.FailureStage
		switch stage {
		case "dial_dns", "dial_tcp", "dial_dns_or_socket", "tls", "policy", "request_build", "request_dns_timeout", "request_dial_timeout", "request_tls_timeout", "request_connect_timeout", "request_write_timeout", "request_response_timeout", "request_timeout", "request_canceled", "request_eof", "request_unknown", "response_body", "response_judgment", "tunnel_unavailable", "local_control_registration", "local_contract_acquisition", "local_transport_admission", "run_ended":
		default:
			stage = "unknown"
		}
		counts[stage]++
	}
	parts := make([]string, 0, len(counts))
	for _, stage := range order {
		if count := counts[stage]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", stage, count))
		}
	}
	return strings.Join(parts, ",")
}

// Lists the loads the run could not measure, in table order.
func (self *Result) NotMeasuredNames() []string {
	return self.names(func(c CheckResult) bool { return c.NotMeasured && !c.Canary })
}

// Lists the canaries that passed, in table order: with CanaryFailedNames,
// what the server reads to tell whether a site works again from a place it is
// marked incompatible with. A canary that was not measured is in neither.
func (self *Result) CanaryPassedNames() []string {
	return self.names(func(c CheckResult) bool { return c.Canary && c.Ok })
}

// Lists the canaries that were measured and did not pass, in table order; see
// CanaryPassedNames.
func (self *Result) CanaryFailedNames() []string {
	return self.names(func(c CheckResult) bool { return c.Canary && !c.Ok && !c.NotMeasured })
}

// Lists the names of the checks want selects, in table order; nil for a nil
// result.
func (self *Result) names(want func(CheckResult) bool) []string {
	if self == nil {
		return nil
	}
	var names []string
	for _, c := range self.Checks {
		if want(c) {
			names = append(names, c.Name)
		}
	}
	return names
}
