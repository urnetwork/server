package dnshost

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

const hostTestRoot = "zone.example."

// A KSK and a ZSK, the window the KSK signed over both, as DNS-M5-19's ceremony would make it.
type hostTestKeys struct {
	ksk       *ecdsa.PrivateKey
	zsk       *ecdsa.PrivateKey
	kskRecord *dns.DNSKEY
	zskRecord *dns.DNSKEY
	window    *dnsservice.DnsKeyWindow
}

func hostTestRdata(record dns.RR) string {
	return strings.TrimSpace(strings.TrimPrefix(record.String(), record.Header().String()))
}

func newHostTestKeys(t testing.TB, start time.Time) *hostTestKeys {
	t.Helper()
	key := func(flags uint16) (*ecdsa.PrivateKey, *dns.DNSKEY) {
		record := &dns.DNSKEY{
			Hdr:       dns.RR_Header{Name: hostTestRoot, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600},
			Flags:     flags,
			Protocol:  3,
			Algorithm: dns.ECDSAP256SHA256,
		}
		private, err := record.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		return private.(*ecdsa.PrivateKey), record
	}
	self := &hostTestKeys{}
	self.ksk, self.kskRecord = key(dns.ZONE | dns.SEP)
	self.zsk, self.zskRecord = key(dns.ZONE)
	notBefore := start.Add(-24 * time.Hour)
	notAfter := start.Add(13 * 24 * time.Hour)
	rrsig := &dns.RRSIG{
		Hdr:        dns.RR_Header{Name: hostTestRoot, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 3600},
		Algorithm:  dns.ECDSAP256SHA256,
		KeyTag:     self.kskRecord.KeyTag(),
		SignerName: hostTestRoot,
		Inception:  uint32(notBefore.Unix()),
		Expiration: uint32(notAfter.Unix()),
	}
	if err := rrsig.Sign(self.ksk, []dns.RR{self.kskRecord, self.zskRecord}); err != nil {
		t.Fatal(err)
	}
	self.window = &dnsservice.DnsKeyWindow{
		Root:      hostTestRoot,
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Dnskeys:   []string{hostTestRdata(self.kskRecord), hostTestRdata(self.zskRecord)},
		Rrsigs:    []string{hostTestRdata(rrsig)},
	}
	return self
}

// dns.yml for the test root, with its anchors.
func hostTestSettings(t testing.TB, keys *hostTestKeys) *dnsservice.DnsSettings {
	t.Helper()
	settings := dnsservice.DefaultDnsSettings()
	settings.Roots = []string{hostTestRoot}
	settings.SignaturesPerSecond = 1000
	settings.Tcp.ConnectionsPerSource = 2
	settings.Rrl.ResponsesPerSecond = 1000
	settings.Edge.UdpReaderCount = 1
	if keys != nil {
		settings.TrustAnchors = map[string][]string{hostTestRoot: {hostTestRdata(keys.kskRecord)}}
	}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	return settings
}

