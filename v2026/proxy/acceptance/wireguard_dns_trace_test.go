package acceptance

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type dnsTraceConn struct {
	wireGuardDiagnosticErrorPacketConn
	port int
}

func (c *dnsTraceConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(10, 42, 13, 7), Port: c.port}
}
func (*dnsTraceConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 91), Port: 53}
}

func dnsTraceStack() *wireGuardStack {
	return &wireGuardStack{clientIPv4: netip.MustParseAddr("10.42.13.7")}
}

func dnsTraceIPv4(protocol byte, source, target [4]byte, payload []byte) []byte {
	p := make([]byte, 20+len(payload))
	p[0], p[9] = 0x45, protocol
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:16], source[:])
	copy(p[16:20], target[:])
	copy(p[20:], payload)
	return p
}

func dnsTraceUDP(port int, inbound bool) []byte {
	local, remote := [4]byte{10, 42, 13, 7}, [4]byte{192, 0, 2, 91}
	payload := append(make([]byte, 8), []byte("SECRET-DNS-NAME-and-transaction")...)
	source, target := uint16(port), uint16(53)
	if inbound {
		local, remote = remote, local
		source, target = target, source
	}
	binary.BigEndian.PutUint16(payload[:2], source)
	binary.BigEndian.PutUint16(payload[2:4], target)
	binary.BigEndian.PutUint16(payload[4:6], uint16(len(payload)))
	return dnsTraceIPv4(17, local, remote, payload)
}

func dnsTraceICMP(port int, kind, code byte) []byte {
	quote := dnsTraceUDP(port, false)
	payload := append(make([]byte, 8), quote[:28]...)
	payload[0], payload[1] = kind, code
	return dnsTraceIPv4(1, [4]byte{203, 0, 113, 19}, [4]byte{10, 42, 13, 7}, payload)
}

func dnsTraceContains(t *testing.T, actual string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(actual, value) {
			t.Errorf("missing %q in %s", value, actual)
		}
	}
}

func TestWireGuardDNSMetadataSeparatesPacketSocketAndLookup(t *testing.T) {
	s := dnsTraceStack()
	r := s.startDNSRequest()
	x := r.exchange(&dnsTraceConn{port: 54321}, false)
	r.lookupEvent(true, nil)
	packet := dnsTraceICMP(54321, 3, 3)
	before := bytes.Clone(packet)
	s.observePacket(packet, false)
	x.ioEvent(0, errors.New("PRIVATE-ERROR"), false)
	r.lookupEvent(false, context.DeadlineExceeded)
	text := r.freeze()
	dnsTraceContains(t, text, "proto=1 bytes=56 icmp=3/3", "quote_shape=udp_header active_dns_tuple=matched", "reads=1 with_data=0 bytes=0 read_errors=1", "started=1 success=0 not_found=0 deadline=1", "packet_scope=request_window_not_ownership", "checksums=unchecked generation=unproven provider=unavailable")
	if !bytes.Equal(packet, before) {
		t.Fatal("observer mutated packet")
	}
	for _, secret := range []string{"10.42.13.7", "192.0.2.91", "203.0.113.19", "54321", "SECRET-DNS", "PRIVATE-ERROR"} {
		if strings.Contains(text, secret) {
			t.Fatalf("metadata contains forbidden value %q", secret)
		}
	}
}

