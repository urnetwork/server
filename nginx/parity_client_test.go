package nginx_test

// The parity harness's client (parity_test.go): one case run sends its
// requests to one target, nginx or the front, and records what it saw as an
// observation, a flat map of keys to values that parity_compare_test.go
// diffs. h1 requests are written byte for byte, so a case controls every
// header line, duplicates and invalid names included; h2 requests go through
// x/net's client connection, or a raw framer where a case needs fields that
// client won't send.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// How an observation writes a value: verbatim up to parityValueBytes, else
// as its length, the first parityValueDigestBytes of its sha256 and its
// first parityValueHeadBytes.
const (
	parityValueBytes       = 256
	parityValueDigestBytes = 8
	parityValueHeadBytes   = 64
)

// HPACK's default dynamic table size, the raw h2 client's decoder's.
const parityHpackTableBytes = 4 * 1024

// One target the corpus runs against.
type parityTarget struct {
	// "nginx" or "front", the first part of the case key
	name      string
	ipv4      netip.Addr
	ipv6      netip.Addr
	httpPort  int
	httpsPort int
}

// The target's address and port for a client of addr's family.
func (self *parityTarget) addrPort(clientAddr netip.Addr, scheme string) netip.AddrPort {
	addr := self.ipv4
	if clientAddr.Is6() {
		addr = self.ipv6
	}
	port := self.httpsPort
	if scheme == "http" {
		port = self.httpPort
	}
	return netip.AddrPortFrom(addr, uint16(port))
}

// One request as the client sends it.
type parityRequest struct {
	// "https" for TCP 443 or "http" for TCP 80; empty is "https"
	scheme string
	// the source address, whose family picks the target's address; the zero
	// value is the corpus's ordinary IPv4 client
	clientAddr netip.Addr
	// the TLS server name; empty sends none
	sni string
	// the ALPN offer; nil offers http/1.1 only
	alpn []string
	// the highest TLS version offered; zero offers Go's default, TLS 1.3
	maxTlsVersion uint16
	// records the cipher suite the server chose. Only the tls cases do: Go
	// and OpenSSL choose a different AES-GCM key size on every connection
	// (the tls-suite-choice proposal), which would otherwise repeat in every
	// case
	recordCipher bool
	// sends over h2 when the target negotiates it
	http2  bool
	method string
	// the request target, as the request line or :path carries it
	target string
	// Host, or h2's :authority; empty sends none on h1
	host string
	// more Host lines after the first, on h1
	extraHosts []string
	// the h1 request line's version; empty is HTTP/1.1
	proto string
	// header lines, in order, after Host and the case's own
	headers [][2]string
	body    []byte
	// sends the body chunked, on h1
	chunked bool
	// leaves out the Connection: close every other h1 request ends its head
	// with
	keepalive bool
}

// The scheme, defaulted.
func (self *parityRequest) schemeOrDefault() string {
	if self.scheme == "" {
		return "https"
	}
	return self.scheme
}

// The source address, defaulted.
func (self *parityRequest) clientAddrOrDefault() netip.Addr {
	if !self.clientAddr.IsValid() {
		return parityClientIpv4
	}
	return self.clientAddr
}

// Whether the case's header lines name a header.
func (self *parityRequest) hasHeader(name string) bool {
	return slices.ContainsFunc(self.headers, func(headerLine [2]string) bool {
		return strings.EqualFold(headerLine[0], name)
	})
}

// What a case run saw, as keys and values. Safe for concurrent use.
type parityObservation struct {
	stateLock sync.Mutex
	keys      []string
	keyValues map[string]string
	// "addr:port" of every connection the case opened, as an upstream's
	// X-UR-Forwarded-For must name it
	peerAddrPorts map[string]bool
	// each response's status, by request sequence
	sequenceStatuses map[int]int
	// the block each upstream attempt reached, by its index; never compared,
	// for acceptances that depend on where a request went
	attemptIndexBlocks map[int]string
}

// An empty observation.
func newParityObservation() *parityObservation {
	return &parityObservation{
		keys:               []string{},
		keyValues:          map[string]string{},
		peerAddrPorts:      map[string]bool{},
		sequenceStatuses:   map[int]int{},
		attemptIndexBlocks: map[int]string{},
	}
}

// Sets a key's value, keeping the order keys were first set in.
func (self *parityObservation) set(key string, value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.setWithLock(key, value)
}

// Sets a key's value with stateLock held.
func (self *parityObservation) setWithLock(key string, value string) {
	if _, ok := self.keyValues[key]; !ok {
		self.keys = append(self.keys, key)
	}
	self.keyValues[key] = value
}

