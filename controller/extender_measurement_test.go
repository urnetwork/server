package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// lag.dev measurement certificates, the controller half (lag.dev/PLAN.md T2):
// reading a certificate request, which extenders may be measured and at
// which addresses, the record field, the activation that returns the id and
// carries a request, and the extender's poll for its certificate.

// The synthetic measurement zone every test here names.
const testMeasurementZone = "extender.ur.example"

func testMeasurementKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A certificate request from `template` under `key`, DER.
func testMeasurementCsr(t testing.TB, key any, template *x509.CertificateRequest) []byte {
	t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

// What A8 names a public key by: the hex SHA-256 of its SubjectPublicKeyInfo.
func testMeasurementKeySha256(t testing.TB, publicKey any) string {
	t.Helper()
	spki, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(spki)
	return hex.EncodeToString(digest[:])
}

// The name is the extender's id under the zone (N1), and parses back to it.
func TestExtenderMeasurementNameIsTheIdUnderTheZone(t *testing.T) {
	extenderId := server.NewId()
	name := ExtenderMeasurementName(extenderId, testMeasurementZone)
	connect.AssertEqual(t, name, extenderId.String()+"."+testMeasurementZone)

	parsedId, ok := ParseExtenderMeasurementName(name, testMeasurementZone)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, parsedId, extenderId)

	for _, other := range []string{
		testMeasurementZone,
		"www." + testMeasurementZone,
		"203-0-113-7." + testMeasurementZone,
		"_acme-challenge." + name,
		strings.ToUpper(name),
		extenderId.String() + ".other.example",
	} {
		if _, ok := ParseExtenderMeasurementName(other, testMeasurementZone); ok {
			t.Errorf("%s parsed as a measurement name", other)
		}
	}
}

// The request an extender sends (C4): its one name, P-256, signed, in standard
// base64. The key comes back as the request's own SubjectPublicKeyInfo.
func TestExtenderMeasurementCsrAcceptsTheExtendersName(t *testing.T) {
	key := testMeasurementKey(t)
	extenderId := server.NewId()
	name := ExtenderMeasurementName(extenderId, testMeasurementZone)
	csr := testMeasurementCsr(t, key, &x509.CertificateRequest{
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		DNSNames:           []string{name},
	})

	for _, csrBase64 := range []string{
		base64.StdEncoding.EncodeToString(csr),
		base64.RawStdEncoding.EncodeToString(csr),
		base64.URLEncoding.EncodeToString(csr),
		base64.RawURLEncoding.EncodeToString(csr),
	} {
		request, err := ParseExtenderMeasurementCsr(csrBase64, extenderId, testMeasurementZone)
		if err != nil {
			t.Fatalf("a valid request was refused: %v", err)
		}
		connect.AssertEqual(t, request.Name, name)
		publicKey, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, bytes.Equal(request.PublicKey, publicKey), true)
		connect.AssertEqual(t, bytes.Equal(request.Csr, csr), true)
	}

	// a common name that is the same name is allowed
	if _, err := ParseExtenderMeasurementCsrDer(testMeasurementCsr(t, key, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: name},
		DNSNames: []string{name},
	}), extenderId, testMeasurementZone); err != nil {
		t.Fatalf("a request with its name as the common name was refused: %v", err)
	}
}

