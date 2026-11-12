package monitor

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsObservationTimeout = 8 * time.Second
const dnsExchangeTimeout = 4 * time.Second

// The EDNS(0) payload size a client's DoH query carries (connect
// net_http_doh.go dohEdnsUdpPayloadSize).
const dnsDohEdnsPayloadSize = 1232

// The EDNS(0) option code of a client subnet (RFC 7871).
const dnsClientSubnetOptionCode = 8

// A UDP answer was marked truncated and its TCP retry failed: the resolver
// answered, but the whole answer never arrived. A client's resolver, which
// retries over TCP the same way, gets nothing either.
type dnsTruncatedRetryError struct {
	err error
}

func (self *dnsTruncatedRetryError) Error() string {
	return "truncated DNS response, TCP retry failed: " + self.err.Error()
}

func (self *dnsTruncatedRetryError) Unwrap() error { return self.err }

// A TCP answer was marked truncated, which leaves no transport to retry.
var errDNSTruncatedOverTCP = errors.New("truncated DNS response over TCP")

type dnsAuthorityResult struct {
	observation DNSResponseObservation
	err         error
}

// dnsAuthoritative discovers every authoritative nameserver for the managed
// zone, queries each directly with recursion disabled, and retains one
// structural response per authority. Individual endpoint identities and wire
// errors never leave this transport boundary.
func (r *runner) dnsAuthoritative(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	nameservers, err := net.DefaultResolver.LookupNS(commandCtx, absoluteDNSName(zone))
	if err != nil {
		if ctx.Err() != nil {
			return DNSAuthoritativeObservation{}, ctx.Err()
		}
		return DNSAuthoritativeObservation{}, fmt.Errorf("discover authoritative nameservers: %w", err)
	}
	if len(nameservers) == 0 {
		return DNSAuthoritativeObservation{}, fmt.Errorf("discover authoritative nameservers: empty answer")
	}
	sort.Slice(nameservers, func(i, j int) bool { return nameservers[i].Host < nameservers[j].Host })

	results := make(chan dnsAuthorityResult, len(nameservers))
	var wait sync.WaitGroup
	for _, nameserver := range nameservers {
		nameserver := absoluteDNSName(nameserver.Host)
		wait.Add(1)
		go func() {
			defer wait.Done()
			observation, queryErr := queryAuthoritativeNameserver(commandCtx, nameserver, hostname, recordType)
			results <- dnsAuthorityResult{observation: observation, err: queryErr}
		}()
	}
	wait.Wait()
	close(results)

	observation := DNSAuthoritativeObservation{NameserverCount: len(nameservers)}
	for result := range results {
		if result.err != nil {
			observation.FailedNameservers++
			continue
		}
		observation.Responses = append(observation.Responses, result.observation)
	}
	if ctx.Err() != nil {
		return DNSAuthoritativeObservation{}, ctx.Err()
	}
	if len(observation.Responses) == 0 {
		if commandCtx.Err() != nil {
			return observation, fmt.Errorf("query authoritative nameservers: timeout")
		}
		return observation, fmt.Errorf("query authoritative nameservers: no response")
	}
	return observation, nil
}

func queryAuthoritativeNameserver(ctx context.Context, nameserver, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, absoluteDNSName(nameserver))
	if err != nil {
		return DNSResponseObservation{}, fmt.Errorf("resolve authoritative nameserver: %w", err)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].IP.String() < addresses[j].IP.String() })
	var lastErr error
	for _, address := range addresses {
		endpoint := net.JoinHostPort(address.IP.String(), "53")
		observation, err := dnsWireQuery(ctx, "udp", endpoint, hostname, recordType)
		if err == nil {
			return observation, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("authoritative nameserver has no address")
	}
	return DNSResponseObservation{}, lastErr
}

// dnsRecursive uses the platform resolver for the requested family. When a
// family is absent, a successful lookup of the companion family proves
// NOERROR/NODATA behavior; absence of both families is retained as name-error
// and can never satisfy an intentionally single-stack alias.
func (r *runner) dnsRecursive(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	network, companion, err := dnsLookupNetworks(recordType)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	absoluteHostname := absoluteDNSName(hostname)
	addresses, err := net.DefaultResolver.LookupNetIP(commandCtx, network, absoluteHostname)
	if err == nil {
		return DNSResponseObservation{
			Addresses:    canonicalNetIPStrings(addresses),
			ResponseCode: DNSResponseSuccess,
		}, nil
	}
	if ctx.Err() != nil {
		return DNSResponseObservation{}, ctx.Err()
	}
	if !dnsNameNotFound(err) {
		return DNSResponseObservation{}, fmt.Errorf("recursive address lookup: %w", err)
	}

	companionAddresses, companionErr := net.DefaultResolver.LookupNetIP(commandCtx, companion, absoluteHostname)
	if companionErr == nil && len(companionAddresses) > 0 {
		return DNSResponseObservation{ResponseCode: DNSResponseSuccess}, nil
	}
	if ctx.Err() != nil {
		return DNSResponseObservation{}, ctx.Err()
	}
	if companionErr != nil && !dnsNameNotFound(companionErr) {
		return DNSResponseObservation{}, fmt.Errorf("recursive companion lookup: %w", companionErr)
	}
	return DNSResponseObservation{ResponseCode: DNSResponseNameError}, nil
}

