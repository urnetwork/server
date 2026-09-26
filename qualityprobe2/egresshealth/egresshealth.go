// Package egresshealth checks whether a provider actually carries traffic to
// the real internet, across several independent classes of destination.
//
// All network access goes through an injected *http.Client; this package never
// constructs a client, transport, or dialer. fleetprobe supplies a client whose
// only dial boundary is the selected provider's userspace TUN, with no host
// fallback. A broken tunnel therefore fails the request even when the host has
// ordinary LAN egress. The standalone command's confinement check remains
// defense in depth; correctness does not depend on a special host route.
//
// # Why this exists
//
// Provider reliability scoring on the server is presence-based:
// reliabilityRunningAggSql counts reported time blocks and sums
// 1.0/valid_client_count, and never consults delivered bytes. A provider that
// stays connected 24/7 while blackholing every byte therefore scores perfectly
// and stays selectable. That is not hypothetical -- a mainnet capture showed a
// provider accepting 87 KB and returning 0 bytes while connected = true and
// valid = true.
//
// It is also, since GEOMAP step 7, the prober's only instrument. What real
// sites do with an exit is the only verdict the prober records about it: no
// ip-intelligence source is consulted for anything (D24). Where the exit is
// comes from the run's warm-up -- the operator's own /ip echo, whose address
// the server places with its own GeoLite2 -- and nothing else.
//
// # Classes
//
// Destinations are grouped so a partial failure is diagnosable. "ok=44/50"
// alone says nothing; "dns=6/6 cdn=0/10 site=26/26" says the tunnel carries
// bytes and resolves names but is being refused by content providers, which
// is the datacenter-IP-rejection case -- a completely different fault from a
// total blackhole (ok=0/50), and a different fault again from one flaky
// destination.
//
// The table deliberately spreads across different operators within each class,
// so a provider that special-cases one vendor's ranges cannot pass a class.
//
// Every class is scored. The sites that used to form an unscored "reputation"
// class -- large properties known to refuse addresses they take for
// datacenters -- are ordinary site destinations now, sampled and scored like
// the rest, and their refusals are failures like any other: a site a user
// cannot reach through an exit is a site a user cannot reach, whatever the
// site's reason (GEOMAP §10.3).
//
// # Sampling: the table is large, a run is not
//
// The table is 139 destinations. A run fetches a bounded random sample of each
// class rather than the whole thing (see sampleSizes for the arithmetic), so
// coverage accumulates across runs instead of being paid on every one. The
// server's pool (see Pool), when there is one, is sampled the same way.
//
// This is one code path with one set of constants for every deployment. There
// is no "small table for mainstream, big table for beta" switch and there must
// not be one: a knob that only one environment exercises is how a gap goes
// unnoticed here -- the untested branch is the one that runs against 100k
// providers.
//
// Sampling also buys a property a fixed table cannot have, and it is a security
// property rather than a cost one. A provider cannot know which destinations it
// will be asked for, because the draw happens at run time from the prober's own
// randomness. Whitelisting a handful of well-known hosts -- which defeats any
// fixed table, and defeated the fixed nine-entry table this descends from --
// now fails: to pass reliably a provider has to carry traffic to essentially
// the whole table, which is the thing being measured. That is a reason to
// prefer sampling a wide table over simply carrying a narrow one.
//
// # Every load gets its tries, spaced
//
// A destination is fetched up to Options.LoadAttempts times (default 3) and
// fails only when every attempt failed. After a failed attempt the next one
// waits a random delay drawn from an exponential distribution with mean
// Options.LoadRetryMeanInterval (default 5 minutes), capped at three times the
// mean, so a site's momentary block, a rate limit or a flapping path is not hit
// three times in one second, and the requests to any one site look like a
// person coming back to it rather than a scanner. A TLS-authentication failure
// is terminal: a forged certificate is a failure whatever a retry does.
//
// Each load's retry chain is its own goroutine and holds a concurrency slot
// only while it fetches, so the loads of a run interleave rather than queue: a
// run whose loads all pass finishes in its first round, and one with a site
// that keeps failing spans about ten to fifteen minutes. Its tunnel stays open
// that long and mostly idle, which is what the prober's per-shard concurrency
// is sized against (see fleetprobe).
//
// # The warm-up
//
// The first fetch of every run, and of every blackhole check, is the
// operator's /ip echo (Options.IpEchoUrl) with its own timeout -- see warmUp
// for why the tunnel needs it. It is never scored. Its answer is the exit
// address (Result.ExitIp), which is all the location submission carries.
//
// # DNS
//
// The dns class is seven DNS-over-HTTPS endpoints. It is not, and cannot
// currently be, a test of resolvers as such: the owner's list names 23 bare
// resolver addresses (8.8.8.8, 1.1.1.1, ...), and a resolver is queried over
// UDP/53 or TCP/53, which this package has no way to reach -- the tunnel is
// exposed to it as an *http.Client and nothing else. Genuine resolver coverage
// needs a UDP path through the tunnel, which is a different piece of work; the
// bare-IP rows are ignored here rather than fetched over http, which would test
// something else entirely and pass or fail for reasons unrelated to resolution.
//
// What the DoH entries do prove is that a name was resolved end to end through
// the tunnel, because every one of them parses its answer (see verifyDnsJson).
// A captive portal answers 200 with bytes; only an answer section proves
// resolution.
//
// # Ambiguity this cannot resolve on its own
//
// Name resolution is a shared precondition: the tunnel resolves every hostname
// through in-tunnel DoH (connect's DefaultDnsResolverSettings, which uses
// 1.1.1.1, 8.8.8.8, 9.9.9.9 and 208.67.222.222), and providertunnel
// deliberately disables the off-tunnel fallback. So a run that comes back 0/50
// is "this provider carried nothing useful", which covers both a blackhole and
// an in-tunnel DoH failure. The per-check Err strings are what separate them:
// a resolution failure names the lookup, a blackhole times out on the request.
// Do not read 0/50 as proof of a blackhole without them.
//
// # Byte budget
//
// Each attempt is a small GET whose body read is capped -- per destination via
// Destination.MaxBytes, and never above MaxBodyBytes, which is 1024 bytes --
// and then closed. The arithmetic for one attempt of every sampled load is on
// sampleSizes and is asserted by TestWorstCaseBytesPerRunFitsTheBudget:
//
//	dns           6 x  768 =  4608
//	connectivity  8 x  256 =  2048
//	cdn          10 x 1024 = 10240
//	site         26 x 1024 = 26624
//	                        ------
//	per attempt round      = 43520 bytes = 42.5 KiB
//
// A retry reads at most the same cap again, so a run in which every load fails
// every attempt after reading its whole cap reads at most three times that,
// 127.5 KiB, plus the warm-up's maxIpEchoBytes.
//
// No request carries a Range header any more (see neverSent), so the capped
// read is the only bound, and it bounds what is kept rather than what is sent:
// a server can put up to one TCP receive window in flight before the capped
// read closes the connection. For most of the table that is the whole response
// anyway -- front pages and robots.txt files a few KiB long, gzip-encoded now
// that the transport may ask for it -- but two cdn entries are large assets
// (cachefly's 10 MB test file and the AWS SDK bundle), and each can cost one
// window on the provider's link when it is drawn. That is the price of not
// sending byte-range requests, which bot managers refuse.
//
// This rides the same budget as the server's active bandwidth probe, which
// spends model.MaxProviderBandwidthBytesPerProbe = 16 MiB per probe (8
// parallel streams of 2 MiB -- see bandwidth.StreamCount for why one stream
// measured the congestion window rather than the provider). A health run's
// capped reads are under 1% of one bandwidth probe; its wire cost, with the
// windows above, stays a small fraction of one.
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

