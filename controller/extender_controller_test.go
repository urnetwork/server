package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Extender activation end to end (connect/EXTENDER.md C2).
//
// Every activation here runs the real probe against the in-process extender of
// the fixture: the carrier dials, the challenge signatures and the forwarded
// `GET /hello` are all genuine, so a change that breaks the probe path fails
// these rather than passing on a stub.

// One client session arriving from the given address, backed by a real network
// and client so the activation attributes to something that exists.
func newTestExtenderSession(
	t testing.TB,
	ctx context.Context,
	clientAddress string,
) *session.ClientSession {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	deviceId := server.NewId()
	clientId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("extender-test-%s", networkId), userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "extender", "extender-test")

	clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
		DeviceId:  &deviceId,
		ClientId:  &clientId,
	})
	clientSession.ClientAddress = clientAddress
	return clientSession
}

// Decodes and verifies one record from an activation answer, returning the
// body that was signed.
func verifyTestExtenderRecord(
	t testing.TB,
	rootPublicKey ed25519.PublicKey,
	recordBase64 string,
) *protocol.ExtenderRecordBody {
	t.Helper()
	recordBytes, err := base64.StdEncoding.DecodeString(recordBase64)
	if err != nil {
		t.Fatalf("the record is not base64: %v", err)
	}
	record := &protocol.ExtenderRecord{}
	if err := proto.Unmarshal(recordBytes, record); err != nil {
		t.Fatalf("the record is not an ExtenderRecord: %v", err)
	}
	body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(record)
	if err != nil {
		t.Fatalf("the record does not verify under the configured root key: %v", err)
	}
	return body
}

// An activation over each loopback family stores the extender and its family
// address, and answers with a record that verifies under the configured root
// key and names exactly what was stored.
func TestExtenderActivateStoresAndSignsPerFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		cases := []struct {
			loopbackIp string
			ipVersion  int
		}{
			{loopbackIp: "127.0.0.1", ipVersion: 4},
			{loopbackIp: "::1", ipVersion: 6},
		}
		for _, c := range cases {
			fixture := newTestExtenderFixture(t)
			rootPrivateKey := installTestExtenderConfig(t, fixture.api)
			rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

			clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress(c.loopbackIp))
			result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
			if err != nil {
				t.Fatalf("%s: %v", c.loopbackIp, err)
			}
			if !result.Activated {
				t.Fatalf("%s was refused: %s", c.loopbackIp, result.Error)
			}

			connect.AssertEqual(t, result.Ip, c.loopbackIp)
			connect.AssertEqual(t, result.IpVersion, c.ipVersion)
			connect.AssertEqual(t, result.Error, "")
			if !slices.Equal(result.Carriers, []string{"tcp", "quic", "dns"}) {
				t.Fatalf("%s carriers = %v", c.loopbackIp, result.Carriers)
			}
			if !slices.Equal(result.AllowedHosts, []string{
				testExtenderNetworkHost,
				"*." + testExtenderNetworkHost,
				testExtenderApiHost,
				"*." + testExtenderApiHost,
			}) {
				t.Fatalf("%s allowed hosts = %v", c.loopbackIp, result.AllowedHosts)
			}

			body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
			connect.AssertEqual(t, hex.EncodeToString(body.PublicKey), hex.EncodeToString(fixture.publicKey))
			connect.AssertEqual(t, body.NetworkHost, testExtenderNetworkHost)
			connect.AssertEqual(t, body.DnsTld, testExtenderDnsTld)
			connect.AssertEqual(t, int(body.TcpPort), fixture.tcpPort)
			connect.AssertEqual(t, int(body.UdpPort), fixture.quicPort)
			connect.AssertEqual(t, int(body.DnsPort), fixture.dnsPort)
			connect.AssertEqual(t, len(body.Addresses), 1)
			connect.AssertEqual(t, body.Addresses[0].Ip, c.loopbackIp)
			connect.AssertEqual(t, int(body.Addresses[0].IpVersion), c.ipVersion)
			if !slices.Equal(body.Addresses[0].Carriers, []string{"tcp", "quic", "dns"}) {
				t.Fatalf("%s record carriers = %v", c.loopbackIp, body.Addresses[0].Carriers)
			}
			// a day out, to the minute (connect/GEOMAP.md §2.8)
			expireTime := time.UnixMilli(int64(body.ExpireTimeMs)).UTC()
			issueTime := time.UnixMilli(int64(body.IssueTimeMs)).UTC()
			connect.AssertEqual(
				t,
				expireTime.Sub(issueTime).Round(time.Minute),
				ExtenderRecordExpireTimeout,
			)
			connect.AssertEqual(t, result.ExpireTime.UTC().Round(time.Second), expireTime.Round(time.Second))

			// the stored rows agree with the record
			stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
			connect.AssertEqual(t, stored.Extender.Active, true)
			connect.AssertEqual(t, stored.Extender.NetworkId, clientSession.ByJwt.NetworkId)
			connect.AssertEqual(t, stored.Extender.ClientId, *clientSession.ByJwt.ClientId)
			connect.AssertEqual(t, len(stored.Addresses), 1)
			connect.AssertEqual(t, stored.Addresses[0].IpVersion, c.ipVersion)
			connect.AssertEqual(t, stored.Addresses[0].Ip.String(), c.loopbackIp)
		}
	})
}