func TestWireGuardDNSMetadataProtocolAndQuoteShapes(t *testing.T) {
	for _, test := range []struct {
		name            string
		packet          []byte
		mutate          func([]byte) []byte
		protocol, quote uint8
		match           bool
	}{
		{name: "udp", packet: dnsTraceUDP(54321, true), protocol: 17, match: true},
		{name: "icmp-refused", packet: dnsTraceICMP(54321, 3, 3), protocol: 1, quote: 1, match: true},
		{name: "icmp-admin", packet: dnsTraceICMP(54321, 3, 13), protocol: 1, quote: 1, match: true},
		{name: "icmp-ttl", packet: dnsTraceICMP(54321, 11, 0), protocol: 1, quote: 1, match: true},
		{name: "icmp-other", packet: dnsTraceICMP(54321, 8, 0), protocol: 1},
		{name: "wrong-port", packet: dnsTraceICMP(54322, 3, 3), protocol: 1, quote: 1},
		{name: "foreign-envelope", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { p[19]++; return p }, protocol: 1, quote: 1},
		{name: "short-outer", packet: []byte{0x45}, quote: 3},
		{name: "short-icmp", packet: dnsTraceIPv4(1, [4]byte{}, [4]byte{}, make([]byte, 4)), protocol: 1, quote: 2},
		{name: "short-quote", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { p = p[:55]; binary.BigEndian.PutUint16(p[2:4], 55); return p }, protocol: 1, quote: 2},
		{name: "malformed-quote", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { p[28] = 0x41; return p }, protocol: 1, quote: 3},
		{name: "short-declared-quote", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { binary.BigEndian.PutUint16(p[30:32], 20); return p }, protocol: 1, quote: 2},
		{name: "malformed-udp", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { binary.BigEndian.PutUint16(p[52:54], 7); return p }, protocol: 1, quote: 3},
		{name: "fragmented-quote", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { p[34] = 0x20; return p }, protocol: 1, quote: 4},
		{name: "fragmented-outer", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { p[6] = 0x20; return p }, protocol: 1, quote: 4},
		{name: "other-quoted-protocol", packet: dnsTraceICMP(54321, 3, 3), mutate: func(p []byte) []byte { p[37] = 6; return p }, protocol: 1, quote: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := dnsTraceStack()
			r := s.startDNSRequest()
			r.exchange(&dnsTraceConn{port: 54321}, false)
			if test.mutate != nil {
				test.packet = test.mutate(test.packet)
			}
			s.observePacket(test.packet, false)
			p := r.packets[0]
			if r.packetN != 1 || p.protocol != test.protocol || p.quote != test.quote || (p.match == 1) != test.match {
				t.Fatalf("unexpected structural observation: %+v", p)
			}
			r.freeze()
		})
	}
}

func TestWireGuardDNSMetadataRequestIsolationExpiryAndAmbiguity(t *testing.T) {
	for _, same := range []bool{false, true} {
		s := dnsTraceStack()
		first, second := s.startDNSRequest(), s.startDNSRequest()
		x := first.exchange(&dnsTraceConn{port: 54321}, false)
		port := 54322
		if same {
			port = 54321
		}
		second.exchange(&dnsTraceConn{port: port}, false)
		x.ioEvent(11, nil, false)
		first.lookupEvent(true, nil)
		first.lookupEvent(false, nil)
		s.observePacket(dnsTraceICMP(54321, 3, 3), false)
		wantFirst, wantSecond := uint8(1), uint8(0)
		if same {
			wantFirst, wantSecond = 2, 2
		}
		if first.packets[0].match != wantFirst || second.packets[0].match != wantSecond {
			t.Fatal("overlapping request tuple attribution is wrong")
		}
		if second.io != [7]uint16{} || second.lookup != [6]uint16{} {
			t.Fatal("borrowed another request's delivery or lookup outcome")
		}
		x.close()
		s.observePacket(dnsTraceICMP(54321, 3, 3), false)
		if first.packets[1].match != 0 {
			t.Fatal("closed socket retained active tuple")
		}
		if same && second.packets[1].match != 1 {
			t.Fatal("remaining active tuple not recognized")
		}
		frozen := first.freeze()
		first.lookupEvent(false, context.Canceled)
		x.ioEvent(99, nil, false)
		s.observePacket(dnsTraceUDP(54321, true), false)
		if first.freeze() != frozen || first.packetN != 2 || first.exchange(&dnsTraceConn{port: 54323}, false) != nil {
			t.Fatal("late activity rewrote frozen request")
		}
		second.freeze()
		for _, r := range s.dnsRequests {
			if r != nil {
				t.Fatal("frozen request still registered")
			}
		}
	}
}

