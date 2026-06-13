package proxy

// Contract-focused load and failure-mode tests for the proxy stack.
//
// Production incidents have shown users unable to route traffic because
// contracts could not be created. These tests put the full proxy path
// (socks/http proxy -> proxy device -> connect server -> provider) under
// concurrent load with forced contract churn, and then verify the contract
// ledger reconciles exactly: escrow is fully released, the redis net escrow
// mirror returns to zero, and the network balance accounts for every payout.
// They also cover the known production failure modes directly:
//
//   - balance exhaustion must surface as failed transfers, and topping the
//     balance up must restore routing on the live device without a restart
//     (TestProxyContractBalanceExhaustionRecovery)
//   - lost redis mirrored state (incomplete migration, eviction, failover)
//     must fall back to the db for provide keys and treat a missing net
//     escrow counter as zero (TestProxyContractRedisMirrorLoss)
//   - the disconnected-client reaper must not take provisioned proxy device
//     clients (or their proxy_device_config, which cascades) with it
//     (TestProxyClientReapSurvival)
//
// Unlike TestProxy these tests do not touch the real internet: the provider
// egresses to a local TLS target server, with the security policies disabled
// (the production policies filter loopback destinations for public provide
// relationships). The target ports are kept below 11000 so return traffic
// passes the device ingress policy's high-source-port filter.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

const (
	testMib = model.ByteCount(1024 * 1024)

	// per-test local target ports (< 11000, see the package comment)
	testTargetPortChurn      = 8980
	testTargetPortExhaustion = 8981
	testTargetPortMirror     = 8982
	testTargetPortReap       = 8983

	// cap granted contracts at a small size so moderate transfer volumes force
	// many contract creations (churn)
	testSmallContractByteCount = 4 * testMib
)

// withSmallContracts lowers the controller's granted contract size cap and
// returns a restore function. The tests in this package run sequentially, so
// mutating the package var is safe.
func withSmallContracts() (restore func()) {
	previousMax := controller.MaxContractTransferByteCount
	controller.MaxContractTransferByteCount = testSmallContractByteCount
	return func() {
		controller.MaxContractTransferByteCount = previousMax
	}
}

// ---- local target server -----------------------------------------------

// startLocalTarget starts a TLS http server on 127.0.0.1:port that serves
// `GET /data?bytes=N` (N pseudo-random-ish bytes) and `POST /upload`
// (discards the body and echoes the byte count).
func startLocalTarget(t testing.TB, port int) (baseUrl string, closeTarget func()) {
	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		byteCount := 0
		fmt.Sscanf(r.URL.Query().Get("bytes"), "%d", &byteCount)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", byteCount))
		for i := 0; i < byteCount; i += len(chunk) {
			n := min(len(chunk), byteCount-i)
			if _, err := w.Write(chunk[0:n]); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "%d", n)
	})

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("target: listen %d: %v", port, err)
	}
	target := httptest.NewUnstartedServer(mux)
	target.Listener.Close()
	target.Listener = listener
	target.StartTLS()
	return target.URL, target.Close
}

// ---- proxy transports ----------------------------------------------------

// the local target presents an httptest self-signed cert
var testTargetTlsConfig = &tls.Config{InsecureSkipVerify: true}

func newSocksProxyClient(t testing.TB, signedProxyId string, timeout time.Duration) *http.Client {
	dialer, err := xproxy.SOCKS5(
		"tcp",
		fmt.Sprintf("127.0.0.1:%d", InternalSocksPort),
		&xproxy.Auth{User: signedProxyId, Password: "x"},
		xproxy.Direct,
	)
	if err != nil {
		t.Fatalf("socks: dialer: %v", err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		t.Fatalf("socks: dialer is not a ContextDialer")
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:     contextDialer.DialContext,
			TLSClientConfig: testTargetTlsConfig,
		},
		Timeout: timeout,
	}
}

func newHttpProxyClient(t testing.TB, signedProxyId string, timeout time.Duration) *http.Client {
	proxyUrl, err := url.Parse(fmt.Sprintf("http://%s:x@127.0.0.1:%d", signedProxyId, InternalHttpPort))
	if err != nil {
		t.Fatalf("http: parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyUrl),
			TLSClientConfig: testTargetTlsConfig,
		},
		Timeout: timeout,
	}
}

// ---- transfer helpers ------------------------------------------------------