// dnsTxt asks one resolver (host:port) for a name's TXT records over the DNS
// wire protocol: UDP first, then TCP when the answer is truncated, as a large
// signed TXT set always is. The platform resolver is not used: a home router
// that answered the name's A query dropped its 4 KB TXT answer over both UDP
// and TCP (2026-11-11). The response code says whether the name serves no
// TXT records (success, no values) or does not exist (name-error).
func (r *runner) dnsTxt(ctx context.Context, resolver, hostname string) (DNSResponseObservation, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	// a recursive resolver asked without recursion desired answers from its
	// cache only, or refuses
	observation, err := dnsWireQueryRecursion(commandCtx, "udp", resolver, hostname, DNSRecordTXT, true)
	if err != nil {
		if ctx.Err() != nil {
			return DNSResponseObservation{}, ctx.Err()
		}
		return DNSResponseObservation{}, fmt.Errorf("txt query: %w", err)
	}
	return observation, nil
}

// dnsAddress asks one resolver (host:port) for a name's A records over the
// DNS wire protocol with recursion desired, UDP then TCP when truncated. It is
// the small question that tells a resolver which drops a name's TXT answer
// from one that answers nothing at all.
func (r *runner) dnsAddress(ctx context.Context, resolver, hostname string) (DNSResponseObservation, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	observation, err := dnsWireQueryRecursion(commandCtx, "udp", resolver, hostname, DNSRecordA, true)
	if err != nil {
		if ctx.Err() != nil {
			return DNSResponseObservation{}, ctx.Err()
		}
		return DNSResponseObservation{}, fmt.Errorf("address query: %w", err)
	}
	return observation, nil
}

// dnsDohTxt asks one DoH server for a name's TXT records the way a client asks
// one (connect net_http_doh.go queryWireRawDetailedWithRoute): an RFC 8484 GET
// of a query with id 0, recursion desired and an EDNS(0) OPT record of 1232
// bytes, Accept application/dns-message, over https to the url's ip literal,
// whose certificate must name that ip. It returns the HTTP status and the
// body, read to at most maxBytes+1 bytes, so an answer longer than maxBytes
// reads as one byte over. No proxy from the environment applies, no redirect
// is followed, and no connection outlives the call.
func (r *runner) dnsDohTxt(ctx context.Context, dohUrl, hostname string, maxBytes int) (int, []byte, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	query, err := buildDohTxtQuery(hostname)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(commandCtx, http.MethodGet, dohUrl+"?dns="+base64.RawURLEncoding.EncodeToString(query), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("doh request: %w", dohRequestError(err))
	}
	request.Header.Set("Accept", "application/dns-message")

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if r.dohTLSConfig != nil {
		tlsConfig = r.dohTLSConfig.Clone()
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{}).DialContext,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   dnsExchangeTimeout,
		ResponseHeaderTimeout: dnsExchangeTimeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		// the url carries the encoded question; keep the transport error only
		return 0, nil, fmt.Errorf("doh query: %w", dohRequestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil, fmt.Errorf("doh query: HTTP status %d", response.StatusCode)
	}
	if maxBytes <= 0 {
		maxBytes = dnsDohDefaultResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		if ctx.Err() != nil {
			return response.StatusCode, nil, ctx.Err()
		}
		return response.StatusCode, nil, fmt.Errorf("doh read: %w", dohRequestError(err))
	}
	return response.StatusCode, body, nil
}

// The read bound of a DoH body when a caller names none: the 16 KiB a client
// reads (connect net_http_doh.go maxDohResponseBytes).
const dnsDohDefaultResponseBytes = 16 * 1024