// Every activation appends a history row with where it came from (M1): the
// family, the privacy-preserving hash of the activating address, and the
// location it resolved to -- none for loopback, which the mmdb cannot place.
// A second activation is a second row, not an overwrite.
func TestExtenderActivateRecordsTheActivationHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		for i := range 2 {
			result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
			if err != nil {
				t.Fatalf("activation %d: %v", i, err)
			}
			if !result.Activated {
				t.Fatalf("activation %d was refused: %s", i, result.Error)
			}
		}

		extenderId := testExtenderIdForKey(ctx, t, fixture.publicKey)
		activations := model.GetNetworkExtenderActivations(ctx, extenderId, time.Time{})
		connect.AssertEqual(t, len(activations), 2)
		expectedHash, err := server.ClientIpHash("127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		for _, activation := range activations {
			connect.AssertEqual(t, activation.ExtenderId, extenderId)
			connect.AssertEqual(t, activation.IpVersion, 4)
			if !slices.Equal(activation.ClientAddressHash, expectedHash[:]) {
				t.Fatalf("activation hash = %x, expected the hash of the loopback address", activation.ClientAddressHash)
			}
			connect.AssertEqual(t, activation.CountryCode, "")
			if activation.LocationId != nil || activation.CityLocationId != nil ||
				activation.RegionLocationId != nil || activation.CountryLocationId != nil {
				t.Fatalf("loopback resolved to a location: %+v", activation)
			}
			// and a lookup that placed nothing has no accuracy radius either:
			// the radius is stored with the location it qualifies, never alone
			if activation.AccuracyKm != nil {
				t.Fatalf("loopback stored an accuracy radius of %f km", *activation.AccuracyKm)
			}
			if activation.ActivateTime.IsZero() {
				t.Fatal("the activation carries no time")
			}
		}
		if activations[1].ActivateTime.Before(activations[0].ActivateTime) {
			t.Fatal("the history is not in activation order")
		}
	})
}

// The extender id of one identity key, read back through the sample reader so
// the test never needs the id the handler kept to itself.
func testExtenderIdForKey(
	ctx context.Context,
	t testing.TB,
	publicKey ed25519.PublicKey,
) server.Id {
	t.Helper()
	for _, entry := range model.GetRandomActiveNetworkExtenders(ctx, 64, server.Id{}) {
		if string(entry.Extender.PublicKey) == string(publicKey) {
			return entry.Extender.ExtenderId
		}
	}
	t.Fatalf("no extender was stored for the fixture key")
	return server.Id{}
}