// fetchData GETs `byteCount` bytes from the target through the client and
// verifies the full body arrives.
func fetchData(client *http.Client, baseUrl string, byteCount model.ByteCount) error {
	resp, err := client.Get(fmt.Sprintf("%s/data?bytes=%d", baseUrl, byteCount))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return err
	}
	if model.ByteCount(n) != byteCount {
		return fmt.Errorf("short body %d < %d", n, byteCount)
	}
	return nil
}

// uploadData POSTs `byteCount` bytes to the target through the client.
func uploadData(client *http.Client, baseUrl string, byteCount model.ByteCount) error {
	body := io.LimitReader(neverEndingReader{}, int64(byteCount))
	resp, err := client.Post(fmt.Sprintf("%s/upload", baseUrl), "application/octet-stream", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

type neverEndingReader struct{}

func (neverEndingReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(i)
	}
	return len(b), nil
}

// requireFetchData retries fetchData until the deadline, like requireProxyGet.
func requireFetchData(t testing.TB, leg string, client *http.Client, baseUrl string, byteCount model.ByteCount, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = fetchData(client, baseUrl, byteCount); lastErr == nil {
			return
		}
		fmt.Printf("[progress][%s]retry err=%v\n", leg, lastErr)
		select {
		case <-time.After(1 * time.Second):
		}
	}
	t.Fatalf("%s: could not fetch %d bytes through proxy: %v", leg, byteCount, lastErr)
}

// ---- contract ledger helpers -----------------------------------------------

// pgActiveBalanceSum reads the raw postgres balance sum for the network's
// active balances, without the redis net escrow adjustment that
// GetActiveTransferBalances applies.
func pgActiveBalanceSum(ctx context.Context, networkId server.Id) model.ByteCount {
	var sum model.ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COALESCE(SUM(balance_byte_count), 0)
				FROM transfer_balance
				WHERE
					network_id = $1 AND
					active = true AND
					start_time <= $2 AND $2 < end_time
			`,
			networkId,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&sum))
			}
		})
	})
	return sum
}

func activeBalanceIds(ctx context.Context, networkId server.Id) []server.Id {
	balanceIds := []server.Id{}
	for _, transferBalance := range model.GetActiveTransferBalances(ctx, networkId) {
		balanceIds = append(balanceIds, transferBalance.BalanceId)
	}
	return balanceIds
}

// escrowedContractCounts returns the total and still-open (no outcome) counts
// of escrowed contracts paid by the network. Control-plane contracts have no
// escrow and are excluded.
func escrowedContractCounts(ctx context.Context, payerNetworkId server.Id) (total int, open int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					COUNT(DISTINCT transfer_contract.contract_id),
					COUNT(DISTINCT transfer_contract.contract_id) FILTER (WHERE transfer_contract.outcome IS NULL)
				FROM transfer_contract
				INNER JOIN transfer_escrow ON
					transfer_escrow.contract_id = transfer_contract.contract_id
				WHERE
					transfer_contract.payer_network_id = $1
			`,
			payerNetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&total, &open))
			}
		})
	})
	return
}

