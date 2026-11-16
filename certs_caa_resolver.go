package server

// certs' resolver and canary client for §12.8's CAA checks of a strong-mode
// custom host (TLS-M6-04; RFC 8659 §3, D-DNS-9, D-TLS-11, FS-32):
//
//   - CertsCaaResolver, certsservice.CertsCaaResolver on miekg/dns: the walk's
//     CAA queries at a recursive resolver, following CNAMEs (a DNAME's
//     synthesized CNAME among them), with each name's zone and that zone's
//     nameservers; and the probe's queries at those nameservers with RD=0,
//     each server's answer apart, so a customer-side ALIAS that flattens CAA
//     shows the canary's nonce.
//   - CertsDnsClient's PublishCaaCanary and DeleteCaaCanary,
//     certsservice.CertsCaaCanaryDns over POST /dns/internal/caa-canary with
//     the internal-certs bearer (plan-TLS-internal-credentials), beside the
//     client's challenge and ready calls.
//
// A failed lookup is an error, never an empty answer: RFC 8659's climb would
// otherwise pass over a pin it never read. A zone is found as the owner of
// the SOA its name's answer is under, its nameservers as its NS RRset; a
// name whose zone has no NS is an error. The resolver keeps no cache: each
// walk asks afresh. Every limit is CertsCaaResolverSettings', and nothing
// here names a host in a log (I-13). Safe for concurrent use.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
)

var (
	_ certsservice.CertsCaaResolver  = (*CertsCaaResolver)(nil)
	_ certsservice.CertsCaaCanaryDns = (*CertsDnsClient)(nil)
)

var (
	// Settings no resolver can run on, or no resolver to ask. The text names
	// the part.
	ErrCertsCaaResolverRefused = errors.New("certs caa resolver refused")

	errCertsCaaResolverNoAnswer      = errors.New("no resolver answered")
	errCertsCaaResolverNoNameservers = errors.New("a zone with no nameservers")
	errCertsCaaResolverAuthoritative = errors.New("no authoritative server answered")
)

// The resolver's tunables. DefaultCertsCaaResolverSettings is the only
// source of a default.
type CertsCaaResolverSettings struct {
	// The recursive resolvers the walk asks, host:port, in order. Empty takes
	// the system's, /etc/resolv.conf's, when the resolver is built.
	Resolvers []string
	// The port an authoritative server is asked on: DNS's 53.
	AuthoritativePort int
	// How long one exchange may take.
	QueryTimeout time.Duration
	// The CNAMEs one answer may follow, past which the lookup fails.
	AliasMaxCount int
	// The labels a zone's search climbs at most from a name.
	ZoneSearchMaxCount int
	// The nameservers of a zone asked, and the addresses of one tried, at most.
	NameserverMaxCount int
	AddressMaxCount    int
	// The EDNS0 UDP payload size offered; a truncated answer is asked again
	// over TCP.
	UdpBufferByteCount int
}

// DNS's port, a few seconds a query, and bounds past what a resolver follows.
func DefaultCertsCaaResolverSettings() *CertsCaaResolverSettings {
	return &CertsCaaResolverSettings{
		AuthoritativePort:  53,
		QueryTimeout:       5 * time.Second,
		AliasMaxCount:      8,
		ZoneSearchMaxCount: 16,
		NameserverMaxCount: 8,
		AddressMaxCount:    4,
		UdpBufferByteCount: 1232,
	}
}

// One exchange of message with server, host:port. The default sends it over
// UDP and again over TCP when the answer comes back truncated.
type CertsCaaExchange func(ctx context.Context, message *dns.Msg, server string) (*dns.Msg, error)

// See the file header.
type CertsCaaResolver struct {
	settings  *CertsCaaResolverSettings
	resolvers []string
	exchange  CertsCaaExchange
}