// One extender activating on both families is one directory entry, and the
// second record lists both addresses. An app that got only the first record
// would never learn the other family.
func TestExtenderActivateAddsTheSecondFamilyToOneRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		firstSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		first, err := ExtenderActivate(fixture.activateArgs(), firstSession)
		if err != nil {
			t.Fatal(err)
		}
		if !first.Activated {
			t.Fatalf("the v4 activation was refused: %s", first.Error)
		}
		firstBody := verifyTestExtenderRecord(t, rootPublicKey, first.Record)
		connect.AssertEqual(t, len(firstBody.Addresses), 1)

		secondSession := newTestExtenderSession(t, ctx, fixture.clientAddress("::1"))
		second, err := ExtenderActivate(fixture.activateArgs(), secondSession)
		if err != nil {
			t.Fatal(err)
		}
		if !second.Activated {
			t.Fatalf("the v6 activation was refused: %s", second.Error)
		}
		secondBody := verifyTestExtenderRecord(t, rootPublicKey, second.Record)
		connect.AssertEqual(t, len(secondBody.Addresses), 2)
		connect.AssertEqual(t, secondBody.Addresses[0].Ip, "127.0.0.1")
		connect.AssertEqual(t, secondBody.Addresses[1].Ip, "::1")
		// the newer record must win over the older one in a directory (B5)
		connect.AssertEqual(t, firstBody.IssueTimeMs <= secondBody.IssueTimeMs, true)

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		connect.AssertEqual(t, len(stored.Addresses), 2)
	})
}

// The answer carries a bounded random sample of OTHER active extenders, signed
// under the same root key, which is the whole directory an app has before it
// has any peer (D6).
func TestExtenderActivateReturnsBootstrapRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		model.Testing_CreateNetworkExtenderPopulation(ctx, server.NewId(), server.NewId(), 20)

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}

		connect.AssertEqual(t, len(result.Bootstrap), ExtenderBootstrapCount)
		seen := map[string]bool{}
		for _, recordBase64 := range result.Bootstrap {
			body := verifyTestExtenderRecord(t, rootPublicKey, recordBase64)
			publicKeyHex := hex.EncodeToString(body.PublicKey)
			if publicKeyHex == hex.EncodeToString(fixture.publicKey) {
				t.Fatal("the bootstrap sample contains the extender that asked for it")
			}
			if seen[publicKeyHex] {
				t.Fatal("the bootstrap sample repeats an extender")
			}
			seen[publicKeyHex] = true
			connect.AssertEqual(t, body.NetworkHost, testExtenderNetworkHost)
			connect.AssertEqual(t, len(body.Addresses), 1)
		}
	})
}

// A key the extender cannot sign with is refused, and nothing is stored. This
// is what stops one host from activating an address it does not control.
func TestExtenderActivateRefusesAWrongPublicKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)

		otherSeed, err := connect.NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		otherPublicKey, err := connect.ExtenderPublicKeyFromSeed(otherSeed)
		if err != nil {
			t.Fatal(err)
		}

		args := fixture.activateArgs()
		args.PublicKeyHex = hex.EncodeToString(otherPublicKey)
		args.Carriers = []string{connect.ExtenderCarrierTcp}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if !strings.Contains(result.Error, "tcp carrier") {
			t.Fatalf("error = %q, want the tcp carrier named", result.Error)
		}
		connect.AssertEqual(t, len(model.GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})), 0)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// A carrier that is not bound fails the whole activation with that carrier
// named, and stores nothing: a record must not promise a carrier the extender
// does not answer on.
//
// The refusal costs the probe budget, because a udp carrier with nothing bound
// gives no answer to fail fast on -- which is exactly why the budget exists.
func TestExtenderActivateRefusesACarrierThatIsNotServed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)

		// a udp port nothing is bound to
		closedConn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := closedConn.LocalAddr().(*net.UDPAddr).Port
		closedConn.Close()

		args := fixture.activateArgs()
		args.UdpPort = closedPort
		args.Carriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if !strings.Contains(result.Error, "quic carrier") {
			t.Fatalf("error = %q, want the quic carrier named", result.Error)
		}
		connect.AssertEqual(t, len(model.GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})), 0)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// A forward that reaches the api on the other family is refused: the record