// A value for an observation: verbatim when short, else its length, digest
// and head.
func parityValueOfBytes(value []byte) string {
	if len(value) <= parityValueBytes {
		return strconv.Quote(string(value))
	}
	digest := sha256.Sum256(value)
	return fmt.Sprintf("len=%d sha256=%s head=%s", len(value), hex.EncodeToString(digest[:parityValueDigestBytes]), strconv.Quote(string(value[:parityValueHeadBytes])))
}

// A short class for an error, without the addresses net errors quote.
func parityErrorClass(err error) string {
	var opError *net.OpError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EPIPE):
		return "broken pipe"
	case errors.As(err, &opError):
		return opError.Err.Error()
	default:
		return err.Error()
	}
}

// One case's run against one target.
type parityCaseRun struct {
	harness  *parityHarness
	target   *parityTarget
	caseName string
	// "<target>/<case>"
	caseKey     string
	observation *parityObservation

	stateLock    sync.Mutex
	nextSequence int
}

// The next request sequence of the run. Call it from the goroutine that
// orders the requests, before handing a request to another goroutine.
func (self *parityCaseRun) sequence() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sequence := self.nextSequence
	self.nextSequence += 1
	return sequence
}

// Records a fact of the case.
func (self *parityCaseRun) fact(name string, value string) {
	self.observation.set("fact."+name, value)
}

// The name of one of the case's barriers on this target.
func (self *parityCaseRun) barrier(name string) string {
	return self.caseKey + "/" + name
}

// Fires one of the case's barriers.
func (self *parityCaseRun) fire(name string) {
	self.harness.recorder.fire(self.barrier(name))
}

// Waits for one of the case's barriers, and reports whether it fired in
// time.
func (self *parityCaseRun) wait(name string, timeout time.Duration) bool {
	return self.harness.recorder.wait(self.barrier(name), timeout)
}

// Sends one request on a connection of its own, and records the
// connection, the response and its body.
func (self *parityCaseRun) send(request *parityRequest) {
	self.sendSequence(request, self.sequence())
}

// Sends as send does, under the sequence the caller took.
func (self *parityCaseRun) sendSequence(request *parityRequest, sequence int) {
	conn, err := self.dial(request, sequence)
	if err != nil {
		return
	}
	defer conn.close()
	if request.http2 && conn.negotiatedProtocol() == "h2" {
		response, err := conn.roundTripH2(request, sequence)
		self.recordResponse(sequence, response, err)
		return
	}
	conn.writeH1(request, sequence)
	response, err := conn.readH1Response(request.method)
	// an interim 100 Continue, then the response
	for err == nil && response.StatusCode == http.StatusContinue {
		self.fact(fmt.Sprintf("response[%d] interim", sequence), strconv.Itoa(response.StatusCode))
		response, err = conn.readH1Response(request.method)
	}
	self.recordResponse(sequence, response, err)
}

// One connection of a case to its target.
type parityConn struct {
	run           *parityCaseRun
	conn          net.Conn
	reader        *bufio.Reader
	localAddrPort netip.AddrPort
	tlsState      *tls.ConnectionState
	h2ClientConn  *http2.ClientConn
	// the h1 writer's result, once it is done
	writeDone chan error
}