// How many destinations of each class one run draws. It is a
// package constant, identical in every deployment -- see the package comment on
// why there is no per-environment knob.
//
// The sizes are set by what the server reads, not by what a run can afford.
// The one-in-ten rule and the egress index read runs of at least 50 scored
// loads (GEOMAP §10.5, MinScoredLoads): at the 26 loads a run used to draw, the
// one-in-ten line was a two-failure line, and one site's policy was a
// provider's verdict. So the sizes sum to 50, roughly doubling the old
// 4/5/5/12:
//
//	dns 6 of 7, connectivity 8 of 14, cdn 10 of 18, site 26 of 100
//
// dns stops at six because the class holds seven: a sample must be drawn from
// more than it takes, or it is a fixed table wearing a sample's name
// (TestSampleSizesAreDeclaredForEveryClass), and six of seven still leaves a
// provider unable to know which DoH operator it will not be asked for. The
// site class takes what dns cannot, being the widest pool and the class whose
// verdict the index weighs most directly.
//
// The wall clock no longer binds these numbers. It used to -- a whole run had
// to fit in one probe timeout -- but a run now spans minutes by design (every
// load gets its spaced tries), and a load holds a concurrency slot only while
// it fetches. A first round of 50 at DefaultConcurrency is ceil(50/6) = 9
// rounds, 90 seconds at the worst, against a run that waits five minutes
// between attempts. Options.RunBudget is the whole bound.
//
// Bytes stay cheap: 43,520 bytes for one attempt of every load, on the package
// comment.
//
// Three is the floor for any class, and none of these is near it. A class
// verdict has to separate one flaky endpoint from a class-wide fault: at 1 the
// class is a single destination wearing a class name, at 2 one flake is half
// the class and cdn=1/2 says nothing, and at 3 "cdn=0/3" means three
// independently operated endpoints all refused in the same run -- which is a
// statement.
//
// Coverage is a rate, not a promise: site draws 26 of 100, so a given site
// destination is asked for on about one run in four. What accumulates quickly
// is fleet-wide coverage, because every provider draws independently.
//
// A class with no entry here is probed whole. That is the safe direction: a
// class added to the table without a sample size costs visible bytes rather
// than silently never being probed.
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