// would publish an address on a family the extender does not actually egress
// on, and every client that used it would get the wrong one (A7).
func TestExtenderActivateRefusesAForwardOnTheOtherFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		fixture.api.setClientAddressOverride("[2001:db8::1]:443")

		args := fixture.activateArgs()
		args.Carriers = []string{connect.ExtenderCarrierTcp}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if !strings.Contains(result.Error, "ipv6") || !strings.Contains(result.Error, "ipv4") {
			t.Fatalf("error = %q, want both families named", result.Error)
		}
		connect.AssertEqual(t, len(model.GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})), 0)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// Without a root key there is nothing to sign a record with, so the activation
// refuses before it probes and stores nothing. An operator that has not set up
// an extender network must not half-activate anyone.
func TestExtenderActivateRefusesWithoutARootKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfigYaml(t, strings.Join([]string{
			"network_host: " + testExtenderNetworkHost,
			"api_url: https://" + testExtenderApiHost,
		}, "\n"))

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if !strings.Contains(result.Error, "root key") {
			t.Fatalf("error = %q, want the missing root key named", result.Error)
		}
		connect.AssertEqual(t, len(model.GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})), 0)
	})
}

// The seventh activation in an hour is refused. The budget is spent by every
// attempt that got past argument validation, which is what stops a client from
// using the api as a probe engine against an address of its choosing.
func TestExtenderActivateRateLimitsPerClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)

		// a closed tcp port on the caller's own address, so every attempt is
		// refused at once and the test spends no probe budget waiting
		closedListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := closedListener.Addr().(*net.TCPAddr).Port
		closedListener.Close()

		args := fixture.activateArgs()
		args.TcpPort = closedPort
		args.Carriers = []string{connect.ExtenderCarrierTcp}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		for i := range ExtenderActivateRateLimit {
			result, err := ExtenderActivate(args, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, result.Activated, false)
			if strings.Contains(result.Error, "attempt") {
				t.Fatalf("attempt %d was rate limited early: %s", i+1, result.Error)
			}
		}

		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if !strings.Contains(result.Error, "attempt") {
			t.Fatalf("the seventh attempt was not rate limited: %q", result.Error)
		}
	})
}

// Argument validation refuses before anything is probed or stored, and before
// any rate limit budget is spent, so a malformed request costs nothing.
func TestExtenderActivateValidatesArguments(t *testing.T) {
	publicKeyHex := hex.EncodeToString(make([]byte, ed25519.PublicKeySize))
	cases := []struct {
		name      string
		args      *ExtenderActivateArgs
		wantError string
	}{
		{
			name:      "no public key",
			args:      &ExtenderActivateArgs{Carriers: []string{"tcp"}},
			wantError: "public key",
		},
		{
			name:      "public key of the wrong length",
			args:      &ExtenderActivateArgs{PublicKeyHex: "aabb", Carriers: []string{"tcp"}},
			wantError: "public key",
		},
		{
			name:      "no carrier",
			args:      &ExtenderActivateArgs{PublicKeyHex: publicKeyHex},
			wantError: "at least one carrier",
		},
		{
			name:      "unknown carrier",
			args:      &ExtenderActivateArgs{PublicKeyHex: publicKeyHex, Carriers: []string{"tcp", "sctp"}},
			wantError: "unknown carrier",
		},
		{
			name:      "no tcp carrier",
			args:      &ExtenderActivateArgs{PublicKeyHex: publicKeyHex, Carriers: []string{"quic"}},
			wantError: "tcp carrier",
		},
		{
			name: "port out of range",
			args: &ExtenderActivateArgs{
				PublicKeyHex: publicKeyHex,
				Carriers:     []string{"tcp"},
				TcpPort:      70000,
			},
			wantError: "tcp_port",
		},
		{
			name: "udp port out of range",
			args: &ExtenderActivateArgs{
				PublicKeyHex: publicKeyHex,
				Carriers:     []string{"tcp"},
				UdpPort:      -1,
			},
			wantError: "udp_port",
		},
		{
			name: "dns port out of range",
			args: &ExtenderActivateArgs{
				PublicKeyHex: publicKeyHex,
				Carriers:     []string{"tcp"},
				DnsPort:      65536,
			},
			wantError: "dns_port",
		},
		{
			name: "a dns port in the list out of range",
			args: &ExtenderActivateArgs{
				PublicKeyHex: publicKeyHex,
				Carriers:     []string{"tcp"},
				DnsPorts:     []int{4053, 65536},
			},
			wantError: "dns_ports",
		},
		{
			name: "dns tld too long",
			args: &ExtenderActivateArgs{
				PublicKeyHex: publicKeyHex,
				Carriers:     []string{"tcp"},
				DnsTld:       strings.Repeat("x", 200),
			},
			wantError: "dns_tld",
		},
	}
	for _, c := range cases {
		clientSession := session.Testing_CreateClientSession(context.Background(), &jwt.ByJwt{})
		clientSession.ClientAddress = "127.0.0.1:54321"
		result, err := ExtenderActivate(c.args, clientSession)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if result.Activated {
			t.Errorf("%s: the activation was accepted", c.name)
			continue
		}
		if !strings.Contains(result.Error, c.wantError) {
			t.Errorf("%s: error = %q, want it to mention %q", c.name, result.Error, c.wantError)
		}
	}
}

