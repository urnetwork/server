package monitor

// SIGNALS.md §28.2, its per-country half; signal_extender_dns_records.go
// holds the whole-zone half and the signal itself. A client resolves the
// extender record name through the resolvers of its connection region, which
// the sdk names per country (sdk control_doh_ui.go IngressCountryCodes and
// IngressDohUrls, device.go GetRegionalDnsServers and
// GetDefaultDnsResolverSettings; connect net_http_doh_regional.go is the
// table they read). This asks each of those resolvers the way a client asks
// it, and judges each (country, resolver) on its own.
//
// The path of a region: its DoH servers (the sdk's regional DoH presets, cn's
// Alidns and DNSPod), then its plain resolvers (Iran: Shecan, then Electro)
// only when no DoH server yields a TXT value, as a client falls back
// (connect net_extender_network.go resolveDnsTxt). A client puts the default
// DoH servers behind a region's presets too, but a region is in the sdk's
// table because those are known not to work there, and from here they always
// answer, so they stand on the undisclosed path ("default") alone, every
// other client's first DoH step. A client's TXT fallback asks the device's
// own resolver, which no monitor can ask; the region's plain resolvers, the
// recursive resolvers the sdk names for it, stand in for it. The sdk's tables
// name no DoT endpoint for the platform's names, so none is asked, and only
// IPv4 endpoints are asked: the monitor host has no IPv6 route.
//
// A DoH answer is read as a client reads it (connect net_http_doh.go): the
// first 16 KiB of the body (maxDohResponseBytes, a LimitReader), parsed up to
// the cut (parseDohWire keeps the TXT records before a record the cut broke),
// and nothing of an answer that carries a policy extended dns error, which a
// client takes from no built-in server (dohGuardedResult; every DoH server
// the sdk names is built in). The monitor reads the body further, to count
// what the cut cost. A plain answer is asked over UDP, then TCP when
// truncated, as a client's resolver asks it. Each value is decoded as a
// client decodes it (connect.DecodeExtenderDnsRecord).
//
// A region's resolvers are asked with their own /24 as the EDNS client subnet
// (RFC 7871), standing in for the region's clients. A resolver that forwards
// its client's subnet is otherwise answered by Route 53 for the monitor's
// location rather than the region's: on 2026-11-12 Alidns and DNSPod both
// answered the monitor North America's set, and any Chinese /24 Asia's. From
// 11-12 China is answered by sets of its own (taskworker/work
// extender_dns_publish.go, extenderDnsCnSet), which only a question carrying a
// Chinese subnet reaches. A resolver that does not forward the subnet answers
// for its own location, as before. The default path is asked as a client asks
// it, with none.
//
// The judgments of one (country, resolver):
//   - fresh: the records of the location's whole set, the newest issued
//     within MaxNewestAge;
//   - no fresh records (extender-dns-country-stale): none served, none a
//     client keeps (an answer marked truncated with none, one with a policy
//     extended dns error, a name error, or a TXT question a plain resolver
//     leaves unanswered while it answers the A question), none that decodes,
//     or only stale or expired ones;
//   - a partial set (extender-dns-country-partial): the client's 16 KiB read
//     cut records off, or the answer holds fewer records than the operator's
//     set for the resolver's location, which the name's authoritative
//     nameservers give from here and with the resolver's /24 as the EDNS
//     client subnet, matched by extender key -- except a run of whole,
//     decodable records from a resolver known to cut what it cannot carry
//     that way (TrimmingResolvers, DNSPod), of a set larger than it delivers
//     whole, which is its trim (acceptTrim);
//   - unreachable from here: no answer (from a plain resolver, to neither the
//     TXT question nor the A question asked after it), a refusal or a server
//     failure. Some regional resolvers refuse foreign questions, so this is
//     visibility only, never a fault: the resolver's identities hold their
//     state.
//
// One run of this half is bounded by a budget, a cap on the questions in
// flight and a least gap between two launches. Labels are the country code
// and the resolver's name from the sdk's table; no record content, address or
// key reaches an alert.

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/sdk/v2026"
)

// The transports of a path's resolvers.
const (
	ExtenderDnsTransportDoh = "doh"
	ExtenderDnsTransportDns = "dns"
)

// The country label of the undisclosed connection region's path: the default
// DoH servers alone.
const ExtenderDnsDefaultCountry = "default"

// What a client reads of one DoH answer (connect net_http_doh.go
// maxDohResponseBytes).
const ExtenderDnsClientDohResponseBytes = 16 * 1024

// The extended dns error option (RFC 8914).
const extenderDnsEdeOptionCode = 15

// One resolver of a connection region's path, as the sdk's table names it.
type ExtenderDnsResolver struct {
	// The label of its alerts: the sdk's name of a plain resolver, with its
	// address when the region names two alike, or "DoH <ip>" for a DoH
	// server, which the sdk names by its url alone.
	Name string
	// ExtenderDnsTransportDoh or ExtenderDnsTransportDns
	Transport string
	// the DoH url, or the plain resolver's ip:port
	Endpoint string
}

// One connection region's path to the extender record name: the DoH servers a
// client asks first, then the plain resolvers it asks only when no DoH server
// yields a TXT value.
type ExtenderDnsCountry struct {
	// the lower case country code, or ExtenderDnsDefaultCountry
	Code string
	Doh  []ExtenderDnsResolver
	Dns  []ExtenderDnsResolver
}