// The built-in table: the owner's curated 159-row list,
// minus the rows that cannot be checked over an *http.Client, plus the seven
// DoH endpoints. A run does not fetch it -- see sampleSizes. It is the seed the
// server's pool starts from and the fallback when the pool cannot be fetched
// (see Pool); a run over the pool follows every rule below, which
// Destination.Validate restates for data.
//
// Every URL is https and on the default port 443. That is a hard requirement,
// not a coincidence -- the confinement self-check dials one fixed port
// (cmd/egress-prober's confinementPort), so a destination on any other port
// would silently fall outside the check. TestEveryDestinationIsHttpsOn443
// enforces it.
//
// Every entry was measured from a datacenter host with the identifying user
// agent this package used to send, and its contract is what that measurement
// says: a 200 with a body takes the default ExpectBody rule, and 202/204/3xx
// declare their status. The rules the measurements forced, in the order they
// matter:
//
//   - A 4xx or 5xx is never declared with ExpectStatus. Six of the site
//     entries that came from the dissolved reputation class answer 403/401
//     from a datacenter exit; declaring those would turn a refusal into a
//     pass, which is the one thing a site load must never do.
//
//   - A 200 with a zero-length body cannot be declared either, because it is
//     the blackhole signature itself. Two endpoints measured that way
//     (d1.awsstatic.com and cachefly.cachefly.net's bare host); both are
//     re-pointed at a url on the same operator that serves a real body, and
//     neither is whitelisted by status. See their entries.
//
//   - A destination whose status is not stable cannot be declared, and this is
//     not hypothetical. Three of the owner's 21 zero-body endpoints answered
//     differently when this table was built, from the same host and address
//     days later: netflix 302 -> 200, cnn 302 -> 200, hulu 302 -> 301. A
//     fourth (timesofindia) went the other way, answering 301-with-no-body
//     where the staged run had a body. Those four are re-pointed at
//     /robots.txt, which is a real body and does not move. The exits that will
//     actually run this are providers in arbitrary countries, so a
//     geography-dependent redirect status is a false failure waiting to
//     happen. The rest of the zero-body list is declared as measured, per the
//     owner's instruction.
//
//   - Redirects are declared, never chased: the production client refuses to
//     follow them (providertunnel's CheckRedirect), which is what makes a 3xx
//     an answer about this url rather than about wherever it pointed.
//
// Other choices worth recording, because the obvious pick was wrong in a way
// only measurement showed:
//
//   - The nine http:// rows in the list are here as https, all of them verified
//     answering the same status over TLS (the five generate_204 endpoints, the
//     three fixed-text portal checks, archive.ubuntu.com). A plaintext
//     destination is not an option: it could be forged by the provider on the
//     path, which is precisely the party under test. Two rows could not survive
//     that: speedtest.tele2.net has no https listener at all (measured: no
//     connection), and the "Example.com Plain HTTP" row duplicates the https
//     one. Both are dropped.
//
//   - ifconfig.me is asked for /ip, not /. The bare host serves an HTML page to
//     anything that does not look like curl -- measured, 10915 bytes of it --
//     and /ip always answers with the address, which is what verifyIpText can
//     actually prove.
//
//   - one.one.one.one and speed.cloudflare.com are in ClassSite rather than
//     ClassConnectivity, where the source list files them. They answer with an
//     ordinary web page rather than a fixed portal-detection token, so they
//     cannot carry this class's body check, and a connectivity entry without
//     one would pass for a captive portal.
//
//   - dns.alidns.com serves the JSON form on /resolve only. Its /dns-query path
//     answers `400 no 'dns' query parameter found` -- it is wire-format only.
//     doh.dns.sb is the mirror image: /dns-query serves JSON, /resolve is a 301.
//     The path is not interchangeable between operators and cannot be guessed.
//
//   - Quad9, OpenDNS, CleanBrowsing, Mullvad and Control D were all rejected as
//     DoH entries. Quad9 and Mullvad serve only RFC 8484 wire format on 443
//     (400 to a JSON GET) and Quad9's JSON API is on port 5053, which breaks the
//     443-only rule above; Control D answers 200 with a non-JSON body, which is
//     precisely the shape a body check exists to reject.
//
//   - Operators repeat inside a class, and that is tolerated here where the
//     narrow table refused it: connectivity carries three Google-operated
//     generate_204 hostnames and three Cloudflare-operated endpoints, because
//     the owner's list carries them and dropping them would narrow the pool a
//     sample is drawn from. What protects the class is the draw: eight of
//     fourteen, chosen per run, so no single operator can be relied on to
//     appear. Do not read "14 destinations" as "14 operators".
var destinations = []Destination{
	// DNS-over-HTTPS, JSON GET form, seven distinct operators including two
	// Chinese ones (dns.alidns.com, doh.pub) for deliberate jurisdictional
	// diversity: a provider whose upstream filters western resolvers, or the
	// reverse, shows up as a partial class rather than a clean pass. A provider
	// that blocks DoH breaks name resolution for every client that uses it.
	//
	// Every entry carries a dns_json body check: a 200 with bytes is not proof
	// that a name was resolved, because a captive portal or an interception box
	// returns exactly that. Only a parseable answer is. These bodies are small,
	// and the cap is sized so a whole answer always fits: a truncated one would
	// not parse.
	{
		Name:     "cloudflare-doh",
		Class:    ClassDns,
		Url:      "https://cloudflare-dns.com/dns-query" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	{
		Name:     "google-doh",
		Class:    ClassDns,
		Url:      "https://dns.google/resolve" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	{
		Name:     "adguard-doh",
		Class:    ClassDns,
		Url:      "https://dns.adguard-dns.com/resolve" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	{
		Name:     "dnssb-doh",
		Class:    ClassDns,
		Url:      "https://doh.dns.sb/dns-query" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	{
		Name:     "nextdns-doh",
		Class:    ClassDns,
		Url:      "https://dns.nextdns.io/dns-query" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	{
		Name:     "alidns-doh",
		Class:    ClassDns,
		Url:      "https://dns.alidns.com/resolve" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	{
		Name:     "dnspod-doh",
		Class:    ClassDns,
		Url:      "https://doh.pub/dns-query" + dnsQuery,
		Headers:  map[string]string{"Accept": acceptDnsJson},
		MaxBytes: maxDnsBytes,
		Verify:   BodyCheck{Kind: BodyCheckDnsJson},
	},
	// Connectivity checks: the endpoints operating systems use to detect captive
	// portals, plus the echo services that answer with the exit address. None is
	// authenticated, none is behind anti-bot machinery, and all of them answer in
	// well under 256 bytes.
	//
	// The generate_204 entries are the reason Expect exists: 204 with no body is
	// the correct answer, and the ExpectBody rule would have scored all of them
	// as failures. They are also the strictest entries in the table -- an exact
	// status match, so a provider that synthesizes a bare 200 fails them.
	//
	// Every body-bearing entry carries a body check, for the captive-portal case
	// these endpoints were literally designed to detect: a portal returns 200
	// with a login page, which is non-empty and would otherwise pass. That rule
	// is what moved one.one.one.one and speed.cloudflare.com out of this class
	// -- they answer with an ordinary page there is nothing fixed to check.
	//
	// These bodies are smaller than the cap already, so the capped read takes
	// them whole.
	{
		Name:     "google-generate-204",
		Class:    ClassConnectivity,
		Url:      "https://www.google.com/generate_204",
		Expect:   ExpectStatus,
		Status:   204,
		MaxBytes: maxConnectivityBytes,
	},
	{
		Name:     "google-connectivitycheck",
		Class:    ClassConnectivity,
		Url:      "https://connectivitycheck.gstatic.com/generate_204",
		Expect:   ExpectStatus,
		Status:   204,
		MaxBytes: maxConnectivityBytes,
	},
	{
		Name:     "gstatic-204",
		Class:    ClassConnectivity,
		Url:      "https://www.gstatic.com/generate_204",
		Expect:   ExpectStatus,
		Status:   204,
		MaxBytes: maxConnectivityBytes,
	},
	{
		Name:     "apple-captive-portal",
		Class:    ClassConnectivity,
		Url:      "https://captive.apple.com/hotspot-detect.html",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckContains, Text: "Success"},
	},
	{
		Name:     "ubuntu-connectivity-check",
		Class:    ClassConnectivity,
		Url:      "https://connectivity-check.ubuntu.com",
		Expect:   ExpectStatus,
		Status:   204,
		MaxBytes: maxConnectivityBytes,
	},
	{
		Name:     "firefox-detectportal",
		Class:    ClassConnectivity,
		Url:      "https://detectportal.firefox.com/success.txt",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckContains, Text: "success"},
	},
	{
		Name:     "gnome-nm-check",
		Class:    ClassConnectivity,
		Url:      "https://nmcheck.gnome.org/check_network_status.txt",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckContains, Text: "online"},
	},
	{
		Name:     "cloudflare-cp-204",
		Class:    ClassConnectivity,
		Url:      "https://cp.cloudflare.com/generate_204",
		Expect:   ExpectStatus,
		Status:   204,
		MaxBytes: maxConnectivityBytes,
	},
	{
		Name:     "cloudflare-trace",
		Class:    ClassConnectivity,
		Url:      "https://1.1.1.1/cdn-cgi/trace",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckContains, Text: "ip="},
	},
	{
		Name:     "aws-checkip",
		Class:    ClassConnectivity,
		Url:      "https://checkip.amazonaws.com",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckIpText},
	},
	{
		Name:     "ifconfig-me",
		Class:    ClassConnectivity,
		Url:      "https://ifconfig.me/ip",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckIpText},
	},
	{
		Name:     "icanhazip",
		Class:    ClassConnectivity,
		Url:      "https://icanhazip.com",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckIpText},
	},
	{
		Name:     "ipify",
		Class:    ClassConnectivity,
		Url:      "https://api.ipify.org",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckIpText},
	},
	// The list's ipinfo row is not here. ipinfo.io is an ip-intelligence
	// vendor, and the prober consults no ip-intelligence source for anything
	// (GEOMAP D24) -- not even as a reachability target, where a vendor that
	// classifies the exit on sight could answer it differently from the echo
	// services beside it. The class keeps four other echo services.
	{
		Name:     "example-com-https",
		Class:    ClassConnectivity,
		Url:      "https://example.com",
		MaxBytes: maxConnectivityBytes,
		Verify:   BodyCheck{Kind: BodyCheckContains, Text: "Example Domain"},
	},

	// Content delivery: CDN edges, distribution mirrors and bulk-download hosts.
	// Eighteen destinations across Cloudflare, Fastly, jsDelivr, unpkg, Google,
	// Microsoft, CloudFront, KeyCDN, CDN77, Sucuri, CacheFly, OVH and five OS
	// distribution mirrors. This is the class that fails when a provider's egress
	// range sits on a CDN blocklist.
	//
	// The four asset urls (cdnjs, googleapis, jsdelivr, sdk.amazonaws) are
	// version-pinned on purpose -- an unversioned url would move under us -- with
	// the tradeoff that if a vendor ever prunes one it becomes a permanent 404
	// and a permanent false failure. If one named entry fails across the whole
	// fleet while its class passes, suspect the URL before suspecting the
	// providers. They are asset urls rather than front pages because a CDN entry
	// should prove the edge serves bytes, which a marketing page fronted by the
	// same CDN does not do as directly.
	{
		Name:     "cloudflare-cdn",
		Class:    ClassCdn,
		Url:      "https://cdnjs.cloudflare.com/ajax/libs/normalize/8.0.1/normalize.min.css",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "cloudflare-sized-1kb",
		Class:    ClassCdn,
		Url:      "https://speed.cloudflare.com/__down?bytes=1024",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "fastly",
		Class:    ClassCdn,
		Url:      "https://www.fastly.net",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "jsdelivr-fastly-mirror",
		Class:    ClassCdn,
		Url:      "https://fastly.jsdelivr.net/npm/normalize.css@8.0.1/normalize.min.css",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "unpkg",
		Class:    ClassCdn,
		Url:      "https://unpkg.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "google-hosted-libraries",
		Class:    ClassCdn,
		Url:      "https://ajax.googleapis.com/ajax/libs/jquery/3.7.1/jquery.min.js",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "microsoft-azure-cdn",
		Class:    ClassCdn,
		Url:      "https://ajax.aspnetcdn.com",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		// d1.awsstatic.com, which the list names, answered 200 with a zero-length
		// body on every measurement -- indistinguishable from the blackhole
		// signature, so it cannot be judged by status and must not be
		// whitelisted into one. Its /robots.txt answers 403. This is the same
		// operator (CloudFront) on the url the previous table measured serving a
		// real body. It is a multi-megabyte bundle, so without a Range header the
		// edge starts sending all of it: the capped read keeps the first KiB and
		// closes, and up to one receive window of the rest can still cross the
		// provider's link first (see the byte budget on the package). A smaller
		// CloudFront-served object in the server's pool would retire that cost.
		Name:     "amazon-cloudfront",
		Class:    ClassCdn,
		Url:      "https://sdk.amazonaws.com/js/aws-sdk-2.1691.0.min.js",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "keycdn",
		Class:    ClassCdn,
		Url:      "https://www.keycdn.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "cdn77",
		Class:    ClassCdn,
		Url:      "https://www.cdn77.com",
		MaxBytes: maxAssetBytes,
	},
	{
		// Re-pointed at robots.txt: the bare host ignores Range and returns
		// 399,407 B. Measured on the beta fleet it was the single largest source
		// of failures (14 of 40 providers) -- not because those providers were
		// unhealthy, but because moving 390 KB inside the per-request timeout is
		// a bandwidth test wearing a health test's clothes. robots.txt is 24
		// bytes, so the capped read takes it whole.
		Name:     "sucuri-cdn",
		Class:    ClassCdn,
		Url:      "https://www.sucuri.net/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		// The list's bare host answered 200 with a zero-length body. CacheFly's own
		// test asset exists to be fetched. It used to be asked for with a Range
		// header, which it honors (206, 1024 bytes), so the 10 MB behind it never
		// left their edge. Without one the capped read still keeps only the first
		// KiB, but up to one receive window of the file can cross the provider's
		// link before the close lands -- the largest wire cost left in the table,
		// and the first entry the server's pool should replace with a small
		// object on the same edge.
		Name:     "cachefly",
		Class:    ClassCdn,
		Url:      "https://cachefly.cachefly.net/10mb.test",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "ovh-proof-eu",
		Class:    ClassCdn,
		Url:      "https://proof.ovh.net",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "kernel-org-mirrors",
		Class:    ClassCdn,
		Url:      "https://mirrors.kernel.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "debian-cdn",
		Class:    ClassCdn,
		Url:      "https://deb.debian.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "ubuntu-archive-plain-http",
		Class:    ClassCdn,
		Url:      "https://archive.ubuntu.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "alpine-cdn",
		Class:    ClassCdn,
		Url:      "https://dl-cdn.alpinelinux.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "fedora-downloads",
		Class:    ClassCdn,
		Url:      "https://dl.fedoraproject.org",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},

	// Ordinary web properties: search, social, video, commerce, developer
	// infrastructure, news, reference, productivity, gaming, and the regional
	// properties that carry the same traffic outside the US and EU. site=0/26
	// means the tunnel is not carrying ordinary web traffic at all.
	//
	// Chosen for availability -- every one of them answered on four consecutive
	// runs from a datacenter host -- with the exception of the last eight,
	// below, which were chosen for the opposite reason.
	//
	// The 3xx entries here declare the status they were measured answering. Read
	// the rule on the table above before adding another: a declared status is
	// weaker evidence than a body, and a status that varies by the exit's
	// geography is not declarable at all.
	{
		Name:     "cloudflare-one-one-one-one",
		Class:    ClassSite,
		Url:      "https://one.one.one.one",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "cloudflare-speed-test",
		Class:    ClassSite,
		Url:      "https://speed.cloudflare.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "google",
		Class:    ClassSite,
		Url:      "https://www.google.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "bing",
		Class:    ClassSite,
		Url:      "https://www.bing.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "yahoo",
		Class:    ClassSite,
		Url:      "https://www.yahoo.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "duckduckgo",
		Class:    ClassSite,
		Url:      "https://duckduckgo.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "baidu",
		Class:    ClassSite,
		Url:      "https://www.baidu.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "yandex",
		Class:    ClassSite,
		Url:      "https://yandex.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "facebook",
		Class:    ClassSite,
		Url:      "https://www.facebook.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "instagram",
		Class:    ClassSite,
		Url:      "https://www.instagram.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "twitter-x",
		Class:    ClassSite,
		Url:      "https://x.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "linkedin",
		Class:    ClassSite,
		Url:      "https://www.linkedin.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "tiktok",
		Class:    ClassSite,
		Url:      "https://www.tiktok.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "pinterest",
		Class:    ClassSite,
		Url:      "https://www.pinterest.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "snapchat",
		Class:    ClassSite,
		Url:      "https://www.snapchat.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "discord",
		Class:    ClassSite,
		Url:      "https://discord.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "telegram",
		Class:    ClassSite,
		Url:      "https://telegram.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "whatsapp",
		Class:    ClassSite,
		Url:      "https://www.whatsapp.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "mastodon",
		Class:    ClassSite,
		Url:      "https://mastodon.social",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "bluesky",
		Class:    ClassSite,
		Url:      "https://bsky.app",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "youtube",
		Class:    ClassSite,
		Url:      "https://www.youtube.com",
		MaxBytes: maxAssetBytes,
	},
	{
		// Re-pointed, and this is the finding that justifies the rule above: the
		// staged measurement recorded 302-with-no-body four times, and this host
		// measured 200 with 3.2 MB days later. Same host, same address. A status
		// that flips like that cannot be declared, and the exit that matters here
		// is a provider's, in an arbitrary country. robots.txt is 3790 bytes of
		// real body and does not move.
		Name:     "netflix",
		Class:    ClassSite,
		Url:      "https://www.netflix.com/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "twitch",
		Class:    ClassSite,
		Url:      "https://www.twitch.tv",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "vimeo",
		Class:    ClassSite,
		Url:      "https://vimeo.com",
		MaxBytes: maxAssetBytes,
	},
	{
		// hulu answers 302-with-empty-body on both the bare host and
		// /robots.txt, so it can never satisfy ExpectBody. Declared as the
		// redirect it actually is. Measured on the beta fleet: 4 of 40
		// providers failed on this entry alone before the fix.
		// Measured 302 from one vantage but 301 through provider tunnels: the
		// exact status depends on where the client exits, so it cannot be
		// declared. Reachability is the question this class actually asks.
		Name:     "hulu",
		Class:    ClassSite,
		Url:      "https://www.hulu.com",
		Expect:   ExpectReachable,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "disney",
		Class:    ClassSite,
		Url:      "https://www.disneyplus.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "spotify",
		Class:    ClassSite,
		Url:      "https://open.spotify.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "soundcloud",
		Class:    ClassSite,
		Url:      "https://soundcloud.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "dailymotion",
		Class:    ClassSite,
		Url:      "https://www.dailymotion.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "amazon",
		Class:    ClassSite,
		Url:      "https://www.amazon.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "ebay",
		Class:    ClassSite,
		Url:      "https://www.ebay.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "walmart",
		Class:    ClassSite,
		Url:      "https://www.walmart.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "alibaba",
		Class:    ClassSite,
		Url:      "https://www.alibaba.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "aliexpress",
		Class:    ClassSite,
		Url:      "https://www.aliexpress.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "target",
		Class:    ClassSite,
		Url:      "https://www.target.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "shopify",
		Class:    ClassSite,
		Url:      "https://www.shopify.com",
		MaxBytes: maxAssetBytes,
	},
	{
		// 206/1024B to a datacenter host, 302/0B through all 40 provider
		// tunnels: a locale redirect keyed on the exit ip. See ExpectReachable.
		Name:     "microsoft",
		Class:    ClassSite,
		Url:      "https://www.microsoft.com",
		Expect:   ExpectReachable,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "apple",
		Class:    ClassSite,
		Url:      "https://www.apple.com/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "github",
		Class:    ClassSite,
		Url:      "https://github.com/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "github-api",
		Class:    ClassSite,
		Url:      "https://api.github.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "gitlab",
		Class:    ClassSite,
		Url:      "https://gitlab.com",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "cloudflare",
		Class:    ClassSite,
		Url:      "https://www.cloudflare.com/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "aws",
		Class:    ClassSite,
		Url:      "https://aws.amazon.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "google-cloud",
		Class:    ClassSite,
		Url:      "https://cloud.google.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "digitalocean",
		Class:    ClassSite,
		Url:      "https://www.digitalocean.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "docker-hub",
		Class:    ClassSite,
		Url:      "https://hub.docker.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "npm-registry",
		Class:    ClassSite,
		Url:      "https://registry.npmjs.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "pypi",
		Class:    ClassSite,
		Url:      "https://pypi.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "rubygems",
		Class:    ClassSite,
		Url:      "https://rubygems.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "python-org",
		Class:    ClassSite,
		Url:      "https://www.python.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "go-dev",
		Class:    ClassSite,
		Url:      "https://go.dev",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "kernel-org",
		Class:    ClassSite,
		Url:      "https://www.kernel.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "mdn",
		Class:    ClassSite,
		Url:      "https://developer.mozilla.org",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},
	{
		// Re-pointed for the same reason as netflix: staged 302-with-no-body,
		// measured here as 200 with 4.9 MB.
		Name:     "cnn",
		Class:    ClassSite,
		Url:      "https://www.cnn.com/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "bbc",
		Class:    ClassSite,
		Url:      "https://www.bbc.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "new-york-times",
		Class:    ClassSite,
		Url:      "https://www.nytimes.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "the-guardian",
		Class:    ClassSite,
		Url:      "https://www.theguardian.com",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "ap-news",
		Class:    ClassSite,
		Url:      "https://apnews.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "al-jazeera",
		Class:    ClassSite,
		Url:      "https://www.aljazeera.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "deutsche-welle",
		Class:    ClassSite,
		Url:      "https://www.dw.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "france-24",
		Class:    ClassSite,
		Url:      "https://www.france24.com",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},
	{
		// The favicon, not the front page: the front page is 120 KB and Wikimedia's
		// ATS does not honor Range (measured: 200, whole asset), so it is the one
		// destination that would routinely exceed the wire budget even though the
		// read cap bounds what is kept.
		Name:     "wikipedia",
		Class:    ClassSite,
		Url:      "https://www.wikipedia.org/static/favicon/wikipedia.ico",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "wordpress",
		Class:    ClassSite,
		Url:      "https://wordpress.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "imdb",
		Class:    ClassSite,
		Url:      "https://www.imdb.com",
		Expect:   ExpectStatus,
		Status:   202,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "internet-archive",
		Class:    ClassSite,
		Url:      "https://archive.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "noaa-weather",
		Class:    ClassSite,
		Url:      "https://www.weather.gov",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "qq",
		Class:    ClassSite,
		Url:      "https://www.qq.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "sina",
		Class:    ClassSite,
		Url:      "https://www.sina.com.cn",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "taobao",
		Class:    ClassSite,
		Url:      "https://www.taobao.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "jd-com",
		Class:    ClassSite,
		Url:      "https://www.jd.com",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "mail-ru",
		Class:    ClassSite,
		Url:      "https://mail.ru",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "vk",
		Class:    ClassSite,
		Url:      "https://vk.com",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "naver",
		Class:    ClassSite,
		Url:      "https://www.naver.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "line",
		Class:    ClassSite,
		Url:      "https://line.me",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},
	{
		// Re-pointed: measured here as 301 with no body, while the staged run had
		// it answering with one. Unstable in the same way, caught from the other
		// direction (it is absent from the zero-body list).
		Name:     "times-of-india",
		Class:    ClassSite,
		Url:      "https://timesofindia.indiatimes.com/robots.txt",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "globo",
		Class:    ClassSite,
		Url:      "https://www.globo.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "mercadolibre",
		Class:    ClassSite,
		Url:      "https://www.mercadolibre.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "abc-australia",
		Class:    ClassSite,
		Url:      "https://www.abc.net.au",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "news24",
		Class:    ClassSite,
		Url:      "https://www.news24.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "zoom",
		Class:    ClassSite,
		Url:      "https://zoom.us",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "slack",
		Class:    ClassSite,
		Url:      "https://slack.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "dropbox",
		Class:    ClassSite,
		Url:      "https://www.dropbox.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "notion",
		Class:    ClassSite,
		Url:      "https://www.notion.so",
		Expect:   ExpectStatus,
		Status:   307,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "trello",
		Class:    ClassSite,
		Url:      "https://trello.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "atlassian",
		Class:    ClassSite,
		Url:      "https://www.atlassian.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "figma",
		Class:    ClassSite,
		Url:      "https://www.figma.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "steam",
		Class:    ClassSite,
		Url:      "https://store.steampowered.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "playstation",
		Class:    ClassSite,
		Url:      "https://www.playstation.com",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "xbox",
		Class:    ClassSite,
		Url:      "https://www.xbox.com",
		Expect:   ExpectStatus,
		Status:   307,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "nintendo",
		Class:    ClassSite,
		Url:      "https://www.nintendo.com",
		Expect:   ExpectStatus,
		Status:   301,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "roblox",
		Class:    ClassSite,
		Url:      "https://www.roblox.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "riot-games",
		Class:    ClassSite,
		Url:      "https://www.riotgames.com",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},

	// The former reputation class, dissolved into site (GEOMAP §11.3) with its
	// contracts unchanged. Six of these eight refused a datacenter exit outright
	// when the table was built (403, except reuters at 401), and on main they
	// refused over 2,600 of 6,577 healthy providers each under the probe's old
	// self-describing request. They were kept out of the score as a verdict on
	// a vendor's ip feed rather than on the exit. They are scored now because
	// that distinction does not survive contact with a user: a site the exit
	// cannot reach is a site the user cannot reach, whatever the reason, and
	// the index pays for it exactly as it would for a site that timed out. The
	// browser-shaped request and the spaced retries are what should separate a
	// refusal of the exit from a refusal of the probe; the field data will say
	// which these were.
	//
	// Two carry a note. reddit answered three different ways from one host and
	// address in one day (403 to curl's default agent, 200 to the Go prober
	// through a provider, 206 to curl carrying the prober's old agent): it is
	// the clearest case of a site judging the client as much as the address.
	// stackoverflow never refused anything -- it redirects everyone.
	{
		Name:     "akamai",
		Class:    ClassSite,
		Url:      "https://www.akamai.com",
		MaxBytes: maxAssetBytes,
	},
	{
		// The staged runs recorded ecosia as not refusing; this host measured 403
		// with 4472 bytes on the same day the table was built. An answer that
		// differs run to run is what the retries are for.
		Name:     "ecosia",
		Class:    ClassSite,
		Url:      "https://www.ecosia.org",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "reddit",
		Class:    ClassSite,
		Url:      "https://www.reddit.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "etsy",
		Class:    ClassSite,
		Url:      "https://www.etsy.com",
		MaxBytes: maxAssetBytes,
	},
	{
		// 302 with an empty body, four staged runs and one here, and it keeps
		// that declared contract. stackoverflow did not refuse the datacenter
		// exit it was filed under reputation for -- it redirects everyone. Before
		// this entry declared its status it failed every run, which reads in a log
		// exactly like a refusal of the exit and is not one. Check the status
		// before reading a stack-overflow failure as anything about the exit.
		Name:     "stack-overflow",
		Class:    ClassSite,
		Url:      "https://stackoverflow.com",
		Expect:   ExpectStatus,
		Status:   302,
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "reuters",
		Class:    ClassSite,
		Url:      "https://www.reuters.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "canva",
		Class:    ClassSite,
		Url:      "https://www.canva.com",
		MaxBytes: maxAssetBytes,
	},
	{
		Name:     "epic-games",
		Class:    ClassSite,
		Url:      "https://www.epicgames.com",
		MaxBytes: maxAssetBytes,
	},
}

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
	Name                     string
	Class                    Class
	Ok                       bool
	StatusCode               int   // 0 when the request never produced a response
	ByteCount                int64 // bytes of body actually read, capped; set on failures too
	Latency                  time.Duration
	Err                      string // "" when OK
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
	// attempt: the tunnel died under the attempt, or it could not be
	// re-created in time (see Path). It is neither a pass nor a failure -- a
	// tunnel that went away says nothing about the site -- and it is left out
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
	// Every sampled destination's outcome after retries, in table
	// order.
	Checks []CheckResult
	// The address the operator's /ip echo saw the warm-up come
	// from: the provider's exit, in canonical form, and the only thing the
	// location submission carries. "" when no echo was configured or it did
	// not answer (IpEchoErr says why).
	ExitIp string
	// When the echo answered, on the Options clock.
	ExitObservedAt time.Time
	// Why the warm-up yielded no exit address; "" when it did,
	// or when no echo was configured.
	IpEchoErr string
	// True when any sampled HTTPS peer -- or the
	// warm-up's, which is the operator's own api host -- failed
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

	// The table the run draws from. Nil means the built-in
	// table, which stays the seed of the server's pool and the fallback when
	// the pool cannot be fetched (see Pool, fleetprobe.LoadPool).
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

	// The operator's /ip echo, fetched first, as the warm-up, on
	// every run and every blackhole check: <api url> + IpEchoPath, reached
	// through the tunnel. Empty skips the warm-up, and with it the exit
	// address -- tests use that; the prober always sets it.
	IpEchoUrl string
	// Bounds the warm-up alone. It is its own setting because
	// it absorbs the cold start (see warmUp), which a load's timeout is not
	// sized for. Zero or negative uses DefaultIpEchoTimeout.
	IpEchoTimeout time.Duration

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
}