func TestWireGuardDNSMetadataCapsAndUnknownLookup(t *testing.T) {
	s := dnsTraceStack()
	requests := make([]*wireGuardDNSRequest, wireGuardDNSCap+1)
	for i := range requests {
		requests[i] = s.startDNSRequest()
	}
	r := requests[0]
	for i := 0; i < wireGuardDNSCap+1; i++ {
		r.exchange(&dnsTraceConn{port: 54321 + i}, false)
	}
	x := &wireGuardDNSExchange{r, 0}
	x.ioEvent(int(^uint(0)>>1), nil, false)
	for i := 0; i < wireGuardDNSCap+1; i++ {
		s.observePacket(dnsTraceICMP(54321, 3, 3), false)
	}
	r.lookupEvent(true, nil)
	text := r.freeze()
	if len(text) > 2048 {
		t.Fatalf("metadata exceeded text cap: %d", len(text))
	}
	dnsTraceContains(t, text, "exchanges=8", "bytes=65535", "limited=true", "started=1 success=0 not_found=0 deadline=0 canceled=0 error=0")
	if r.packetN != wireGuardDNSCap {
		t.Fatal("packet cap not applied")
	}
	for _, r := range requests[1:] {
		r.freeze()
	}
	if !requests[wireGuardDNSCap].limited || requests[wireGuardDNSCap].packetN != 0 {
		t.Fatal("concurrent request overflow not explicit")
	}
	fresh := s.startDNSRequest()
	if fresh.limited {
		t.Fatal("frozen slots not reusable")
	}
	fresh.freeze()
}

type dnsTracePrivateError struct{ value []byte }

func (dnsTracePrivateError) Error() string { panic("diagnostic must never stringify error") }

func TestWireGuardDNSMetadataFiniteLookupOutcomes(t *testing.T) {
	for _, test := range []struct {
		err   error
		index int
	}{
		{nil, 1}, {context.DeadlineExceeded, 3}, {context.Canceled, 4},
		{&net.DNSError{IsNotFound: true, Err: "PRIVATE", Name: "PRIVATE", Server: "PRIVATE"}, 2},
		{&net.DNSError{IsTimeout: true}, 3}, {&net.DNSError{UnwrapErr: context.Canceled}, 4},
		{&net.DNSError{UnwrapErr: fmt.Errorf("PRIVATE: %w", context.Canceled)}, 4},
		{&net.DNSError{UnwrapErr: fmt.Errorf("PRIVATE: %w", context.DeadlineExceeded)}, 3},
		{fmt.Errorf("PRIVATE: %w", context.DeadlineExceeded), 3},
		{dnsTracePrivateError{[]byte("PRIVATE")}, 5},
	} {
		r := dnsTraceStack().startDNSRequest()
		r.lookupEvent(true, nil)
		r.lookupEvent(false, test.err)
		if r.lookup[0] != 1 || r.lookup[test.index] != 1 {
			t.Fatal("wrong finite lookup class")
		}
		if strings.Contains(r.freeze(), "PRIVATE") {
			t.Fatal("leaked lookup detail")
		}
	}
}

func TestWireGuardDNSMetadataConcurrentFreeze(t *testing.T) {
	s := dnsTraceStack()
	r := s.startDNSRequest()
	x := r.exchange(&dnsTraceConn{port: 54321}, false)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	texts := make(chan string, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			for j := 0; j < 64; j++ {
				switch i {
				case 0:
					s.observePacket(dnsTraceICMP(54321, 3, 3), false)
				case 1:
					x.ioEvent(1, nil, false)
				case 2:
					r.lookupEvent(true, nil)
					r.lookupEvent(false, nil)
				case 3:
					x.close()
					r.exchange(&dnsTraceConn{port: 54321 + j}, false)
				}
			}
			texts <- r.freeze()
		}(i)
	}
	close(gate)
	wg.Wait()
	close(texts)
	want := r.freeze()
	for text := range texts {
		if text != want {
			t.Fatal("concurrent snapshots differ")
		}
	}
}

func TestWireGuardDNSMetadataPreservesWrapperIO(t *testing.T) {
	s := dnsTraceStack()
	r := s.startDNSRequest()
	original := errors.New("synthetic I/O error")
	raw := &dnsTraceConn{port: 54321}
	raw.err = original
	base := &wireGuardDNSConn{Conn: raw, ctx: context.Background(), stopCancel: func() bool { return true }, dns: r.exchange(raw, false)}
	packet := &wireGuardDNSPacketConn{wireGuardDNSConn: base}
	for _, operation := range []string{"read", "write", "readfrom", "writeto"} {
		var n int
		var err error
		switch operation {
		case "read":
			n, err = packet.Read(make([]byte, 2))
		case "write":
			n, err = packet.Write(make([]byte, 2))
		case "readfrom":
			n, _, err = packet.ReadFrom(make([]byte, 2))
		case "writeto":
			n, err = packet.WriteTo(make([]byte, 2), raw.RemoteAddr())
		}
		if n != 1 || err != original {
			t.Fatalf("%s altered underlying result", operation)
		}
	}
	if _, ok := any(base).(net.PacketConn); ok {
		t.Fatal("TCP wrapper acquired UDP framing")
	}
	if r.io != [7]uint16{2, 2, 2, 2, 2, 2, 2} {
		t.Fatalf("I/O totals=%v", r.io)
	}
	packet.Close()
	r.freeze()
}