// Everything C5 refuses: another key type or curve, a signature that does not
// verify, any name but the extender's own in its canonical spelling, a second
// name, names of other kinds, a common name that is another name, and text
// that is not a request at all.
func TestExtenderMeasurementCsrRefusesWhatC5Refuses(t *testing.T) {
	key := testMeasurementKey(t)
	extenderId := server.NewId()
	name := ExtenderMeasurementName(extenderId, testMeasurementZone)
	otherName := ExtenderMeasurementName(server.NewId(), testMeasurementZone)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tampered := testMeasurementCsr(t, key, &x509.CertificateRequest{DNSNames: []string{name}})
	tampered[len(tampered)-1] ^= 0x01

	cases := []struct {
		name string
		csr  []byte
	}{
		{name: "rsa key", csr: testMeasurementCsr(t, rsaKey, &x509.CertificateRequest{DNSNames: []string{name}})},
		{name: "p-384 key", csr: testMeasurementCsr(t, p384Key, &x509.CertificateRequest{DNSNames: []string{name}})},
		{name: "bad signature", csr: tampered},
		{name: "no names", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{})},
		{name: "another extender's name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{otherName},
		})},
		{name: "a second name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{name, otherName},
		})},
		{name: "its name twice", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{name, name},
		})},
		{name: "another zone", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{extenderId.String() + ".other.example"},
		})},
		{name: "upper case", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{strings.ToUpper(name)},
		})},
		{name: "the id without its hyphens", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{strings.ReplaceAll(extenderId.String(), "-", "") + "." + testMeasurementZone},
		})},
		{name: "a wildcard", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{"*." + testMeasurementZone},
		})},
		{name: "an address for a name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{"203-0-113-7." + testMeasurementZone},
		})},
		{name: "ip name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames:    []string{name},
			IPAddresses: []net.IP{net.ParseIP("203.0.113.7")},
		})},
		{name: "email name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames:       []string{name},
			EmailAddresses: []string{"extender@ur.example"},
		})},
		{name: "uri name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			DNSNames: []string{name},
			URIs:     []*url.URL{{Scheme: "https", Host: "ur.example"}},
		})},
		{name: "another common name", csr: testMeasurementCsr(t, key, &x509.CertificateRequest{
			Subject:  pkix.Name{CommonName: otherName},
			DNSNames: []string{name},
		})},
		{name: "not a request", csr: []byte("not a certificate request")},
	}
	for _, c := range cases {
		if _, err := ParseExtenderMeasurementCsrDer(c.csr, extenderId, testMeasurementZone); err == nil {
			t.Errorf("%s: the request was accepted", c.name)
		}
	}

	for _, csrBase64 := range []string{"", "!!!", strings.Repeat("A", ExtenderMeasurementCsrMaxLength+4)} {
		if _, err := ParseExtenderMeasurementCsr(csrBase64, extenderId, testMeasurementZone); err == nil {
			t.Errorf("%q...: the text was accepted", csrBase64[:min(len(csrBase64), 8)])
		}
	}

	// the zero id has no name, so no request is for it, not even one whose
	// name is empty or the zero id's spelling
	connect.AssertEqual(t, ExtenderMeasurementName(server.Id{}, testMeasurementZone), "")
	for _, zeroName := range []string{"", server.Id{}.String() + "." + testMeasurementZone} {
		csr := testMeasurementCsr(t, key, &x509.CertificateRequest{DNSNames: []string{zeroName}})
		if _, err := ParseExtenderMeasurementCsrDer(csr, server.Id{}, testMeasurementZone); err == nil {
			t.Errorf("%q: a request for the zero id was accepted", zeroName)
		}
	}
	if _, ok := ParseExtenderMeasurementName(server.Id{}.String()+"."+testMeasurementZone, testMeasurementZone); ok {
		t.Errorf("the zero id's spelling parsed as a measurement name")
	}
}

func testMeasurementAddress(ip string, carriers ...string) *model.NetworkExtenderAddress {
	addr := netip.MustParseAddr(ip)
	ipVersion := 6
	if addr.Is4() {
		ipVersion = 4
	}
	return &model.NetworkExtenderAddress{
		IpVersion: ipVersion,
		Ip:        addr,
		Carriers:  carriers,
		Active:    true,
	}
}

func testMeasurementExtender() *model.NetworkExtender {
	return &model.NetworkExtender{
		ExtenderId:    server.NewId(),
		PublicKey:     []byte("extender-public-key-measurement-1"),
		TcpPort:       ExtenderMeasurementPort,
		Active:        true,
		DirectoryTier: connect.ExtenderDirectoryTierOpen,
	}
}