// The underlying error of a url.Error, whose text holds the request url.
func dohRequestError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// The wire query of a client's DoH TXT question (connect net_http_doh.go):
// id 0 (RFC 8484 §4.1), recursion desired, and an EDNS(0) OPT record without
// options, which asks for extended dns errors.
func buildDohTxtQuery(hostname string) ([]byte, error) {
	name, err := dnsmessage.NewName(absoluteDNSName(hostname))
	if err != nil {
		return nil, err
	}
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(dnsDohEdnsPayloadSize, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	question := dnsmessage.Question{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}
	message := dnsmessage.Message{
		Header:      dnsmessage.Header{RecursionDesired: true},
		Questions:   []dnsmessage.Question{question},
		Additionals: []dnsmessage.Resource{{Header: optHeader, Body: &dnsmessage.OPTResource{}}},
	}
	return message.Pack()
}

// dnsNameservers finds the authoritative nameservers of the zone that holds a
// name, through the platform resolver as dnsAuthoritative does: the name's own
// NS set when it is a zone apex (extender.bringyour.com is delegated to a
// hosted zone of its own), else its parent's, up to two labels. Each is
// returned as ipv4:53, sorted.
func (r *runner) dnsNameservers(ctx context.Context, name string) ([]string, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	zone := absoluteDNSName(name)
	var nameservers []*net.NS
	for {
		found, err := net.DefaultResolver.LookupNS(commandCtx, zone)
		if err == nil && 0 < len(found) {
			nameservers = found
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil && !dnsNameNotFound(err) {
			return nil, fmt.Errorf("discover authoritative nameservers: %w", err)
		}
		_, parent, _ := strings.Cut(zone, ".")
		if strings.Count(parent, ".") < 2 {
			return nil, fmt.Errorf("discover authoritative nameservers: no zone holds the name")
		}
		zone = parent
	}

	hosts := make([]string, 0, len(nameservers))
	for _, nameserver := range nameservers {
		hosts = append(hosts, absoluteDNSName(nameserver.Host))
	}
	sort.Strings(hosts)
	endpoints := []string{}
	for _, nameserverHost := range hosts {
		addresses, err := net.DefaultResolver.LookupNetIP(commandCtx, "ip4", nameserverHost)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		for _, address := range canonicalNetIPStrings(addresses) {
			endpoint := net.JoinHostPort(address, "53")
			if !slicesContainString(endpoints, endpoint) {
				endpoints = append(endpoints, endpoint)
			}
		}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("discover authoritative nameservers: no address")
	}
	sort.Strings(endpoints)
	return endpoints, nil
}

func slicesContainString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// dnsTxtAuthoritative asks a name's authoritative nameservers (host:port
// endpoints) for its TXT records over TCP, with recursion not desired. With a
// valid clientSubnet the question carries it as an EDNS client subnet (RFC
// 7871), which Route 53 answers by the subnet's location as it answers a
// resolver that forwards its client's subnet; without one it answers by the
// monitor's own location. The first nameserver that answers decides.
func (r *runner) dnsTxtAuthoritative(ctx context.Context, hostname string, servers []string, clientSubnet netip.Prefix) (DNSResponseObservation, error) {
	commandCtx, cancel := boundedDNSContext(ctx, r.cfg.commandTimeout)
	defer cancel()

	additional, err := dnsClientSubnetAdditional(clientSubnet)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	var lastErr error
	for _, server := range servers {
		observation, err := dnsWireQueryOptions(commandCtx, "tcp", server, hostname, DNSRecordTXT, false, additional)
		if err == nil {
			return observation, nil
		}
		if ctx.Err() != nil {
			return DNSResponseObservation{}, ctx.Err()
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no authoritative nameserver")
	}
	return DNSResponseObservation{}, fmt.Errorf("authoritative txt query: %w", lastErr)
}

// The OPT record of a question: an EDNS client subnet option when the subnet
// is valid (RFC 7871 §6: family, source prefix length, scope 0, and the
// prefix's significant bytes), else none.
func dnsClientSubnetAdditional(clientSubnet netip.Prefix) (*dnsmessage.Resource, error) {
	if !clientSubnet.IsValid() {
		return nil, nil
	}
	clientSubnet = clientSubnet.Masked()
	family := uint16(1)
	if clientSubnet.Addr().Is6() {
		family = 2
	}
	bits := clientSubnet.Bits()
	address := clientSubnet.Addr().AsSlice()[:(bits+7)/8]
	data := make([]byte, 4+len(address))
	binary.BigEndian.PutUint16(data[:2], family)
	data[2] = byte(bits)
	copy(data[4:], address)
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(dnsDohEdnsPayloadSize, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	return &dnsmessage.Resource{
		Header: optHeader,
		Body:   &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: dnsClientSubnetOptionCode, Data: data}}},
	}, nil
}

func boundedDNSContext(ctx context.Context, configured time.Duration) (context.Context, context.CancelFunc) {
	timeout := configured
	if timeout <= 0 || timeout > dnsObservationTimeout {
		timeout = dnsObservationTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// absoluteDNSName prevents the platform resolver from appending local search
// suffixes after an exact public name returns no data or does not exist.
func absoluteDNSName(name string) string {
	return strings.TrimSuffix(strings.TrimSpace(name), ".") + "."
}

func dnsLookupNetworks(recordType DNSRecordType) (network, companion string, err error) {
	switch recordType {
	case DNSRecordA:
		return "ip4", "ip6", nil
	case DNSRecordAAAA:
		return "ip6", "ip4", nil
	default:
		return "", "", fmt.Errorf("unsupported DNS record type %q", recordType)
	}
}

func dnsNameNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func canonicalNetIPStrings(addresses []netip.Addr) []string {
	values := make([]string, 0, len(addresses))
	seen := map[string]struct{}{}
	for _, address := range addresses {
		value := address.Unmap().String()
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// dnsWireQuery asks a server directly with recursion not desired, as an
// authoritative nameserver is asked.
func dnsWireQuery(ctx context.Context, network, endpoint, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
	return dnsWireQueryRecursion(ctx, network, endpoint, hostname, recordType, false)
}

// dnsWireQueryRecursion asks a server over UDP, or TCP, retrying a truncated
// UDP answer over TCP; recursionDesired is set when the server is a recursive
// resolver.
func dnsWireQueryRecursion(ctx context.Context, network, endpoint, hostname string, recordType DNSRecordType, recursionDesired bool) (DNSResponseObservation, error) {
	return dnsWireQueryOptions(ctx, network, endpoint, hostname, recordType, recursionDesired, nil)
}

// dnsWireQueryOptions is dnsWireQueryRecursion with an optional OPT record in
// the question's additional section. A truncated UDP answer whose TCP retry
// fails returns a *dnsTruncatedRetryError, and a truncated TCP answer
// errDNSTruncatedOverTCP.
func dnsWireQueryOptions(ctx context.Context, network, endpoint, hostname string, recordType DNSRecordType, recursionDesired bool, additional *dnsmessage.Resource) (DNSResponseObservation, error) {
	packet, queryID, questionName, questionType, err := buildDNSQuestionWith(hostname, recordType, recursionDesired, additional)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	response, err := exchangeDNSPacket(ctx, network, endpoint, packet)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	observation, truncated, err := parseDNSResponse(response, queryID, questionName, questionType)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	if !truncated {
		return observation, nil
	}
	if network == "tcp" {
		return DNSResponseObservation{}, errDNSTruncatedOverTCP
	}
	response, err = exchangeDNSPacket(ctx, "tcp", endpoint, packet)
	if err != nil {
		return DNSResponseObservation{}, &dnsTruncatedRetryError{err: err}
	}
	observation, truncated, err = parseDNSResponse(response, queryID, questionName, questionType)
	if err != nil {
		return DNSResponseObservation{}, &dnsTruncatedRetryError{err: err}
	}
	if truncated {
		return DNSResponseObservation{}, errDNSTruncatedOverTCP
	}
	return observation, nil
}

func buildDNSQuestion(hostname string, recordType DNSRecordType, recursionDesired bool) ([]byte, uint16, dnsmessage.Name, dnsmessage.Type, error) {
	return buildDNSQuestionWith(hostname, recordType, recursionDesired, nil)
}

func buildDNSQuestionWith(hostname string, recordType DNSRecordType, recursionDesired bool, additional *dnsmessage.Resource) ([]byte, uint16, dnsmessage.Name, dnsmessage.Type, error) {
	name, err := dnsmessage.NewName(strings.TrimSuffix(hostname, ".") + ".")
	if err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
	}
	messageType, err := dnsMessageType(recordType)
	if err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
	}
	var random [2]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
	}
	queryID := binary.BigEndian.Uint16(random[:])
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: queryID, RecursionDesired: recursionDesired})
	if err := builder.StartQuestions(); err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
	}
	if err := builder.Question(dnsmessage.Question{Name: name, Type: messageType, Class: dnsmessage.ClassINET}); err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
	}
	if additional != nil {
		if err := builder.StartAdditionals(); err != nil {
			return nil, 0, dnsmessage.Name{}, 0, err
		}
		option, ok := additional.Body.(*dnsmessage.OPTResource)
		if !ok {
			return nil, 0, dnsmessage.Name{}, 0, fmt.Errorf("unsupported additional record")
		}
		if err := builder.OPTResource(additional.Header, *option); err != nil {
			return nil, 0, dnsmessage.Name{}, 0, err
		}
	}
	packet, err := builder.Finish()
	return packet, queryID, name, messageType, err
}

