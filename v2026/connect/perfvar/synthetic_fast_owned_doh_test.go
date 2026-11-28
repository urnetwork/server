// An owned, verified-TLS DoH service stands behind the unchanged default
// resolver destinations. It supplies fixture answers, never public DNS truth.
package perfvar

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"
)

const syntheticFastOwnedDohControlName = "owned-doh-control.example."

// Reserved fixture addresses are returned only for Warm and the owned control.
func syntheticFastOwnedDohAnswer4() [4]byte { return [4]byte{192, 0, 2, 200} }
func syntheticFastOwnedDohAnswer6() [16]byte {
	return [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x02, 0x00}
}

// Writes are service receipts; the composed control separately parses the
// returned TUN response. Neither receipt invents private exit ownership.
type syntheticFastOwnedDohReport struct {
	DefaultUrlsIpv4, DefaultUrlsIpv6, OwnedEndpoints                         int
	HandlersStarted, HandlersFinished, ResponsesWritten                      uint64
	WarmResponses, ControlAResponses, ControlAaaaResponses                   uint64
	Http2Responses, MalformedRequests, OtherQuestions, WriteFailures         uint64
	DialsStartedIpv4, DialsStartedIpv6, DialsFinishedIpv4, DialsFinishedIpv6 uint64
	DialSocketsIpv4, DialSocketsIpv6, ResponsesIpv4, ResponsesIpv6           uint64
	MissingRoutes                                                            uint64
	Closing, ServeJoined, HandlersJoined, ServeFailed                        bool
}

// Two loopback listeners preserve the requested upstream address family.
// Immutable endpoint keys derive only from DefaultUpgradeMuxSettings.
type syntheticFastOwnedDoh struct {
	endpoints        map[netip.AddrPort]bool
	clientTls        *tls.Config
	listeners        [2]net.Listener
	server           *http.Server
	responseTtl      uint32
	answered         chan struct{}
	answeredOnce     sync.Once
	missingRoute     chan struct{}
	missingRouteOnce sync.Once
	closeOnce        sync.Once
	serveDone        chan struct{}
	handlersDone     chan struct{}
	stateLock        sync.Mutex
	activeHandlers   int
	serveReturns     int
	closeErr         error
	report           syntheticFastOwnedDohReport
}

// Only literal HTTPS endpoints are currently part of the production defaults.
// A changed default fails this fixture explicitly instead of guessing a route.
func newSyntheticFastOwnedDoh(t testing.TB) (*syntheticFastOwnedDoh, error) {
	t.Helper()
	settings := clientconnect.DefaultUpgradeMuxSettings()
	if settings == nil || settings.Dns == nil || settings.Dns.Resolver == nil {
		return nil, errors.New("owned DoH requires the actual default resolver")
	}
	resolver := settings.Dns.Resolver
	owner := &syntheticFastOwnedDoh{endpoints: map[netip.AddrPort]bool{}, responseTtl: settings.Dns.ResponseTtl,
		answered: make(chan struct{}), missingRoute: make(chan struct{}), serveDone: make(chan struct{}), handlersDone: make(chan struct{})}
	owner.report.DefaultUrlsIpv4, owner.report.DefaultUrlsIpv6 = len(resolver.RemoteDohUrlsIpv4), len(resolver.RemoteDohUrlsIpv6)
	var certificateIps []net.IP
	for family, urls := range [][]string{resolver.RemoteDohUrlsIpv4, resolver.RemoteDohUrlsIpv6} {
		if len(urls) == 0 {
			return nil, errors.New("owned DoH default family has no endpoints")
		}
		for _, raw := range urls {
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "/dns-query" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return nil, errors.New("owned DoH default URL shape changed")
			}
			endpoint, err := syntheticFastOwnedDohEndpoint(parsed.Host)
			if err != nil || endpoint.Addr().Is4() != (family == 0) {
				return nil, errors.New("owned DoH default endpoint family changed")
			}
			if !owner.endpoints[endpoint] {
				owner.endpoints[endpoint] = true
				certificateIps = append(certificateIps, net.IP(endpoint.Addr().AsSlice()))
			}
		}
	}
	owner.report.OwnedEndpoints = len(owner.endpoints)
	serverTls, clientTls, err := syntheticFastOwnedDohTls(certificateIps)
	if err != nil {
		return nil, err
	}
	owner.clientTls = clientTls
	// A genuine SYN-only qualification probe can close before TLS. Match the
	// existing owned-origin fixture's quiet stdlib server logger; actual TLS,
	// wire-response and Serve/handler failure evidence remains checked.
	owner.server = &http.Server{Handler: owner, TLSConfig: serverTls, ErrorLog: log.New(io.Discard, "", 0)}
	if err := http2.ConfigureServer(owner.server, &http2.Server{}); err != nil {
		return nil, err
	}
	for index, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		network := "tcp4"
		if index == 1 {
			network = "tcp6"
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			for _, prior := range owner.listeners {
				if prior != nil {
					_ = prior.Close()
				}
			}
			return nil, fmt.Errorf("owned DoH listener family %d: %w", index, err)
		}
		owner.listeners[index] = listener
	}
	for _, listener := range owner.listeners {
		go func() {
			err := owner.server.Serve(tls.NewListener(listener, serverTls))
			owner.stateLock.Lock()
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				owner.report.ServeFailed = true
				owner.closeErr = errors.Join(owner.closeErr, err)
			}
			owner.serveReturns++
			if owner.serveReturns == len(owner.listeners) {
				owner.report.ServeJoined = true
				close(owner.serveDone)
			}
			owner.stateLock.Unlock()
		}()
	}
	return owner, nil
}

