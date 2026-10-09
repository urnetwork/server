package work

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"google.golang.org/protobuf/proto"
)

type privateOobReply struct {
	messages []proto.Message
	err      error
}

func privateOobSend(ctx context.Context, control connect.OutOfBandControl, messages ...proto.Message) privateOobReply {
	frames := make([]*protocol.Frame, 0, len(messages))
	for _, message := range messages {
		frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
		if err != nil {
			for _, owned := range frames {
				connect.MessagePoolReturn(owned.MessageBytes)
			}
			return privateOobReply{err: err}
		}
		frames = append(frames, frame)
	}
	done := make(chan privateOobReply, 1)
	control.SendControl(frames, func(frames []*protocol.Frame, err error) {
		out := privateOobReply{err: err}
		for _, frame := range frames {
			message, decodeErr := connect.FromFrame(frame)
			if decodeErr != nil {
				out.err = errors.Join(out.err, decodeErr)
			} else {
				out.messages = append(out.messages, proto.Clone(message))
			}
		}
		done <- out
	})
	select {
	case out := <-done:
		return out
	case <-ctx.Done():
		return privateOobReply{err: ctx.Err()}
	}
}
func privateNotificationCount(t testing.TB, event string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_contract_origin_notifications_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "event" && label.GetValue() == event {
						return metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	t.Fatal("notification counter unavailable")
	return 0
}
func privateCpuSeconds() float64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		panic(err)
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}

type privateOobSample struct {
	Mode            string  `json:"mode"`
	Total           float64 `json:"total_seconds"`
	Credential      float64 `json:"credential_seconds"`
	Registration    float64 `json:"registration_seconds"`
	Contract        float64 `json:"contract_seconds"`
	Close           float64 `json:"close_seconds"`
	Retire          float64 `json:"retire_seconds"`
	Cpu             float64 `json:"cpu_seconds"`
	Allocated       uint64  `json:"allocated_bytes"`
	Allocations     uint64  `json:"allocations"`
	Dials           int64   `json:"loopback_dials"`
	Requests        int64   `json:"http_control_requests"`
	GoroutinesStart int     `json:"goroutines_start"`
	GoroutinesEnd   int     `json:"goroutines_end"`
}