// Opens a connection for request, the TLS handshake included, recording
// the connection's keys under the sequence of its first request.
func (self *parityCaseRun) dial(request *parityRequest, sequence int) (*parityConn, error) {
	settings := self.harness.settings
	prefix := fmt.Sprintf("conn[%d].", sequence)
	clientAddr := request.clientAddrOrDefault()
	dialer := &net.Dialer{
		LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(clientAddr, 0)),
		Timeout:   settings.ExchangeTimeout,
	}
	addrPort := self.target.addrPort(clientAddr, request.schemeOrDefault())
	netConn, err := dialer.Dial("tcp", addrPort.String())
	if err != nil {
		self.observation.set(prefix+"error", parityErrorClass(err))
		return nil, err
	}
	localAddrPort, err := netip.ParseAddrPort(netConn.LocalAddr().String())
	if err != nil {
		netConn.Close()
		self.observation.set(prefix+"error", "local address: "+err.Error())
		return nil, err
	}
	func() {
		self.observation.stateLock.Lock()
		defer self.observation.stateLock.Unlock()
		self.observation.peerAddrPorts[localAddrPort.String()] = true
	}()
	conn := &parityConn{
		run:           self,
		conn:          netConn,
		localAddrPort: localAddrPort,
	}
	if request.schemeOrDefault() == "https" {
		alpn := request.alpn
		if alpn == nil {
			alpn = []string{"http/1.1"}
		}
		tlsConn := tls.Client(netConn, &tls.Config{
			ServerName: request.sni,
			// the harness compares which certificate each side presents;
			// it never relies on verifying one
			InsecureSkipVerify: true,
			NextProtos:         alpn,
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         request.maxTlsVersion,
		})
		tlsConn.SetDeadline(time.Now().Add(settings.ExchangeTimeout))
		if err := tlsConn.Handshake(); err != nil {
			netConn.Close()
			self.observation.set(prefix+"tls.error", parityErrorClass(err))
			return nil, err
		}
		tlsState := tlsConn.ConnectionState()
		conn.tlsState = &tlsState
		conn.conn = tlsConn
		self.observation.set(prefix+"tls.version", tls.VersionName(tlsState.Version))
		self.observation.set(prefix+"tls.alpn", tlsState.NegotiatedProtocol)
		if request.recordCipher {
			self.observation.set(prefix+"tls.cipher", tls.CipherSuiteName(tlsState.CipherSuite))
		}
		certificateName := ""
		if 0 < len(tlsState.PeerCertificates) {
			certificateName = tlsState.PeerCertificates[0].Subject.CommonName
		}
		self.observation.set(prefix+"tls.certificate", certificateName)
	}
	conn.reader = bufio.NewReader(conn.conn)
	conn.conn.SetDeadline(time.Now().Add(settings.ExchangeTimeout))
	return conn, nil
}

// The protocol ALPN chose, "" for plaintext or none.
func (self *parityConn) negotiatedProtocol() string {
	if self.tlsState == nil {
		return ""
	}
	return self.tlsState.NegotiatedProtocol
}

// Closes the connection.
func (self *parityConn) close() {
	if self.h2ClientConn != nil {
		self.h2ClientConn.Close()
	}
	self.conn.Close()
}

// The head of an h1 request, byte for byte: the request line, Host, the
// case's own headers, the request's header lines, Connection: close unless
// the request keeps the connection, and the body's framing.
func (self *parityConn) h1Head(request *parityRequest, sequence int) []byte {
	proto := request.proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	head := &bytes.Buffer{}
	fmt.Fprintf(head, "%s %s %s\r\n", request.method, request.target, proto)
	if request.host != "" {
		fmt.Fprintf(head, "Host: %s\r\n", request.host)
	}
	for _, extraHost := range request.extraHosts {
		fmt.Fprintf(head, "Host: %s\r\n", extraHost)
	}
	fmt.Fprintf(head, "%s: %s\r\n", parityCaseHeader, self.run.caseKey)
	fmt.Fprintf(head, "%s: %d\r\n", paritySequenceHeader, sequence)
	if !request.hasHeader("User-Agent") {
		head.WriteString("User-Agent: parity\r\n")
	}
	for _, headerLine := range request.headers {
		fmt.Fprintf(head, "%s: %s\r\n", headerLine[0], headerLine[1])
	}
	if !request.keepalive && !request.hasHeader("Connection") {
		head.WriteString("Connection: close\r\n")
	}
	if request.body != nil && !request.hasHeader("Content-Length") && !request.hasHeader("Transfer-Encoding") {
		if request.chunked {
			head.WriteString("Transfer-Encoding: chunked\r\n")
		} else {
			fmt.Fprintf(head, "Content-Length: %d\r\n", len(request.body))
		}
	}
	head.WriteString("\r\n")
	return head.Bytes()
}

// The body of an h1 request as written, chunked when the request is.
func parityH1Body(request *parityRequest) []byte {
	if !request.chunked || request.body == nil {
		return request.body
	}
	body := &bytes.Buffer{}
	if 0 < len(request.body) {
		fmt.Fprintf(body, "%x\r\n", len(request.body))
		body.Write(request.body)
		body.WriteString("\r\n")
	}
	body.WriteString("0\r\n\r\n")
	return body.Bytes()
}

// Writes an h1 request in the background, so a response that comes before
// the whole body was written is still read.
func (self *parityConn) writeH1(request *parityRequest, sequence int) {
	message := append(self.h1Head(request, sequence), parityH1Body(request)...)
	self.writeDone = make(chan error, 1)
	go func() {
		_, err := self.conn.Write(message)
		self.writeDone <- err
	}()
}

// Reads one h1 response head; its body is the caller's to read.
func (self *parityConn) readH1Response(method string) (*http.Response, error) {
	self.conn.SetReadDeadline(time.Now().Add(self.run.harness.settings.ExchangeTimeout))
	return http.ReadResponse(self.reader, &http.Request{Method: method})
}