// Both raw socket addresses and HTTP authorities keep their original port.
func syntheticFastOwnedDohEndpoint(authority string) (netip.AddrPort, error) {
	parsed := &url.URL{Host: authority}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil {
		return netip.AddrPort{}, err
	}
	port := 443
	if text := parsed.Port(); text != "" {
		port, err = strconv.Atoi(text)
		if err != nil || port <= 0 || 65535 < port {
			return netip.AddrPort{}, errors.New("owned DoH endpoint has an invalid port")
		}
	}
	return netip.AddrPortFrom(address.Unmap(), uint16(port)), nil
}

// Each caller receives fresh factory settings; only its resolver trust changes.
func (self *syntheticFastOwnedDoh) settings() *clientconnect.UpgradeMuxSettings {
	settings := clientconnect.DefaultUpgradeMuxSettings()
	settings.Dns.Resolver.TlsConfig = self.clientTls.Clone()
	settings.Dns.Resolver.TlsConfig.RootCAs = self.clientTls.RootCAs.Clone()
	return settings
}

// The caller also checks TCP; no other public destination gains a fixture route.
func (self *syntheticFastOwnedDoh) matches(ip net.IP, port int) bool {
	if self == nil || port <= 0 || 65535 < port {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	return ok && self.endpoints[netip.AddrPortFrom(address.Unmap(), uint16(port))]
}

// This is the provider's upstream substitution, not its carrier/API fallback.
func (self *syntheticFastOwnedDoh) dial(ctx context.Context, network, address string) (net.Conn, error) {
	endpoint, err := syntheticFastOwnedDohEndpoint(address)
	if err != nil || !self.endpoints[endpoint] {
		return nil, errors.New("unowned DoH dial destination")
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("owned DoH requires TCP")
	}
	index, localNetwork := 0, "tcp4"
	if endpoint.Addr().Is6() {
		index, localNetwork = 1, "tcp6"
	}
	if network != "tcp" && network != localNetwork {
		return nil, errors.New("owned DoH dial family mismatch")
	}
	self.stateLock.Lock()
	if index == 0 {
		self.report.DialsStartedIpv4++
	} else {
		self.report.DialsStartedIpv6++
	}
	self.stateLock.Unlock()
	connection, dialErr := (&net.Dialer{}).DialContext(ctx, localNetwork, self.listeners[index].Addr().String())
	self.stateLock.Lock()
	if index == 0 {
		self.report.DialsFinishedIpv4++
		if connection != nil {
			self.report.DialSocketsIpv4++
		}
	} else {
		self.report.DialsFinishedIpv6++
		if connection != nil {
			self.report.DialSocketsIpv6++
		}
	}
	self.stateLock.Unlock()
	return connection, dialErr
}

// Only the RED integration calls this at the exact original missing-route edge.
func (self *syntheticFastOwnedDoh) observeMissingRoute() {
	self.stateLock.Lock()
	self.report.MissingRoutes++
	self.stateLock.Unlock()
	self.missingRouteOnce.Do(func() { close(self.missingRoute) })
}

// The actual DoH client's GET must negotiate h2 and carry one valid wire query.
func (self *syntheticFastOwnedDoh) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	self.stateLock.Lock()
	if self.report.Closing {
		self.stateLock.Unlock()
		return
	}
	self.activeHandlers++
	self.report.HandlersStarted++
	self.stateLock.Unlock()
	defer func() {
		self.stateLock.Lock()
		self.activeHandlers--
		self.report.HandlersFinished++
		if self.report.Closing && self.activeHandlers == 0 {
			self.report.HandlersJoined = true
			close(self.handlersDone)
		}
		self.stateLock.Unlock()
	}()
	bad := func() {
		self.stateLock.Lock()
		self.report.MalformedRequests++
		self.stateLock.Unlock()
		http.Error(writer, "invalid owned DoH request", http.StatusBadRequest)
	}
	endpoint, err := syntheticFastOwnedDohEndpoint(request.Host)
	if request.Method != http.MethodGet || request.URL.Path != "/dns-query" || request.TLS == nil ||
		request.ProtoMajor != 2 || request.TLS.NegotiatedProtocol != "h2" || err != nil || !self.endpoints[endpoint] {
		bad()
		return
	}
	local, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		bad()
		return
	}
	localEndpoint, err := netip.ParseAddrPort(local.String())
	if err != nil {
		bad()
		return
	}
	encoded := request.URL.Query().Get("dns")
	if len(encoded) > base64.RawURLEncoding.EncodedLen(65535) {
		bad()
		return
	}
	query, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(query) > 65535 {
		bad()
		return
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil || header.Response || header.OpCode != 0 {
		bad()
		return
	}
	question, err := parser.Question()
	if err != nil || question.Class != dnsmessage.ClassINET {
		bad()
		return
	}
	if _, err := parser.Question(); err != dnsmessage.ErrSectionDone {
		bad()
		return
	}
	if err := parser.SkipAllAnswers(); err != nil {
		bad()
		return
	}
	if err := parser.SkipAllAuthorities(); err != nil {
		bad()
		return
	}
	if err := parser.SkipAllAdditionals(); err != nil {
		bad()
		return
	}
	name := strings.ToLower(question.Name.String())
	warm := name == "example.com." && question.Type == dnsmessage.TypeA
	control := name == syntheticFastOwnedDohControlName
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true,
		RecursionDesired: header.RecursionDesired, RecursionAvailable: true})
	if err := builder.StartQuestions(); err != nil {
		bad()
		return
	}
	if err := builder.Question(question); err != nil {
		bad()
		return
	}
	if err := builder.StartAnswers(); err != nil {
		bad()
		return
	}
	resource := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: self.responseTtl}
	if warm || control {
		switch question.Type {
		case dnsmessage.TypeA:
			err = builder.AResource(resource, dnsmessage.AResource{A: syntheticFastOwnedDohAnswer4()})
		case dnsmessage.TypeAAAA:
			err = builder.AAAAResource(resource, dnsmessage.AAAAResource{AAAA: syntheticFastOwnedDohAnswer6()})
		}
	}
	if err != nil {
		bad()
		return
	}
	response, err := builder.Finish()
	if err != nil {
		bad()
		return
	}
	writer.Header().Set("Content-Type", "application/dns-message")
	written, err := writer.Write(response)
	self.stateLock.Lock()
	if err != nil || written != len(response) {
		self.report.WriteFailures++
		self.stateLock.Unlock()
		return
	}
	self.report.ResponsesWritten++
	self.report.Http2Responses++
	if localEndpoint.Addr().Unmap().Is4() {
		self.report.ResponsesIpv4++
	} else {
		self.report.ResponsesIpv6++
	}
	if warm {
		self.report.WarmResponses++
	}
	if control && question.Type == dnsmessage.TypeA {
		self.report.ControlAResponses++
	}
	if control && question.Type == dnsmessage.TypeAAAA {
		self.report.ControlAaaaResponses++
	}
	if !warm && !control {
		self.report.OtherQuestions++
	}
	self.stateLock.Unlock()
	if warm {
		self.answeredOnce.Do(func() { close(self.answered) })
	}
}