// A control-path experiment: identical actual derived credentials, processed
// Provide, public paid contract ledger and terminal cleanup. The only baseline
// boundary removed is authenticated loopback TLS/HTTP and its JSON handling.
// No provider carrier, URL result, DNS duration or Main network latency is modeled.
func TestPrivateProviderLocalOobControllerParityAndPerf(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		network, user, parent, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, network, "local-oob-fixture", user)
		model.Testing_CreateDevice(ctx, network, device, parent, "oob parent", "fixture")
		parentToken := session.NewByJwt(network, user, "local-oob-fixture", false, false).Client(device, parent).Testing_Sign()
		credentials, err := newProviderEgressCredentials(&model.ProberIdentity{NetworkId: &network, UserId: &user, ClientId: &parent, ByClientJwt: parentToken})
		if err != nil {
			t.Fatal(err)
		}
		credit, err := model.CreateBalanceCode(ctx, 64*1024*1024, 24*time.Hour, 0, "local-oob-fixture", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: credit.Secret, NetworkId: network}, ctx); err != nil {
			t.Fatal(err)
		}
		var providers [2]server.Id
		var providerKeys [2][]byte
		for i := range providers {
			providerNetwork, providerUser := server.NewId(), server.NewId()
			providers[i] = server.NewId()
			model.Testing_CreateNetwork(ctx, providerNetwork, "local-provider-"+providerNetwork.String(), providerUser)
			model.Testing_CreateDevice(ctx, providerNetwork, server.NewId(), providers[i], "provider", "fixture")
			providerKeys[i] = bytes.Repeat([]byte{byte(17 + i)}, 32)
			model.SetProvide(ctx, providers[i], map[model.ProvideMode][]byte{model.ProvideModePublic: providerKeys[i]})
		}
		notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
		defer notifications.Close()
		local, err := newProviderEgressControl(credentials, notifications)
		if err != nil {
			t.Fatal(err)
		}
		var httpCalls, dialCalls atomic.Int64
		endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/hello":
				w.WriteHeader(http.StatusOK)
			case "/connect/control":
				httpCalls.Add(1)
				router.WrapWithInputRequireClient(controller.ConnectControl, w, r.WithContext(model.WithContractOriginNotifications(r.Context(), notifications)))
			default:
				t.Errorf("unexpected baseline path %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		defer endpoint.Close()
		roots := x509.NewCertPool()
		roots.AddCert(endpoint.Certificate())
		addr := endpoint.Listener.Addr().String()
		newStrategy := func() *connect.ClientStrategy {
			settings := connect.DefaultClientStrategySettings()
			settings.EnableNormal = true
			settings.EnableResilient = false
			settings.AltUrl = ""
			settings.ExtenderDirectory = nil
			settings.InternalDohDomains = nil
			settings.RequestTimeout = 3 * time.Second
			settings.TlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			settings.Resolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("fixture refuses DNS") }}
			settings.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != addr {
					return nil, errors.New("fixture refuses non-loopback authority")
				}
				dialCalls.Add(1)
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}}
			return connect.NewClientStrategy(ctx, settings)
		}
		source := connect.Id(parent)
		mint := func() (server.Id, string) {
			result, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := session.ParseByJwtForAudience(ctx, result.ByClientJwt, session.ByJwtAudienceApi)
			if err != nil || claims.ClientId == nil {
				t.Fatal("minted credential invalid", err)
			}
			return *claims.ClientId, result.ByClientJwt
		}
		makeOwner := func(mode, token string) (*connect.ApiOutOfBandControl, *connect.ClientStrategy) {
			strategy := newStrategy()
			if mode == "local" {
				return connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, token, endpoint.URL, local), strategy
			}
			return connect.NewApiOutOfBandControl(ctx, strategy, token, endpoint.URL), strategy
		}
		requireOK := func(reply privateOobReply) {
			if reply.err != nil {
				t.Fatal(reply.err)
			}
		}
		run := func(mode string, index int) privateOobSample {
			sample := privateOobSample{Mode: mode, GoroutinesStart: runtime.NumGoroutine()}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			cpu := privateCpuSeconds()
			dials, requests := dialCalls.Load(), httpCalls.Load()
			start := time.Now()
			child, token := mint()
			sample.Credential = time.Since(start).Seconds()
			owner, strategy := makeOwner(mode, token)
			defer strategy.Close()
			defer owner.CloseAndWait(context.Background())
			registerAt := time.Now()
			requireOK(privateOobSend(ctx, owner, &protocol.Provide{Keys: []*protocol.ProvideKey{{Mode: protocol.ProvideMode_Network, ProvideSecretKey: bytes.Repeat([]byte{42}, 32)}}}))
			modes, err := model.GetProvideModes(ctx, child)
			if err != nil || !modes[model.ProvideModeNetwork] {
				t.Fatal("Provide acknowledged before registration", err)
			}
			sample.Registration = time.Since(registerAt).Seconds()
			which := index % len(providers)
			contractAt := time.Now()
			reply := privateOobSend(ctx, owner, &protocol.CreateContract{DestinationId: providers[which].Bytes(), TransferByteCount: 4096})
			requireOK(reply)
			if len(reply.messages) != 1 {
				t.Fatal("one contract response required")
			}
			result, ok := reply.messages[0].(*protocol.CreateContractResult)
			if !ok || result.Error != nil || result.Contract == nil {
				t.Fatal("contract did not commit")
			}
			stored := &protocol.StoredContract{}
			if err := proto.Unmarshal(result.Contract.StoredContractBytes, stored); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored.SourceId, child.Bytes()) || !bytes.Equal(stored.DestinationId, providers[which].Bytes()) || stored.TransferByteCount == 0 {
				t.Fatal("contract crossed provider/client boundary")
			}
			mac := hmac.New(sha256.New, providerKeys[which])
			_, _ = mac.Write(result.Contract.StoredContractBytes)
			if !hmac.Equal(mac.Sum(nil), result.Contract.StoredContractHmac) {
				t.Fatal("provider authentication changed")
			}
			contractId, err := server.IdFromBytes(stored.ContractId)
			if err != nil {
				t.Fatal(err)
			}
			sample.Contract = time.Since(contractAt).Seconds()
			closeAt := time.Now()
			requireOK(privateOobSend(ctx, owner, &protocol.CloseContract{ContractId: stored.ContractId, AckedByteCount: 1024}))
			if err := model.CloseContract(ctx, contractId, providers[which], 1024, false); err != nil {
				t.Fatal(err)
			}
			if _, open := model.GetOpenContractIdsWithNoPartialClose(ctx, child, providers[which])[contractId]; open {
				t.Fatal("contract cleanup was not processed")
			}
			if err := owner.CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
			strategy.Close()
			sample.Close = time.Since(closeAt).Seconds()
			retireAt := time.Now()
			if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(child)}); err != nil {
				t.Fatal(err)
			}
			if _, present := credentials.children.Load(child); present {
				t.Fatal("retired identity retained local authority")
			}
			sample.Retire = time.Since(retireAt).Seconds()
			sample.Total = time.Since(start).Seconds()
			sample.Cpu = privateCpuSeconds() - cpu
			runtime.ReadMemStats(&after)
			sample.Allocated = after.TotalAlloc - before.TotalAlloc
			sample.Allocations = after.Mallocs - before.Mallocs
			sample.Dials = dialCalls.Load() - dials
			sample.Requests = httpCalls.Load() - requests
			sample.GoroutinesEnd = runtime.NumGoroutine()
			if mode == "local" && (sample.Dials != 0 || sample.Requests != 0) {
				t.Fatal("local control contacted HTTP")
			}
			if mode == "http" && (sample.Dials == 0 || sample.Requests != 3) {
				t.Fatalf("baseline did not execute exact HTTP workload: dials=%d requests=%d", sample.Dials, sample.Requests)
			}
			return sample
		}
		// Warm both real paths before counterbalanced measurements. Warmups are
		// excluded, but their processed contract counts still participate in parity.
		beforeNotifications := privateNotificationCount(t, "enqueued")
		run("http", 0)
		run("local", 0)
		samples := []privateOobSample{}
		for _, mode := range []string{"http", "local", "local", "http", "local", "http", "http", "local"} {
			for i := range 3 {
				samples = append(samples, run(mode, i))
			}
		}
		if delta := privateNotificationCount(t, "enqueued") - beforeNotifications; delta != float64(len(samples)+2) {
			t.Fatalf("committed origin publication parity failed delta=%v", delta)
		}
		for _, mode := range []string{"http", "local"} {
			values := []float64{}
			sum, cpu := 0.0, 0.0
			var allocated, allocations uint64
			var dials, requests int64
			for _, sample := range samples {
				if sample.Mode == mode {
					values = append(values, sample.Total)
					sum += sample.Total
					cpu += sample.Cpu
					allocated += sample.Allocated
					allocations += sample.Allocations
					dials += sample.Dials
					requests += sample.Requests
				}
			}
			sort.Float64s(values)
			result := map[string]any{"mode": mode, "n": len(values), "control_cycles_per_second": float64(len(values)) / sum, "mean_seconds": sum / float64(len(values)), "p50_seconds": values[(len(values)-1)/2], "p95_seconds": values[len(values)-1], "cpu_seconds": cpu, "allocated_bytes": allocated, "allocations": allocations, "loopback_dials": dials, "http_control_requests": requests, "url_accepted_results_measured": false, "main_contact": false}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("OOB_PERF %s", encoded)
		}
		encoded, err := json.Marshal(samples)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("OOB_SAMPLES %s", encoded)
		// Real authorization controls on a minted child: signature, owner,
		// cancellation, and revocation cannot become processed acknowledgments.
		child, token := mint()
		for _, invalid := range []string{parentToken, "invalid", session.NewByJwt(network, user, "local-oob-fixture", false, false).Client(device, providers[0]).Testing_Sign()} {
			if _, err := local.ConnectControl(ctx, invalid, &connect.ConnectControlArgs{}); err == nil {
				t.Fatal("unauthorized local control accepted")
			}
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		if _, err := local.ConnectControl(canceled, token, &connect.ConnectControlArgs{}); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled control accepted")
		}
		key := bytes.Repeat([]byte{9}, 32)
		owner, strategy := makeOwner("local", token)
		requireOK(privateOobSend(ctx, owner, &protocol.ClientKey{PublicKey: key}))
		if got, err := model.GetClientPublicKey(ctx, child); err != nil || !bytes.Equal(got, key) {
			t.Fatal("key registration was not processed", err)
		}
		if reply := privateOobSend(ctx, owner, &protocol.ClientKey{PublicKey: []byte{1}}, &protocol.ClientKey{PublicKey: bytes.Repeat([]byte{10}, 32)}); reply.err == nil {
			t.Fatal("partial controller error was acknowledged")
		}
		if got, err := model.GetClientPublicKey(ctx, child); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{10}, 32)) {
			t.Fatal("later frame lost after error", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, child))
		})
		if _, err := local.ConnectControl(ctx, token, &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(nil)}); err == nil {
			t.Fatal("revoked child accepted by retained registry")
		}
		if err := owner.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		strategy.Close()
		if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(child)}); err != nil {
			t.Fatal(err)
		}
	})
}