// The connection's h2 client connection, made on first use. Call it once
// before sending streams from several goroutines.
func (self *parityConn) h2() (*http2.ClientConn, error) {
	if self.h2ClientConn == nil {
		transport := &http2.Transport{
			DisableCompression: true,
		}
		h2ClientConn, err := transport.NewClientConn(self.conn)
		if err != nil {
			return nil, err
		}
		self.h2ClientConn = h2ClientConn
	}
	return self.h2ClientConn, nil
}

// Sends one request on the connection's h2 client connection.
func (self *parityConn) roundTripH2(request *parityRequest, sequence int) (*http.Response, error) {
	h2ClientConn, err := self.h2()
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if request.body != nil {
		body = bytes.NewReader(request.body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), self.run.harness.settings.ExchangeTimeout)
	h2Request, err := http.NewRequestWithContext(ctx, request.method, "https://"+request.host+"/", body)
	if err != nil {
		cancel()
		return nil, err
	}
	// the target as given, so a case controls :path
	h2Request.URL = &url.URL{
		Scheme: "https",
		Host:   request.host,
		Opaque: request.target,
	}
	h2Request.Host = request.host
	h2Request.Header.Set(parityCaseHeader, self.run.caseKey)
	h2Request.Header.Set(paritySequenceHeader, strconv.Itoa(sequence))
	if !request.hasHeader("User-Agent") {
		h2Request.Header.Set("User-Agent", "parity")
	}
	for _, headerLine := range request.headers {
		h2Request.Header.Add(headerLine[0], headerLine[1])
	}
	response, err := h2ClientConn.RoundTrip(h2Request)
	if err != nil {
		cancel()
		return nil, err
	}
	// the body is read under the same deadline
	response.Body = &parityCancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// A response body that ends its request's context when closed.
type parityCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

// Closes the body and ends its context.
func (self *parityCancelBody) Close() error {
	err := self.ReadCloser.Close()
	self.cancel()
	return err
}

// Sends one h2 request whose header fields are exactly fields, pseudo
// fields included, with a raw framer, and returns its response. Neither of
// Go's h2 clients sends a Host field beside :authority.
func (self *parityConn) rawH2(fields [][2]string) (*http.Response, error) {
	if _, err := io.WriteString(self.conn, http2.ClientPreface); err != nil {
		return nil, err
	}
	framer := http2.NewFramer(self.conn, self.reader)
	framer.ReadMetaHeaders = hpack.NewDecoder(parityHpackTableBytes, nil)
	if err := framer.WriteSettings(); err != nil {
		return nil, err
	}
	headerBlock := &bytes.Buffer{}
	encoder := hpack.NewEncoder(headerBlock)
	for _, field := range fields {
		encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]})
	}
	err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: headerBlock.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
	if err != nil {
		return nil, err
	}
	var response *http.Response
	body := &bytes.Buffer{}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return response, err
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				framer.WriteSettingsAck()
			}
		case *http2.MetaHeadersFrame:
			if frame.StreamID != 1 {
				continue
			}
			if response == nil {
				response = &http.Response{
					Proto:      "HTTP/2.0",
					ProtoMajor: 2,
					Header:     http.Header{},
				}
			}
			for _, field := range frame.Fields {
				if field.Name == ":status" {
					response.StatusCode, _ = strconv.Atoi(field.Value)
					continue
				}
				response.Header.Add(http.CanonicalHeaderKey(field.Name), field.Value)
			}
			if frame.StreamEnded() {
				response.Body = io.NopCloser(body)
				return response, nil
			}
		case *http2.DataFrame:
			if frame.StreamID != 1 {
				continue
			}
			body.Write(frame.Data())
			if frame.StreamEnded() {
				if response == nil {
					return nil, errors.New("data before headers")
				}
				response.Body = io.NopCloser(body)
				return response, nil
			}
		case *http2.RSTStreamFrame:
			if frame.StreamID == 1 {
				return response, fmt.Errorf("stream reset: %s", frame.ErrCode)
			}
		case *http2.GoAwayFrame:
			if response == nil {
				return nil, fmt.Errorf("goaway: %s", frame.ErrCode)
			}
		}
	}
}