// Defaults for Options. They are vars so tests can lower them.
var (
	// Bounds one attempt of one load. By the time a
	// load starts the warm-up has brought the path up, so it no longer has to
	// cover a cold start -- only an in-tunnel DoH resolution, a TCP connect and
	// a TLS handshake per request, since keep-alives are disabled. Treat it as
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
	// timeouts spread evenly across all classes, on providers whose warm-up
	// succeeded. That means lower it, not that the providers are bad.
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
	// Bounds the warm-up. It is the cold-start budget --
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
// It is a clean skip, not a failure. A prober pointed at an older server keeps
// probing and keeps logging health; it simply records none.
var ErrUnsupported = errors.New("egresshealth: the server does not implement the provider egress health endpoint")

// Runs a random sample of the destination table through client and
// returns the full pattern of what worked, after every load's tries.
//
// The table is Options.Destinations -- the server's pool, when the caller has
// one -- or the built-in table. The sample is drawn per call and per class
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
		// The sample and the canaries in table order: Checks, FailedNames and
		// the log line read in the order the table declares whatever was
		// drawn, canaries included.
		pickedNames := map[string]bool{}
		for _, d := range chosen {
			pickedNames[d.Name] = true
		}
		for _, d := range canaries {
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

// How many sequential rounds one attempt of every
// destination in the built-in table takes at AllConcurrency.
func RoundsForAllDestinations() int {
	return (len(destinations) + AllConcurrency - 1) / AllConcurrency
}

// How many loads one run of the built-in table makes: the sum
// of the per-class sample sizes, bounded by what the table actually holds.
func SamplePerRun() int {
	return SamplePerRunOf(destinations)
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
// and what Budget defaults to: the warm-up, then every load's every attempt in
// rounds of Concurrency -- as if every retry landed at the same moment, the
// worst the random spacing can produce -- and the longest spacing between
// attempts, which is the cap. At the defaults and a 50-load sample that is
// 60s + 3 x 9 x 10s + 2 x 15m, about 35 minutes; a run that passes everything
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
	if self.IpEchoUrl != "" {
		budget += self.ipEchoTimeout()
	}
	return budget
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

// Returns the table a run draws from: Destinations, or the built-in table.
func (self Options) table() []Destination {
	if self.Destinations != nil {
		return self.Destinations
	}
	return destinations
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
	exit := &exitRecord{}
	r := newRun(path, opts, opts.concurrency(), budget, opts.rng(), exit)
	r.warmUp(ctx)

	res.Checks = r.loadAll(ctx, dests)
	if err := parent.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	res.ExitIp, res.ExitObservedAt, res.IpEchoErr, res.TlsAuthenticationFailure = exit.read()

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
	r := CheckResult{Name: d.Name, Class: d.Class}
	start := now()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Url, nil)
	if err != nil {
		r.Latency = now().Sub(start)
		r.Err = err.Error()
		return r
	}
	applyHeaders(req, profile, d.Headers)

	resp, err := client.Do(req)
	if err != nil {
		r.Latency = now().Sub(start)
		r.Err = err.Error()
		r.TlsAuthenticationFailure = isTlsAuthenticationFailure(err)
		return r
	}
	// Closed after the capped read, whatever is left on the wire: the first
	// kilobyte is the whole question (see MaxBodyBytes and neverSent).
	defer resp.Body.Close()
	r.StatusCode = resp.StatusCode

	// The body is read even on an error status, and ByteCount is recorded
	// either way: a 403 with a page of explanation proves the tunnel carried
	// bytes in both directions and the content provider refused, which is a
	// different fault from nothing coming back at all.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, d.maxBytes()))
	r.ByteCount = int64(len(body))
	r.Latency = now().Sub(start)

	if readErr != nil {
		r.Err = readErr.Error()
		return r
	}
	if err := d.judge(resp.StatusCode, body); err != nil {
		r.Err = err.Error()
		return r
	}
	r.Ok = true
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

// Returns the host of every destination in the built-in
// table, de-duplicated and in table order.
//
// Nothing outside this package should keep a second copy of the endpoint
// list. The prober's container cannot resolve DNS, so the operator passes
// explicit addresses to the confinement self-check with -confinement-address,
// and this is how they obtain the host list to translate. A hand-maintained
// copy would drift on the first table change while the check kept reporting
// success.
//
// It covers the whole table, not a sample: any destination can be drawn on any
// run, so the confinement check has to prove every one of them unreachable
// directly, and the tunnel client has to be allowed to reach every one of them.
// The hosts of a server pool are HostsOf that pool.
func DestinationHosts() []string {
	return HostsOf(destinations)
}

// Returns the host of every destination in dests, de-duplicated and in
// order. It is the tunnel allowlist a run over dests needs.
//
// A URL that does not parse, or carries no host, is skipped rather than
// panicking -- the built-in table is a compile-time constant that
// TestDestinationHostsCoversEveryDestination holds, and a pool is refused at
// FetchPool unless every url parses.
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

// Returns a copy of the built-in table -- all of it, not one
// run's sample.
//
// A copy, not the table: a caller that mutated it -- or the Headers map inside
// an entry -- would silently change what every subsequent probe measures, and
// the drift would be invisible in the table's own source. Callers that only
// need the host list want DestinationHosts; callers sizing a run want
// SamplePerRun. The server seeds its pool from this.
func Destinations() []Destination {
	out := make([]Destination, len(destinations))
	for i, d := range destinations {
		out[i] = d
		if d.Headers != nil {
			headers := make(map[string]string, len(d.Headers))
			for k, v := range d.Headers {
				headers[k] = v
			}
			out[i].Headers = headers
		}
		if d.Incompatible != nil {
			out[i].Incompatible = append([]Place(nil), d.Incompatible...)
		}
	}
	return out
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
// exit=unobserved says the warm-up yielded no exit address. The address itself
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