// The paths of the sdk's per-country DNS settings, read from the sdk at each
// call: the undisclosed path (the default DoH servers), then every connection
// region the sdk offers (IngressCountryCodes) with its regional DoH presets
// (IngressDohUrls) and its plain resolvers (GetRegionalDnsServers), in the
// sdk's order. IPv4 endpoints only.
func ExtenderDnsCountriesFromSdk() []ExtenderDnsCountry {
	defaultCountry := ExtenderDnsCountry{Code: ExtenderDnsDefaultCountry}
	if defaults := sdk.GetDefaultDnsResolverSettings(); defaults != nil {
		defaultCountry.Doh = extenderDnsDohResolvers(extenderDnsStringListValues(defaults.RemoteDohUrlsIpv4))
	}
	countries := []ExtenderDnsCountry{defaultCountry}

	regionalServers := sdk.GetRegionalDnsServers()
	for _, countryCode := range extenderDnsStringListValues(sdk.IngressCountryCodes()) {
		country := ExtenderDnsCountry{
			Code: countryCode,
			Doh:  extenderDnsDohResolvers(extenderDnsStringListValues(sdk.IngressDohUrls(countryCode))),
		}
		servers := []*sdk.RegionalDnsServer{}
		nameCounts := map[string]int{}
		for index := 0; regionalServers != nil && index < regionalServers.Len(); index++ {
			server := regionalServers.Get(index)
			if server == nil || server.CountryCode != countryCode {
				continue
			}
			if address, err := netip.ParseAddr(server.Ipv4); err != nil || !address.Is4() {
				continue
			}
			servers = append(servers, server)
			nameCounts[server.Name]++
		}
		for _, server := range servers {
			name := server.Name
			if 1 < nameCounts[name] {
				name += " " + server.Ipv4
			}
			country.Dns = append(country.Dns, ExtenderDnsResolver{
				Name:      name,
				Transport: ExtenderDnsTransportDns,
				Endpoint:  net.JoinHostPort(server.Ipv4, "53"),
			})
		}
		countries = append(countries, country)
	}
	return countries
}

func extenderDnsStringListValues(list *sdk.StringList) []string {
	values := []string{}
	for index := 0; list != nil && index < list.Len(); index++ {
		values = append(values, list.Get(index))
	}
	return values
}

// The DoH servers of urls whose host is an IPv4 literal, named by it.
func extenderDnsDohResolvers(dohUrls []string) []ExtenderDnsResolver {
	var resolvers []ExtenderDnsResolver
	for _, dohUrl := range dohUrls {
		address, ok := extenderDnsDohAddress(dohUrl)
		if !ok || !address.Is4() {
			continue
		}
		resolvers = append(resolvers, ExtenderDnsResolver{
			Name:      "DoH " + address.String(),
			Transport: ExtenderDnsTransportDoh,
			Endpoint:  dohUrl,
		})
	}
	return resolvers
}