// Which extenders may be measured, and at which addresses (C5, C8, §2,
// decisions 1, 4 and 14): an active open tier extender that is no canary,
// serving tcp on 443, at each active address that serves tcp, IPv4 first.
func TestExtenderMeasurementEligibleAddresses(t *testing.T) {
	v4 := testMeasurementAddress("203.0.113.7", connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic)
	v6 := testMeasurementAddress("2001:db8::7", connect.ExtenderCarrierTcp)

	ips, reason := ExtenderMeasurementEligibleAddresses(testMeasurementExtender(), []*model.NetworkExtenderAddress{v6, v4})
	connect.AssertEqual(t, reason, "")
	if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}) {
		t.Fatalf("ips = %v", ips)
	}

	inactiveV6 := testMeasurementAddress("2001:db8::7", connect.ExtenderCarrierTcp)
	inactiveV6.Active = false
	webRtcV6 := testMeasurementAddress("2001:db8::7", connect.ExtenderCarrierWebRtc)
	for _, v6 := range []*model.NetworkExtenderAddress{inactiveV6, webRtcV6} {
		ips, reason = ExtenderMeasurementEligibleAddresses(testMeasurementExtender(), []*model.NetworkExtenderAddress{v4, v6})
		connect.AssertEqual(t, reason, "")
		if !slices.Equal(ips, []netip.Addr{netip.MustParseAddr("203.0.113.7")}) {
			t.Fatalf("an address without active public inbound tcp was kept: %v", ips)
		}
	}

	cases := []struct {
		name      string
		extender  func(*model.NetworkExtender)
		addresses []*model.NetworkExtenderAddress
	}{
		{name: "inactive", extender: func(e *model.NetworkExtender) { e.Active = false }, addresses: []*model.NetworkExtenderAddress{v4}},
		{name: "gated", extender: func(e *model.NetworkExtender) { e.DirectoryTier = connect.ExtenderDirectoryTierGated }, addresses: []*model.NetworkExtenderAddress{v4}},
		{name: "dns canary", extender: func(e *model.NetworkExtender) { e.CanaryChannel = connect.ExtenderChannelDns }, addresses: []*model.NetworkExtenderAddress{v4}},
		{name: "another tcp port", extender: func(e *model.NetworkExtender) { e.TcpPort = 8443 }, addresses: []*model.NetworkExtenderAddress{v4}},
		{name: "webrtc only", extender: func(e *model.NetworkExtender) {}, addresses: []*model.NetworkExtenderAddress{webRtcV6}},
		{name: "no address", extender: func(e *model.NetworkExtender) {}, addresses: []*model.NetworkExtenderAddress{}},
	}
	for _, c := range cases {
		extender := testMeasurementExtender()
		c.extender(extender)
		ips, reason := ExtenderMeasurementEligibleAddresses(extender, c.addresses)
		if reason == "" || ips != nil {
			t.Errorf("%s: measured at %v", c.name, ips)
		}
	}
	if _, reason := ExtenderMeasurementEligibleAddresses(nil, nil); reason == "" {
		t.Fatalf("a missing extender may be measured")
	}
}

// A certificate is withdrawn when its extender is deactivated, or is in the
// gated tier or a canary (C9), and for nothing else: neither an address
// change nor a tcp port keeps it from being served again, so neither revokes
// it.
func TestExtenderMeasurementWithdrawReason(t *testing.T) {
	connect.AssertEqual(t, ExtenderMeasurementWithdrawReason(testMeasurementExtender()), "")
	otherPort := testMeasurementExtender()
	otherPort.TcpPort = 8443
	connect.AssertEqual(t, ExtenderMeasurementWithdrawReason(otherPort), "")

	gated := testMeasurementExtender()
	gated.DirectoryTier = connect.ExtenderDirectoryTierGated
	canary := testMeasurementExtender()
	canary.CanaryChannel = connect.ExtenderChannelGated
	inactive := testMeasurementExtender()
	inactive.Active = false
	for _, extender := range []*model.NetworkExtender{gated, canary, inactive, nil} {
		if ExtenderMeasurementWithdrawReason(extender) == "" {
			t.Fatalf("a certificate stays for %+v", extender)
		}
	}
}