// Zero ports and an empty tld take the defaults, which is what a provider that
// configured nothing sends. The record is what every client dials from, so a
// default that was dropped rather than filled in would publish an extender on
// port zero.
//
// The dns default is the whodis port, which every extender binds, rather than
// 53, which only the platforms that can bind it without privilege offer (L2):
// an old reader dials `DnsPort` alone, and 53 would send it to a port this
// extender never bound.
//
// The tcp port cannot be defaulted in a test, since 443 is not bindable here;
// the three ports share one defaulting and one range check, and the udp and dns
// ports prove both.
func TestExtenderActivateDefaultsThePortsAndTld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		args.UdpPort = 0
		args.DnsPort = 0
		args.DnsTld = ""
		// only the carrier whose port is given, so the defaulted ports are
		// stored without being dialed
		args.Carriers = []string{connect.ExtenderCarrierTcp}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}

		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, int(body.TcpPort), fixture.tcpPort)
		connect.AssertEqual(t, int(body.UdpPort), 443)
		connect.AssertEqual(t, int(body.DnsPort), connect.ExtenderDnsPort)
		connect.AssertEqual(t, body.DnsTld, connect.DefaultExtenderDnsTld)
		// the dns carrier was not offered, so no port was probed and the record
		// promises none (L2)
		connect.AssertEqual(t, len(result.DnsPorts), 0)
		connect.AssertEqual(t, len(body.DnsPorts), 0)

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		connect.AssertEqual(t, stored.Extender.UdpPort, 443)
		connect.AssertEqual(t, stored.Extender.DnsPort, connect.ExtenderDnsPort)
		connect.AssertEqual(t, stored.Extender.DnsTld, connect.DefaultExtenderDnsTld)
		connect.AssertEqual(t, len(stored.Addresses[0].DnsPorts), 0)
	})
}

// The carrier list is normalized before anything is probed or stored: case and
// surrounding space are the caller's, the order is the caller's, and one
// carrier is dialed once however many times it was named. A duplicate that
// survived would spend the shared probe budget twice on the same dial.
func TestExtenderActivateNormalizesTheCarrierList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		args.Carriers = []string{" QUIC ", "quic", "", "  ", "Tcp", "TCP"}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		if !slices.Equal(result.Carriers, []string{"quic", "tcp"}) {
			t.Fatalf("carriers = %v, want the caller's order, deduplicated", result.Carriers)
		}

		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, len(body.Addresses), 1)
		if !slices.Equal(body.Addresses[0].Carriers, []string{"quic", "tcp"}) {
			t.Fatalf("record carriers = %v", body.Addresses[0].Carriers)
		}

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		if !slices.Equal(stored.Addresses[0].Carriers, []string{"quic", "tcp"}) {
			t.Fatalf("stored carriers = %v", stored.Addresses[0].Carriers)
		}
	})
}

