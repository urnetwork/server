package server

// certs' CAA resolver and canary client (TLS-M6-04): the walk's lookups on a
// scripted DNS, the recursive resolver and each zone's authoritative servers
// apart, so a provider that flattens an ALIAS target's CAA shows the canary
// only to RD=0 queries at its own servers; once over real UDP on loopback;
// and the canary route's answers, each mapped to what certs' probe reads.
// Names are under .example, addresses RFC 5737's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
)

// The recursive resolver's address in the scripted world.
const testCaaRecursive = "192.0.2.1:53"

// A scripted DNS: every zone's records, which server serves which zone, and
// the names a zone's servers answer CAA for from an ALIAS target instead.
type testCaaWorld struct {
	records []dns.RR
	// server address to the zone it serves
	servers map[string]string
	// a name to the target whose CAA its servers flatten into its answers
	flatten map[string]string
	// addresses that never answer
	silent    map[string]bool
	servfails map[string]bool

	stateLock sync.Mutex
	asked     []string
}

func newTestCaaWorld(t *testing.T, zoneLines ...string) *testCaaWorld {
	t.Helper()
	world := &testCaaWorld{servers: map[string]string{}, flatten: map[string]string{}, silent: map[string]bool{}, servfails: map[string]bool{}}
	for _, line := range zoneLines {
		record, err := dns.NewRR(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		world.records = append(world.records, record)
	}
	return world
}

// The zones the world holds, by their SOA owners.
func (self *testCaaWorld) zones() []string {
	zones := []string{}
	for _, record := range self.records {
		if record.Header().Rrtype == dns.TypeSOA {
			zones = append(zones, dns.CanonicalName(record.Header().Name))
		}
	}
	return zones
}

// The closest zone name is in, "" for none.
func (self *testCaaWorld) zoneOf(name string) string {
	best := ""
	for _, zone := range self.zones() {
		if dns.IsSubDomain(zone, name) && len(best) < len(zone) {
			best = zone
		}
	}
	return best
}

func (self *testCaaWorld) soa(zone string) dns.RR {
	for _, record := range self.records {
		if record.Header().Rrtype == dns.TypeSOA && dns.CanonicalName(record.Header().Name) == zone {
			return record
		}
	}
	return nil
}

// The records at name of qtype, a CNAME standing for any type, within zone
// when one is given.
func (self *testCaaWorld) at(name string, qtype uint16, zone string) (records []dns.RR, exists bool) {
	for _, record := range self.records {
		owner := dns.CanonicalName(record.Header().Name)
		if zone != "" && self.zoneOf(owner) != zone {
			continue
		}
		if dns.IsSubDomain(name, owner) {
			exists = true
		}
		if owner != name {
			continue
		}
		if record.Header().Rrtype == qtype || record.Header().Rrtype == dns.TypeCNAME {
			records = append(records, record)
		}
	}
	return records, exists
}

// An answer to question: recursive, following CNAMEs everywhere, or
// authoritative for zone alone, flattening where the world says.
func (self *testCaaWorld) answer(question *dns.Msg, recursive bool, zone string) *dns.Msg {
	response := &dns.Msg{}
	response.SetReply(question)
	name := dns.CanonicalName(question.Question[0].Name)
	qtype := question.Question[0].Qtype
	if !recursive {
		response.Authoritative = true
		if target, ok := self.flatten[name]; ok && qtype == dns.TypeCAA {
			targetRecords, _ := self.at(target, dns.TypeCAA, "")
			for _, record := range targetRecords {
				copied := dns.Copy(record)
				copied.Header().Name = name
				response.Answer = append(response.Answer, copied)
			}
			return response
		}
	}
	current := name
	for range 10 {
		recordZone := ""
		if !recursive {
			recordZone = zone
		}
		records, exists := self.at(current, qtype, recordZone)
		response.Answer = append(response.Answer, records...)
		if !exists && len(records) == 0 {
			if len(response.Answer) == 0 {
				response.Rcode = dns.RcodeNameError
			}
			if soa := self.soa(self.zoneOf(current)); soa != nil {
				response.Ns = append(response.Ns, soa)
			}
			return response
		}
		next := ""
		for _, record := range records {
			if cname, ok := record.(*dns.CNAME); ok && qtype != dns.TypeCNAME {
				next = dns.CanonicalName(cname.Target)
			}
		}
		if next == "" || !recursive {
			if len(records) == 0 {
				if soa := self.soa(self.zoneOf(current)); soa != nil {
					response.Ns = append(response.Ns, soa)
				}
			}
			return response
		}
		current = next
	}
	response.Rcode = dns.RcodeServerFailure
	return response
}

// The exchange the resolver uses: the recursive address answers
// recursively, a zone server's address for its zone.
func (self *testCaaWorld) exchange(ctx context.Context, message *dns.Msg, server string) (*dns.Msg, error) {
	self.stateLock.Lock()
	self.asked = append(self.asked, server+" "+strconv.FormatBool(message.RecursionDesired)+" "+message.Question[0].Name)
	self.stateLock.Unlock()
	if self.silent[server] {
		return nil, errors.New("timed out")
	}
	if self.servfails[server] {
		response := &dns.Msg{}
		response.SetRcode(message, dns.RcodeServerFailure)
		return response, nil
	}
	if server == testCaaRecursive {
		if !message.RecursionDesired {
			return nil, errors.New("the recursive resolver asked without recursion")
		}
		return self.answer(message, true, ""), nil
	}
	zone, ok := self.servers[server]
	if !ok {
		return nil, fmt.Errorf("no server at %s", server)
	}
	if message.RecursionDesired {
		return nil, errors.New("an authoritative server asked for recursion")
	}
	return self.answer(message, false, zone), nil
}

// The customer's zone on its provider's two servers with the pin at the
// domain, a CDN's zone, and <root>, whose host owner carries certs' canary.
func newTestCaaCustomerWorld(t *testing.T) *testCaaWorld {
	world := newTestCaaWorld(
		t,
		`customer.example. 3600 IN SOA ns1.provider.example. hostmaster.customer.example. 1 3600 600 86400 60`,
		`customer.example. 3600 IN NS ns1.provider.example.`,
		`customer.example. 3600 IN NS ns2.provider.example.`,
		`customer.example. 3600 IN CAA 0 issue "ca.example; accounturi=https://ca.example/acme/acct/1"`,
		`app.customer.example. 60 IN A 192.0.2.10`,
		`www.customer.example. 60 IN CNAME edge.cdn.example.`,
		`loop.customer.example. 60 IN CNAME loop2.customer.example.`,
		`loop2.customer.example. 60 IN CNAME loop.customer.example.`,
		`provider.example. 3600 IN SOA ns1.provider.example. hostmaster.provider.example. 1 3600 600 86400 60`,
		`provider.example. 3600 IN NS ns1.provider.example.`,
		`ns1.provider.example. 3600 IN A 192.0.2.53`,
		`ns2.provider.example. 3600 IN A 192.0.2.54`,
		`cdn.example. 3600 IN SOA ns1.provider.example. hostmaster.cdn.example. 1 3600 600 86400 60`,
		`cdn.example. 3600 IN NS ns1.provider.example.`,
		`edge.cdn.example. 60 IN CAA 0 issue "other-ca.example"`,
		`root.example. 3600 IN SOA ns1.root.example. hostmaster.root.example. 1 3600 600 86400 60`,
		`root.example. 3600 IN NS ns1.root.example.`,
		`ns1.root.example. 3600 IN A 198.51.100.53`,
		`web.net1.root.example. 60 IN CAA 0 issue "ca.example; accounturi=https://ca.example/acme/acct/1"`,
		`web.net1.root.example. 60 IN CAA 0 urcanary "00112233445566778899aabbccddeeff"`,
	)
	world.servers["192.0.2.53:53"] = "customer.example."
	world.servers["192.0.2.54:53"] = "customer.example."
	world.servers["198.51.100.53:53"] = "root.example."
	return world
}

func newTestCaaResolver(t *testing.T, world *testCaaWorld) *CertsCaaResolver {
	t.Helper()
	settings := DefaultCertsCaaResolverSettings()
	settings.Resolvers = []string{testCaaRecursive}
	resolver, err := NewCertsCaaResolver(settings, world.exchange)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestCertsCaaResolverAnswersAHostWithItsZone(t *testing.T) {
	resolver := newTestCaaResolver(t, newTestCaaCustomerWorld(t))
	answer, err := resolver.LookupCaa(context.Background(), "app.customer.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Hops) != 1 || len(answer.Records) != 0 {
		t.Fatalf("%+v", answer)
	}
	hop := answer.Hops[0]
	if hop.Name != "app.customer.example" || hop.Zone != "customer.example" || !slices.Equal(hop.Nameservers, []string{"ns1.provider.example", "ns2.provider.example"}) {
		t.Errorf("hop %+v", hop)
	}
	pinned, err := resolver.LookupCaa(context.Background(), "customer.example")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pinned.Records, []string{`0 issue "ca.example; accounturi=https://ca.example/acme/acct/1"`}) || pinned.Hops[0].Zone != "customer.example" {
		t.Errorf("the pin %+v", pinned)
	}
}

func TestCertsCaaResolverFollowsAnAliasToItsTargetsZone(t *testing.T) {
	resolver := newTestCaaResolver(t, newTestCaaCustomerWorld(t))
	answer, err := resolver.LookupCaa(context.Background(), "www.customer.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Hops) != 2 || answer.Hops[1].Name != "edge.cdn.example" || answer.Hops[1].Zone != "cdn.example" {
		t.Fatalf("hops %+v %+v", answer.Hops[0], answer.Hops[len(answer.Hops)-1])
	}
	if !slices.Equal(answer.Records, []string{`0 issue "other-ca.example"`}) {
		t.Errorf("records %v", answer.Records)
	}
	if _, err := resolver.LookupCaa(context.Background(), "loop.customer.example"); err == nil {
		t.Errorf("an alias loop answered")
	}
}

func TestCertsCaaResolverWalksToThePin(t *testing.T) {
	resolver := newTestCaaResolver(t, newTestCaaCustomerWorld(t))
	result, err := sidecar.TlsCaaWalk(context.Background(), resolver, "app.customer.example", []string{"root.example"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveOwner != "customer.example" || !result.AuthorizesAccount("ca.example", "https://ca.example/acme/acct/1") || result.AuthorizesAccount("ca.example", "https://ca.example/acme/acct/2") {
		t.Errorf("%+v", result)
	}
}

func TestCertsCaaResolverFailsOnAFailedLookup(t *testing.T) {
	world := newTestCaaCustomerWorld(t)
	world.servfails[testCaaRecursive] = true
	if _, err := newTestCaaResolver(t, world).LookupCaa(context.Background(), "app.customer.example"); err == nil {
		t.Fatal("a SERVFAIL read as an answer")
	}
	silent := newTestCaaCustomerWorld(t)
	silent.silent[testCaaRecursive] = true
	if _, err := newTestCaaResolver(t, silent).LookupCaa(context.Background(), "app.customer.example"); err == nil {
		t.Fatal("no answer read as an answer")
	}
}

// The canary at the ALIAS target shows only where the provider flattens it,
// and only at the authoritative servers, each heard apart.
func TestCertsCaaResolverAsksEachAuthoritativeServerWithoutRecursion(t *testing.T) {
	world := newTestCaaCustomerWorld(t)
	resolver := newTestCaaResolver(t, world)
	nameservers := []string{"ns1.provider.example", "ns2.provider.example"}
	plain, err := resolver.AuthoritativeCaa(context.Background(), "app.customer.example", nameservers)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 || len(plain["ns1.provider.example"]) != 0 || len(plain["ns2.provider.example"]) != 0 {
		t.Fatalf("an address-only host answered %v", plain)
	}
	world.flatten["app.customer.example."] = "web.net1.root.example."
	flattened, err := resolver.AuthoritativeCaa(context.Background(), "app.customer.example", nameservers)
	if err != nil {
		t.Fatal(err)
	}
	for _, nameserver := range nameservers {
		if !slices.Contains(flattened[nameserver], `0 urcanary "00112233445566778899aabbccddeeff"`) {
			t.Errorf("%s answered %v", nameserver, flattened[nameserver])
		}
	}
	// one server down: the other is heard; both down: no answer at all
	world.silent["192.0.2.54:53"] = true
	partly, err := resolver.AuthoritativeCaa(context.Background(), "app.customer.example", nameservers)
	if err != nil || len(partly) != 1 || partly["ns1.provider.example"] == nil {
		t.Fatalf("%v, %v", partly, err)
	}
	world.silent["192.0.2.53:53"] = true
	if _, err := resolver.AuthoritativeCaa(context.Background(), "app.customer.example", nameservers); err == nil {
		t.Fatal("no server answered and no error")
	}
	for _, asked := range world.asked {
		if !strings.HasPrefix(asked, testCaaRecursive) && strings.Contains(asked, " true ") {
			t.Errorf("an authoritative query asked for recursion: %s", asked)
		}
	}
}

func TestCertsCaaResolverRefusesSettingsItCannotRunOn(t *testing.T) {
	settings := DefaultCertsCaaResolverSettings()
	settings.Resolvers = []string{testCaaRecursive}
	settings.QueryTimeout = 0
	if _, err := NewCertsCaaResolver(settings, nil); !errors.Is(err, ErrCertsCaaResolverRefused) {
		t.Fatal(err)
	}
}

// The default exchange over UDP on loopback: one server answers as the
// recursive resolver for RD=1 and as every zone's for RD=0.
func TestCertsCaaResolverOverUdp(t *testing.T) {
	world := newTestCaaWorld(
		t,
		`customer.example. 3600 IN SOA ns1.provider.example. hostmaster.customer.example. 1 3600 600 86400 60`,
		`customer.example. 3600 IN NS ns1.provider.example.`,
		`customer.example. 3600 IN CAA 0 issue "ca.example; accounturi=https://ca.example/acme/acct/1"`,
		`provider.example. 3600 IN SOA ns1.provider.example. hostmaster.provider.example. 1 3600 600 86400 60`,
		`provider.example. 3600 IN NS ns1.provider.example.`,
		`ns1.provider.example. 3600 IN A 127.0.0.1`,
		`root.example. 3600 IN SOA ns1.provider.example. hostmaster.root.example. 1 3600 600 86400 60`,
		`web.net1.root.example. 60 IN CAA 0 urcanary "00112233445566778899aabbccddeeff"`,
	)
	world.flatten["app.customer.example."] = "web.net1.root.example."
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := &dns.Server{
		PacketConn:        packetConn,
		NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, question *dns.Msg) {
			zone := world.zoneOf(dns.CanonicalName(question.Question[0].Name))
			writer.WriteMsg(world.answer(question, question.RecursionDesired, zone))
		}),
	}
	go server.ActivateAndServe()
	<-started
	t.Cleanup(func() {
		server.Shutdown()
	})
	_, port, _ := net.SplitHostPort(packetConn.LocalAddr().String())
	settings := DefaultCertsCaaResolverSettings()
	settings.Resolvers = []string{packetConn.LocalAddr().String()}
	settings.AuthoritativePort, _ = strconv.Atoi(port)
	resolver, err := NewCertsCaaResolver(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := sidecar.TlsCaaWalk(context.Background(), resolver, "app.customer.example", []string{"root.example"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveOwner != "customer.example" {
		t.Fatalf("%+v", result)
	}
	answers, err := resolver.AuthoritativeCaa(context.Background(), "app.customer.example", []string{"ns1.provider.example"})
	if err != nil || !slices.Contains(answers["ns1.provider.example"], `0 urcanary "00112233445566778899aabbccddeeff"`) {
		t.Fatalf("%v, %v", answers, err)
	}
}

// The canary tag certs looks for is the one dns publishes.
func TestCertsCaaCanaryTagIsDnsTag(t *testing.T) {
	if sidecar.TlsCaaCanaryTag != dnsservice.DnsCaaCanaryTag {
		t.Fatalf("%q, %q", sidecar.TlsCaaCanaryTag, dnsservice.DnsCaaCanaryTag)
	}
}

// POST /dns/internal/caa-canary as certs' client sends it, and each answer
// as the probe reads it.
func TestCertsDnsClientCaaCanaryAnswers(t *testing.T) {
	type exchange struct {
		status int
		body   string
	}
	var stateLock sync.Mutex
	next := exchange{}
	requests := []*dnsservice.DnsCaaCanaryRequest{}
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != dnsservice.DnsCaaCanaryPath || request.Header.Get("Authorization") != "Bearer test-bearer" {
			http.Error(writer, "wrong", http.StatusTeapot)
			return
		}
		canary := &dnsservice.DnsCaaCanaryRequest{}
		if err := json.NewDecoder(request.Body).Decode(canary); err != nil {
			http.Error(writer, "bad", http.StatusTeapot)
			return
		}
		stateLock.Lock()
		requests = append(requests, canary)
		answer := next
		stateLock.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(answer.status)
		writer.Write([]byte(answer.body))
	}))
	t.Cleanup(httpServer.Close)
	settings := DefaultCertsDnsClientSettings()
	settings.BaseUrl = httpServer.URL
	settings.Root = "root.example"
	client, err := NewCertsDnsClient(settings, "test-bearer", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	script := func(status int, body string) {
		stateLock.Lock()
		defer stateLock.Unlock()
		next = exchange{status: status, body: body}
	}

	script(http.StatusOK, `{"serial":7,"nonce":"00112233445566778899aabbccddeeff"}`)
	serial, nonce, err := client.PublishCaaCanary(ctx, "web.net1.root.example", []string{`0 issue "ca.example"`}, "ffeeddccbbaa99887766554433221100")
	if err != nil || serial != 7 || nonce != "00112233445566778899aabbccddeeff" {
		t.Fatalf("%d, %q, %v", serial, nonce, err)
	}
	sent := requests[len(requests)-1]
	if sent.Owner != "web.net1.root.example" || !slices.Equal(sent.Caa, []string{`0 issue "ca.example"`}) || sent.Replaces != "ffeeddccbbaa99887766554433221100" || sent.Root != "root.example" || sent.Delete {
		t.Errorf("sent %+v", sent)
	}
	for _, c := range []struct {
		status int
		body   string
		want   error
	}{
		{status: http.StatusConflict, body: `{"code":"busy"}`, want: certsservice.ErrCertsCaaCanaryBusy},
		{status: http.StatusConflict, body: `{"code":"lint_refused"}`, want: certsservice.ErrCertsCaaCanaryLint},
		{status: http.StatusBadRequest, body: `canary refused`, want: certsservice.ErrCertsCaaCanaryRefused},
		{status: http.StatusServiceUnavailable, body: ``, want: certsservice.ErrCertsDnsRestoring},
		{status: http.StatusForbidden, body: ``, want: ErrCertsDnsCredentialRefused},
	} {
		script(c.status, c.body)
		if _, _, err := client.PublishCaaCanary(ctx, "web.net1.root.example", []string{`0 issue "ca.example"`}, ""); !errors.Is(err, c.want) {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
	}
	// a 409 this client does not know is no verdict
	script(http.StatusConflict, `{"code":"something_new"}`)
	_, _, err = client.PublishCaaCanary(ctx, "web.net1.root.example", []string{`0 issue "ca.example"`}, "")
	if err == nil || errors.Is(err, certsservice.ErrCertsCaaCanaryLint) || errors.Is(err, certsservice.ErrCertsCaaCanaryBusy) {
		t.Errorf("an unknown 409: %v", err)
	}
	// a publish with no set is refused before it is sent
	before := len(requests)
	if _, _, err := client.PublishCaaCanary(ctx, "web.net1.root.example", nil, ""); !errors.Is(err, certsservice.ErrCertsCaaCanaryRefused) || len(requests) != before {
		t.Errorf("an empty set: %v", err)
	}

	script(http.StatusOK, `{"serial":9}`)
	if serial, err := client.DeleteCaaCanary(ctx, "web.net1.root.example"); err != nil || serial != 9 {
		t.Fatalf("delete: %d, %v", serial, err)
	}
	if sent := requests[len(requests)-1]; !sent.Delete || len(sent.Caa) != 0 || sent.Replaces != "" {
		t.Errorf("the delete sent %+v", sent)
	}
}