// The record field (C10): a record carries the extender's id as its
// MeasurementId exactly while measurement is enabled, the extender may be
// measured at one of the record's addresses, and it holds a live certificate
// unexpired at the record's issue time. Otherwise the field is empty.
func TestSignExtenderRecordCarriesTheMeasurementId(t *testing.T) {
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

	issueTime := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	notAfter := issueTime.Add(30 * 24 * time.Hour)
	expired := issueTime.Add(-time.Minute)
	tcpAddresses := []*model.NetworkExtenderAddress{
		testMeasurementAddress("203.0.113.7", connect.ExtenderCarrierTcp),
	}
	webRtcAddresses := []*model.NetworkExtenderAddress{
		testMeasurementAddress("203.0.113.7", connect.ExtenderCarrierWebRtc),
	}

	measurementId := func(enabled bool, addresses []*model.NetworkExtenderAddress, mutate func(*model.NetworkExtender)) ([]byte, server.Id) {
		config := &ExtenderConfig{
			NetworkHost: testExtenderNetworkHost,
			Measurement: ExtenderMeasurementConfig{Enabled: enabled},
		}
		extender := testMeasurementExtender()
		extender.MeasurementCertNotAfter = &notAfter
		mutate(extender)
		record, message, err := SignExtenderRecord(config, rootPrivateKey, extender, addresses, issueTime)
		if err != nil {
			t.Fatal(err)
		}
		body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		// the gossip message carries the same record
		gossipMessage := &protocol.ExtenderGossipMessage{}
		if err := proto.Unmarshal(message, gossipMessage); err != nil {
			t.Fatal(err)
		}
		gossipBody, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(gossipMessage.GetRecord())
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, bytes.Equal(gossipBody.MeasurementId, body.MeasurementId), true)
		return body.MeasurementId, extender.ExtenderId
	}

	id, extenderId := measurementId(true, tcpAddresses, func(*model.NetworkExtender) {})
	connect.AssertEqual(t, len(id), 16)
	connect.AssertEqual(t, bytes.Equal(id, extenderId.Bytes()), true)
	// a reader computes the name from it (N1)
	readId, err := connect.IdFromBytes(id)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, connect.ExtenderMeasurementName(readId, testMeasurementZone), ExtenderMeasurementName(extenderId, testMeasurementZone))

	cases := []struct {
		name      string
		enabled   bool
		addresses []*model.NetworkExtenderAddress
		mutate    func(*model.NetworkExtender)
	}{
		{name: "disabled", enabled: false, addresses: tcpAddresses, mutate: func(*model.NetworkExtender) {}},
		{name: "no certificate", enabled: true, addresses: tcpAddresses, mutate: func(e *model.NetworkExtender) { e.MeasurementCertNotAfter = nil }},
		{name: "expired", enabled: true, addresses: tcpAddresses, mutate: func(e *model.NetworkExtender) { e.MeasurementCertNotAfter = &expired }},
		{name: "gated", enabled: true, addresses: tcpAddresses, mutate: func(e *model.NetworkExtender) { e.DirectoryTier = connect.ExtenderDirectoryTierGated }},
		{name: "canary", enabled: true, addresses: tcpAddresses, mutate: func(e *model.NetworkExtender) { e.CanaryChannel = connect.ExtenderChannelDns }},
		{name: "no public inbound address", enabled: true, addresses: webRtcAddresses, mutate: func(*model.NetworkExtender) {}},
	}
	for _, c := range cases {
		if id, _ := measurementId(c.enabled, c.addresses, c.mutate); len(id) != 0 {
			t.Errorf("%s: the record carries a measurement id", c.name)
		}
	}

	// a record signed for the zero id, which names no extender, carries none
	zeroExtender := testMeasurementExtender()
	zeroExtender.ExtenderId = server.Id{}
	zeroExtender.MeasurementCertNotAfter = &notAfter
	connect.AssertEqual(t, len(extenderMeasurementId(
		&ExtenderConfig{Measurement: ExtenderMeasurementConfig{Enabled: true}},
		zeroExtender,
		tcpAddresses,
		issueTime,
	)), 0)
}

// Installs the fixture's `extender.yml` with a measurement block, enabled,
// in the synthetic zone.
func installTestExtenderMeasurementConfig(t testing.TB, api *testExtenderApi) ed25519.PrivateKey {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	installTestExtenderConfigYaml(t, strings.Join([]string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPublicKey)),
		fmt.Sprintf("network_host: %s", testExtenderNetworkHost),
		"network_hosts:",
		fmt.Sprintf("  - %s", testExtenderNetworkHost),
		fmt.Sprintf("  - %s", testExtenderApiHost),
		fmt.Sprintf("api_url: https://%s", testExtenderApiHost),
		"measurement:",
		"  enabled: true",
		"  zone_name: " + testMeasurementZone,
	}, "\n"))
	if api != nil {
		previousTlsConfig := extenderForwardProbeTlsConfig
		extenderForwardProbeTlsConfig = func() *tls.Config {
			return &tls.Config{RootCAs: api.rootCAs}
		}
		t.Cleanup(func() {
			extenderForwardProbeTlsConfig = previousTlsConfig
		})
	}
	return rootPrivateKey
}