func extenderDnsDohAddress(dohUrl string) (netip.Addr, bool) {
	parsed, err := url.Parse(dohUrl)
	if err != nil || parsed.Scheme != "https" {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

// The address a resolver is asked at.
func (self ExtenderDnsResolver) address() (netip.Addr, bool) {
	if self.Transport == ExtenderDnsTransportDoh {
		return extenderDnsDohAddress(self.Endpoint)
	}
	host, _, err := net.SplitHostPort(self.Endpoint)
	if err != nil {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

// Whether every path names resolvers the half can ask.
func extenderDnsCountriesValid(countries []ExtenderDnsCountry) bool {
	for _, country := range countries {
		if country.Code == "" || strings.ContainsAny(country.Code, "/ ") {
			return false
		}
		for _, resolver := range slices.Concat(country.Doh, country.Dns) {
			if strings.TrimSpace(resolver.Name) == "" || resolver.Endpoint == "" {
				return false
			}
			if _, ok := resolver.address(); !ok {
				return false
			}
		}
	}
	return true
}

// Launches a run's questions within its bounds: at most limit at once, each at
// least pacing after the one before, and none once the budget has ended.
type extenderDnsLauncher struct {
	ctx    context.Context
	slots  chan struct{}
	pacing time.Duration
	last   time.Time
	wait   sync.WaitGroup
}

func newExtenderDnsLauncher(ctx context.Context, limit int, pacing time.Duration) *extenderDnsLauncher {
	return &extenderDnsLauncher{ctx: ctx, slots: make(chan struct{}, max(1, limit)), pacing: pacing}
}

// Runs task in its own goroutine once a slot is free and the pacing allows it,
// with the budget's context. False, and the task not run, when the budget
// ended first. Called from one goroutine.
func (self *extenderDnsLauncher) launch(task func(ctx context.Context)) bool {
	if !self.last.IsZero() && 0 < self.pacing {
		if delay := time.Until(self.last.Add(self.pacing)); 0 < delay {
			timer := time.NewTimer(delay)
			select {
			case <-self.ctx.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
	}
	select {
	case <-self.ctx.Done():
		return false
	case self.slots <- struct{}{}:
	}
	if self.ctx.Err() != nil {
		<-self.slots
		return false
	}
	self.last = time.Now()
	self.wait.Add(1)
	go func() {
		defer self.wait.Done()
		defer func() { <-self.slots }()
		task(self.ctx)
	}()
	return true
}

// Waits for every launched task.
func (self *extenderDnsLauncher) finish() {
	self.wait.Wait()
}

// What one resolver answered, reduced to what a client keeps of it.
type extenderDnsCountryAnswer struct {
	// a fixed reason when there is no answer to judge from here, else empty
	unreachable  string
	responseCode DNSResponseCode
	// the values a client keeps, in answer order
	kept []string
	// the values the whole answer carries: for DoH, the body the monitor read
	// past the client's cut
	served int
	// the answer was marked truncated
	truncatedFlag bool
	// a plain resolver left the TXT question unanswered and answered the A
	// question
	txtDropped bool
	// the answer carried a policy extended dns error, so a client keeps none
	filtered bool
	// the DoH body's bytes as read, at most the monitor's read bound and one
	responseBytes int
}

// What a DoH answer's body holds, parsed as a client parses it (connect
// net_http_doh.go parseDohWire): TXT values joined from their character
// strings, empty and repeated values left out, the answers read until the
// first that does not parse, none for a name error, and the policy extended
// dns error read only when every section before it parses.
type extenderDnsDohView struct {
	// the header parsed and the response code is NOERROR or NXDOMAIN
	answered      bool
	failure       string
	responseCode  DNSResponseCode
	truncatedFlag bool
	values        []string
	policyEde     bool
}

func parseExtenderDnsDohTxt(data []byte) extenderDnsDohView {
	var view extenderDnsDohView
	var parser dnsmessage.Parser
	header, err := parser.Start(data)
	if err != nil {
		view.failure = "malformed"
		return view
	}
	view.responseCode = classifyDNSResponseCode(header.RCode)
	view.truncatedFlag = header.Truncated
	switch header.RCode {
	case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
	case dnsmessage.RCodeRefused:
		view.failure = "refused"
		return view
	case dnsmessage.RCodeServerFailure:
		view.failure = "server_failure"
		return view
	default:
		view.failure = "rcode_other"
		return view
	}
	view.answered = true
	nameError := header.RCode == dnsmessage.RCodeNameError
	done := func() extenderDnsDohView {
		if nameError {
			view.values = nil
		}
		return view
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return done()
	}
	for {
		answerHeader, err := parser.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return done()
		}
		if answerHeader.Type == dnsmessage.TypeTXT {
			resource, err := parser.TXTResource()
			if err != nil {
				return done()
			}
			value := strings.Join(resource.TXT, "")
			if value != "" && !slices.Contains(view.values, value) {
				view.values = append(view.values, value)
			}
			continue
		}
		if err := parser.SkipAnswer(); err != nil {
			return done()
		}
	}
	view.policyEde = extenderDnsPolicyEde(&parser)
	return done()
}

// Whether the rest of a response whose answers the parser consumed carries a
// policy extended dns error (connect net_http_doh.go dnsResponseEdeCodes and
// dnsPolicyEdeCode: 4 Forged Answer, 15 Blocked, 16 Censored, 17 Filtered, 18
// Prohibited).
func extenderDnsPolicyEde(parser *dnsmessage.Parser) bool {
	if err := parser.SkipAllAuthorities(); err != nil {
		return false
	}
	for {
		header, err := parser.AdditionalHeader()
		if err != nil {
			return false
		}
		if header.Type != dnsmessage.TypeOPT {
			if err := parser.SkipAdditional(); err != nil {
				return false
			}
			continue
		}
		option, err := parser.OPTResource()
		if err != nil {
			return false
		}
		for _, one := range option.Options {
			if one.Code != extenderDnsEdeOptionCode || len(one.Data) < 2 {
				continue
			}
			switch binary.BigEndian.Uint16(one.Data[:2]) {
			case 4, 15, 16, 17, 18:
				return true
			}
		}
	}
}

// What a client keeps of a DoH answer: the values its parse reads in the first
// clientBytes of the body, or none when the answer carries a policy extended
// dns error. served counts the values of the body as far as the monitor read
// it (fullBytes).
func extenderDnsDohAnswer(body []byte, clientBytes int, fullBytes int) extenderDnsCountryAnswer {
	clientView := parseExtenderDnsDohTxt(body[:min(len(body), clientBytes)])
	answer := extenderDnsCountryAnswer{
		responseCode:  clientView.responseCode,
		truncatedFlag: clientView.truncatedFlag,
		responseBytes: len(body),
	}
	if !clientView.answered {
		answer.unreachable = clientView.failure
		return answer
	}
	answer.served = max(len(clientView.values), len(parseExtenderDnsDohTxt(body[:min(len(body), fullBytes)]).values))
	if clientView.policyEde {
		answer.filtered = true
		return answer
	}
	answer.kept = clientView.values
	return answer
}

// What a client's resolver keeps of a plain answer: every TXT value of a
// NOERROR or NXDOMAIN answer. A UDP answer marked truncated whose TCP retry
// failed is an answer with none, as a client's resolver, which retries the
// same way, gets none.
func extenderDnsPlainAnswer(observation DNSResponseObservation, err error) extenderDnsCountryAnswer {
	if err != nil {
		var retry *dnsTruncatedRetryError
		if errors.As(err, &retry) || errors.Is(err, errDNSTruncatedOverTCP) {
			return extenderDnsCountryAnswer{responseCode: DNSResponseSuccess, truncatedFlag: true}
		}
		return extenderDnsCountryAnswer{unreachable: extenderDnsFailureReason(err)}
	}
	switch observation.ResponseCode {
	case DNSResponseSuccess, DNSResponseNameError:
	case DNSResponseRefused:
		return extenderDnsCountryAnswer{unreachable: "refused", responseCode: observation.ResponseCode}
	case DNSResponseServerFailure:
		return extenderDnsCountryAnswer{unreachable: "server_failure", responseCode: observation.ResponseCode}
	default:
		return extenderDnsCountryAnswer{unreachable: "rcode_other", responseCode: observation.ResponseCode}
	}
	return extenderDnsCountryAnswer{
		responseCode: observation.ResponseCode,
		kept:         observation.Texts,
		served:       len(observation.Texts),
	}
}

// The fixed reason of a question that got no answer.
func extenderDnsFailureReason(err error) string {
	var netErr net.Error
	var certificateErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed):
		return "connection_reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "no_route"
	case errors.As(err, &certificateErr):
		return "tls_certificate"
	default:
		return "failed"
	}
}

// The operator's TXT set for one location, by extender key, and the size of
// a resolver's answer that carries it whole.
type extenderDnsAuthoritySet struct {
	ok          bool
	keys        map[string]bool
	answerBytes int
}

func newExtenderDnsAuthoritySet(observation DNSResponseObservation, err error, now time.Time, maxRecords int, name string) extenderDnsAuthoritySet {
	if err != nil {
		return extenderDnsAuthoritySet{}
	}
	switch observation.ResponseCode {
	case DNSResponseSuccess, DNSResponseNameError:
	default:
		return extenderDnsAuthoritySet{}
	}
	set := extenderDnsAuthoritySet{ok: true, keys: map[string]bool{}, answerBytes: extenderDnsTxtAnswerSize(name, observation.Texts)}
	for _, key := range observeExtenderDnsRecords(observation.Texts, now, maxRecords).keys {
		set.keys[key] = true
	}
	return set
}

// The bytes of a resolver's TXT answer of these values for name: the header,
// the question, each value as a record of strings of at most 255 bytes under
// a compressed owner name, and an OPT record with an IPv4 /24 client subnet,
// the shape China's resolvers answered the probe in on 2026-11-12.
func extenderDnsTxtAnswerSize(name string, values []string) int {
	size := 12 + len(absoluteDNSName(name)) + 1 + 4 + 11 + 11
	for _, value := range values {
		size += 2 + 10 + max(1, (len(value)+254)/255) + len(value)
	}
	return size
}

// A resolver known to cut an answer it cannot carry to a run of its whole
// records rather than mark it truncated (settings.TrimmingResolvers): a run of
// whole records of the location's set, each decoding as a record, when the set
// is larger than the resolver delivers whole, is its trim and not a partial
// set. China's own set is sized so Alidns delivers it whole and DNSPod one
// whole record of it (owner, 2026-11-12). An empty answer, or one with a value
// that does not decode, is judged as before.
func (self *extenderDnsCountryJudgment) acceptTrim(wholeBytes int, vantage extenderDnsAuthoritySet, subnet extenderDnsAuthoritySet) {
	if !slices.Equal(self.partialReasons, []string{"fewer_than_authoritative"}) ||
		self.observation.undecodable != 0 || self.observation.other != 0 || self.observation.records == 0 {
		return
	}
	matched := vantage
	if self.authority == "client_subnet" {
		matched = subnet
	}
	if matched.answerBytes <= wholeBytes {
		return
	}
	self.partialReasons = nil
	self.trimmed = true
}

// Whether keys are exactly the set's.
func (self extenderDnsAuthoritySet) equals(keys []string) bool {
	if !self.ok || len(keys) != len(self.keys) {
		return false
	}
	for _, key := range keys {
		if !self.keys[key] {
			return false
		}
	}
	return true
}

// Whether keys are all in the set.
func (self extenderDnsAuthoritySet) holds(keys []string) bool {
	if !self.ok || len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if !self.keys[key] {
			return false
		}
	}
	return true
}

// One question of a run: a resolver of a region's path, its answer and its
// judgment.
type extenderDnsCountryQuestion struct {
	country  string
	resolver ExtenderDnsResolver
	// the run sent the question; a resolver the client would not fall back to
	// is never asked
	launched bool
	// the budget ended before the question was sent or answered
	unfinished bool
	answer     extenderDnsCountryAnswer
	judgment   extenderDnsCountryJudgment
}

func (self *extenderDnsCountryQuestion) answered() bool {
	return self.launched && !self.unfinished && self.answer.unreachable == ""
}

// What one answer gives a client.
type extenderDnsCountryJudgment struct {
	observation extenderDnsRecordsObservation
	// why a client keeps no fresh record of it; none when it keeps one
	staleReasons []string
	// why the records a client keeps are a partial set; none when whole
	partialReasons []string
	// the set it was matched against: vantage (the authoritative answer
	// here), client_subnet (the answer for the resolver's /24), unmatched,
	// unavailable, or none when a client keeps nothing
	authority     string
	authoritative int
	missing       int
	// nothing shows whether the set is partial: the authoritative set could
	// not be read, and the client's read cut nothing off
	partialUnknown bool
	// a resolver known to trim cut the set to a run of its whole records
	// (acceptTrim)
	trimmed bool
}

// Judges one answer against the authoritative sets from here and for the
// resolver's location.
func judgeExtenderDnsCountryAnswer(answer extenderDnsCountryAnswer, now time.Time, settings *ExtenderDnsRecordsSettings, vantage extenderDnsAuthoritySet, subnet extenderDnsAuthoritySet) extenderDnsCountryJudgment {
	observation := observeExtenderDnsRecords(answer.kept, now, settings.MaxRecords)
	judgment := extenderDnsCountryJudgment{observation: observation, authority: "none"}
	if len(answer.kept) == 0 {
		switch {
		case answer.filtered:
			judgment.staleReasons = []string{"filtered"}
		case answer.txtDropped:
			judgment.staleReasons = []string{"txt_dropped"}
		case answer.truncatedFlag:
			judgment.staleReasons = []string{"truncated_empty"}
		case answer.responseCode == DNSResponseNameError:
			judgment.staleReasons = []string{"name_error"}
		default:
			judgment.staleReasons = []string{"none_served"}
		}
		return judgment
	}
	judgment.staleReasons = observation.staleReasons(now, settings.MaxNewestAge)

	if len(answer.kept) < answer.served {
		judgment.partialReasons = append(judgment.partialReasons, "client_cap")
	}
	keys := observation.keys
	switch {
	case len(keys) == 0:
		judgment.authority = "unmatched"
	case vantage.equals(keys):
		judgment.authority = "vantage"
		judgment.authoritative = len(vantage.keys)
	case subnet.equals(keys):
		judgment.authority = "client_subnet"
		judgment.authoritative = len(subnet.keys)
	default:
		// the fewest missing of the sets that hold every key
		for _, candidate := range []struct {
			name string
			set  extenderDnsAuthoritySet
		}{{"vantage", vantage}, {"client_subnet", subnet}} {
			if !candidate.set.holds(keys) {
				continue
			}
			missing := len(candidate.set.keys) - len(keys)
			if judgment.authority == "none" || missing < judgment.missing {
				judgment.authority = candidate.name
				judgment.authoritative = len(candidate.set.keys)
				judgment.missing = missing
			}
		}
		switch {
		case judgment.authority != "none" && subnet.ok:
			judgment.partialReasons = append(judgment.partialReasons, "fewer_than_authoritative")
		case judgment.authority != "none":
			// held by the set from here, but the set of the resolver's
			// location, which may be smaller, could not be read
			judgment.authority = "unavailable"
			judgment.authoritative = 0
			judgment.missing = 0
		case !vantage.ok && !subnet.ok:
			judgment.authority = "unavailable"
		default:
			judgment.authority = "unmatched"
		}
	}
	if judgment.authority == "unavailable" && len(judgment.partialReasons) == 0 {
		judgment.partialUnknown = true
	}
	return judgment
}

// The per-country half of one run, as findings: every path's resolvers asked
// within the budget, judged one by one. zoneReasons are why the whole-zone
// half found the zone itself stale: a resolver stale for those reasons alone
// serves the zone's staleness, which extender-dns-records-stale reports, so
// its own identity holds.
func (self extenderDnsRecordsProbe) checkCountries(ctx context.Context, env *probeEnv, settings *ExtenderDnsRecordsSettings, name string, zoneTarget string, zoneReasons []string) []finding {
	if len(settings.Countries) == 0 {
		return nil
	}
	budgetCtx, cancel := context.WithTimeout(ctx, settings.CountryBudget)
	defer cancel()
	launcher := newExtenderDnsLauncher(budgetCtx, settings.CountryConcurrency, settings.CountryPacing)
	now := env.now()

	countryQuestions := make([][]*extenderDnsCountryQuestion, len(settings.Countries))
	for index, country := range settings.Countries {
		for _, resolver := range slices.Concat(country.Doh, country.Dns) {
			countryQuestions[index] = append(countryQuestions[index], &extenderDnsCountryQuestion{country: country.Code, resolver: resolver})
		}
	}
	// the /24 of a resolver's own address: the client subnet a region's
	// resolver is asked with, standing in for the region's clients, and the
	// location whose set its answer is compared with
	subnetFor := func(question *extenderDnsCountryQuestion) (netip.Prefix, bool) {
		address, ok := question.resolver.address()
		if !ok || !address.Is4() {
			return netip.Prefix{}, false
		}
		prefix, err := address.Prefix(24)
		return prefix, err == nil
	}
	// the question's fields are written before its task starts, or by the
	// task alone, and read after launcher.finish
	ask := func(question *extenderDnsCountryQuestion) {
		question.launched = true
		clientSubnet := netip.Prefix{}
		if question.country != ExtenderDnsDefaultCountry {
			clientSubnet, _ = subnetFor(question)
		}
		launched := launcher.launch(func(ctx context.Context) {
			answer := askExtenderDnsResolver(ctx, env, settings, name, question.resolver, clientSubnet)
			if answer.unreachable != "" && ctx.Err() != nil {
				// the budget ended the question, not the resolver
				question.unfinished = true
			}
			question.answer = answer
		})
		if !launched {
			question.launched = false
			question.unfinished = true
		}
	}

	// the DoH servers of every path, and the operator's set from here
	var servers []string
	var vantage extenderDnsAuthoritySet
	launcher.launch(func(ctx context.Context) {
		servers = settings.AuthoritativeServers
		if len(servers) == 0 {
			discovered, err := env.runner.dnsNameservers(ctx, name)
			if err != nil {
				return
			}
			servers = discovered
		}
		observation, err := env.runner.dnsTxtAuthoritative(ctx, name, servers, netip.Prefix{})
		vantage = newExtenderDnsAuthoritySet(observation, err, now, settings.MaxRecords, name)
	})
	for _, questions := range countryQuestions {
		for _, question := range questions {
			if question.resolver.Transport == ExtenderDnsTransportDoh {
				ask(question)
			}
		}
	}
	launcher.finish()

	// the plain resolvers of every path whose DoH yielded no TXT value, as a
	// client falls back
	for _, questions := range countryQuestions {
		yielded := false
		for _, question := range questions {
			if question.resolver.Transport == ExtenderDnsTransportDoh && question.answered() && 0 < len(question.answer.kept) {
				yielded = true
			}
		}
		if yielded {
			continue
		}
		for _, question := range questions {
			if question.resolver.Transport == ExtenderDnsTransportDns {
				ask(question)
			}
		}
	}
	launcher.finish()

	// the operator's set for the location of each resolver whose answer is
	// not exactly the set from here
	prefixes := []netip.Prefix{}
	if 0 < len(servers) {
		for _, questions := range countryQuestions {
			for _, question := range questions {
				if !question.answered() || len(question.answer.kept) == 0 {
					continue
				}
				keys := observeExtenderDnsRecords(question.answer.kept, now, settings.MaxRecords).keys
				if len(keys) == 0 || vantage.equals(keys) {
					continue
				}
				if prefix, ok := subnetFor(question); ok && !slices.Contains(prefixes, prefix) {
					prefixes = append(prefixes, prefix)
				}
			}
		}
	}
	subnetSets := map[netip.Prefix]extenderDnsAuthoritySet{}
	var subnetLock sync.Mutex
	for _, prefix := range prefixes {
		launcher.launch(func(ctx context.Context) {
			observation, err := env.runner.dnsTxtAuthoritative(ctx, name, servers, prefix)
			set := newExtenderDnsAuthoritySet(observation, err, now, settings.MaxRecords, name)
			subnetLock.Lock()
			subnetSets[prefix] = set
			subnetLock.Unlock()
		})
	}
	launcher.finish()

	for _, questions := range countryQuestions {
		for _, question := range questions {
			if !question.answered() {
				continue
			}
			subnet := extenderDnsAuthoritySet{}
			if prefix, ok := subnetFor(question); ok {
				subnet = subnetSets[prefix]
			}
			question.judgment = judgeExtenderDnsCountryAnswer(question.answer, now, settings, vantage, subnet)
			if wholeBytes, ok := settings.TrimmingResolvers[question.resolver.Endpoint]; ok {
				question.judgment.acceptTrim(wholeBytes, vantage, subnet)
			}
		}
	}
	return self.countryFindings(settings, countryQuestions, name, zoneTarget, zoneReasons, now)
}

// Asks one resolver of a path, as a client asks it, with the client subnet
// that stands in for the region's clients when it is valid.
func askExtenderDnsResolver(ctx context.Context, env *probeEnv, settings *ExtenderDnsRecordsSettings, name string, resolver ExtenderDnsResolver, clientSubnet netip.Prefix) extenderDnsCountryAnswer {
	if resolver.Transport == ExtenderDnsTransportDoh {
		status, body, err := env.runner.dnsDohTxt(ctx, resolver.Endpoint, name, settings.FullDohResponseBytes, clientSubnet)
		if err != nil {
			if status != 0 && status != http.StatusOK {
				return extenderDnsCountryAnswer{unreachable: "http_status"}
			}
			return extenderDnsCountryAnswer{unreachable: extenderDnsFailureReason(err)}
		}
		return extenderDnsDohAnswer(body, settings.ClientDohResponseBytes, settings.FullDohResponseBytes)
	}
	observation, err := env.runner.dnsTxt(ctx, resolver.Endpoint, name, clientSubnet)
	answer := extenderDnsPlainAnswer(observation, err)
	if err != nil && answer.unreachable != "" && ctx.Err() == nil {
		// No answer to the TXT question. A resolver that answers the name's
		// A question drops its TXT answer, as the monitor host's router did on
		// 2026-11-11; one that answers neither is unreachable from here.
		address, addressErr := env.runner.dnsAddress(ctx, resolver.Endpoint, name)
		if addressErr == nil && (address.ResponseCode == DNSResponseSuccess || address.ResponseCode == DNSResponseNameError) {
			return extenderDnsCountryAnswer{responseCode: DNSResponseSuccess, txtDropped: true}
		}
	}
	return answer
}

// The findings of the per-country half.
func (self extenderDnsRecordsProbe) countryFindings(settings *ExtenderDnsRecordsSettings, countryQuestions [][]*extenderDnsCountryQuestion, name string, zoneTarget string, zoneReasons []string, now time.Time) []finding {
	// each path's resolvers unreachable from here, and the regions none of
	// whose resolvers answered (blind)
	blind := []string{}
	countryUnreachable := make([][]string, len(countryQuestions))
	unfinished := 0
	partialUnknown := 0
	answeredAny := false
	launchedAny := false
	for index, questions := range countryQuestions {
		answered := false
		launched := false
		for _, question := range questions {
			if question.unfinished {
				unfinished++
				continue
			}
			if !question.launched {
				continue
			}
			launched = true
			if question.answered() {
				answered = true
				if question.judgment.partialUnknown {
					partialUnknown++
				}
			} else {
				countryUnreachable[index] = append(countryUnreachable[index], question.resolver.Name)
			}
		}
		launchedAny = launchedAny || launched
		answeredAny = answeredAny || answered
		if launched && !answered {
			blind = append(blind, settings.Countries[index].Code)
		}
	}

	findings := []finding{}
	for index, questions := range countryQuestions {
		country := settings.Countries[index].Code
		unreachable := countryUnreachable[index]
		for _, question := range questions {
			target := zoneTarget + "/" + country + "/" + question.resolver.Name
			if !question.launched && !question.unfinished {
				// not on the path a client takes this run
				continue
			}
			if !question.answered() {
				// unreachable from here, or unasked within the budget: no
				// evidence either way
				findings = append(findings,
					stormClearHold(extenderDnsRecordsProbeId, "extender-dns-country-stale", target),
					stormClearHold(extenderDnsRecordsProbeId, "extender-dns-country-partial", target),
				)
				continue
			}
			judgment := question.judgment
			pathUnreachable := extenderDnsListed(unreachable)
			switch {
			case len(judgment.staleReasons) == 0:
				findings = append(findings, healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-country-stale", target))
			case extenderDnsReasonsWithin(judgment.staleReasons, zoneReasons):
				// the zone itself is stale (extender-dns-records-stale): this
				// resolver serving it adds nothing
				findings = append(findings, stormClearHold(extenderDnsRecordsProbeId, "extender-dns-country-stale", target))
			default:
				findings = append(findings, finding{
					probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-country-stale", target: target, sustain: settings.Sustain,
					symptom:   fmt.Sprintf("Clients %s that resolve the extender records through %s get no fresh records: %s", extenderDnsCountryClients(country), question.resolver.Name, strings.Join(judgment.staleReasons, ", ")),
					mechanism: fmt.Sprintf("A client resolves the extender record name through its connection region's resolvers, which the sdk names per country: the region's DoH servers first, read 16 KiB deep, then its plain resolvers only when DoH yields no TXT value. It verifies each signed record of the answer and adds it to its directory. This resolver, on that path, gave no fresh record: none served, none a client keeps (an answer marked truncated with none, one carrying a policy extended dns error, or a TXT question left unanswered by a resolver that answers the A question), none that decodes, or only records issued over %s ago or expired. A client whose lookup lands here learns no extender from DNS.", settings.MaxNewestAge),
					baseline:  fmt.Sprintf("Every resolver on a region's path serves the whole TXT set of its location (13 to 15 records, about 5 KB, from Route 53 on 2026-11-12; China's own set, about three records in about 1.1 KB, from the same day, of which DNSPod hands on one whole record), the newest issued within about 11 minutes. WARN when this resolver, on the path a client of the region takes, serves no fresh record for %d consecutive runs %s apart.", settings.Sustain, self.cadence()),
					observed:  extenderDnsCountryObserved(question, now, pathUnreachable, blind, judgment.staleReasons),
					evidence:  extenderDnsCountryEvidence(settings),
					context:   extenderDnsCountryContext(name),
					action:    "Compare the resolver's answer with a default server's: `dig +tcp +subnet=<resolver ip>/24 TXT " + name + " @<resolver ip>` (the subnet stands in for the region's clients, as the probe asks) against `@1.1.1.1` (a DoH server answers the same question as `curl -H 'accept: application/dns-message' '<url>?dns=<query>'`). none_served, name_error or filtered from a resolver of the region is the region refusing the name; truncated_empty or txt_dropped is a resolver that cannot carry the answer (a truncated UDP answer whose TCP retry failed, a DoH answer marked truncated with no records, or a TXT question left unanswered while the A question is answered); newest_too_old or all_expired alone, with a fresh whole zone, is the resolver's cache. A resolver that keeps failing its region belongs out of the sdk's per-country table (connect net_http_doh_regional.go), or the location's TXT set must shrink to what it carries.",
					verify:    fmt.Sprintf("A run reads fresh records through the resolver, the newest issued within %s.", settings.MaxNewestAge),
					playbook:  "SIGNALS.md §28.2",
				})
			}
			switch {
			case len(judgment.partialReasons) != 0:
				findings = append(findings, finding{
					probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-country-partial", target: target, sustain: settings.Sustain,
					symptom:   fmt.Sprintf("Clients %s that resolve the extender records through %s get a partial set: %s", extenderDnsCountryClients(country), question.resolver.Name, extenderDnsPartialSummary(question)),
					mechanism: "A client resolves the extender record name through its connection region's resolvers, which the sdk names per country, keeps the signed records of the TXT answer it reads, and dials only the addresses those records vouch for. This resolver's answer gave a client fewer records than the operator's set for the resolver's location: the client's 16 KiB read of the DoH answer cut records off (client_cap), or the resolver trimmed the set without marking it truncated (fewer_than_authoritative, against Route 53's answer for the location, asked directly). A client whose lookup lands here learns only part of the location's extenders.",
					baseline:  fmt.Sprintf("Every resolver on a region's path serves the whole TXT set of its location (13 to 15 records, about 5 KB, from Route 53 on 2026-11-12; China's own set, about three records in about 1.1 KB, from the same day, of which DNSPod hands on one whole record). WARN when this resolver, on the path a client of the region takes, gives a client fewer records than the location's set for %d consecutive runs %s apart.", settings.Sustain, self.cadence()),
					observed:  extenderDnsCountryObserved(question, now, pathUnreachable, blind, judgment.partialReasons),
					evidence:  extenderDnsCountryEvidence(settings),
					context:   extenderDnsCountryContext(name),
					action:    "Compare the resolver's answer with Route 53's for its location: `dig +tcp +subnet=<resolver ip>/24 TXT " + name + " @<resolver ip>` against `dig +tcp +norec +subnet=<resolver ip>/24 TXT " + name + " @<a nameserver of the name>` (`dig NS " + name + "`). client_cap: the location's set is larger than a client reads in one DoH answer; read the publisher's TXT budget (extenderDnsTxtMaxAnswerBytes, taskworker/work/extender_dns_publish.go) against the answer's response_bytes. fewer_than_authoritative alone: the resolver trims the set; the location's TXT set must fit what the resolver carries, or the resolver belongs out of the sdk's per-country table (connect net_http_doh_regional.go). China's own set is held to what Alidns delivered whole on 2026-11-12 (extenderDnsCnAlidnsMaxMessageBytes, capped by the dns block's cn_record_count), each record within what DNSPod's trimmed answers ran to (extenderDnsCnDnspodMaxMessageBytes); DNSPod's run of whole records of it is its trim and is not reported, so a partial cn answer means Alidns's ceiling moved or DNSPod's answer held a value that does not decode.",
					verify:    "A run reads the location's whole set through the resolver.",
					playbook:  "SIGNALS.md §28.2",
				})
			case judgment.partialUnknown:
				findings = append(findings, stormClearHold(extenderDnsRecordsProbeId, "extender-dns-country-partial", target))
			default:
				findings = append(findings, healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-country-partial", target))
			}
		}
	}

	countriesTarget := zoneTarget + "/countries"
	reasons := []string{}
	if launchedAny && !answeredAny {
		reasons = append(reasons, "none_answered")
	}
	if 0 < partialUnknown {
		reasons = append(reasons, "authority_unavailable")
	}
	if 0 < unfinished {
		reasons = append(reasons, "budget_exhausted")
	}
	if len(reasons) == 0 {
		return append(findings, healthyFinding(extenderDnsRecordsProbeId, tierWarn, "extender-dns-country-unobservable", countriesTarget))
	}
	return append(findings, finding{
		probeId: extenderDnsRecordsProbeId, tier: tierWarn, class: "extender-dns-country-unobservable", target: countriesTarget, sustain: 2,
		symptom:   "The §28.2 signal could not judge every connection region's path to the extender records",
		mechanism: "No resolver of any region's path answered from here (the monitor host's network or its outbound DNS and https), the operator's set to compare answers with could not be read from the name's authoritative nameservers, or the run's budget ended before every question was answered. What those clients are served is unknown, not fresh.",
		baseline:  "Every run reads an answer from the regions' resolvers, the authoritative set, and every question within the budget.",
		observed: fmt.Sprintf("visibility_reasons=%s blind=%s unfinished=%d authority_unknown=%d budget_seconds=%.0f",
			strings.Join(reasons, ","), extenderDnsListed(blind), unfinished, partialUnknown, settings.CountryBudget.Seconds()),
		evidence: "Fixed reasons, country codes and counts only.",
		context:  "A resolver unreachable from here alone is not reported, nor a region none of whose resolvers answers (listed as blind): some regional resolvers refuse foreign questions. Their identities hold their state while it lasts.",
		action:   "For none_answered, ask a region's resolvers from the monitor host (`dig +tcp TXT " + name + " @77.88.8.8`, and a DoH server with curl). For authority_unavailable, check `dig NS " + name + "` and a TCP question to one of its nameservers. For budget_exhausted, read which resolvers time out and raise CountryBudget or lower CountryPacing.",
		verify:   "Two consecutive runs judge every region's path.",
		playbook: "SIGNALS.md §28.2",
	})
}

// Who a path serves, for an alert.
func extenderDnsCountryClients(country string) string {
	if country == ExtenderDnsDefaultCountry {
		return "with an undisclosed connection region"
	}
	return "in " + country
}

// A list for an alert, or none.
func extenderDnsListed(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

// Whether every reason is among the zone's own, which then explain them.
func extenderDnsReasonsWithin(reasons []string, zoneReasons []string) bool {
	for _, reason := range reasons {
		if !slices.Contains(zoneReasons, reason) {
			return false
		}
	}
	return 0 < len(reasons)
}

// "3 of 15 records (fewer_than_authoritative)" and the like.
func extenderDnsPartialSummary(question *extenderDnsCountryQuestion) string {
	judgment := question.judgment
	whole := max(question.answer.served, judgment.authoritative)
	return fmt.Sprintf("%d of %d records (%s)", len(judgment.observation.keys), whole, strings.Join(judgment.partialReasons, ", "))
}

func extenderDnsCountryObserved(question *extenderDnsCountryQuestion, at time.Time, pathUnreachable string, blind []string, reasons []string) string {
	answer := question.answer
	judgment := question.judgment
	observation := judgment.observation
	newestAge := "none"
	if !observation.newestIssue.IsZero() {
		newestAge = fmt.Sprintf("%.0f", at.Sub(observation.newestIssue).Seconds())
	}
	return fmt.Sprintf("country=%s resolver=%q transport=%s endpoint=%s rcode=%s response_bytes=%d truncated_flag=%t filtered=%t served=%d kept=%d records=%d undecodable=%d other=%d valid=%d expired=%d newest_issued_unix=%s newest_issued_age_seconds=%s latest_expires_unix=%s authority=%s authoritative=%d missing=%d path_unreachable=%s blind=%s reasons=%s",
		question.country, question.resolver.Name, question.resolver.Transport, question.resolver.Endpoint,
		answer.responseCode, answer.responseBytes, answer.truncatedFlag, answer.filtered,
		answer.served, len(answer.kept), observation.records, observation.undecodable, observation.other,
		observation.valid, observation.expired,
		extenderDnsRecordsUnix(observation.newestIssue), newestAge, extenderDnsRecordsUnix(observation.latestExpire),
		judgment.authority, judgment.authoritative, judgment.missing,
		pathUnreachable, extenderDnsListed(blind), strings.Join(reasons, ","),
	)
}

func extenderDnsCountryEvidence(settings *ExtenderDnsRecordsSettings) string {
	return fmt.Sprintf("The resolver's TXT answer asked as a client asks it (DoH: an RFC 8484 GET, read %d bytes deep and parsed to the cut as connect's parseDohWire does, the body read on to %d bytes to count what the cut dropped; plain: UDP, then TCP when truncated), a region's resolver with its own /24 as the EDNS client subnet in place of its clients', each value decoded with connect.DecodeExtenderDnsRecord; the operator's set for the location from the name's authoritative nameservers, asked from here and with the resolver's /24 as the EDNS client subnet, matched by extender key. Counts, times, fixed reasons and the resolver's name and endpoint from the sdk's table only; no record content reaches the alert.", settings.ClientDohResponseBytes, settings.FullDohResponseBytes)
}

func extenderDnsCountryContext(name string) string {
	return "The resolver is on the path the sdk's per-country DNS settings give a client of the region (sdk IngressDohUrls and GetRegionalDnsServers, from connect net_http_doh_regional.go): the region's DoH servers, then its plain resolvers only when those yield no TXT value; the undisclosed path is the default DoH servers. The name " + name + " is geo-routed by continent, and China by a set of its own; a region's resolver is asked with its own /24 as the client subnet, so one that forwards a client's subnet is answered with its region's set, and one that does not with the set of its own location. A whole zone gone stale (extender-dns-records-stale) holds this signal's per-resolver staleness, which then adds nothing; a resolver unreachable from here is held, never reported."
}