func TestWireGuardDNSMetadataCanceledConnectionExpiresTuple(t *testing.T) {
	transport, _, _, _ := acceptanceDNSWireGuardPair(t, false, false)
	r := transport.stack.startDNSRequest()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), wireGuardDNSContextKey{}, r))
	defer cancel()
	connection, err := transport.stack.dialDNSContext(ctx, "udp", "ignored.invalid:53")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := connection.Read(make([]byte, 512)); done <- err }()
	cancel()
	select {
	case err = <-done:
	case <-time.After(time.Second):
		connection.Close()
		<-done
		t.Fatal("canceled reader did not join")
	}
	connection.Close()
	r.mu.Lock()
	active := r.flows[0].active
	r.mu.Unlock()
	if !errors.Is(err, context.Canceled) || active {
		t.Fatal("canceled DNS connection retained active tuple or changed error")
	}
	dnsTraceContains(t, r.freeze(), "reads=1 with_data=0 bytes=0 read_errors=1")
}

func TestWireGuardDNSMetadataRealResolverBoundaries(t *testing.T) {
	for _, test := range []struct {
		name               string
		truncated, missing bool
	}{
		{"answer", false, false}, {"nxdomain", false, true}, {"tcp-fallback", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport, udp, tcp, origin := acceptanceDNSWireGuardPair(t, test.truncated, test.missing)
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://acceptance-dns.invalid/", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
			defer cancel()
			response, err := transport.RoundTrip(request.WithContext(ctx))
			var text string
			if test.missing {
				var dnsErr *net.DNSError
				if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
					t.Fatalf("original NXDOMAIN changed: %v", err)
				}
				text = err.Error()
				dnsTraceContains(t, text, "success=0 not_found=1")
				if origin.Load() != 0 {
					t.Fatal("resolver rejection still reached origin")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, ok := response.Body.(*wireGuardDiagnosticBody)
				if !ok {
					t.Fatal("missing diagnostic body")
				}
				text = body.dnsTrace
				response.Body.Close()
				dnsTraceContains(t, text, "success=1 not_found=0")
				if origin.Load() != 1 {
					t.Fatal("changed origin request count")
				}
			}
			dnsTraceContains(t, text, "packet_observed_before_injection=[{proto=17", "quote_shape=none active_dns_tuple=matched", "socket_delivered_candidate{reads=", "provider=unavailable")
			if strings.Contains(text, "with_data=0") {
				t.Fatal("successful DNS exchange had no delivered reads")
			}
			if udp.Load() == 0 || (test.truncated && tcp.Load() == 0) {
				t.Fatal("expected real tunneled DNS path missing")
			}
		})
	}
}

func TestWireGuardDNSMetadataRoundTripErrorAndBodyFreeze(t *testing.T) {
	for _, bodyFailure := range []bool{false, true} {
		s := dnsTraceStack()
		original := errors.New("original boundary")
		var trace *wireGuardDNSRequest
		transport := &wireGuardDiagnosticTransport{stack: s, roundTripper: acceptanceRoundTripper(func(req *http.Request) (*http.Response, error) {
			trace, _ = req.Context().Value(wireGuardDNSContextKey{}).(*wireGuardDNSRequest)
			trace.lookupEvent(true, nil)
			trace.lookupEvent(false, context.DeadlineExceeded)
			if bodyFailure {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(&wireGuardDiagnosticErrorConn{err: original})}, nil
			}
			return nil, original
		})}
		request, _ := http.NewRequest(http.MethodGet, "http://synthetic.invalid/", nil)
		response, err := transport.RoundTrip(request)
		frozen := trace.freeze()
		trace.lookupEvent(false, nil)
		if bodyFailure {
			_, err = response.Body.Read(make([]byte, 1))
			response.Body.Close()
		}
		if !errors.Is(err, original) {
			t.Fatal("original error chain changed")
		}
		dnsTraceContains(t, err.Error(), frozen, "success=0 not_found=0 deadline=1")
	}
}
