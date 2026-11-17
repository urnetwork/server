package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A refusing API is distinct from the fixture provider's control origin and
// the payload origin. Actual hosted mint/renew/refresh/reconnect must still
// deliver traffic and settle the caller's ledger without touching that API.
func TestProxyCriticalControlUsesLocalAuthorityWithoutHttp(t *testing.T) {
	// The local authority asks for provider locations from the unspecified
	// address its sessions carry. No ip override covers that address, so the
	// lookup reads the GeoLite2 database, which a config home may not have:
	// the repository's synthetic empty one stands in, and knows no address.
	server.UseIpDbFileForTest(t, filepath.Join("..", "local", "testdata", "config", "local", "mmdb", "geolite2.mmdb"))
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var requests atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.Error(w, "control API disabled", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		opts := defaultProxyTestOptions()
		opts.hostedApiUrl = trap.URL
		opts.disableSecurityPolicies = true
		restore := withSmallContracts()
		defer restore()
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(h.ctx, 90*time.Second)
		defer cancel()
		refreshed, err := pd.deviceLocal.GetApi().RefreshJwtSyncWithContext(ctx)
		if err != nil || refreshed == nil || refreshed.ByJwt == "" {
			t.Fatal("local refresh failed", err)
		}
		balanceDone := make(chan error, 1)
		pd.deviceLocal.GetApi().SubscriptionBalance(connect.NewApiCallback[*sdk.SubscriptionBalanceResult](func(result *sdk.SubscriptionBalanceResult, err error) {
			if err == nil && result == nil {
				err = fmt.Errorf("missing balance result")
			}
			balanceDone <- err
		}))
		select {
		case err := <-balanceDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		locationsDone := make(chan error, 1)
		pd.deviceLocal.GetApi().GetProviderLocations(connect.NewApiCallback[*sdk.FindLocationsResult](func(result *sdk.FindLocationsResult, err error) {
			if err == nil && result == nil {
				err = fmt.Errorf("missing locations result")
			}
			locationsDone <- err
		}))
		select {
		case err := <-locationsDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		target, closeTarget := startLocalTarget(t)
		defer closeTarget()
		client := newSocksProxyClient(t, h.signedProxyId, h.socksPort, 40*time.Second)
		defer client.CloseIdleConnections()
		// A fixed-only window bypasses discovery. Mix a real BestAvailable
		// request with the known payload peer so a lost SDK discovery hook
		// reaches the trap even when its fixed-destination fallback works.
		specs := sdk.NewProviderSpecList()
		specs.Add(&sdk.ProviderSpec{ClientId: server.ToSdkId(h.providerClientId)})
		specs.Add(&sdk.ProviderSpec{BestAvailable: true})
		pd.deviceLocal.SetDestination(pd.deviceLocal.GetConnectLocation(), specs)
		if !pd.WaitForReady(ctx, 30*time.Second) {
			t.Fatal("discovery window did not become ready")
		}
		if err := fetchData(client, target, 8*testMib); err != nil {
			t.Fatal("first transfer", err)
		}
		pd.deviceLocal.Reconnect(pd.deviceLocal.GetConnectLocation())
		if !pd.WaitForReady(ctx, 30*time.Second) {
			t.Fatal("reconnect did not become ready")
		}
		if err := fetchData(client, target, 8*testMib); err != nil {
			t.Fatal("renewed transfer", err)
		}
		if total, _ := escrowedContractCounts(ctx, h.pdNetworkId); total < 4 {
			t.Fatalf("renewal did not create enough real escrows: %d", total)
		}
		client.CloseIdleConnections()
		if err := h.proxyDeviceManager.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		h.closeProvider(t)
		settleAllEscrowedContracts(t, ctx, h.pdNetworkId)
		for shard := range model.TransferDebitShardCount {
			if _, err := model.FlushTransferDebits(ctx, shard, nil, 64); err != nil {
				t.Fatal("asynchronous payer debit", err)
			}
		}
		evidence := readProxyPayerReconciliation(ctx, h.pdNetworkId, opts.pdInitialBalance)
		if err := validateProxyPayerReconciliation(evidence); err != nil {
			t.Fatal(err)
		}
		if n := requests.Load(); n != 0 {
			t.Fatalf("hosted critical path made %d HTTP API requests", n)
		}
	})
}

// Ablate each optional generator boundary separately. Discovery deliberately
// uses BestAvailable (fixed client ids bypass discovery). Positive operations
// use the real local authority; each missing hook must trip the HTTP trap.
func TestProxyLocalHooksHaveIndependentHttpRedControls(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var requests atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.Error(w, "API disabled", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		opts := defaultProxyTestOptions()
		opts.hostedApiUrl = trap.URL
		opts.controlPlaneOnly = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
		defer cancel()
		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.EnableResilient = false
		strategySettings.RequestTimeout = time.Second
		strategy := connect.NewClientStrategy(ctx, strategySettings)
		defer strategy.Close()
		parent := connect.Id(h.pdClientId)
		generator := func(credentials, discovery bool) *connect.ApiMultiClientGenerator {
			s := connect.DefaultApiMultiClientGeneratorSettings()
			if credentials {
				s.ClientCredentials = pd.localControl
			}
			if discovery {
				s.ProviderDiscovery = pd.localControl
			}
			return connect.NewApiMultiClientGenerator(ctx, []*connect.ProviderSpec{{BestAvailable: true}}, strategy, nil, trap.URL, h.pdByClientJwt, h.platformUrl, "test", "test", "test", &parent, connect.DefaultClientSettings, s)
		}
		for _, kind := range []string{"credentials", "discovery"} {
			for _, enabled := range []bool{true, false} {
				before := requests.Load()
				g := generator(kind != "credentials" || enabled, kind != "discovery" || enabled)
				var callErr error
				if kind == "credentials" {
					args, e := g.NewClientArgsContext(ctx)
					callErr = e
					if e == nil {
						g.RemoveClientArgs(args)
					}
				} else {
					_, callErr = g.NextDestinationsContext(ctx, 2, nil, "quality")
				}
				if err := g.CloseAndWait(ctx); err != nil {
					t.Fatal(err)
				}
				if enabled && (callErr != nil || requests.Load() != before) {
					t.Fatalf("local %s failed or used HTTP: %v", kind, callErr)
				}
				if !enabled && (callErr == nil || requests.Load() == before) {
					t.Fatalf("missing %s hook did not expose HTTP dependency", kind)
				}
			}
		}
		child, err := pd.localControl.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &parent, Description: "control-test", DeviceSpec: "test"})
		if err != nil || child == nil || child.Error != nil {
			t.Fatal("real child mint failed", err)
		}
		claims, err := connect.ParseByJwtUnverified(child.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{true, false} {
			before := requests.Load()
			var local connect.NetworkClientControl
			if enabled {
				local = pd.localControl
			}
			var oob *connect.ApiOutOfBandControl
			if local != nil {
				oob = connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, child.ByClientJwt, trap.URL, local)
			} else {
				oob = connect.NewApiOutOfBandControl(ctx, strategy, child.ByClientJwt, trap.URL)
			}
			frame, e := connect.ToFrame(&protocol.Provide{Keys: []*protocol.ProvideKey{{Mode: protocol.ProvideMode_Network, ProvideSecretKey: make([]byte, 32)}}}, connect.DefaultProtocolVersion)
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			oob.SendControlWithCtx(ctx, []*protocol.Frame{frame}, func(_ []*protocol.Frame, e error) { done <- e })
			var callErr error
			select {
			case callErr = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := oob.CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
			if enabled && (callErr != nil || requests.Load() != before) {
				t.Fatal("local control failed or used HTTP", callErr)
			}
			if !enabled && (callErr == nil || requests.Load() == before) {
				t.Fatal("missing control hook did not expose HTTP dependency")
			}
		}
		modes, err := model.GetProvideModes(ctx, server.Id(claims.ClientId))
		if err != nil || !modes[model.ProvideModeNetwork] {
			t.Fatal("positive control did not perform real registration", err)
		}
		if _, err = pd.localControl.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: claims.ClientId}); err != nil {
			t.Fatal(err)
		}
		// Join asynchronous refresh, removals and data owners before the final
		// stable count. Only the three deliberate negative requests may be HTTP.
		before := requests.Load()
		if err = h.proxyDeviceManager.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		if requests.Load() != before {
			t.Fatal("retirement unexpectedly used the HTTP API")
		}
	})
}