// The activation answer carries the extender's id (A10, N4), the same at
// every activation, which is what its measurement name is made of. The
// request rides a later activation: a successful one stores it as the
// extender's pending request, and an activation without one leaves it as it
// is. A request that cannot be read, or that names another extender, is
// stored refused, with the reason naming the extender's own name, and A8
// answers the refusal with the key and names of as much of the request as
// could be read (key_sha256).
//
// The activations offer the tcp carrier alone, a challenge and a forward
// each, so three of them stay under the fixture extender's admission limit
// of eight actions a minute from one subnet; the refusals, and the request
// after them, go through the storage the activation calls.
func TestExtenderActivateReturnsTheIdAndStoresTheMeasurementRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderMeasurementConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		activate := func(csr []byte) *ExtenderActivateResult {
			t.Helper()
			args := fixture.activateArgs()
			args.Carriers = []string{connect.ExtenderCarrierTcp}
			if csr != nil {
				args.MeasurementCsr = base64.StdEncoding.EncodeToString(csr)
			}
			result, err := ExtenderActivate(args, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Activated {
				t.Fatalf("the activation was refused: %s", result.Error)
			}
			return result
		}
		poll := func() *ExtenderMeasurementCertResult {
			t.Helper()
			result, err := ExtenderMeasurementCert(&ExtenderMeasurementCertArgs{}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		// the first activation of a new identity tells it its id
		first := activate(nil)
		if first.ExtenderId == nil {
			t.Fatalf("the activation answer carries no extender id")
		}
		extenderId := *first.ExtenderId
		connect.AssertEqual(t, extenderId, testExtenderIdForKey(ctx, t, fixture.publicKey))
		connect.AssertEqual(t, poll().State, model.NetworkExtenderMeasurementStateNone)

		// the request for that id's name rides the next activation
		name := ExtenderMeasurementName(extenderId, testMeasurementZone)
		requestKey := testMeasurementKey(t)
		csr := testMeasurementCsr(t, requestKey, &x509.CertificateRequest{DNSNames: []string{name}})
		second := activate(csr)
		connect.AssertEqual(t, *second.ExtenderId, extenderId)
		measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		if measurement == nil {
			t.Fatalf("the request was not stored")
		}
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		if !slices.Equal(measurement.Names, []string{name}) {
			t.Fatalf("names = %v", measurement.Names)
		}
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, csr), true)
		pending := poll()
		connect.AssertEqual(t, pending.State, model.NetworkExtenderMeasurementStatePending)
		if !slices.Equal(pending.Names, []string{name}) {
			t.Fatalf("poll names = %v", pending.Names)
		}
		connect.AssertEqual(t, pending.ChainPem, "")

		// most activations carry no request, and the stored one stands
		third := activate(nil)
		connect.AssertEqual(t, *third.ExtenderId, extenderId)
		measurement = model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, csr), true)

		config, err := EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		// an unreadable request is refused with the reason; it has no key or
		// names to answer with
		storeExtenderMeasurementRequest(ctx, config, extenderId, base64.StdEncoding.EncodeToString([]byte("not a certificate request")), server.NowUtc())
		refused := poll()
		connect.AssertEqual(t, refused.State, model.NetworkExtenderMeasurementStateRefused)
		if refused.Reason == "" {
			t.Fatalf("a refusal carries no reason")
		}
		connect.AssertEqual(t, refused.KeySha256, "")
		connect.AssertEqual(t, len(refused.Names), 0)

		// and so is a request for another extender's name. The answer names
		// the request's key and names, so the extender knows the refusal is of
		// its own request rather than reading it as nothing held (C4)
		otherName := ExtenderMeasurementName(server.NewId(), testMeasurementZone)
		otherKey := testMeasurementKey(t)
		otherCsr := testMeasurementCsr(t, otherKey, &x509.CertificateRequest{DNSNames: []string{otherName}})
		storeExtenderMeasurementRequest(ctx, config, extenderId, base64.StdEncoding.EncodeToString(otherCsr), server.NowUtc())
		refused = poll()
		connect.AssertEqual(t, refused.State, model.NetworkExtenderMeasurementStateRefused)
		if !strings.Contains(refused.Reason, name) {
			t.Fatalf("the refusal does not name the extender's own name: %q", refused.Reason)
		}
		connect.AssertEqual(t, refused.KeySha256, testMeasurementKeySha256(t, &otherKey.PublicKey))
		if !slices.Equal(refused.Names, []string{otherName}) {
			t.Fatalf("refused names = %v", refused.Names)
		}

		// and a request under a key that is not P-256, for the right name: its
		// key is named all the same
		p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		p384Csr := testMeasurementCsr(t, p384Key, &x509.CertificateRequest{DNSNames: []string{name}})
		storeExtenderMeasurementRequest(ctx, config, extenderId, base64.StdEncoding.EncodeToString(p384Csr), server.NowUtc())
		refused = poll()
		connect.AssertEqual(t, refused.State, model.NetworkExtenderMeasurementStateRefused)
		connect.AssertEqual(t, refused.KeySha256, testMeasurementKeySha256(t, &p384Key.PublicKey))
		if !slices.Equal(refused.Names, []string{name}) {
			t.Fatalf("refused names = %v", refused.Names)
		}

		// a valid request after a refusal waits for a certificate again
		storeExtenderMeasurementRequest(ctx, config, extenderId, base64.StdEncoding.EncodeToString(csr), server.NowUtc())
		pending = poll()
		connect.AssertEqual(t, pending.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, pending.KeySha256, testMeasurementKeySha256(t, &requestKey.PublicKey))
	})
}