// Records a response and its body under the request's sequence, then closes
// the body.
func (self *parityCaseRun) recordResponse(sequence int, response *http.Response, err error) {
	prefix := fmt.Sprintf("response[%d].", sequence)
	if err != nil {
		self.observation.set(prefix+"error", parityErrorClass(err))
		return
	}
	defer response.Body.Close()
	body, bodyErr := io.ReadAll(response.Body)
	self.recordResponseHead(sequence, response)
	self.observation.set(prefix+"body", parityValueOfBytes(body))
	if bodyErr != nil {
		self.observation.set(prefix+"body.error", parityErrorClass(bodyErr))
	}
	// a HEAD's gzip has no body to decode
	if response.Header.Get("Content-Encoding") == "gzip" && 0 < len(body) {
		decoded, err := parityGunzip(body)
		if err != nil {
			self.observation.set(prefix+"body.decoded", "invalid gzip: "+err.Error())
		} else {
			self.observation.set(prefix+"body.decoded", parityValueOfBytes(decoded))
		}
	}
}

// Records a response's status line, headers and framing under the request's
// sequence; Date is left out, as every response's differs.
func (self *parityCaseRun) recordResponseHead(sequence int, response *http.Response) {
	prefix := fmt.Sprintf("response[%d].", sequence)
	self.observation.set(prefix+"proto", response.Proto)
	self.observation.set(prefix+"status", strconv.Itoa(response.StatusCode))
	func() {
		self.observation.stateLock.Lock()
		defer self.observation.stateLock.Unlock()
		self.observation.sequenceStatuses[sequence] = response.StatusCode
	}()
	names := []string{}
	for name := range response.Header {
		if name != "Date" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		self.observation.set(prefix+"header."+name, strings.Join(response.Header.Values(name), " | "))
	}
	framing := "unknown length"
	switch {
	case 0 < len(response.TransferEncoding):
		framing = strings.Join(response.TransferEncoding, ",")
	case 0 <= response.ContentLength:
		framing = "length " + strconv.FormatInt(response.ContentLength, 10)
	}
	self.observation.set(prefix+"framing", framing)
}

// The decoded bytes of a gzip body.
func parityGunzip(body []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// Adds the upstream attempts of the case to the observation, once every
// request is done: each attempt's request as the upstream received it, the
// X-Parity-Case header without its target and X-UR-Forwarded-For checked
// against the case's own connections, and each response marked as the
// upstream's own or the proxy's.
func (self *parityCaseRun) finish(compareBlocks bool) {
	attempts := self.harness.recorder.attempts(self.caseKey)
	slices.SortStableFunc(attempts, func(a parityAttempt, b parityAttempt) int {
		return a.sequence - b.sequence
	})
	observation := self.observation
	observation.stateLock.Lock()
	defer observation.stateLock.Unlock()
	observation.setWithLock("upstream.count", strconv.Itoa(len(attempts)))
	for i, attempt := range attempts {
		prefix := fmt.Sprintf("upstream[%d].", i)
		observation.setWithLock(prefix+"sequence", strconv.Itoa(attempt.sequence))
		observation.setWithLock(prefix+"service", attempt.service)
		observation.attemptIndexBlocks[i] = attempt.block
		if compareBlocks {
			observation.setWithLock(prefix+"block", attempt.block)
		}
		observation.setWithLock(prefix+"method", attempt.method)
		observation.setWithLock(prefix+"uri", attempt.requestUri)
		observation.setWithLock(prefix+"proto", attempt.proto)
		observation.setWithLock(prefix+"host", attempt.host)
		names := []string{}
		for name := range attempt.header {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			values := slices.Clone(attempt.header.Values(name))
			switch name {
			case parityCaseHeader:
				for j, value := range values {
					if _, caseName, ok := strings.Cut(value, "/"); ok {
						values[j] = caseName
					}
				}
			case "X-Ur-Forwarded-For":
				for j, value := range values {
					if observation.peerAddrPorts[value] {
						values[j] = "<the client's address and port>"
					}
				}
			}
			observation.setWithLock(prefix+"header."+name, strings.Join(values, " | "))
		}
		framing := "length " + strconv.FormatInt(attempt.contentLength, 10)
		if 0 < len(attempt.transferEncoding) {
			framing = strings.Join(attempt.transferEncoding, ",")
		}
		observation.setWithLock(prefix+"framing", framing)
		observation.setWithLock(prefix+"body", parityValueOfBytes(attempt.body))
		if attempt.bodyError != "" {
			observation.setWithLock(prefix+"body.error", attempt.bodyError)
		}
		observation.setWithLock(prefix+"status", strconv.Itoa(attempt.status))
	}
	for _, sequence := range slices.Sorted(maps.Keys(observation.sequenceStatuses)) {
		status := observation.sequenceStatuses[sequence]
		generator := "proxy"
		for _, attempt := range attempts {
			if attempt.sequence == sequence && attempt.status == status {
				generator = "upstream"
			}
		}
		observation.setWithLock(fmt.Sprintf("response[%d].generator", sequence), generator)
	}
}