// A secret directory holding the files given.
func hostTestSecrets(t testing.TB, files map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// Refuses every probe: no interface answers.
type hostTestDialer struct{}

func (hostTestDialer) DialProbe(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("no lb here")
}

// Takes every report.
type hostTestRouteProbes struct{}

func (hostTestRouteProbes) RouteProbes(ctx context.Context, report *dnsservice.DnsRouteProbeReport) error {
	return nil
}

// A ZSK file with no key, a missing ZSK, and a root with no anchors each leave the edge role not ready
// and serving nothing, so its block reverts.
func TestEdgeHostNotReadyWithoutItsKeys(t *testing.T) {
	keys := newHostTestKeys(t, sidecartest.DefaultClockStart())
	zskFile := keys.zskRecord.PrivateKeyString(keys.zsk)
	cases := []struct {
		name     string
		files    map[string]string
		settings *dnsservice.DnsSettings
	}{
		{name: "a bad zsk", files: map[string]string{HostZskFileName: "Private-key-format: v1.3\nAlgorithm: 13 (ECDSAP256SHA256)\nPrivateKey: bm90IGEga2V5\n"}, settings: hostTestSettings(t, keys)},
		{name: "no zsk", files: map[string]string{}, settings: hostTestSettings(t, keys)},
		{name: "no anchors", files: map[string]string{HostZskFileName: zskFile}, settings: hostTestSettings(t, nil)},
	}
	for _, c := range cases {
		host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
		engine, err := NewEdgeHost(t.Context(), host, &HostSettings{
			Dns:               c.settings,
			SecretDirectory:   hostTestSecrets(t, c.files),
			SnapshotDirectory: t.TempDir(),
		}, &HostDeps{Feed: sidecartest.NewFakeDnsFeed(sidecartest.NewFakeDnsStore(nil, nil), nil, nil, nil), ProbeDialer: hostTestDialer{}, RouteProbes: hostTestRouteProbes{}})
		if err == nil || engine.Ready() {
			t.Errorf("%s: err %v, ready %v", c.name, err, engine.Ready())
		}
	}
}

// The edge starts not ready, is ready once its feed gives a snapshot and its key set checks, and then
// answers over UDP with every Postgres acquisition in the process refused (I-12).
func TestEdgeHostServesFromItsFeedWithNoPostgres(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	keys := newHostTestKeys(t, clock.Now())
	store := sidecartest.NewFakeDnsStore(clock, nil)
	ctx := t.Context()
	if err := store.EnsureRoot(ctx, hostTestRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(ctx, &dnsservice.DnsZoneWrite{Root: hostTestRoot, Upserts: []dnsservice.DnsRecordSet{
		{Owner: hostTestRoot, Type: dns.TypeSOA, Ttl: 3600, Kind: dnsservice.DnsRecordKindSystem, Rdata: []string{"ns1.zone.example. hostmaster.zone.example. 1 7200 3600 1209600 60"}},
		{Owner: hostTestRoot, Type: dns.TypeNS, Ttl: 86400, Kind: dnsservice.DnsRecordKindSystem, Rdata: []string{"ns1.zone.example."}},
	}}); err != nil {
		t.Fatal(err)
	}
	feed := sidecartest.NewFakeDnsFeed(store, clock, nil, nil)
	feed.SetKeyWindow(keys.window)
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil).ServiceHost()
	host.Redis = nil
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	engine, err := NewEdgeHost(ctx, host, &HostSettings{
		Dns:               hostTestSettings(t, keys),
		SecretDirectory:   hostTestSecrets(t, map[string]string{HostZskFileName: keys.zskRecord.PrivateKeyString(keys.zsk)}),
		SnapshotDirectory: t.TempDir(),
	}, &HostDeps{Feed: feed, ProbeDialer: hostTestDialer{}, RouteProbes: hostTestRouteProbes{}})
	if err != nil {
		t.Fatal(err)
	}
	edgeHost := engine.(*EdgeHost)
	defer edgeHost.Close()
	for {
		serial, changed := edgeHost.edge.ServedSerial(hostTestRoot)
		if 1 <= serial {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the edge never served the feed's snapshot")
		}
	}
	if !edgeHost.Ready() {
		t.Fatal("serving and not ready")
	}
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go edgeHost.ServeUdp(conn)
	client := &dns.Client{Net: "udp"}
	query := &dns.Msg{}
	query.SetQuestion(hostTestRoot, dns.TypeSOA)
	reply, _, err := client.ExchangeContext(ctx, query, conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 || reply.Answer[0].(*dns.SOA).Serial != 1 {
		t.Fatalf("reply %v", reply)
	}
	if count := attempts(); count != 0 {
		t.Fatalf("the edge tried Postgres %d times", count)
	}
}

// Without its tables the API role's store reads nothing, so the block is not ready, and its verifier
// and lb inventory come from its secret files.
func TestApiHostNotReadyWithoutItsTables(t *testing.T) {
	if server.SidecarMg5Linked() {
		t.Skip("this binary links MG5; the tables' presence is TestApiHostReadyOnItsTables's")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	host := fakeHost.ServiceHost()
	if err := fakeHost.Gates.Set("sidecar:\n  dns: {enabled: true}\n"); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(host.Redis)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := NewApiHost(t.Context(), host, &HostSettings{
		Dns: hostTestSettings(t, nil),
		SecretDirectory: hostTestSecrets(t, map[string]string{
			HostServicesFileName: "domain: example\nversions:\n  - lb:\n      interfaces:\n        a-edge-1.example:\n          eth0: {ipv4: 192.0.2.10, ipv6: \"2001:db8::10\"}\n",
		}),
	}, &HostDeps{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	api := engine.(*dnsservice.DnsApi)
	defer api.Close()
	for {
		count, changed := api.Probes()
		if 1 <= count {
			break
		}
		select {
		case <-changed:
		case <-t.Context().Done():
			t.Fatal("no probe")
		}
	}
	if api.Ready() {
		t.Fatal("ready with no tables")
	}
}

// The API role's windows (DNS-M5-19): a root's window file in secret_files is released, verified by
// dns.yml's anchors; one whose signature was damaged refuses the block; and with no file nothing is
// released, so no edge becomes ready.
func TestApiHostReleasesTheWindowFileInItsSecrets(t *testing.T) {
	start := time.Date(2027, time.February, 3, 12, 0, 0, 0, time.UTC)
	keys := newHostTestKeys(t, start)
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	file := &dnsservice.DnsKeyWindowFile{Format: dnsservice.DnsKeyWindowFileFormat, Root: hostTestRoot, Windows: []dnsservice.DnsKeyWindow{*keys.window}}
	data, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	settings := &HostSettings{
		Dns:             hostTestSettings(t, keys),
		SecretDirectory: hostTestSecrets(t, map[string]string{dnsservice.DnsKeyWindowFileName(hostTestRoot): string(data)}),
	}
	release, err := hostKeyWindowRelease(settings, host)
	if err != nil {
		t.Fatal(err)
	}
	if window := release.Window(hostTestRoot, start); window == nil || !window.NotBefore.Equal(keys.window.NotBefore) {
		t.Errorf("released %+v", window)
	}

	// another signature of the right size, made by no key the window publishes
	other := newHostTestKeys(t, start)
	damaged := *keys.window
	damaged.Rrsigs = other.window.Rrsigs
	file.Windows = []dnsservice.DnsKeyWindow{damaged}
	damagedData, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	settings.SecretDirectory = hostTestSecrets(t, map[string]string{dnsservice.DnsKeyWindowFileName(hostTestRoot): string(damagedData)})
	if _, err := hostKeyWindowRelease(settings, host); !errors.Is(err, dnsservice.ErrDnsKeyWindowRefused) {
		t.Errorf("a signature by another key: %v", err)
	}

	settings.SecretDirectory = hostTestSecrets(t, nil)
	release, err = hostKeyWindowRelease(settings, host)
	if err != nil {
		t.Fatal(err)
	}
	if window := release.Window(hostTestRoot, start); window != nil {
		t.Errorf("released %+v with no file", window)
	}
}

// The lb inventory as the feed names it: every edge host's public interfaces, by host/interface.
func TestHostLbInterfacesFromServicesYaml(t *testing.T) {
	interfaces, err := hostLbInterfaces([]byte("domain: example\nversions:\n  - lb:\n      interfaces:\n        a-edge-1.example:\n          eth0: {ipv4: 192.0.2.10, ipv6: \"2001:db8::10\"}\n          eth1: {ipv4: 192.0.2.11, transparent: true}\n        a-core-1.example:\n          eth0: {ipv4: 192.0.2.20}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 1 || interfaces[0].Name != "a-edge-1/eth0" || interfaces[0].Ipv4 != netip.MustParseAddr("192.0.2.10") || interfaces[0].Ipv6 != netip.MustParseAddr("2001:db8::10") {
		t.Fatalf("interfaces %+v", interfaces)
	}
}

// A role that is neither api nor edge is never ready, and RunHost runs no block for it.
func TestNewHostRefusesAnUnknownRole(t *testing.T) {
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	engine, err := NewHost(t.Context(), "both", host, &HostSettings{Dns: hostTestSettings(t, nil)}, nil)
	if err == nil || engine.Ready() {
		t.Fatalf("err %v, ready %v", err, engine.Ready())
	}
	if err := RunHost(t.Context(), "both", &HostSettings{Dns: hostTestSettings(t, nil)}, nil, &HostPorts{Status: 80, Dns: 53}); err == nil {
		t.Fatal("RunHost ran a role that is neither")
	}
}