// An activation is attributed to a network and a client and is probed back at
// the caller's own address, so a session without either is refused before any
// configuration is read, any budget is spent and any dial is made.
func TestExtenderActivateRefusesACallerItCannotAttribute(t *testing.T) {
	publicKeyHex := hex.EncodeToString(make([]byte, ed25519.PublicKeySize))
	validArgs := func() *ExtenderActivateArgs {
		return &ExtenderActivateArgs{
			PublicKeyHex: publicKeyHex,
			Carriers:     []string{connect.ExtenderCarrierTcp},
		}
	}
	cases := []struct {
		name          string
		byJwt         *jwt.ByJwt
		clientAddress string
		wantError     string
	}{
		{
			name:          "no client jwt at all",
			byJwt:         nil,
			clientAddress: "127.0.0.1:54321",
			wantError:     "requires a client",
		},
		{
			name:          "a jwt with no client",
			byJwt:         &jwt.ByJwt{},
			clientAddress: "127.0.0.1:54321",
			wantError:     "requires a client",
		},
		{
			name:          "an address that is not an address",
			byJwt:         &jwt.ByJwt{ClientId: &server.Id{}},
			clientAddress: "not-an-address.example:54321",
			wantError:     "not readable",
		},
		{
			name:          "no address at all",
			byJwt:         &jwt.ByJwt{ClientId: &server.Id{}},
			clientAddress: "",
			wantError:     "not readable",
		},
	}
	for _, c := range cases {
		clientSession := session.Testing_CreateClientSession(context.Background(), c.byJwt)
		clientSession.ClientAddress = c.clientAddress
		result, err := ExtenderActivate(validArgs(), clientSession)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if result.Activated {
			t.Errorf("%s: the activation was accepted", c.name)
			continue
		}
		if !strings.Contains(result.Error, c.wantError) {
			t.Errorf("%s: error = %q, want it to mention %q", c.name, result.Error, c.wantError)
		}
	}
}

// The country of an activation is the geolocation of the caller's address, and
// a lookup that finds nothing leaves it empty rather than refusing a reachable
// extender. A loopback caller has no geolocation, so this is exactly that case:
// what it must not do is cost an operator its extender over a lookup.
func TestExtenderActivateSurvivesAGeolocationThatFindsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		args.Carriers = []string{connect.ExtenderCarrierTcp}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		connect.AssertEqual(t, stored.Extender.CountryCode, "")
		// and the record carries the same country the row does, since both come
		// out of the one signing transaction
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, body.CountryCode, stored.Extender.CountryCode)
	})
}

// The dns ports of a record body as ints, which is the form a test compares
// against the fixture's own ports.
func testRecordDnsPorts(body *protocol.ExtenderRecordBody) []int {
	dnsPorts := []int{}
	for _, dnsPort := range body.DnsPorts {
		dnsPorts = append(dnsPorts, int(dnsPort))
	}
	return dnsPorts
}

// Every listed dns port is probed on its own, and the ones that answer are what
// the address row, the answer and the record carry, ascending (L2). The order
// is the operator's rather than the caller's, because it is the order a client
// dials them in: 53 before the whodis port when both are served.
func TestExtenderActivateProbesEveryDnsPortAndRecordsThemAscending(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixtureWithDnsPorts(t, 2)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		// the caller's order reversed and one repeat: each port is probed once
		// and the list that comes back is ascending
		args.DnsPorts = []int{fixture.dnsPorts[1], fixture.dnsPorts[0], fixture.dnsPorts[1]}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		if !slices.Equal(result.DnsPorts, fixture.dnsPorts) {
			t.Fatalf("dns ports = %v, want %v", result.DnsPorts, fixture.dnsPorts)
		}

		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		if !slices.Equal(testRecordDnsPorts(body), fixture.dnsPorts) {
			t.Fatalf("record dns ports = %v, want %v", testRecordDnsPorts(body), fixture.dnsPorts)
		}
		// the configured port stays what it was, since a reader that predates
		// the list dials it alone
		connect.AssertEqual(t, int(body.DnsPort), fixture.dnsPort)

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		connect.AssertEqual(t, len(stored.Addresses), 1)
		if !slices.Equal(stored.Addresses[0].DnsPorts, fixture.dnsPorts) {
			t.Fatalf("stored dns ports = %v, want %v", stored.Addresses[0].DnsPorts, fixture.dnsPorts)
		}
	})
}

