package providertunnel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/localclient"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
	"golang.org/x/net/dns/dnsmessage"
)

type countedProbeAuthority struct {
	*localclient.Authority
	mint, control, retire atomic.Int64
	// Used only by the deliberately missing-retirement-hook RED overlay.
	rejectingApiUrl string
}

func (self *countedProbeAuthority) AuthNetworkClient(ctx context.Context, args *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
	self.mint.Add(1)
	return self.Authority.AuthNetworkClient(ctx, args)
}
func (self *countedProbeAuthority) ConnectControl(ctx context.Context, token string, args *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	self.control.Add(1)
	return self.Authority.ConnectControl(ctx, token, args)
}
func (self *countedProbeAuthority) RemoveNetworkClient(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
	self.retire.Add(1)
	return self.Authority.RemoveNetworkClient(ctx, args)
}

// The control API refuses every request. The platform exchange, provider DoH
// origin and provider HTTPS payload are separate real listeners. No dial,
// registration, client generator or contract callback is substituted.
func TestProviderTunnelActualLocalAuthorityWithoutApi(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		var apiCalls, dnsCalls, payloadCalls atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apiCalls.Add(1); http.Error(w, "API disabled", 503) }))
		defer trap.Close()
		tb.Cleanup(func() { fmt.Printf("actual rejected API boundary attempts=%d\n", apiCalls.Load()) })
		seed := func(label string) (server.Id, server.Id, string) {
			n, u, d, c := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, n, label, u)
			model.Testing_CreateDevice(ctx, n, d, c, label, label)
			code, err := model.CreateBalanceCode(ctx, model.ByteCount(8<<30), 24*time.Hour, 0, label+server.NewId().String(), "", "")
			if err != nil {
				tb.Fatal(err)
			}
			redeemed, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: n}, ctx)
			if err != nil || redeemed.Error != nil {
				tb.Fatal("balance redemption", err)
			}
			return n, c, session.NewByJwt(n, u, label, false, false).Client(d, c).Testing_Sign()
		}
		_, providerClientId, providerJwt := seed("local-provider")
		sourceNetworkId, sourceClientId, sourceJwt := seed("local-probe")
		pa, err := localclient.New(ctx, providerJwt, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer pa.Close()
		sa, err := localclient.New(ctx, sourceJwt, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer sa.Close()
		counted := &countedProbeAuthority{Authority: sa, rejectingApiUrl: trap.URL}
		host := "localquality"
		exchange := connectserver.NewExchange(ctx, host, "connect", "test", map[int]int{7300: 7300}, map[string]string{host: "127.0.0.1"}, connectserver.DefaultExchangeSettings())
		hs := connectserver.DefaultConnectHandlerSettings()
		hs.ListenH3Port = 0
		hs.ListenDnsPort = 0
		hs.ConnectionAnnounceTimeout = 0
		handler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, hs)
		platform := httptest.NewServer(router.NewRouter(ctx, []*router.Route{router.NewRoute("GET", "/", handler.Connect)}))
		defer platform.Close()
		defer func() { handler.Close(); exchange.Close() }()
		settings := connect.DefaultClientStrategySettings()
		settings.EnableResilient = false
		strategy := connect.NewClientStrategy(ctx, settings)
		defer strategy.Close()
		oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, providerJwt, trap.URL, pa)
		provider := connect.NewClient(ctx, connect.Id(providerClientId), oob, connect.DefaultClientSettings())
		transport := connect.NewPlatformTransportWithDefaults(provider.Ctx(), strategy, provider.RouteManager(), "ws"+platform.URL[4:], &connect.ClientAuth{ByJwt: providerJwt, InstanceId: connect.NewId(), AppVersion: server.RequireVersion()})
		localNat := connect.NewLocalUserNatWithDefaults(provider.Ctx(), providerClientId.String())
		natSettings := connect.DefaultRemoteUserNatProviderSettings()
		natSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
		nat := connect.NewRemoteUserNatProvider(provider, localNat, natSettings)
		closeProvider := func() {
			nat.Close()
			join, jc := context.WithTimeout(context.Background(), 15*time.Second)
			defer jc()
			_ = transport.CloseAndWait(join)
			_ = localNat.CloseAndWait(join)
			_ = provider.CloseAndWait(join)
			_ = oob.CloseAndWait(join)
		}
		defer closeProvider()
		provider.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true, protocol.ProvideMode_Network: true})
		deadline := time.Now().Add(15 * time.Second)
		for {
			m, e := model.GetProvideModes(ctx, providerClientId)
			if e == nil && len(m) > 0 {
				break
			}
			if time.Now().After(deadline) {
				tb.Fatal("provider registration did not commit")
			}
			time.Sleep(20 * time.Millisecond)
		}
		tlsOrigin := func(hostname string, serve http.Handler) *httptest.Server {
			leaf, key := issueLeaf(t, hostname)
			if ip := net.ParseIP(hostname); ip != nil {
				copy := *leaf
				copy.IPAddresses = []net.IP{ip}
				copy.DNSNames = nil
				der, e := x509.CreateCertificate(rand.Reader, &copy, testCaCert, &key.PublicKey, testCaKey)
				if e != nil {
					tb.Fatal(e)
				}
				leaf, e = x509.ParseCertificate(der)
				if e != nil {
					tb.Fatal(e)
				}
			}
			s := httptest.NewUnstartedServer(serve)
			s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
			s.StartTLS()
			return s
		}
		dns := tlsOrigin("127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dnsCalls.Add(1)
			raw, e := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
			if e != nil {
				http.Error(w, "dns", 400)
				return
			}
			var message dnsmessage.Message
			if e = message.Unpack(raw); e != nil || len(message.Questions) != 1 {
				http.Error(w, "dns", 400)
				return
			}
			message.Header.Response = true
			message.Header.Authoritative = true
			q := message.Questions[0]
			message.Answers = nil
			if q.Type == dnsmessage.TypeA {
				message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}}}
			}
			raw, e = message.Pack()
			if e != nil {
				http.Error(w, "dns", 500)
				return
			}
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(raw)
		}))
		defer dns.Close()
		const payloadHost = "payload.localquality.example"
		payload := tlsOrigin(payloadHost, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payloadCalls.Add(1)
			_, _ = io.WriteString(w, "actual provider payload")
		}))
		defer payload.Close()
		cfg := Config{ApiUrl: trap.URL, PlatformUrl: "ws" + platform.URL[4:], ByJwt: sourceJwt, ClientId: connect.Id(sourceClientId), DataOnlyProbe: true, ClientCredentials: counted, ClientControl: counted, CloseTimeout: 15 * time.Second, DnsResolverSettings: &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{dns.URL + "/dns-query"}}, ContractReservationByteCount: 1 << 20}
		tunnel, e := Open(ctx, cfg, connect.Id(providerClientId))
		if e != nil {
			tb.Fatal(e)
		}
		defer tunnel.Close()
		client := tunnel.HttpClientForHosts(20*time.Second, []string{payloadHost})
		defer client.CloseIdleConnections()
		_, port, _ := net.SplitHostPort(payload.Listener.Addr().String())
		response, e := client.Get("https://" + net.JoinHostPort(payloadHost, port) + "/payload")
		if e != nil {
			tb.Fatal("provider DNS/TLS payload", e)
		}
		body, e := io.ReadAll(response.Body)
		response.Body.Close()
		if e != nil || string(body) != "actual provider payload" {
			tb.Fatal("payload body", e)
		}
		client.CloseIdleConnections()
		if e = tunnel.Close(); e != nil {
			tb.Fatal("joined retirement", e)
		}
		closeProvider()
		if counted.mint.Load() == 0 || counted.control.Load() == 0 || counted.retire.Load() == 0 || dnsCalls.Load() == 0 || payloadCalls.Load() == 0 {
			tb.Fatal("missing real mint/control/retire/DNS/payload boundary")
		}
		var liveChildren, contracts, usage int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE source_client_id=$1 AND active=true`, sourceClientId).Scan(&liveChildren))
			server.Raise(conn.QueryRow(ctx, `SELECT count(DISTINCT tc.contract_id), COALESCE(SUM(cc.used_transfer_byte_count),0) FROM transfer_contract tc LEFT JOIN contract_close cc ON cc.contract_id=tc.contract_id WHERE tc.payer_network_id=$1`, sourceNetworkId).Scan(&contracts, &usage))
		})
		if liveChildren != 0 || contracts == 0 || usage == 0 {
			tb.Fatalf("durable boundary: children=%d contracts=%d usage=%d", liveChildren, contracts, usage)
		}
		var payout, balance, unsettled int64
		settlementDeadline := time.Now().Add(20 * time.Second)
		for {
			if err := model.ForceCloseAllOpenContractIds(ctx, server.NowUtc().Add(time.Second)); err != nil {
				tb.Fatal("settlement", err)
			}
			for shard := range model.TransferDebitShardCount {
				if _, err := model.FlushTransferDebits(ctx, shard, nil, 64); err != nil {
					tb.Fatal("asynchronous payer debit", err)
				}
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(SUM(te.payout_byte_count) FILTER (WHERE te.settled),0), count(*) FILTER (WHERE NOT te.settled) FROM transfer_escrow te JOIN transfer_contract tc ON tc.contract_id=te.contract_id WHERE tc.payer_network_id=$1`, sourceNetworkId).Scan(&payout, &unsettled))
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(SUM(balance_byte_count),0) FROM transfer_balance WHERE network_id=$1 AND active=true AND start_time<=$2 AND $2<end_time`, sourceNetworkId, server.NowUtc()).Scan(&balance))
			})
			if unsettled == 0 {
				break
			}
			if time.Now().After(settlementDeadline) {
				tb.Fatal("settlement did not finish")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if payout <= 0 || balance != (8<<30)-payout {
			tb.Fatalf("payer ledger mismatch: payout=%d balance=%d", payout, balance)
		}
		if apiCalls.Load() != 0 {
			tb.Fatalf("control API used %d times", apiCalls.Load())
		}
		fmt.Printf("local authority boundaries mint=%d control=%d retire=%d DNS=%d TLS=%d contracts=%d usage=%d API=0\n", counted.mint.Load(), counted.control.Load(), counted.retire.Load(), dnsCalls.Load(), payloadCalls.Load(), contracts, usage)
	})
}