// Retained report counts authorize payment; verified response bodies authorize delivery.
type proxyPayerReconciliation struct {
	initialByteCount               model.ByteCount
	payoutByteCount                model.ByteCount
	balanceByteCount               model.ByteCount
	reportedPayoutByteCount        model.ByteCount
	sourceReportedByteCount        model.ByteCount
	receivedReportedByteCount      model.ByteCount
	contractCount                  int
	invalidContractCount           int
	remainingDebitCount            int
	reservedByteCount              model.ByteCount
	nonzeroReservationBalanceCount int
}

// Read only completed fixture evidence after the original debit flush finishes.
func readProxyPayerReconciliation(ctx context.Context, payerNetworkId server.Id, initial model.ByteCount) proxyPayerReconciliation {
	evidence := proxyPayerReconciliation{initialByteCount: initial}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT contract.outcome,
            source_close.used_transfer_byte_count,source_close.checkpoint,
            destination_close.used_transfer_byte_count,destination_close.checkpoint,
            COALESCE(sum(escrow.payout_byte_count),0),bool_and(escrow.settled)
            FROM transfer_contract AS contract
            INNER JOIN transfer_escrow AS escrow USING(contract_id)
            LEFT JOIN contract_close AS source_close ON source_close.contract_id=contract.contract_id AND source_close.party='source'
            LEFT JOIN contract_close AS destination_close ON destination_close.contract_id=contract.contract_id AND destination_close.party='destination'
            WHERE contract.payer_network_id=$1
            GROUP BY contract.contract_id,contract.outcome,
                source_close.used_transfer_byte_count,source_close.checkpoint,
                destination_close.used_transfer_byte_count,destination_close.checkpoint`, payerNetworkId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var outcome *model.ContractOutcome
				var sourceBytes, receivedBytes *model.ByteCount
				var sourceCheckpoint, destinationCheckpoint *bool
				var payout model.ByteCount
				var settled bool
				server.Raise(rows.Scan(&outcome, &sourceBytes, &sourceCheckpoint, &receivedBytes, &destinationCheckpoint, &payout, &settled))
				evidence.contractCount++
				evidence.payoutByteCount += payout
				if outcome == nil || *outcome != model.ContractOutcomeSettled || !settled ||
					sourceBytes == nil || receivedBytes == nil || sourceCheckpoint == nil || destinationCheckpoint == nil ||
					*sourceCheckpoint || *destinationCheckpoint || *sourceBytes < 0 || *receivedBytes < 0 {
					evidence.invalidContractCount++
					continue
				}
				// The published two-final payment policy is the floor mean. Compute
				// it independently from retained reports, without a model helper.
				lower, upper := min(*sourceBytes, *receivedBytes), max(*sourceBytes, *receivedBytes)
				expected := lower + (upper-lower)/2
				evidence.reportedPayoutByteCount += expected
				evidence.sourceReportedByteCount += *sourceBytes
				evidence.receivedReportedByteCount += *receivedBytes
				if payout != expected {
					evidence.invalidContractCount++
				}
			}
		})
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_debit_journal AS journal
            INNER JOIN transfer_balance AS balance USING(balance_id) WHERE balance.network_id=$1`, payerNetworkId).Scan(&evidence.remainingDebitCount))
	})
	evidence.balanceByteCount = pgActiveBalanceSum(ctx, payerNetworkId)
	for _, balanceId := range activeBalanceIds(ctx, payerNetworkId) {
		reserved := model.Testing_NetEscrowByteCount(ctx, balanceId)
		evidence.reservedByteCount += reserved
		if reserved != 0 {
			evidence.nonzeroReservationBalanceCount++
		}
	}
	return evidence
}