// A resolver on settings, nil taking the defaults, exchanging through
// exchange, nil taking UDP with a TCP fallback. No resolver in settings
// reads the system's.
func NewCertsCaaResolver(settings *CertsCaaResolverSettings, exchange CertsCaaExchange) (*CertsCaaResolver, error) {
	if settings == nil {
		settings = DefaultCertsCaaResolverSettings()
	}
	for name, value := range map[string]int64{
		"AuthoritativePort":  int64(settings.AuthoritativePort),
		"QueryTimeout":       int64(settings.QueryTimeout),
		"AliasMaxCount":      int64(settings.AliasMaxCount),
		"ZoneSearchMaxCount": int64(settings.ZoneSearchMaxCount),
		"NameserverMaxCount": int64(settings.NameserverMaxCount),
		"AddressMaxCount":    int64(settings.AddressMaxCount),
		"UdpBufferByteCount": int64(settings.UdpBufferByteCount),
	} {
		if value <= 0 {
			return nil, fmt.Errorf("%w: %s is not positive", ErrCertsCaaResolverRefused, name)
		}
	}
	resolvers := slices.Clone(settings.Resolvers)
	if len(resolvers) == 0 {
		config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return nil, fmt.Errorf("%w: no resolvers, and the system's: %v", ErrCertsCaaResolverRefused, err)
		}
		for _, server := range config.Servers {
			resolvers = append(resolvers, net.JoinHostPort(server, config.Port))
		}
		if len(resolvers) == 0 {
			return nil, fmt.Errorf("%w: the system names no resolver", ErrCertsCaaResolverRefused)
		}
	}
	if exchange == nil {
		exchange = func(ctx context.Context, message *dns.Msg, server string) (*dns.Msg, error) {
			client := &dns.Client{Net: "udp", Timeout: settings.QueryTimeout, UDPSize: uint16(settings.UdpBufferByteCount)}
			response, _, err := client.ExchangeContext(ctx, message, server)
			if err == nil && response.Truncated {
				tcpClient := &dns.Client{Net: "tcp", Timeout: settings.QueryTimeout}
				response, _, err = tcpClient.ExchangeContext(ctx, message, server)
			}
			return response, err
		}
	}
	return &CertsCaaResolver{settings: settings, resolvers: resolvers, exchange: exchange}, nil
}

// A question for name and qtype, with recursion desired or not.
func (self *CertsCaaResolver) question(name string, qtype uint16, recursionDesired bool) *dns.Msg {
	message := &dns.Msg{}
	message.SetQuestion(dns.Fqdn(name), qtype)
	message.RecursionDesired = recursionDesired
	message.SetEdns0(uint16(self.settings.UdpBufferByteCount), false)
	return message
}

// Asks the recursive resolvers in turn: the first answer whose rcode is
// NOERROR or NXDOMAIN.
func (self *CertsCaaResolver) recursive(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	var lastErr error = errCertsCaaResolverNoAnswer
	for _, resolver := range self.resolvers {
		callCtx, cancel := context.WithTimeout(ctx, self.settings.QueryTimeout)
		response, err := self.exchange(callCtx, self.question(name, qtype, true), resolver)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError {
			lastErr = fmt.Errorf("a resolver answered %s", dns.RcodeToString[response.Rcode])
			continue
		}
		return response, nil
	}
	return nil, lastErr
}

// A record's RDATA in presentation form: `0 issue "..."` for a CAA.
func certsCaaRdata(record dns.RR) string {
	return strings.TrimPrefix(record.String(), record.Header().String())
}

// The zone name is in and the zone's nameservers: the owner of the SOA its
// answer carries, found climbing from name, then the zone's NS RRset.
func (self *CertsCaaResolver) zone(ctx context.Context, name string) (string, []string, error) {
	zone := ""
	candidate := dns.CanonicalName(name)
	for range self.settings.ZoneSearchMaxCount {
		response, err := self.recursive(ctx, candidate, dns.TypeSOA)
		if err != nil {
			return "", nil, err
		}
		for _, record := range response.Answer {
			if soa, ok := record.(*dns.SOA); ok && dns.CanonicalName(soa.Hdr.Name) == candidate {
				zone = candidate
			}
		}
		if zone == "" && len(response.Answer) == 0 {
			// NODATA or NXDOMAIN: the authority's SOA is the enclosing zone's
			for _, record := range response.Ns {
				if soa, ok := record.(*dns.SOA); ok && dns.IsSubDomain(dns.CanonicalName(soa.Hdr.Name), candidate) {
					zone = dns.CanonicalName(soa.Hdr.Name)
				}
			}
		}
		if zone != "" || candidate == "." {
			break
		}
		// an alias, whose own zone is its parent's or above
		labels := dns.SplitDomainName(candidate)
		candidate = dns.Fqdn(strings.Join(labels[1:], "."))
	}
	if zone == "" || zone == "." {
		return "", nil, errors.New("no zone found for a name")
	}
	response, err := self.recursive(ctx, zone, dns.TypeNS)
	if err != nil {
		return "", nil, err
	}
	nameservers := []string{}
	for _, record := range response.Answer {
		if ns, ok := record.(*dns.NS); ok && dns.CanonicalName(ns.Hdr.Name) == zone {
			nameserver := strings.TrimSuffix(dns.CanonicalName(ns.Ns), ".")
			if !slices.Contains(nameservers, nameserver) {
				nameservers = append(nameservers, nameserver)
			}
		}
	}
	if len(nameservers) == 0 {
		return "", nil, errCertsCaaResolverNoNameservers
	}
	slices.Sort(nameservers)
	return strings.TrimSuffix(zone, "."), nameservers, nil
}