// The extender's poll (A8) over the life of a request: none before any;
// pending; refused when the issuance refuses it, until the extender asks
// again; issued with the chain once a certificate answers it; withdrawn,
// with the certificate's name, from the withdrawal until the extender asks
// again; then pending once more. Every answer but none names the key it is
// for (key_sha256). A request for the name and key already held changes
// nothing. While the operator does not issue, a pending request reads as
// refused and keeps its row. Another client's poll never reads it, a key
// picks the extender, and without a key the client's active extender answers
// rather than one it left behind.
func TestExtenderMeasurementCertAnswersThePoll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderMeasurementConfig(t, nil)

		clientSession := newTestExtenderSession(t, ctx, "203.0.113.7:54321")
		otherSession := newTestExtenderSession(t, ctx, "203.0.113.8:54321")

		extenderPublicKey := []byte("extender-public-key-measurement-poll")
		extenderId := server.NewId()
		createTime := server.NowUtc()
		model.Testing_CreateNetworkExtender(
			ctx,
			&model.NetworkExtender{
				ExtenderId:  extenderId,
				NetworkId:   clientSession.ByJwt.NetworkId,
				ClientId:    *clientSession.ByJwt.ClientId,
				PublicKey:   extenderPublicKey,
				CreateTime:  createTime,
				TcpPort:     ExtenderMeasurementPort,
				UdpPort:     443,
				DnsPort:     53,
				DnsTld:      connect.DefaultExtenderDnsTld,
				CountryCode: "us",
				Active:      true,
			},
			[]*model.NetworkExtenderAddress{
				{
					IpVersion:    4,
					Ip:           netip.MustParseAddr("203.0.113.7"),
					Carriers:     []string{connect.ExtenderCarrierTcp},
					ActivateTime: createTime,
					Active:       true,
				},
			},
		)
		// the extender the client ran under an earlier identity key: inactive,
		// and stamped later than the one it runs now
		leftBehindTime := createTime.Add(time.Hour)
		model.Testing_CreateNetworkExtender(
			ctx,
			&model.NetworkExtender{
				ExtenderId:      server.NewId(),
				NetworkId:       clientSession.ByJwt.NetworkId,
				ClientId:        *clientSession.ByJwt.ClientId,
				PublicKey:       []byte("extender-public-key-measurement-left"),
				CreateTime:      createTime.Add(-24 * time.Hour),
				TcpPort:         ExtenderMeasurementPort,
				UdpPort:         443,
				DnsPort:         53,
				DnsTld:          connect.DefaultExtenderDnsTld,
				CountryCode:     "us",
				Active:          false,
				RecordIssueTime: &leftBehindTime,
			},
			[]*model.NetworkExtenderAddress{},
		)

		answer := func(publicKeyHex string) *ExtenderMeasurementCertResult {
			t.Helper()
			result, err := ExtenderMeasurementCert(&ExtenderMeasurementCertArgs{PublicKeyHex: publicKeyHex}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		measurementConfig := &ExtenderConfig{Measurement: ExtenderMeasurementConfig{ZoneName: testMeasurementZone}}
		// the extender's request under `key`, signed anew each time, as an
		// activation stores it
		key := testMeasurementKey(t)
		name := ExtenderMeasurementName(extenderId, testMeasurementZone)
		ask := func() {
			t.Helper()
			csr := testMeasurementCsr(t, key, &x509.CertificateRequest{DNSNames: []string{name}})
			storeExtenderMeasurementRequest(ctx, measurementConfig, extenderId, base64.StdEncoding.EncodeToString(csr), server.NowUtc())
		}

		none := answer("")
		connect.AssertEqual(t, none.State, model.NetworkExtenderMeasurementStateNone)
		connect.AssertEqual(t, len(none.Names), 0)

		ask()
		publicKey, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDigest := sha256.Sum256(publicKey)
		keySha256 := hex.EncodeToString(keyDigest[:])
		pending := answer("")
		connect.AssertEqual(t, pending.State, model.NetworkExtenderMeasurementStatePending)
		if !slices.Equal(pending.Names, []string{name}) {
			t.Fatalf("pending names = %v", pending.Names)
		}
		connect.AssertEqual(t, pending.KeySha256, keySha256)
		connect.AssertEqual(t, answer(hex.EncodeToString(extenderPublicKey)).State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, answer(hex.EncodeToString([]byte("another key"))).State, model.NetworkExtenderMeasurementStateNone)

		// the same name and key again, signed anew, is the request already
		// held: nothing changes, down to the stored bytes
		held := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		ask()
		again := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, again.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, bytes.Equal(again.Csr, held.Csr), true)
		connect.AssertEqual(t, again.PendingTime.UTC(), held.PendingTime.UTC())

		// while the operator does not issue, the request reads as refused, so
		// the extender does not keep polling for it, and it stays pending in
		// its row
		installTestExtenderConfig(t, nil)
		notIssuing := answer("")
		connect.AssertEqual(t, notIssuing.State, model.NetworkExtenderMeasurementStateRefused)
		if notIssuing.Reason == "" {
			t.Fatalf("a refusal carries no reason")
		}
		connect.AssertEqual(t, notIssuing.KeySha256, keySha256)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStatePending)
		installTestExtenderMeasurementConfig(t, nil)
		connect.AssertEqual(t, answer("").State, model.NetworkExtenderMeasurementStatePending)

		// the issuance refuses the request (C5): refused, for its name and key
		pendingCsr := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).Csr
		connect.AssertEqual(t, model.RefusePendingNetworkExtenderMeasurement(ctx, extenderId, pendingCsr, "the extender has no public inbound address", server.NowUtc()), true)
		refused := answer("")
		connect.AssertEqual(t, refused.State, model.NetworkExtenderMeasurementStateRefused)
		if !slices.Equal(refused.Names, []string{name}) {
			t.Fatalf("refused names = %v", refused.Names)
		}
		connect.AssertEqual(t, refused.KeySha256, keySha256)
		connect.AssertEqual(t, refused.Reason, "the extender has no public inbound address")
		// a refusal holds nothing, so the extender's next activation carries
		// its request again, and it waits once more
		ask()
		connect.AssertEqual(t, answer("").State, model.NetworkExtenderMeasurementStatePending)

		now := server.NowUtc()
		notAfter := now.Add(90 * 24 * time.Hour).Truncate(time.Second)
		certId := server.NewId()
		chainPem := "-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n"
		model.AddNetworkExtenderMeasurementCert(ctx, &model.NetworkExtenderMeasurementCert{
			CertId:     certId,
			ExtenderId: extenderId,
			Names:      held.Names,
			Csr:        held.Csr,
			PublicKey:  held.PublicKey,
			ChainPem:   chainPem,
			SerialHex:  "1",
			NotBefore:  now,
			NotAfter:   notAfter,
			IssueTime:  now,
			CapCounted: true,
			RenewTime:  now.Add(60 * 24 * time.Hour),
		})
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].DeliverTime == nil, true)
		beforeDelivery := server.NowUtc()
		issued := answer("")
		connect.AssertEqual(t, issued.State, model.NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, issued.ChainPem, chainPem)
		connect.AssertEqual(t, issued.NotAfter.UTC(), notAfter)
		if !slices.Equal(issued.Names, []string{name}) {
			t.Fatalf("issued names = %v", issued.Names)
		}
		connect.AssertEqual(t, issued.KeySha256, keySha256)
		// the answer with the chain is its delivery, which the lag.dev sample
		// and the records wait for (A4, C10); a later answer keeps the first
		delivered := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].DeliverTime
		if delivered == nil || delivered.Before(beforeDelivery.Truncate(time.Microsecond)) {
			t.Fatalf("the answer with the chain recorded its delivery as %v", delivered)
		}
		// held, so asking again orders nothing
		ask()
		connect.AssertEqual(t, answer("").State, model.NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, len(model.GetPendingNetworkExtenderMeasurements(ctx, server.NowUtc(), 10)), 0)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)[0].DeliverTime.Equal(*delivered), true)

		// the request is the client's own extender's, and no one else's
		other, err := ExtenderMeasurementCert(&ExtenderMeasurementCertArgs{}, otherSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, other.State, model.NetworkExtenderMeasurementStateNone)

		// C9: from the withdrawal on, the answer is withdrawn, for that name
		model.WithdrawNetworkExtenderMeasurementCert(ctx, certId, "the extender is not active", server.NowUtc())
		for range 2 {
			withdrawn := answer("")
			connect.AssertEqual(t, withdrawn.State, model.NetworkExtenderMeasurementStateWithdrawn)
			if !slices.Equal(withdrawn.Names, []string{name}) {
				t.Fatalf("withdrawn names = %v", withdrawn.Names)
			}
			connect.AssertEqual(t, withdrawn.ChainPem, "")
			connect.AssertEqual(t, withdrawn.Reason, "the extender is not active")
			connect.AssertEqual(t, withdrawn.KeySha256, keySha256)
		}

		// withdrawn holds nothing, so the extender's next activation carries
		// its request again, and it waits for a certificate once more
		ask()
		connect.AssertEqual(t, answer("").State, model.NetworkExtenderMeasurementStatePending)
	})
}