// A dns port that does not answer costs its own budget and nothing else: the
// extender still activates on the ports that do, and the record names only
// those. This is the ordinary case of an extender that binds 53 on a path that
// blocks it.
func TestExtenderActivateRecordsOnlyTheDnsPortsThatAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		// the dead port leads, so the served port and the forward behind it are
		// reached only if the dead one is bounded on its own
		args.DnsPorts = []int{testClosedUdpPortBelow(t, fixture.dnsPort), fixture.dnsPort}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		if !slices.Equal(result.DnsPorts, []int{fixture.dnsPort}) {
			t.Fatalf("dns ports = %v, want only the served port %d", result.DnsPorts, fixture.dnsPort)
		}

		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		if !slices.Equal(testRecordDnsPorts(body), []int{fixture.dnsPort}) {
			t.Fatalf("record dns ports = %v, want only %d", testRecordDnsPorts(body), fixture.dnsPort)
		}

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		if !slices.Equal(stored.Addresses[0].DnsPorts, []int{fixture.dnsPort}) {
			t.Fatalf("stored dns ports = %v, want only %d", stored.Addresses[0].DnsPorts, fixture.dnsPort)
		}
	})
}

// A dns carrier with no port left is the carrier failing, and it is refused
// exactly as an unserved carrier is, with nothing stored: a record must not
// promise a dns carrier that answers on no port at all.
func TestExtenderActivateRefusesWhenNoDnsPortAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)

		firstDeadPort := testClosedUdpPortBelow(t, fixture.dnsPort)
		args := fixture.activateArgs()
		args.Carriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns}
		args.DnsPorts = []int{firstDeadPort, testClosedUdpPortBelow(t, firstDeadPort)}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if !strings.Contains(result.Error, "dns carrier") {
			t.Fatalf("error = %q, want the dns carrier named", result.Error)
		}
		connect.AssertEqual(t, len(model.GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})), 0)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// An activation that lists no dns ports offers the one configured port, which
// is what an extender that predates the list sends. The record then names that
// port in both fields, so a reader of either finds the same carrier.
func TestExtenderActivateDefaultsTheDnsPortsToTheConfiguredPort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		args.DnsPorts = nil

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		if !slices.Equal(result.DnsPorts, []int{fixture.dnsPort}) {
			t.Fatalf("dns ports = %v, want the configured port %d", result.DnsPorts, fixture.dnsPort)
		}

		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, int(body.DnsPort), fixture.dnsPort)
		if !slices.Equal(testRecordDnsPorts(body), []int{fixture.dnsPort}) {
			t.Fatalf("record dns ports = %v, want %d", testRecordDnsPorts(body), fixture.dnsPort)
		}

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		if !slices.Equal(stored.Addresses[0].DnsPorts, []int{fixture.dnsPort}) {
			t.Fatalf("stored dns ports = %v, want %d", stored.Addresses[0].DnsPorts, fixture.dnsPort)
		}
	})
}