// A CAA query for name at the recursive resolvers, following CNAMEs, with
// each name's zone and nameservers. See sidecar.TlsCaaResolver.
func (self *CertsCaaResolver) LookupCaa(ctx context.Context, name string) (*sidecar.TlsCaaAnswer, error) {
	response, err := self.recursive(ctx, name, dns.TypeCAA)
	if err != nil {
		return nil, err
	}
	current := dns.CanonicalName(name)
	names := []string{current}
	for {
		target := ""
		for _, record := range response.Answer {
			if cname, ok := record.(*dns.CNAME); ok && dns.CanonicalName(cname.Hdr.Name) == current {
				target = dns.CanonicalName(cname.Target)
			}
		}
		if target == "" {
			break
		}
		if slices.Contains(names, target) || self.settings.AliasMaxCount < len(names) {
			return nil, errors.New("an alias chain that loops or runs past its bound")
		}
		names = append(names, target)
		current = target
	}
	answer := &sidecar.TlsCaaAnswer{}
	for _, record := range response.Answer {
		if caa, ok := record.(*dns.CAA); ok && dns.CanonicalName(caa.Hdr.Name) == current {
			answer.Records = append(answer.Records, certsCaaRdata(caa))
		}
	}
	for _, hopName := range names {
		zone, nameservers, err := self.zone(ctx, hopName)
		if err != nil {
			return nil, err
		}
		answer.Hops = append(answer.Hops, &sidecar.TlsCaaHop{
			Name:        strings.TrimSuffix(hopName, "."),
			Zone:        zone,
			Nameservers: nameservers,
		})
	}
	return answer, nil
}

// A nameserver's addresses, A then AAAA, at most AddressMaxCount.
func (self *CertsCaaResolver) addresses(ctx context.Context, nameserver string) []string {
	addresses := []string{}
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		response, err := self.recursive(ctx, nameserver, qtype)
		if err != nil {
			continue
		}
		for _, record := range response.Answer {
			switch value := record.(type) {
			case *dns.A:
				addresses = append(addresses, value.A.String())
			case *dns.AAAA:
				addresses = append(addresses, value.AAAA.String())
			}
		}
	}
	return addresses[:min(len(addresses), self.settings.AddressMaxCount)]
}

// A CAA query for name with RD=0 at each nameserver, at the first of its
// addresses that answers. See certsservice.CertsCaaResolver.
func (self *CertsCaaResolver) AuthoritativeCaa(ctx context.Context, name string, nameservers []string) (map[string][]string, error) {
	answers := map[string][]string{}
	for _, nameserver := range nameservers[:min(len(nameservers), self.settings.NameserverMaxCount)] {
		for _, address := range self.addresses(ctx, nameserver) {
			callCtx, cancel := context.WithTimeout(ctx, self.settings.QueryTimeout)
			response, err := self.exchange(callCtx, self.question(name, dns.TypeCAA, false), net.JoinHostPort(address, strconv.Itoa(self.settings.AuthoritativePort)))
			cancel()
			if err != nil || (response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError) {
				continue
			}
			// every CAA in the answer, whatever its owner: the nonce shows wherever a flattening puts it
			records := []string{}
			for _, record := range response.Answer {
				if caa, ok := record.(*dns.CAA); ok {
					records = append(records, certsCaaRdata(caa))
				}
			}
			answers[nameserver] = records
			break
		}
	}
	if len(answers) == 0 {
		return nil, errCertsCaaResolverAuthoritative
	}
	return answers, nil
}