func dnsMessageType(recordType DNSRecordType) (dnsmessage.Type, error) {
	switch recordType {
	case DNSRecordA:
		return dnsmessage.TypeA, nil
	case DNSRecordAAAA:
		return dnsmessage.TypeAAAA, nil
	case DNSRecordTXT:
		return dnsmessage.TypeTXT, nil
	default:
		return 0, fmt.Errorf("unsupported DNS record type %q", recordType)
	}
}

func exchangeDNSPacket(ctx context.Context, network, endpoint string, packet []byte) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, dnsExchangeTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(commandCtx, network, endpoint)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	stopCancellation := context.AfterFunc(commandCtx, func() {
		_ = connection.SetDeadline(time.Now())
	})
	defer stopCancellation()
	if deadline, ok := commandCtx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	if network == "tcp" {
		if len(packet) > 65535 {
			return nil, fmt.Errorf("DNS packet exceeds TCP frame")
		}
		framed := make([]byte, 2+len(packet))
		binary.BigEndian.PutUint16(framed[:2], uint16(len(packet)))
		copy(framed[2:], packet)
		if err := writeDNSPacket(connection, framed); err != nil {
			return nil, err
		}
		var length [2]byte
		if _, err := io.ReadFull(connection, length[:]); err != nil {
			return nil, err
		}
		response := make([]byte, int(binary.BigEndian.Uint16(length[:])))
		if _, err := io.ReadFull(connection, response); err != nil {
			return nil, err
		}
		return response, nil
	}

	if err := writeDNSPacket(connection, packet); err != nil {
		return nil, err
	}
	response := make([]byte, 65535)
	count, err := connection.Read(response)
	if err != nil {
		return nil, err
	}
	return response[:count], nil
}

