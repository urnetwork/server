package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/sidecarhost"
)

const testDnsTestRoot = "zone.example."

// A fresh state directory below the test's own.
func testDnsTestState(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

// dns.yml for the test root, as config would give it: no anchors.
func testDnsTestConfig(t *testing.T) *dnsservice.DnsSettings {
	t.Helper()
	settings := dnsservice.DefaultDnsSettings()
	settings.Roots = []string{testDnsTestRoot}
	settings.SignaturesPerSecond = 1000
	settings.Tcp.ConnectionsPerSource = 2
	settings.Rrl.ResponsesPerSecond = 1000
	settings.Edge.UdpReaderCount = 1
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	return settings
}

// Every file of the set, by name, as it is now.
func testDnsTestFiles(t *testing.T, state string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range testDnsKeySetFiles() {
		data, err := os.ReadFile(filepath.Join(state, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	return files
}

// The main scrubs its logs before anything can log, as each block's main does, since it runs both.
func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// keys makes the set once, owner-only, and a second run keeps it byte for byte.
func TestKeysAreMadeOnceAndKept(t *testing.T) {
	state := testDnsTestState(t)
	settings := DefaultTestDnsSettings()
	if err := makeTestDnsKeys(state, settings); err != nil {
		t.Fatal(err)
	}
	first := testDnsTestFiles(t, state)
	for _, name := range testDnsKeySetFiles() {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != testDnsFileMode {
			t.Errorf("%s has mode %v", name, info.Mode().Perm())
		}
	}
	for _, name := range []string{".", testDnsApiDirectoryName, testDnsEdgeDirectoryName, testDnsSnapshotDirectoryName} {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != testDnsDirectoryMode {
			t.Errorf("%s has mode %v", name, info.Mode().Perm())
		}
	}
	if err := makeTestDnsKeys(state, settings); err != nil {
		t.Fatal(err)
	}
	for name, data := range testDnsTestFiles(t, state) {
		if !bytes.Equal(data, first[name]) {
			t.Errorf("a second run changed %s", name)
		}
	}
	// neither role's secret directory holds the KSK's private key
	for _, directory := range []string{testDnsApiDirectoryName, testDnsEdgeDirectoryName} {
		entries, err := os.ReadDir(filepath.Join(state, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(state, directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(data, first[testDnsKskPrivateFileName]) {
				t.Errorf("%s/%s is the KSK's private key", directory, entry.Name())
			}
		}
	}
}

// A state holding part of a set, one whose parts disagree, a relative path and a directory open to
// others are each refused, and nothing is written over.
func TestKeysRefuseAPartialSetAndAnOpenState(t *testing.T) {
	settings := DefaultTestDnsSettings()

	partial := testDnsTestState(t)
	if err := makeTestDnsKeys(partial, settings); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(partial, testDnsZskKeyFileName)); err != nil {
		t.Fatal(err)
	}
	if err := makeTestDnsKeys(partial, settings); err == nil {
		t.Error("a partial set was taken")
	}
	if _, err := os.Stat(filepath.Join(partial, testDnsZskKeyFileName)); err == nil {
		t.Error("a partial set was mended")
	}

	mismatched := testDnsTestState(t)
	other := testDnsTestState(t)
	for _, state := range []string{mismatched, other} {
		if err := makeTestDnsKeys(state, settings); err != nil {
			t.Fatal(err)
		}
	}
	otherZsk, err := os.ReadFile(filepath.Join(other, testDnsZskKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mismatched, testDnsZskKeyFileName), otherZsk, testDnsFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTestDnsKeys(mismatched, settings); err == nil {
		t.Error("a ZSK the edge does not hold was taken for the windows")
	}

	if err := makeTestDnsKeys("relative/state", settings); err == nil {
		t.Error("a relative state was taken")
	}

	open := testDnsTestState(t)
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := makeTestDnsKeys(open, settings); err == nil {
		t.Error("a state open to others was taken")
	}
	if entries, err := os.ReadDir(open); err != nil || len(entries) != 0 {
		t.Errorf("a refused state was written to: %v %v", entries, err)
	}
}

// One window a step, signed once, always current and always over 7 d from its end, as DNS-M5-17's
// check holds a released window.
func TestWindowsServeOverSevenDaysAndSignOncePerStep(t *testing.T) {
	state := testDnsTestState(t)
	settings := DefaultTestDnsSettings()
	if err := makeTestDnsKeys(state, settings); err != nil {
		t.Fatal(err)
	}
	keys, err := loadTestDnsKeys(state, settings)
	if err != nil {
		t.Fatal(err)
	}
	start := sidecartest.DefaultClockStart()
	var last *dnsservice.DnsKeyWindow
	changes := 0
	for now := start; now.Before(start.Add(30 * 24 * time.Hour)); now = now.Add(time.Hour) {
		window := keys.Window(testDnsTestRoot, now)
		if window == nil {
			t.Fatalf("no window at %v", now)
		}
		if now.Before(window.NotBefore) || window.NotAfter.Sub(now) <= 7*24*time.Hour {
			t.Fatalf("at %v the window runs %v to %v", now, window.NotBefore, window.NotAfter)
		}
		if window != last {
			changes += 1
			if last != nil && window.NotBefore.Sub(last.NotBefore) != settings.WindowStep {
				t.Fatalf("windows %v apart", window.NotBefore.Sub(last.NotBefore))
			}
		}
		last = window
	}
	if changes < 4 || 6 < changes {
		t.Fatalf("%d windows in 30 days", changes)
	}
}

// A dns.yml that names its own anchors is refused, since only this KSK signs here; one with none takes
// the KSK as every root's anchor.
func TestAnchoredRefusesAConfigWithItsOwnAnchors(t *testing.T) {
	state := testDnsTestState(t)
	if err := makeTestDnsKeys(state, DefaultTestDnsSettings()); err != nil {
		t.Fatal(err)
	}
	public, err := loadTestDnsPublicKeys(state)
	if err != nil {
		t.Fatal(err)
	}
	config := testDnsTestConfig(t)
	anchored, err := public.anchored(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchored.TrustAnchors[testDnsTestRoot]) != 1 || anchored.TrustAnchors[testDnsTestRoot][0] != testDnsRdata(public.ksk) {
		t.Fatalf("anchors %v", anchored.TrustAnchors)
	}
	if len(config.TrustAnchors[testDnsTestRoot]) != 0 {
		t.Fatal("config's settings were changed")
	}
	if _, err := public.anchored(anchored); err == nil {
		t.Fatal("a dns.yml with its own anchors was taken")
	}
}

// config's local dns.yml beside this checkout, which the launcher's roles read, names no anchor of its
// own and takes the local KSK as every root's.
func TestTheLocalConfigTakesTheLocalKsk(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "local", "dns.yml"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("not read: no config checkout beside this one")
	}
	if err != nil {
		t.Fatal(err)
	}
	config, err := dnsservice.LoadDnsSettings(data)
	if err != nil {
		t.Fatal(err)
	}
	state := testDnsTestState(t)
	if err := makeTestDnsKeys(state, DefaultTestDnsSettings()); err != nil {
		t.Fatal(err)
	}
	public, err := loadTestDnsPublicKeys(state)
	if err != nil {
		t.Fatal(err)
	}
	anchored, err := public.anchored(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range anchored.Roots {
		if _, err := dnsservice.ParseDnsTrustAnchors(root, anchored.TrustAnchors[root]); err != nil {
			t.Errorf("%s: %v", root, err)
		}
	}
}

// The whole local path: the API block over a store, releasing the local KSK's windows, and an edge that
// follows it over HTTP with the ZSK and bearer keys made. The edge becomes ready and answers the apex's
// SOA signed by the ZSK, and its DNSKEY RRset signed by the KSK its dns.yml anchors.
func TestTheLocalKeySetMakesAnEdgeServeSignedAnswers(t *testing.T) {
	state := testDnsTestState(t)
	settings := DefaultTestDnsSettings()
	if err := makeTestDnsKeys(state, settings); err != nil {
		t.Fatal(err)
	}
	keys, err := loadTestDnsKeys(state, settings)
	if err != nil {
		t.Fatal(err)
	}
	dnsSettings, err := keys.anchored(testDnsTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	store := sidecartest.NewFakeDnsStore(clock, nil)
	if err := store.EnsureRoot(ctx, testDnsTestRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(ctx, &dnsservice.DnsZoneWrite{Root: testDnsTestRoot, Upserts: []dnsservice.DnsRecordSet{
		{Owner: testDnsTestRoot, Type: dns.TypeSOA, Ttl: 3600, Kind: dnsservice.DnsRecordKindSystem, Rdata: []string{"ns1.zone.example. hostmaster.zone.example. 1 7200 3600 1209600 60"}},
		{Owner: testDnsTestRoot, Type: dns.TypeNS, Ttl: 86400, Kind: dnsservice.DnsRecordKindSystem, Rdata: []string{"ns1.zone.example."}},
	}}); err != nil {
		t.Fatal(err)
	}

	apiFake := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	if err := apiFake.Gates.Set("sidecar:\n  dns: {enabled: true}\n"); err != nil {
		t.Fatal(err)
	}
	apiEngine, err := dnshost.NewApiHost(ctx, apiFake.ServiceHost(), &dnshost.HostSettings{
		Dns:             dnsSettings,
		SecretDirectory: filepath.Join(state, testDnsApiDirectoryName),
	}, &dnshost.HostDeps{Store: store, KeyWindow: keys.Window})
	if err != nil {
		t.Fatal(err)
	}
	defer apiEngine.Close()
	apiServer := httptest.NewServer(apiEngine.(sidecar.HttpEngine).Handler())
	defer apiServer.Close()

	edgeHost := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil).ServiceHost()
	edgeHost.Redis = nil
	edgeEngine, err := dnshost.NewEdgeHost(ctx, edgeHost, &dnshost.HostSettings{
		Dns:                dnsSettings,
		SecretDirectory:    filepath.Join(state, testDnsEdgeDirectoryName),
		CredentialFileName: testDnsEdgeCredentialFileName(),
		ApiUrl:             apiServer.URL,
		SnapshotDirectory:  filepath.Join(state, testDnsSnapshotDirectoryName),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := edgeEngine.(*dnshost.EdgeHost)
	defer edge.Close()
	// a bound on a failure, not a wait: the edge serves as soon as the feed's first answer checks
	waitCtx, waitCancel := context.WithTimeout(ctx, time.Minute)
	defer waitCancel()
	for {
		serial, changed := edge.ServedSerial(testDnsTestRoot)
		if 1 <= serial {
			break
		}
		select {
		case <-changed:
		case <-waitCtx.Done():
			t.Fatal("the edge never served the API's journal")
		}
	}
	if !edge.Ready() {
		t.Fatal("serving and not ready")
	}

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go edge.ServeUdp(conn)
	client := &dns.Client{Net: "udp"}
	signed := func(qtype uint16) (dns.RR, *dns.RRSIG, []dns.RR) {
		t.Helper()
		query := &dns.Msg{}
		query.SetQuestion(testDnsTestRoot, qtype)
		query.SetEdns0(1232, true)
		reply, _, err := client.ExchangeContext(ctx, query, conn.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		var rrsig *dns.RRSIG
		rrset := []dns.RR{}
		for _, record := range reply.Answer {
			if signature, ok := record.(*dns.RRSIG); ok && signature.TypeCovered == qtype {
				rrsig = signature
			} else if record.Header().Rrtype == qtype {
				rrset = append(rrset, record)
			}
		}
		if reply.Rcode != dns.RcodeSuccess || rrsig == nil || len(rrset) == 0 {
			t.Fatalf("%s: reply %v", dns.TypeToString[qtype], reply)
		}
		return rrset[0], rrsig, rrset
	}

	zsk := dns.Copy(keys.zsk).(*dns.DNSKEY)
	zsk.Hdr.Name = testDnsTestRoot
	soa, soaRrsig, soaRrset := signed(dns.TypeSOA)
	if soa.(*dns.SOA).Serial != 1 || soaRrsig.KeyTag != zsk.KeyTag() {
		t.Fatalf("soa %v signed by %d", soa, soaRrsig.KeyTag)
	}
	if err := soaRrsig.Verify(zsk, soaRrset); err != nil || !soaRrsig.ValidityPeriod(clock.Now()) {
		t.Fatalf("the SOA's signature does not verify by the ZSK: %v", err)
	}

	anchors, err := dnsservice.ParseDnsTrustAnchors(testDnsTestRoot, dnsSettings.TrustAnchors[testDnsTestRoot])
	if err != nil {
		t.Fatal(err)
	}
	_, dnskeyRrsig, dnskeyRrset := signed(dns.TypeDNSKEY)
	if err := dnskeyRrsig.Verify(anchors[0], dnskeyRrset); err != nil || !dnskeyRrsig.ValidityPeriod(clock.Now()) {
		t.Fatalf("the DNSKEY RRset's signature does not verify by the anchor: %v", err)
	}
}