// One POST of the canary route: the answer for a 200, and the route's
// refusal otherwise, a 409's code read from its body.
func (self *CertsDnsClient) callCaaCanary(ctx context.Context, request *dnsservice.DnsCaaCanaryRequest) (*dnsservice.DnsCaaCanaryAnswer, error) {
	request.Root = self.settings.Root
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	callUrl := *self.baseUrl
	callUrl.Path = dnsservice.DnsCaaCanaryPath
	if self.settings.Root != "" {
		callUrl.RawQuery = url.Values{"root": {self.settings.Root}}.Encode()
	}
	httpRequest, err := http.NewRequestWithContext(callCtx, http.MethodPost, callUrl.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+self.bearer)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := self.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("dns %s: %w", dnsservice.DnsCaaCanaryPath, err)
	}
	defer response.Body.Close()
	answerBytes, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaxResponseByteCount+1))
	if err != nil {
		return nil, fmt.Errorf("dns %s: %w", dnsservice.DnsCaaCanaryPath, err)
	}
	if self.settings.MaxResponseByteCount < int64(len(answerBytes)) {
		return nil, fmt.Errorf("dns %s: an answer past %d bytes", dnsservice.DnsCaaCanaryPath, self.settings.MaxResponseByteCount)
	}
	switch response.StatusCode {
	case http.StatusOK:
		answer := &dnsservice.DnsCaaCanaryAnswer{}
		if err := json.Unmarshal(answerBytes, answer); err != nil {
			return nil, fmt.Errorf("dns %s: an answer that does not parse", dnsservice.DnsCaaCanaryPath)
		}
		return answer, nil
	case http.StatusConflict:
		refusal := &dnsservice.DnsCaaCanaryRefusal{}
		if err := json.Unmarshal(answerBytes, refusal); err == nil {
			switch refusal.Code {
			case dnsservice.DnsCaaCanaryCodeLintRefused:
				return nil, certsservice.ErrCertsCaaCanaryLint
			case dnsservice.DnsCaaCanaryCodeBusy:
				return nil, certsservice.ErrCertsCaaCanaryBusy
			}
		}
		// a 409 this client does not know is never read as the probe's verdict
		return nil, fmt.Errorf("dns %s: status 409 with a code this client does not know", dnsservice.DnsCaaCanaryPath)
	case http.StatusBadRequest:
		return nil, certsservice.ErrCertsCaaCanaryRefused
	case http.StatusServiceUnavailable:
		return nil, certsservice.ErrCertsDnsRestoring
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: status %d", ErrCertsDnsCredentialRefused, response.StatusCode)
	}
	return nil, fmt.Errorf("dns %s: status %d", dnsservice.DnsCaaCanaryPath, response.StatusCode)
}

// POST /dns/internal/caa-canary's publish. See
// certsservice.CertsCaaCanaryDns.
func (self *CertsDnsClient) PublishCaaCanary(ctx context.Context, owner string, caa []string, replaces string) (uint64, string, error) {
	if len(caa) == 0 {
		return 0, "", certsservice.ErrCertsCaaCanaryRefused
	}
	answer, err := self.callCaaCanary(ctx, &dnsservice.DnsCaaCanaryRequest{Owner: owner, Caa: caa, Replaces: replaces})
	if err != nil {
		return 0, "", err
	}
	if answer.Serial == 0 || answer.Nonce == "" {
		return 0, "", fmt.Errorf("dns %s: a publish answered no serial or nonce", dnsservice.DnsCaaCanaryPath)
	}
	return answer.Serial, answer.Nonce, nil
}

// The same route's delete. See certsservice.CertsCaaCanaryDns.
func (self *CertsDnsClient) DeleteCaaCanary(ctx context.Context, owner string) (uint64, error) {
	answer, err := self.callCaaCanary(ctx, &dnsservice.DnsCaaCanaryRequest{Owner: owner, Delete: true})
	if err != nil {
		return 0, err
	}
	return answer.Serial, nil
}