// A bootstrap record carries the dns ports too, unioned over the families of
// the extender it describes: the sample is the whole directory an app has
// before it has any peer, so a port missing here is a carrier it never dials.
func TestExtenderActivateBootstrapRecordsCarryTheDnsPorts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		// one other active extender whose families answered on different port
		// sets, which is what a host that binds 53 on one family only looks like
		otherPublicKey := []byte("extender-public-key-bootstrap-001")
		otherCreateTime := server.NowUtc()
		model.Testing_CreateNetworkExtender(
			ctx,
			&model.NetworkExtender{
				ExtenderId:  server.NewId(),
				NetworkId:   server.NewId(),
				ClientId:    server.NewId(),
				PublicKey:   otherPublicKey,
				CreateTime:  otherCreateTime,
				TcpPort:     443,
				UdpPort:     443,
				DnsPort:     connect.DefaultWhodisPort,
				DnsTld:      connect.DefaultExtenderDnsTld,
				CountryCode: "US",
				Active:      true,
			},
			[]*model.NetworkExtenderAddress{
				{
					IpVersion:    4,
					Ip:           netip.MustParseAddr("192.0.2.20"),
					Carriers:     []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns},
					DnsPorts:     []int{connect.DefaultWhodisPort},
					ActivateTime: otherCreateTime,
					Active:       true,
				},
				{
					IpVersion:    6,
					Ip:           netip.MustParseAddr("2001:db8::20"),
					Carriers:     []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns},
					DnsPorts:     []int{connect.DefaultDnsPort, connect.DefaultWhodisPort},
					ActivateTime: otherCreateTime,
					Active:       true,
				},
			},
		)

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		connect.AssertEqual(t, len(result.Bootstrap), 1)

		body := verifyTestExtenderRecord(t, rootPublicKey, result.Bootstrap[0])
		connect.AssertEqual(t, hex.EncodeToString(body.PublicKey), hex.EncodeToString(otherPublicKey))
		wantDnsPorts := []int{connect.DefaultDnsPort, connect.DefaultWhodisPort}
		if !slices.Equal(testRecordDnsPorts(body), wantDnsPorts) {
			t.Fatalf("bootstrap dns ports = %v, want %v", testRecordDnsPorts(body), wantDnsPorts)
		}
	})
}

// The record carries the continent of its country, from the same mapping the
// geo dns sets use (connect/DESIGNNOTES4.md §2), and lives a day
// (connect/GEOMAP.md §2.8, D19). Signing reads no database, so this runs
// anywhere.
func TestSignExtenderRecordStampsTheContinentAndTheDay(t *testing.T) {
	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := &ExtenderConfig{
		NetworkHost: testExtenderNetworkHost,
	}
	issueTime := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		countryCode   string
		continentCode string
	}{
		{countryCode: "us", continentCode: "NA"},
		{countryCode: "DE", continentCode: "EU"},
		{countryCode: "jp", continentCode: "AS"},
		{countryCode: "br", continentCode: "SA"},
		// an extender the operator could not place has no continent, which a
		// client sorts last rather than guessing
		{countryCode: "", continentCode: ""},
		{countryCode: "zz", continentCode: ""},
	} {
		extender := &model.NetworkExtender{
			ExtenderId:  server.NewId(),
			PublicKey:   rootPublicKey,
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     connect.ExtenderDnsPort,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: test.countryCode,
		}
		addresses := []*model.NetworkExtenderAddress{
			{
				IpVersion: 4,
				Ip:        netip.MustParseAddr("192.0.2.7"),
				Carriers:  []string{connect.ExtenderCarrierTcp},
			},
		}
		record, _, err := SignExtenderRecord(config, rootPrivateKey, extender, addresses, issueTime)
		if err != nil {
			t.Fatal(err)
		}
		body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, body.CountryCode, test.countryCode)
		connect.AssertEqual(t, body.ContinentCode, test.continentCode)
		connect.AssertEqual(t, body.ContinentCode, model.ContinentCodeForCountry(test.countryCode))
		connect.AssertEqual(t, int64(body.IssueTimeMs), issueTime.UnixMilli())
		connect.AssertEqual(t, int64(body.ExpireTimeMs), issueTime.Add(24*time.Hour).UnixMilli())
	}
	connect.AssertEqual(t, ExtenderRecordExpireTimeout, 24*time.Hour)
}