// An owner's opt-out rides the activation (lag.dev/PLAN.md C4, C9, decision
// 18): the extender's live certificate is withdrawn, A8 answers withdrawn for
// its name and key, and its certificate request is dropped. While the flag
// keeps coming a request carried beside it is not stored, and an activation
// with neither the flag nor a request changes nothing. A later activation
// with a request and without the flag puts it back in the queue.
//
// The activations offer the tcp carrier alone and split between the two
// loopback families, so each family's subnet stays under the fixture
// extender's admission limit of eight actions a minute.
func TestExtenderActivateOptsOutOfMeasurement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderMeasurementConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		activate := func(loopbackIp string, csr []byte, measurementDisabled bool) *ExtenderActivateResult {
			t.Helper()
			clientSession.ClientAddress = fixture.clientAddress(loopbackIp)
			args := fixture.activateArgs()
			args.Carriers = []string{connect.ExtenderCarrierTcp}
			if csr != nil {
				args.MeasurementCsr = base64.StdEncoding.EncodeToString(csr)
			}
			args.MeasurementDisabled = measurementDisabled
			result, err := ExtenderActivate(args, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Activated {
				t.Fatalf("the activation was refused: %s", result.Error)
			}
			return result
		}
		poll := func() *ExtenderMeasurementCertResult {
			t.Helper()
			result, err := ExtenderMeasurementCert(&ExtenderMeasurementCertArgs{}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		first := activate("127.0.0.1", nil, false)
		extenderId := *first.ExtenderId
		name := ExtenderMeasurementName(extenderId, testMeasurementZone)
		key := testMeasurementKey(t)
		csr := testMeasurementCsr(t, key, &x509.CertificateRequest{DNSNames: []string{name}})
		request, err := ParseExtenderMeasurementCsrDer(csr, extenderId, testMeasurementZone)
		if err != nil {
			t.Fatal(err)
		}
		keyDigest := sha256.Sum256(request.PublicKey)
		keySha256 := hex.EncodeToString(keyDigest[:])

		// the extender holds a live certificate for its name
		now := server.NowUtc()
		model.SetNetworkExtenderMeasurementRequest(ctx, extenderId, request.Csr, request.PublicKey, []string{name}, now)
		certId := server.NewId()
		model.AddNetworkExtenderMeasurementCert(ctx, &model.NetworkExtenderMeasurementCert{
			CertId:     certId,
			ExtenderId: extenderId,
			Names:      []string{name},
			Csr:        request.Csr,
			PublicKey:  request.PublicKey,
			ChainPem:   "-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n",
			SerialHex:  "1",
			NotBefore:  now,
			NotAfter:   now.Add(90 * 24 * time.Hour),
			IssueTime:  now,
			CapCounted: true,
			RenewTime:  now.Add(60 * 24 * time.Hour),
		})
		connect.AssertEqual(t, poll().State, model.NetworkExtenderMeasurementStateIssued)

		// the owner opts out
		activate("127.0.0.1", nil, true)
		withdrawn := poll()
		connect.AssertEqual(t, withdrawn.State, model.NetworkExtenderMeasurementStateWithdrawn)
		if !slices.Equal(withdrawn.Names, []string{name}) {
			t.Fatalf("withdrawn names = %v", withdrawn.Names)
		}
		connect.AssertEqual(t, withdrawn.KeySha256, keySha256)
		connect.AssertEqual(t, withdrawn.Reason, ExtenderMeasurementOptOutReason)
		connect.AssertEqual(t, withdrawn.ChainPem, "")
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		if certs[0].WithdrawTime == nil {
			t.Fatalf("the certificate was not withdrawn")
		}
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).Csr), 0)

		// the flag keeps coming: a request beside it is not stored
		activate("::1", csr, true)
		connect.AssertEqual(t, poll().State, model.NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).Csr), 0)
		connect.AssertEqual(t, len(model.GetPendingNetworkExtenderMeasurements(ctx, server.NowUtc(), 10)), 0)

		// neither the flag nor a request: neither an opt-out nor a re-enable
		activate("::1", nil, false)
		connect.AssertEqual(t, poll().State, model.NetworkExtenderMeasurementStateWithdrawn)

		// a request without the flag puts the extender back in the queue
		activate("::1", csr, false)
		pending := poll()
		connect.AssertEqual(t, pending.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, pending.KeySha256, keySha256)
		connect.AssertEqual(t, len(model.GetPendingNetworkExtenderMeasurements(ctx, server.NowUtc(), 10)), 1)
	})
}