func writeDNSPacket(writer io.Writer, packet []byte) error {
	for len(packet) > 0 {
		count, err := writer.Write(packet)
		if count > 0 {
			packet = packet[count:]
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func parseDNSResponse(packet []byte, queryID uint16, questionName dnsmessage.Name, questionType dnsmessage.Type) (DNSResponseObservation, bool, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(packet)
	if err != nil {
		return DNSResponseObservation{}, false, err
	}
	if !header.Response || header.ID != queryID {
		return DNSResponseObservation{}, false, fmt.Errorf("DNS response does not match query")
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		return DNSResponseObservation{}, false, err
	}
	if len(questions) != 1 || !strings.EqualFold(questions[0].Name.String(), questionName.String()) ||
		questions[0].Type != questionType || questions[0].Class != dnsmessage.ClassINET {
		return DNSResponseObservation{}, false, fmt.Errorf("DNS response question does not match query")
	}
	if header.Truncated {
		return DNSResponseObservation{}, true, nil
	}
	answers, err := parser.AllAnswers()
	if err != nil {
		return DNSResponseObservation{}, false, err
	}
	observation := DNSResponseObservation{
		ResponseCode:  classifyDNSResponseCode(header.RCode),
		Authoritative: header.Authoritative,
	}
	queryOwner := strings.ToLower(questionName.String())
	for _, answer := range answers {
		if strings.ToLower(answer.Header.Name.String()) != queryOwner ||
			answer.Header.Class != dnsmessage.ClassINET {
			continue
		}
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			if questionType == dnsmessage.TypeA {
				observation.Addresses = append(observation.Addresses, netip.AddrFrom4(body.A).String())
			}
		case *dnsmessage.AAAAResource:
			if questionType == dnsmessage.TypeAAAA {
				observation.Addresses = append(observation.Addresses, netip.AddrFrom16(body.AAAA).String())
			}
		case *dnsmessage.CNAMEResource:
			observation.CNAMECount++
		case *dnsmessage.TXTResource:
			if questionType == dnsmessage.TypeTXT {
				// one record's character strings are one value, as a
				// client joins them
				observation.Texts = append(observation.Texts, strings.Join(body.TXT, ""))
			}
		}
	}
	observation.Addresses = canonicalAddressStrings(observation.Addresses)
	return observation, header.Truncated, nil
}

func classifyDNSResponseCode(code dnsmessage.RCode) DNSResponseCode {
	switch code {
	case dnsmessage.RCodeSuccess:
		return DNSResponseSuccess
	case dnsmessage.RCodeNameError:
		return DNSResponseNameError
	case dnsmessage.RCodeServerFailure:
		return DNSResponseServerFailure
	case dnsmessage.RCodeRefused:
		return DNSResponseRefused
	default:
		return DNSResponseOther
	}
}
