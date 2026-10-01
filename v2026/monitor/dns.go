package monitor

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsObservationTimeout = 8 * time.Second
const dnsExchangeTimeout = 4 * time.Second

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

func dnsWireQuery(ctx context.Context, network, endpoint, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
	packet, queryID, questionName, questionType, err := buildDNSQuestion(hostname, recordType)
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
	if !truncated || network == "tcp" {
		return observation, nil
	}
	response, err = exchangeDNSPacket(ctx, "tcp", endpoint, packet)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	observation, truncated, err = parseDNSResponse(response, queryID, questionName, questionType)
	if err != nil {
		return DNSResponseObservation{}, err
	}
	if truncated {
		return DNSResponseObservation{}, fmt.Errorf("truncated DNS response over TCP")
	}
	return observation, nil
}

func buildDNSQuestion(hostname string, recordType DNSRecordType) ([]byte, uint16, dnsmessage.Name, dnsmessage.Type, error) {
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
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: queryID})
	if err := builder.StartQuestions(); err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
	}
	if err := builder.Question(dnsmessage.Question{Name: name, Type: messageType, Class: dnsmessage.ClassINET}); err != nil {
		return nil, 0, dnsmessage.Name{}, 0, err
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