// Shutdown stops admission and joins the server and its already owned handlers.
// Close remains the bounded fallback when the supplied shutdown context expires.
func (self *syntheticFastOwnedDoh) close(ctx context.Context) error {
	self.closeOnce.Do(func() {
		self.stateLock.Lock()
		self.report.Closing = true
		if self.activeHandlers == 0 {
			self.report.HandlersJoined = true
			close(self.handlersDone)
		}
		self.stateLock.Unlock()
		err := self.server.Shutdown(ctx)
		closeErr := self.server.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		for _, listener := range self.listeners {
			_ = listener.Close()
		}
		self.stateLock.Lock()
		self.closeErr = errors.Join(self.closeErr, err, closeErr)
		self.stateLock.Unlock()
	})
	for _, done := range []<-chan struct{}{self.serveDone, self.handlersDone} {
		select {
		case <-done:
		default:
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closeErr
}

// Snapshots contain finite counters, never URLs, query payloads or keys.
func (self *syntheticFastOwnedDoh) snapshot() syntheticFastOwnedDohReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.report
}

// The leaf proves every unchanged default IP SAN under one fixture-only CA.
func syntheticFastOwnedDohTls(ips []net.IP) (*tls.Config, *tls.Config, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic Fast owned DoH CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "synthetic Fast owned DoH service"},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: ips,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafBytes, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafBytes, caBytes}, PrivateKey: key}},
		MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}}, &tls.Config{RootCAs: roots}, nil
}