// A payload receipt is not the sender's acknowledgement. Reconcile the exact
// payment authorized by both retained reports, including unused contracts.
func validateProxyPayerReconciliation(evidence proxyPayerReconciliation) error {
	if evidence.contractCount == 0 || evidence.invalidContractCount != 0 ||
		evidence.payoutByteCount != evidence.reportedPayoutByteCount ||
		evidence.balanceByteCount != evidence.initialByteCount-evidence.reportedPayoutByteCount ||
		evidence.remainingDebitCount != 0 || evidence.reservedByteCount != 0 || evidence.nonzeroReservationBalanceCount != 0 {
		return fmt.Errorf("reported transfer usage did not reconcile: %+v", evidence)
	}
	return nil
}

// Public identified receiver checkpoints precede sender finals. Four 4MiB
// reported receipts authorize 14MiB payment when the senders acknowledged 12MiB.
func TestProxyPayerReconciliationUsesAcknowledgedReportPolicy(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		payerNetworkId, payerClientId := server.NewId(), server.NewId()
		providerNetworkId, providerClientId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, payerNetworkId, "payer-reconciliation-source", server.NewId())
		model.Testing_CreateNetwork(ctx, providerNetworkId, "payer-reconciliation-provider", server.NewId())
		model.Testing_CreateDevice(ctx, payerNetworkId, server.NewId(), payerClientId, "payer", "fixture")
		model.Testing_CreateDevice(ctx, providerNetworkId, server.NewId(), providerClientId, "provider", "fixture")
		redeemBalance(t, ctx, payerNetworkId, testInitialBalance)
		for range 4 {
			contractId, escrow, err := model.CreateContract(ctx, payerNetworkId, payerClientId, providerNetworkId, providerClientId, 4*testMib)
			if err != nil || escrow == nil || escrow.TransferByteCount != 4*testMib {
				t.Fatal("synthetic report contract did not open", err)
			}
			if applied, err := model.CloseContractReport(ctx, contractId, providerClientId, 4*testMib, true, server.NewId()); !applied || err != nil {
				t.Fatal("receiver checkpoint did not commit", applied, err)
			}
			if applied, err := model.CloseContractReport(ctx, contractId, payerClientId, 3*testMib, false, server.NewId()); !applied || err != nil {
				t.Fatal("sender final did not commit", applied, err)
			}
		}
		for shard := range model.TransferDebitShardCount {
			result, err := model.FlushTransferDebits(ctx, shard, nil, 64)
			if err != nil || result.Failed != 0 || result.Busy != 0 {
				t.Fatal("synthetic report debit did not finish", result, err)
			}
		}
		evidence := readProxyPayerReconciliation(ctx, payerNetworkId, testInitialBalance)
		// Fixed independent numbers pin the policy before invoking the oracle.
		if evidence.contractCount != 4 || evidence.invalidContractCount != 0 ||
			evidence.sourceReportedByteCount != 12*testMib || evidence.receivedReportedByteCount != 16*testMib ||
			evidence.payoutByteCount != 14*testMib || evidence.reportedPayoutByteCount != 14*testMib ||
			evidence.balanceByteCount != testInitialBalance-14*testMib || evidence.remainingDebitCount != 0 || evidence.reservedByteCount != 0 || evidence.nonzeroReservationBalanceCount != 0 {
			t.Fatalf("published unequal-report payment changed: %+v", evidence)
		}
		if err := validateProxyPayerReconciliation(evidence); err != nil {
			t.Fatal(err)
		}
		for _, control := range []struct {
			name   string
			change func(*proxyPayerReconciliation)
		}{
			{name: "payout", change: func(changed *proxyPayerReconciliation) { changed.payoutByteCount++ }},
			{name: "balance", change: func(changed *proxyPayerReconciliation) { changed.balanceByteCount-- }},
			{name: "journal", change: func(changed *proxyPayerReconciliation) { changed.remainingDebitCount = 1 }},
			{name: "reservation", change: func(changed *proxyPayerReconciliation) {
				changed.reservedByteCount = 1
				changed.nonzeroReservationBalanceCount = 1
			}},
			{name: "offsetting_reservations", change: func(changed *proxyPayerReconciliation) { changed.nonzeroReservationBalanceCount = 2 }},
			{name: "report", change: func(changed *proxyPayerReconciliation) { changed.invalidContractCount = 1 }},
		} {
			changed := evidence
			control.change(&changed)
			if err := validateProxyPayerReconciliation(changed); err == nil {
				t.Fatal("accounting corruption was accepted", control.name)
			}
		}
	})
}