// settledPayoutSum returns the total payout bytes swept out of the network's
// escrows so far.
func settledPayoutSum(ctx context.Context, payerNetworkId server.Id) model.ByteCount {
	var sum model.ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COALESCE(SUM(COALESCE(transfer_escrow.payout_byte_count, 0)), 0)
				FROM transfer_escrow
				INNER JOIN transfer_contract ON
					transfer_contract.contract_id = transfer_escrow.contract_id
				WHERE
					transfer_contract.payer_network_id = $1 AND
					transfer_escrow.settled = true
			`,
			payerNetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&sum))
			}
		})
	})
	return sum
}

// settleAllEscrowedContracts repeatedly runs the force-close janitor until the
// network has no open escrowed contracts (so all escrow is either swept or
// returned), the way the production expiry task eventually settles stragglers.
func settleAllEscrowedContracts(t testing.TB, ctx context.Context, payerNetworkId server.Id) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if err := model.ForceCloseAllOpenContractIds(ctx, server.NowUtc().Add(time.Second)); err != nil {
			t.Fatalf("force close: %v", err)
		}
		if _, open := escrowedContractCounts(ctx, payerNetworkId); open == 0 {
			return
		}
		select {
		case <-time.After(2 * time.Second):
		}
	}
	_, open := escrowedContractCounts(ctx, payerNetworkId)
	t.Fatalf("timed out settling contracts: %d still open", open)
}

// ---- tests -------------------------------------------------------------------

// TestProxyContractChurnLoad drives concurrent mixed-size transfers through the
// socks and http proxies with the granted contract size capped low, forcing the
// device and provider through many contract create/close cycles. It then
// settles everything and requires the contract ledger to reconcile exactly:
// every escrowed byte is either swept as payout or returned to the balance, and
// the redis net escrow mirror is exactly zero (any residue is the
// balance-lockup bug class where networks permanently lose apparent balance and
// can no longer create contracts).
func TestProxyContractChurnLoad(t *testing.T) {
	if testing.Short() {
		return
	}
	restore := withSmallContracts()
	defer restore()

	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyContractChurnLoad\n")
		opts := defaultProxyTestOptions()
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		targetUrl, closeTarget := startLocalTarget(t, testTargetPortChurn)
		defer closeTarget()

		ctx := h.ctx

		// make sure the path works before applying load
		warmClient := newSocksProxyClient(t, h.signedProxyId, 60*time.Second)
		requireFetchData(t, "warmup", warmClient, targetUrl, 64*1024, 120*time.Second)
		fmt.Printf("[progress]warmup ok\n")

		// 6 workers x 5 rounds x (2MiB + 256KiB + 16KiB down, 1MiB up) ~ 98MiB,
		// alternating socks and http proxy entry paths
		workerCount := 6
		roundCount := 5
		var wg sync.WaitGroup
		errs := make(chan error, workerCount)
		for w := 0; w < workerCount; w += 1 {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				var client *http.Client
				if w%2 == 0 {
					client = newSocksProxyClient(t, h.signedProxyId, 60*time.Second)
				} else {
					client = newHttpProxyClient(t, h.signedProxyId, 60*time.Second)
				}
				defer client.CloseIdleConnections()
				for round := 0; round < roundCount; round += 1 {
					legs := []func() error{
						func() error { return fetchData(client, targetUrl, 2*testMib) },
						func() error { return fetchData(client, targetUrl, 256*1024) },
						func() error { return uploadData(client, targetUrl, 1*testMib) },
						func() error { return fetchData(client, targetUrl, 16*1024) },
					}
					for legIndex, leg := range legs {
						// retry transient failures within a bounded window; the
						// load itself races contract churn so brief stalls are
						// expected, hard failure is not
						var err error
						deadline := time.Now().Add(90 * time.Second)
						for {
							if err = leg(); err == nil {
								break
							}
							if !time.Now().Before(deadline) {
								errs <- fmt.Errorf("worker %d round %d leg %d: %w", w, round, legIndex, err)
								return
							}
							select {
							case <-time.After(1 * time.Second):
							}
						}
					}
					fmt.Printf("[progress]worker %d round %d/%d ok\n", w, round+1, roundCount)
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("load: %v", err)
		}

		totalContracts, openContracts := escrowedContractCounts(ctx, h.pdNetworkId)
		fmt.Printf("[progress]load done: %d escrowed contracts (%d open)\n", totalContracts, openContracts)

		// the cap (4MiB) must have forced real churn: ~98MiB of payload cannot
		// fit in a handful of contracts
		if totalContracts < 16 {
			t.Fatalf("expected forced contract churn, got only %d escrowed contracts", totalContracts)
		}

		// stop the device so its window keepalives stop minting contracts under
		// the assertions; abandoned in-flight contracts become janitor debris
		// (no-close / one-side-close shapes), which is exactly the production
		// straggler population the force-close task has to settle
		h.proxyDeviceManager.Close()
		// let in-flight closes land, then settle the stragglers like the
		// production janitor
		select {
		case <-time.After(3 * time.Second):
		}
		settleAllEscrowedContracts(t, ctx, h.pdNetworkId)
		fmt.Printf("[progress]all contracts settled\n")

		// ---- ledger reconciliation ----
		initial := opts.pdInitialBalance
		pgSum := pgActiveBalanceSum(ctx, h.pdNetworkId)
		available := model.GetActiveTransferBalanceByteCount(ctx, h.pdNetworkId)
		payouts := settledPayoutSum(ctx, h.pdNetworkId)

		fmt.Printf("[progress]ledger: initial=%d pg=%d available=%d payouts=%d\n", initial, pgSum, available, payouts)

		// every settled byte must be accounted: balance decreased exactly by payouts
		if initial-payouts != pgSum {
			t.Fatalf("balance does not reconcile: initial=%d - payouts=%d != pg balance=%d (leak=%d)", initial, payouts, pgSum, initial-payouts-pgSum)
		}
		// the redis net escrow mirror must fully unwind: a positive residue
		// permanently hides balance from the network (the cannot-create-contracts
		// lockup); a negative residue over-reports it
		for _, balanceId := range activeBalanceIds(ctx, h.pdNetworkId) {
			if netEscrow := model.Testing_NetEscrowByteCount(ctx, balanceId); netEscrow != 0 {
				t.Fatalf("net escrow residue for balance %s: %d", balanceId, netEscrow)
			}
		}
		// and the user-visible available balance equals the raw pg balance
		if available != pgSum {
			t.Fatalf("available balance %d != pg balance %d (net escrow residue)", available, pgSum)
		}
		// usage actually flowed through escrow
		if payouts < 32*testMib {
			t.Fatalf("expected at least 32MiB of settled payouts, got %d", payouts)
		}
		// the provider network pays for nothing in this topology (forward
		// contracts and destination-pays companion contracts are both paid by
		// the proxy device network)
		if providerAvailable := model.GetActiveTransferBalanceByteCount(ctx, h.providerNetworkId); providerAvailable != opts.providerInitialBalance {
			t.Fatalf("provider network balance changed: %d != %d", providerAvailable, opts.providerInitialBalance)
		}
	})
}

// TestProxyContractBalanceExhaustionRecovery funds the proxy device network
// with a small balance, drives downloads until contract creation fails for
// insufficient balance, and then requires that topping up the balance restores
// routing on the live device, without recreating the device or its connection.
// (The recovery half is the user-visible production property: a stuck client
// after a balance failure means users stay offline even after paying.)
func TestProxyContractBalanceExhaustionRecovery(t *testing.T) {
	if testing.Short() {
		return
	}
	restore := withSmallContracts()
	defer restore()

	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyContractBalanceExhaustionRecovery\n")
		opts := defaultProxyTestOptions()
		opts.disableSecurityPolicies = true
		// small enough to exhaust quickly with 4MiB downloads
		opts.pdInitialBalance = 48 * testMib
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		targetUrl, closeTarget := startLocalTarget(t, testTargetPortExhaustion)
		defer closeTarget()

		ctx := h.ctx

		client := newSocksProxyClient(t, h.signedProxyId, 15*time.Second)
		defer client.CloseIdleConnections()
		requireFetchData(t, "warmup", client, targetUrl, 64*1024, 120*time.Second)

		// download until the balance runs out: with a 48MiB balance, 4MiB legs
		// must start failing well within 40 attempts
		successCount := 0
		failureCount := 0
		for attempt := 0; attempt < 40 && failureCount < 2; attempt += 1 {
			if err := fetchData(client, targetUrl, 4*testMib); err != nil {
				failureCount += 1
				fmt.Printf("[progress]attempt %d failed (%d successes so far): %v\n", attempt, successCount, err)
			} else {
				failureCount = 0
				successCount += 1
			}
		}
		if failureCount < 2 {
			t.Fatalf("balance never exhausted after %d successful 4MiB downloads with a %d byte balance", successCount, opts.pdInitialBalance)
		}
		if successCount < 1 {
			t.Fatalf("no successful downloads before exhaustion")
		}
		available := model.GetActiveTransferBalanceByteCount(ctx, h.pdNetworkId)
		fmt.Printf("[progress]exhausted after %d successes, available=%d\n", successCount, available)
		// most of the balance must be consumed or escrowed (the failures above
		// are insufficient-balance failures, not something unrelated)
		if 16*testMib <= available {
			t.Fatalf("transfers failed while %d bytes were still available", available)
		}

		// top up and require routing to resume on the same live device
		redeemBalance(t, ctx, h.pdNetworkId, testInitialBalance)
		fmt.Printf("[progress]balance topped up\n")
		recoveryStart := time.Now()
		requireFetchData(t, "recovery", client, targetUrl, 4*testMib, 180*time.Second)
		fmt.Printf("[progress]recovered %v after top-up\n", time.Since(recoveryStart).Round(time.Second))

		// stable after recovery
		for i := 0; i < 3; i += 1 {
			requireFetchData(t, "post-recovery", client, targetUrl, 1*testMib, 60*time.Second)
		}
	})
}

// TestProxyContractRedisMirrorLoss deletes the redis-mirrored provide keys for
// the provider and the net escrow counters for the device network mid-session,
// then requires fresh contract creation to keep working. Provide keys must fall
// back to the db `provide_key` rows (an incomplete migration or lost redis
// state must not make a provider unreachable), and a missing net escrow counter
// must read as zero rather than blocking escrow.
func TestProxyContractRedisMirrorLoss(t *testing.T) {
	if testing.Short() {
		return
	}
	restore := withSmallContracts()
	defer restore()

	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyContractRedisMirrorLoss\n")
		opts := defaultProxyTestOptions()
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		targetUrl, closeTarget := startLocalTarget(t, testTargetPortMirror)
		defer closeTarget()

		ctx := h.ctx

		client := newSocksProxyClient(t, h.signedProxyId, 60*time.Second)
		defer client.CloseIdleConnections()
		requireFetchData(t, "warmup", client, targetUrl, 1*testMib, 120*time.Second)
		fmt.Printf("[progress]warmup ok\n")

		// lose the mirrored state
		model.Testing_DeleteProvideMirror(ctx, h.providerClientId)
		for _, balanceId := range activeBalanceIds(ctx, h.pdNetworkId) {
			model.Testing_DeleteNetEscrow(ctx, balanceId)
		}
		fmt.Printf("[progress]redis mirror state deleted\n")

		// each 8MiB download spans multiple 4MiB-capped contracts, forcing fresh
		// contract creation (and so provide key lookups) after the loss
		for i := 0; i < 3; i += 1 {
			requireFetchData(t, fmt.Sprintf("post-loss-%d", i), client, targetUrl, 8*testMib, 120*time.Second)
		}
		fmt.Printf("[progress]contract creation survived mirror loss\n")
	})
}

// TestProxyClientReapSurvival runs the disconnected-client reaper with the
// production arguments while the proxy device is live and provisioned. A
// synthetic stale nested client must be reaped, while the device's clients and
// the proxy_device_config (which cascades on the client row) must survive, and
// traffic must keep flowing. (Provisioned proxy clients cannot recover from a
// reaped client_id, which is why the reap window is 30 days of auth_time.)
func TestProxyClientReapSurvival(t *testing.T) {
	if testing.Short() {
		return
	}

	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyClientReapSurvival\n")
		opts := defaultProxyTestOptions()
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		targetUrl, closeTarget := startLocalTarget(t, testTargetPortReap)
		defer closeTarget()

		ctx := h.ctx

		client := newSocksProxyClient(t, h.signedProxyId, 60*time.Second)
		defer client.CloseIdleConnections()
		requireFetchData(t, "warmup", client, targetUrl, 1*testMib, 120*time.Second)

		// a stale nested client: provisioned a month+ ago, never connected
		staleDeviceId := server.NewId()
		staleClientId := server.NewId()
		model.Testing_CreateDevice(ctx, h.pdNetworkId, staleDeviceId, staleClientId, "stale", "stale")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_client
					SET auth_time = $2, source_client_id = $3
					WHERE client_id = $1
				`,
				staleClientId,
				server.NowUtc().Add(-31*24*time.Hour),
				h.pdClientId,
			))
		})

		// the production worker arguments (see RemoveDisconnectedNetworkClients)
		minConnectionTime := server.NowUtc().Add(-8 * time.Hour)
		minClientTime := server.NowUtc().Add(-30 * 24 * time.Hour)
		model.RemoveDisconnectedNetworkClients(ctx, minConnectionTime, minClientTime)
		fmt.Printf("[progress]reaper ran\n")

		// the stale client is gone
		if _, err := model.FindClientNetwork(ctx, staleClientId); err == nil {
			t.Fatalf("stale client survived the reaper")
		}
		// the provisioned device client and the provider survive
		if _, err := model.FindClientNetwork(ctx, h.pdClientId); err != nil {
			t.Fatalf("proxy device client was reaped: %v", err)
		}
		if _, err := model.FindClientNetwork(ctx, h.providerClientId); err != nil {
			t.Fatalf("provider client was reaped: %v", err)
		}
		// the proxy device config (cascades on the client row) survives
		if proxyDeviceConfig := model.GetProxyDeviceConfig(ctx, h.proxyId); proxyDeviceConfig == nil {
			t.Fatalf("proxy device config was cascade-deleted by the reaper")
		}

		// and traffic still flows
		requireFetchData(t, "post-reap", client, targetUrl, 1*testMib, 120*time.Second)
		fmt.Printf("[progress]traffic survived the reaper\n")
	})
}
