//go:build crypto_regtest

package controller

// The crypto payment rail's wallet-daemon clients against real daemons on
// private regtest chains (mmm/ZCASH-CONTRACT.md §3.4): monerod with a
// view-only monero-wallet-rpc, and zebrad with a watch-only zecd, each next to
// a funded spending wallet that pays the wallet under test. The daemons are the
// pinned production release binaries, run by local/crypto-regtest (up.sh).
//
// Opt in twice: the build tag keeps a normal `go test ./controller/...` from
// compiling this file, and the tests skip unless URNETWORK_CRYPTO_REGTEST=1.
//
//	cd local/crypto-regtest && ./up.sh
//	URNETWORK_CRYPTO_REGTEST=1 go test -tags crypto_regtest -count=1 -v -timeout 60m \
//	    -run 'TestCryptoRegtest(Monero|Zcash)$' ./controller/
//
// TestCryptoRegtestEndToEnd also needs the test database: run it with the
// repository's test environment (local/crypto-regtest/README.md).
//
// The clients are built the way production builds them: the daemons' vault
// files (monero.yml, zcash.yml) are served through the vault, and
// cryptoWalletForAsset reads them.
//
// Monero regtest uses mainnet's address prefixes, so the xmr asset is
// configured as mainnet. The zcash client knows the mainnet and testnet chains
// only: while it has no regtest network, the zec asset is configured as
// testnet and the client talks to zecd through cryptoRegtestZecdShim, which
// changes two things in zecd's answers and nothing else (see there).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

const (
	cryptoRegtestEnv      = "URNETWORK_CRYPTO_REGTEST"
	cryptoRegtestStateEnv = "URNETWORK_CRYPTO_REGTEST_STATE"
)

// cryptoRegtestHarness is local/crypto-regtest/.state/harness.json. It holds
// regtest-only credentials; nothing here prints them.
type cryptoRegtestHarness struct {
	Monero struct {
		DaemonRpcUrl       string `json:"daemon_rpc_url"`
		FunderRpcUrl       string `json:"funder_rpc_url"`
		FunderLogin        string `json:"funder_login"`
		FunderAddress      string `json:"funder_address"`
		ViewRpcUrl         string `json:"view_rpc_url"`
		ViewLogin          string `json:"view_login"`
		ViewWalletPassword string `json:"view_wallet_password"`
		PrimaryAddress     string `json:"primary_address"`
		RestoreHeight      uint64 `json:"restore_height"`
		ViewContainer      string `json:"view_container"`
	} `json:"monero"`
	Zcash struct {
		NodeRpcUrl    string `json:"node_rpc_url"`
		FunderRpcUrl  string `json:"funder_rpc_url"`
		FunderLogin   string `json:"funder_login"`
		FunderAddress string `json:"funder_address"`
		ViewRpcUrl    string `json:"view_rpc_url"`
		ViewLogin     string `json:"view_login"`
		Birthday      uint64 `json:"birthday"`
		ViewContainer string `json:"view_container"`
	} `json:"zcash"`
}

func cryptoRegtestLoad(t testing.TB) *cryptoRegtestHarness {
	t.Helper()
	if os.Getenv(cryptoRegtestEnv) != "1" {
		t.Skipf("set %s=1 to run against the regtest harness (local/crypto-regtest/up.sh)", cryptoRegtestEnv)
	}
	path := os.Getenv(cryptoRegtestStateEnv)
	if path == "" {
		path = filepath.Join("..", "local", "crypto-regtest", ".state", "harness.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the regtest harness is not up (local/crypto-regtest/up.sh): %s", err)
	}
	harness := &cryptoRegtestHarness{}
	if err := json.Unmarshal(data, harness); err != nil {
		t.Fatalf("%s is unreadable", path)
	}
	return harness
}

func cryptoRegtestLogin(login string) (string, string) {
	user, password, _ := strings.Cut(login, ":")
	return user, password
}

// cryptoRegtestConfig is every coin on with the contract's numbers, xmr on
// mainnet's prefixes (Monero regtest uses them) and zec on zecNetwork.
func cryptoRegtestConfig(zecNetwork string) *CryptoPayConfig {
	config := cryptoTestConfig()
	config.Assets[model.CryptoAssetXmr].Network = cryptoNetworkMainnet
	config.Assets[model.CryptoAssetZec].Network = zecNetwork
	config.normalize()
	return config
}

// cryptoRegtestMoneroYml is vault/<env>/monero.yml for the wallet under test.
func cryptoRegtestMoneroYml(harness *cryptoRegtestHarness, primaryAddress string) string {
	user, password := cryptoRegtestLogin(harness.Monero.ViewLogin)
	var b strings.Builder
	b.WriteString("monero:\n")
	fmt.Fprintf(&b, "  wallet_rpc_url: %q\n", harness.Monero.ViewRpcUrl)
	fmt.Fprintf(&b, "  wallet_rpc_user: %q\n", user)
	fmt.Fprintf(&b, "  wallet_rpc_password: %q\n", password)
	fmt.Fprintf(&b, "  primary_address: %q\n", primaryAddress)
	fmt.Fprintf(&b, "  wallet_password: %q\n", harness.Monero.ViewWalletPassword)
	// monerod has no RPC login, as on our nodes
	fmt.Fprintf(&b, "  daemon_rpc_url: %q\n", harness.Monero.DaemonRpcUrl)
	return b.String()
}

// cryptoRegtestZcashYml is vault/<env>/zcash.yml for the wallet under test,
// with zebrad as the node when withNode.
func cryptoRegtestZcashYml(harness *cryptoRegtestHarness, walletRpcUrl string, withNode bool) string {
	user, password := cryptoRegtestLogin(harness.Zcash.ViewLogin)
	var b strings.Builder
	b.WriteString("zcash:\n")
	fmt.Fprintf(&b, "  wallet_rpc_url: %q\n", walletRpcUrl)
	fmt.Fprintf(&b, "  wallet_rpc_user: %q\n", user)
	fmt.Fprintf(&b, "  wallet_rpc_password: %q\n", password)
	if withNode {
		// Zebra's cookie auth is off in the harness, so no node login
		fmt.Fprintf(&b, "  node_rpc_url: %q\n", harness.Zcash.NodeRpcUrl)
	}
	return b.String()
}

// cryptoRegtestPushVault serves a vault file until the test ends.
func cryptoRegtestPushVault(t testing.TB, resource string, content string) {
	pop := server.Vault.PushSimpleResource(resource, []byte(content))
	t.Cleanup(pop)
}

// cryptoRegtestWalletFor is the coin's client as production builds it: the
// vault's settings, the config's network.
func cryptoRegtestWalletFor(t testing.TB, config *CryptoPayConfig, asset string) cryptoWallet {
	t.Helper()
	wallet, settings, err := cryptoWalletForAsset(config, asset)
	if err != nil {
		t.Fatalf("%s wallet: %s", asset, err)
	}
	connect.AssertEqual(t, settings.Asset, asset)
	return wallet
}

// cryptoRegtestEventually retries check every 2 s until it is done.
func cryptoRegtestEventually(t testing.TB, timeout time.Duration, what string, check func() (bool, error)) {
	t.Helper()
	start := time.Now()
	var lastErr error
	for {
		done, err := check()
		if done {
			return
		}
		if err != nil {
			lastErr = err
		}
		if timeout < time.Since(start) {
			t.Fatalf("timed out after %s waiting for %s (last error: %v)", timeout, what, lastErr)
		}
		time.Sleep(2 * time.Second)
	}
}

func cryptoRegtestFind(receipts []*cryptoWalletReceipt, txId string, outputKey string) *cryptoWalletReceipt {
	for _, receipt := range receipts {
		if receipt.TxId == txId && receipt.OutputKey == outputKey {
			return receipt
		}
	}
	return nil
}

func cryptoRegtestRandomTxId() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func cryptoRegtestDocker(t testing.TB, args ...string) {
	t.Helper()
	output, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %s: %s", args[0], err, strings.TrimSpace(string(output)))
	}
}

// cryptoRegtestIssued is an address the client issued.
type cryptoRegtestIssued struct {
	address string
	index   uint64
	// zec: the unified address's single receivers
	orchard string
	sapling string
}

// ----- Monero -----

type cryptoRegtestMonero struct {
	harness *cryptoRegtestHarness
	daemon  *cryptoRpcClient
	funder  *cryptoRpcClient
	// the wallet under test, for what the client does not do (refresh now,
	// get_address_index)
	view *cryptoRpcClient
}

func newCryptoRegtestMonero(harness *cryptoRegtestHarness) *cryptoRegtestMonero {
	funderUser, funderPassword := cryptoRegtestLogin(harness.Monero.FunderLogin)
	viewUser, viewPassword := cryptoRegtestLogin(harness.Monero.ViewLogin)
	return &cryptoRegtestMonero{
		harness: harness,
		daemon:  newCryptoRpcClient(harness.Monero.DaemonRpcUrl, "", "", cryptoRpcAuthDigest, "2.0"),
		funder:  newCryptoRpcClient(harness.Monero.FunderRpcUrl, funderUser, funderPassword, cryptoRpcAuthDigest, "2.0"),
		view:    newCryptoRpcClient(harness.Monero.ViewRpcUrl, viewUser, viewPassword, cryptoRpcAuthDigest, "2.0"),
	}
}

// top is monerod's top block (get_info's height is the block count).
func (self *cryptoRegtestMonero) top(t testing.TB, ctx context.Context) uint64 {
	t.Helper()
	var info struct {
		Height uint64 `json:"height"`
	}
	if err := self.daemon.call(ctx, "get_info", map[string]any{}, &info); err != nil {
		t.Fatalf("get_info: %s", err)
	}
	return info.Height - 1
}

// mine mines blocks to the funder and returns the new top block.
func (self *cryptoRegtestMonero) mine(t testing.TB, ctx context.Context, blocks int) uint64 {
	t.Helper()
	if err := self.daemon.call(ctx, "generateblocks", map[string]any{
		"amount_of_blocks": blocks,
		"wallet_address":   self.harness.Monero.FunderAddress,
	}, nil); err != nil {
		t.Fatalf("generateblocks: %s", err)
	}
	return self.top(t, ctx)
}

// refreshView has the wallet under test scan now instead of at its next
// auto-refresh (every 20 s).
func (self *cryptoRegtestMonero) refreshView(ctx context.Context) error {
	return self.view.call(ctx, "refresh", map[string]any{}, nil)
}

type cryptoRegtestOutput struct {
	address string
	atomic  uint64
}

// pay sends one transaction from the funder with these outputs.
func (self *cryptoRegtestMonero) pay(ctx context.Context, outputs ...cryptoRegtestOutput) (string, error) {
	if err := self.funder.call(ctx, "refresh", map[string]any{}, nil); err != nil {
		return "", err
	}
	destinations := []map[string]any{}
	for _, output := range outputs {
		destinations = append(destinations, map[string]any{"amount": output.atomic, "address": output.address})
	}
	var result struct {
		TxHash string `json:"tx_hash"`
	}
	if err := self.funder.call(ctx, "transfer", map[string]any{
		"destinations":  destinations,
		"account_index": 0,
		"priority":      0,
	}, &result); err != nil {
		return "", err
	}
	return strings.ToLower(result.TxHash), nil
}

// receipt waits until IncomingSince(minHeight) reports the transaction's
// output to the subaddress and ready says it is what the step waits for.
func (self *cryptoRegtestMonero) receipt(
	t testing.TB,
	ctx context.Context,
	wallet cryptoWallet,
	minHeight uint64,
	txId string,
	outputKey string,
	ready func(*cryptoWalletReceipt) bool,
) *cryptoWalletReceipt {
	t.Helper()
	var found *cryptoWalletReceipt
	cryptoRegtestEventually(t, 3*time.Minute, fmt.Sprintf("xmr receipt %s", outputKey), func() (bool, error) {
		receipts, _, err := wallet.IncomingSince(ctx, minHeight)
		if err != nil {
			return false, err
		}
		found = cryptoRegtestFind(receipts, txId, outputKey)
		if found != nil && ready(found) {
			return true, nil
		}
		return false, self.refreshView(ctx)
	})
	return found
}

func cryptoRegtestPending(receipt *cryptoWalletReceipt) bool {
	return receipt.BlockHeight == 0
}

func cryptoRegtestMined(receipt *cryptoWalletReceipt) bool {
	return 0 < receipt.BlockHeight
}

func TestCryptoRegtestMonero(t *testing.T) {
	harness := cryptoRegtestLoad(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	xmr := newCryptoRegtestMonero(harness)
	config := cryptoRegtestConfig(cryptoNetworkTestnet)
	cryptoRegtestPushVault(t, "monero.yml", cryptoRegtestMoneroYml(harness, harness.Monero.PrimaryAddress))
	wallet := cryptoRegtestWalletFor(t, config, model.CryptoAssetXmr).(*moneroWallet)

	t.Run("CheckNetwork", func(t *testing.T) {
		connect.AssertEqual(t, wallet.CheckNetwork(ctx), nil)
		// a wallet configured for another network is refused
		for _, network := range []string{cryptoNetworkStagenet, cryptoNetworkTestnet} {
			other := cryptoRegtestConfig(cryptoNetworkTestnet)
			other.Assets[model.CryptoAssetXmr].Network = network
			otherWallet := cryptoRegtestWalletFor(t, other, model.CryptoAssetXmr).(*moneroWallet)
			err := otherWallet.CheckNetwork(ctx)
			connect.AssertNotEqual(t, err, nil)
			t.Logf("%s refused: %s", network, err)
		}
		// a daemon serving another wallet than the vault names is refused
		pop := server.Vault.PushSimpleResource("monero.yml", []byte(cryptoRegtestMoneroYml(harness, harness.Monero.FunderAddress)))
		otherWallet := cryptoRegtestWalletFor(t, config, model.CryptoAssetXmr).(*moneroWallet)
		pop()
		err := otherWallet.CheckNetwork(ctx)
		connect.AssertNotEqual(t, err, nil)
		t.Logf("another wallet refused: %s", err)
	})

	t.Run("WrongCredentialsRefused", func(t *testing.T) {
		// the daemon's digest challenge, answered with the wrong password:
		// refused, and the error names no credential
		user, _ := cryptoRegtestLogin(harness.Monero.ViewLogin)
		wrong := *harness
		wrong.Monero.ViewLogin = user + ":wrong-regtest-password"
		pop := server.Vault.PushSimpleResource("monero.yml", []byte(cryptoRegtestMoneroYml(&wrong, harness.Monero.PrimaryAddress)))
		refused := cryptoRegtestWalletFor(t, config, model.CryptoAssetXmr).(*moneroWallet)
		pop()
		_, _, err := refused.IncomingSince(ctx, 0)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(err.Error(), "wrong-regtest-password"), false)
		t.Logf("refused: %s", err)
	})

	t.Run("Heights", func(t *testing.T) {
		connect.AssertEqual(t, xmr.refreshView(ctx), nil)
		top := xmr.top(t, ctx)
		walletHeight, nodeHeight, networkHeight, err := wallet.Heights(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, walletHeight, top)
		connect.AssertEqual(t, nodeHeight, top)
		// an offline regtest monerod reports no sync target: the network is the node
		connect.AssertEqual(t, networkHeight, top)

		mined := xmr.mine(t, ctx, 3)
		connect.AssertEqual(t, mined, top+3)
		connect.AssertEqual(t, xmr.refreshView(ctx), nil)
		walletHeight, nodeHeight, networkHeight, err = wallet.Heights(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, walletHeight, top+3)
		connect.AssertEqual(t, nodeHeight, top+3)
		connect.AssertEqual(t, networkHeight, top+3)
		_, incomingHeight, err := wallet.IncomingSince(ctx, top)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, incomingHeight, top+3)
	})

	issued := []*cryptoRegtestIssued{}
	t.Run("NewAddress", func(t *testing.T) {
		var last uint64
		seen := map[string]bool{}
		for range 4 {
			address, index, err := wallet.NewAddress(ctx, cryptoPoolAddressLabel)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, last < index, true)
			connect.AssertEqual(t, seen[address], false)
			// a mainnet-prefixed subaddress, as Monero regtest makes them
			connect.AssertEqual(t, len(address), 95)
			connect.AssertEqual(t, strings.HasPrefix(address, "8"), true)
			var where struct {
				Index moneroSubaddrIndex `json:"index"`
			}
			connect.AssertEqual(t, xmr.view.call(ctx, "get_address_index", map[string]any{"address": address}, &where), nil)
			connect.AssertEqual(t, where.Index.Major, uint32(0))
			connect.AssertEqual(t, uint64(where.Index.Minor), index)
			seen[address] = true
			last = index
			issued = append(issued, &cryptoRegtestIssued{address: address, index: index})
		}
		// the daemon holds the pool's own label, never user data
		var listed struct {
			Addresses []struct {
				Label        string `json:"label"`
				AddressIndex uint64 `json:"address_index"`
			} `json:"addresses"`
		}
		connect.AssertEqual(t, xmr.view.call(ctx, "get_address", map[string]any{
			"account_index": 0,
			"address_index": []uint64{issued[0].index},
		}, &listed), nil)
		connect.AssertEqual(t, len(listed.Addresses), 1)
		connect.AssertEqual(t, listed.Addresses[0].Label, cryptoPoolAddressLabel)
		// the wallet owns what it issued: nothing to widen (mmm/ZCASH-CONTRACT.md
		// §3.13.9). An address it does not own (the funder's) is answered -2
		// by get_address_index, and the lookahead is widened with the vault's
		// wallet FILE password, which set_subaddress_lookahead verifies
		connect.AssertEqual(t, wallet.EnsureLookahead(ctx, last, issued[len(issued)-1].address), nil)
		connect.AssertEqual(t, wallet.EnsureLookahead(ctx, last, harness.Monero.FunderAddress), nil)
	})
	requireIssued := func(t *testing.T, n int) {
		if len(issued) < n {
			t.Skip("needs the addresses NewAddress issues")
		}
	}

	var firstTxId string
	var firstHeight uint64
	var firstAmount uint64 = 123_456_789_012
	t.Run("MempoolThenConfirmed", func(t *testing.T) {
		requireIssued(t, 1)
		target := issued[0]
		key := fmt.Sprintf("0.%d", target.index)
		// the scanner asks from a height near the tip (its cursor less the reorg
		// window): mempool transfers must be reported from there too
		top := xmr.top(t, ctx)
		txId, err := xmr.pay(ctx, cryptoRegtestOutput{target.address, firstAmount})
		connect.AssertEqual(t, err, nil)

		pool := xmr.receipt(t, ctx, wallet, top, txId, key, cryptoRegtestPending)
		connect.AssertEqual(t, pool.BlockHeight, uint64(0))
		connect.AssertEqual(t, pool.Confirmations, uint64(0))
		connect.AssertEqual(t, pool.AmountAtomic, firstAmount)
		connect.AssertEqual(t, *pool.AddressIndex, target.index)
		connect.AssertEqual(t, pool.Address, target.address)
		connect.AssertEqual(t, pool.UnlockTime, uint64(0))
		connect.AssertEqual(t, pool.DoubleSpend, false)
		// re-verification finds it in the pool too
		receipts, found, err := wallet.Transaction(ctx, txId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, len(receipts), 1)
		connect.AssertEqual(t, receipts[0].AmountAtomic, firstAmount)
		connect.AssertEqual(t, receipts[0].BlockHeight, uint64(0))

		height := xmr.mine(t, ctx, 1)
		mined := xmr.receipt(t, ctx, wallet, height, txId, key, cryptoRegtestMined)
		connect.AssertEqual(t, mined.BlockHeight, height)
		connect.AssertEqual(t, mined.Confirmations, uint64(1))
		connect.AssertEqual(t, mined.AmountAtomic, firstAmount)
		connect.AssertEqual(t, *mined.AddressIndex, target.index)

		xmr.mine(t, ctx, 2)
		deeper := xmr.receipt(t, ctx, wallet, height, txId, key, func(receipt *cryptoWalletReceipt) bool {
			return receipt.Confirmations == 3
		})
		connect.AssertEqual(t, deeper.BlockHeight, height)
		receipts, found, err = wallet.Transaction(ctx, txId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, receipts[0].BlockHeight, height)
		connect.AssertEqual(t, receipts[0].Confirmations, uint64(3))
		firstTxId, firstHeight = txId, height
	})

	t.Run("OneTransactionTwoOutputsAndTwoAddresses", func(t *testing.T) {
		requireIssued(t, 3)
		a, b := issued[1], issued[2]
		keyA := fmt.Sprintf("0.%d", a.index)
		keyB := fmt.Sprintf("0.%d", b.index)
		wantA := uint64(10_000_000_001 + 20_000_000_002)
		wantB := uint64(40_000_000_004)
		txId, err := xmr.pay(ctx,
			cryptoRegtestOutput{a.address, 10_000_000_001},
			cryptoRegtestOutput{a.address, 20_000_000_002},
			cryptoRegtestOutput{b.address, wantB},
		)
		if err != nil {
			t.Logf("the funder could not pay one address twice in one transaction (%s); paying each address once", err)
			wantA = 10_000_000_001
			txId, err = xmr.pay(ctx, cryptoRegtestOutput{a.address, wantA}, cryptoRegtestOutput{b.address, wantB})
		}
		connect.AssertEqual(t, err, nil)

		top := xmr.top(t, ctx)
		poolA := xmr.receipt(t, ctx, wallet, top, txId, keyA, cryptoRegtestPending)
		poolB := xmr.receipt(t, ctx, wallet, top, txId, keyB, cryptoRegtestPending)
		connect.AssertEqual(t, poolA.AmountAtomic, wantA)
		connect.AssertEqual(t, poolB.AmountAtomic, wantB)

		height := xmr.mine(t, ctx, 1)
		minedA := xmr.receipt(t, ctx, wallet, height, txId, keyA, cryptoRegtestMined)
		minedB := xmr.receipt(t, ctx, wallet, height, txId, keyB, cryptoRegtestMined)
		connect.AssertEqual(t, minedA.AmountAtomic, wantA)
		connect.AssertEqual(t, *minedA.AddressIndex, a.index)
		connect.AssertEqual(t, minedB.AmountAtomic, wantB)
		connect.AssertEqual(t, *minedB.AddressIndex, b.index)
		connect.AssertEqual(t, minedA.BlockHeight, height)
		connect.AssertEqual(t, minedB.BlockHeight, height)
		t.Logf("one transaction: subaddress %d received %d piconero in one receipt, subaddress %d %d", a.index, minedA.AmountAtomic, b.index, minedB.AmountAtomic)
		// exactly one receipt per address of the transaction
		receipts, _, err := wallet.IncomingSince(ctx, height)
		connect.AssertEqual(t, err, nil)
		ofTx := 0
		for _, receipt := range receipts {
			if receipt.TxId == txId {
				ofTx += 1
			}
		}
		connect.AssertEqual(t, ofTx, 2)

		reread, found, err := wallet.Transaction(ctx, txId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, len(reread), 2)
		connect.AssertEqual(t, cryptoRegtestFind(reread, txId, keyA).AmountAtomic, wantA)
		connect.AssertEqual(t, cryptoRegtestFind(reread, txId, keyB).AmountAtomic, wantB)
	})

	t.Run("PrimaryAddressNamesNoInvoice", func(t *testing.T) {
		amount := uint64(1_000_000_007)
		txId, err := xmr.pay(ctx, cryptoRegtestOutput{harness.Monero.PrimaryAddress, amount})
		connect.AssertEqual(t, err, nil)
		receipt := xmr.receipt(t, ctx, wallet, xmr.top(t, ctx), txId, "0.0", cryptoRegtestPending)
		connect.AssertEqual(t, receipt.AmountAtomic, amount)
		connect.AssertEqual(t, receipt.AddressIndex, (*uint64)(nil))
		height := xmr.mine(t, ctx, 1)
		receipt = xmr.receipt(t, ctx, wallet, height, txId, "0.0", cryptoRegtestMined)
		connect.AssertEqual(t, receipt.AddressIndex, (*uint64)(nil))
	})

	t.Run("TransactionUnknown", func(t *testing.T) {
		receipts, found, err := wallet.Transaction(ctx, cryptoRegtestRandomTxId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, false)
		connect.AssertEqual(t, len(receipts), 0)
	})

	t.Run("IncomingSinceHeights", func(t *testing.T) {
		if firstTxId == "" {
			t.Skip("needs MempoolThenConfirmed")
		}
		key := fmt.Sprintf("0.%d", issued[0].index)
		// minHeight is inclusive
		receipts, _, err := wallet.IncomingSince(ctx, firstHeight)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, cryptoRegtestFind(receipts, firstTxId, key), nil)
		receipts, _, err = wallet.IncomingSince(ctx, firstHeight+1)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cryptoRegtestFind(receipts, firstTxId, key), (*cryptoWalletReceipt)(nil))
	})

	// Last: it restarts the wallet under test.
	t.Run("RestartKeepsAddresses", func(t *testing.T) {
		requireIssued(t, 1)
		highest := issued[len(issued)-1].index
		// SIGKILL, so no clean shutdown stores the wallet: only the store that
		// followed each create_address can have kept the subaddresses
		cryptoRegtestDocker(t, "kill", "--signal", "KILL", harness.Monero.ViewContainer)
		cryptoRegtestDocker(t, "start", harness.Monero.ViewContainer)
		// the client finds the restarted daemon again (a new connection and a
		// fresh digest challenge)
		firstErrors := 0
		cryptoRegtestEventually(t, 3*time.Minute, "the restarted view-only wallet-rpc", func() (bool, error) {
			_, _, _, err := wallet.Heights(ctx)
			if err != nil {
				firstErrors += 1
			}
			return err == nil, err
		})
		t.Logf("the client's calls before the restarted daemon answered: %d failed", firstErrors)

		for _, address := range issued {
			var where struct {
				Index moneroSubaddrIndex `json:"index"`
			}
			connect.AssertEqual(t, xmr.view.call(ctx, "get_address_index", map[string]any{"address": address.address}, &where), nil)
			connect.AssertEqual(t, uint64(where.Index.Minor), address.index)
		}
		// a client that does not know the pool's high-water mark still gets a
		// new index, not one again
		fresh := cryptoRegtestWalletFor(t, config, model.CryptoAssetXmr).(*moneroWallet)
		address, index, err := fresh.NewAddress(ctx, cryptoPoolAddressLabel)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, index, highest+1)
		for _, other := range issued {
			connect.AssertNotEqual(t, address, other.address)
		}
		// what the wallet received is still reported
		if firstTxId != "" {
			receipt := xmr.receipt(t, ctx, wallet, 0, firstTxId, fmt.Sprintf("0.%d", issued[0].index), cryptoRegtestMined)
			connect.AssertEqual(t, receipt.AmountAtomic, firstAmount)
			connect.AssertEqual(t, receipt.BlockHeight, firstHeight)
		}
		issued = append(issued, &cryptoRegtestIssued{address: address, index: index})
	})

	// A crash between create_address and store rolls the daemon's counter
	// back (mmm/ZCASH-WALLET-DAEMONS.md §1 item 19); the pool's high-water mark keeps the client
	// from handing an index out twice.
	t.Run("CrashBeforeStoreSkipsAhead", func(t *testing.T) {
		requireIssued(t, 1)
		subaddresses := func() int {
			var listed struct {
				Addresses []struct {
					AddressIndex uint64 `json:"address_index"`
				} `json:"addresses"`
			}
			connect.AssertEqual(t, xmr.view.call(ctx, "get_address", map[string]any{"account_index": 0}, &listed), nil)
			return len(listed.Addresses)
		}
		stored := subaddresses()
		// three addresses the pool would have stored, created without a store
		var created struct {
			AddressIndices []uint64 `json:"address_indices"`
		}
		connect.AssertEqual(t, xmr.view.call(ctx, "create_address", map[string]any{
			"account_index": 0,
			"count":         3,
			"label":         cryptoPoolAddressLabel,
		}, &created), nil)
		connect.AssertEqual(t, len(created.AddressIndices), 3)
		highWater := created.AddressIndices[2]
		connect.AssertEqual(t, subaddresses(), stored+3)

		cryptoRegtestDocker(t, "kill", "--signal", "KILL", harness.Monero.ViewContainer)
		cryptoRegtestDocker(t, "start", harness.Monero.ViewContainer)
		cryptoRegtestEventually(t, 3*time.Minute, "the restarted view-only wallet-rpc", func() (bool, error) {
			_, _, _, err := wallet.Heights(ctx)
			return err == nil, err
		})
		// the daemon forgot them
		connect.AssertEqual(t, subaddresses(), stored)

		fresh := cryptoRegtestWalletFor(t, config, model.CryptoAssetXmr).(*moneroWallet)
		fresh.SetAddressHighWater(highWater)
		address, index, err := fresh.NewAddress(ctx, cryptoPoolAddressLabel)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, index, highWater+1)
		var where struct {
			Index moneroSubaddrIndex `json:"index"`
		}
		connect.AssertEqual(t, xmr.view.call(ctx, "get_address_index", map[string]any{"address": address}, &where), nil)
		connect.AssertEqual(t, uint64(where.Index.Minor), highWater+1)
		// and the skip was stored: a second crash keeps it
		cryptoRegtestDocker(t, "kill", "--signal", "KILL", harness.Monero.ViewContainer)
		cryptoRegtestDocker(t, "start", harness.Monero.ViewContainer)
		cryptoRegtestEventually(t, 3*time.Minute, "the restarted view-only wallet-rpc", func() (bool, error) {
			_, _, _, err := wallet.Heights(ctx)
			return err == nil, err
		})
		connect.AssertEqual(t, subaddresses(), stored+4)
	})
}

// ----- Zcash -----

// cryptoRegtestZecdShim presents a regtest zecd as a testnet one, for a
// client that knows only the mainnet and testnet chains. It changes exactly
// two things in zecd's answers: getblockchaininfo's "chain":"regtest" reads
// "test", and a unified address zecd hands out (z_getaddressforaccount,
// getnewaddress) has its "uregtest1" prefix read "utest1" (the tests map it
// back before paying it). Requests, amounts, indices, heights, pools, txids
// and every other answer pass through as zecd sent them.
func cryptoRegtestZecdShim(t testing.TB, target string) string {
	chain := regexp.MustCompile(`("chain"\s*:\s*)"regtest"`)
	address := regexp.MustCompile(`"uregtest1`)
	shim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable request", http.StatusBadRequest)
			return
		}
		var request struct {
			Method string `json:"method"`
		}
		json.Unmarshal(body, &request)
		upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "bad upstream", http.StatusBadGateway)
			return
		}
		upstream.Header.Set("Content-Type", "application/json")
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			upstream.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(upstream)
		if err != nil {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			http.Error(w, "upstream unreadable", http.StatusBadGateway)
			return
		}
		switch request.Method {
		case "getblockchaininfo":
			answer = chain.ReplaceAll(answer, []byte(`${1}"test"`))
		case "z_getaddressforaccount", "getnewaddress":
			answer = address.ReplaceAll(answer, []byte(`"utest1`))
		}
		for _, header := range []string{"Content-Type", "WWW-Authenticate"} {
			if value := response.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		w.Write(answer)
	}))
	t.Cleanup(shim.Close)
	return shim.URL + "/"
}

type cryptoRegtestZcash struct {
	harness *cryptoRegtestHarness
	node    *cryptoRpcClient
	funder  *cryptoRpcClient
	// the wallet under test, for what the client does not do
	view *cryptoRpcClient
	// the network the client is configured with, and the url it calls zecd at
	network   string
	walletUrl string
	shimmed   bool
}

func newCryptoRegtestZcash(t testing.TB, harness *cryptoRegtestHarness) *cryptoRegtestZcash {
	funderUser, funderPassword := cryptoRegtestLogin(harness.Zcash.FunderLogin)
	viewUser, viewPassword := cryptoRegtestLogin(harness.Zcash.ViewLogin)
	zec := &cryptoRegtestZcash{
		harness: harness,
		node:    newCryptoRpcClient(harness.Zcash.NodeRpcUrl, "", "", cryptoRpcAuthBasic, "1.0"),
		funder:  newCryptoRpcClient(harness.Zcash.FunderRpcUrl, funderUser, funderPassword, cryptoRpcAuthBasic, "1.0"),
		view:    newCryptoRpcClient(harness.Zcash.ViewRpcUrl, viewUser, viewPassword, cryptoRpcAuthBasic, "1.0"),
	}
	if zcashChainName("regtest") == "regtest" && zcashUnifiedAddressPrefix("regtest") == "uregtest1" {
		zec.network = "regtest"
		zec.walletUrl = harness.Zcash.ViewRpcUrl
	} else {
		zec.network = cryptoNetworkTestnet
		zec.walletUrl = cryptoRegtestZecdShim(t, harness.Zcash.ViewRpcUrl)
		zec.shimmed = true
	}
	return zec
}

// realAddress is the address zecd handed out, for an address the client
// returned through the shim.
func (self *cryptoRegtestZcash) realAddress(address string) string {
	if self.shimmed && strings.HasPrefix(address, "utest1") {
		return "uregtest1" + strings.TrimPrefix(address, "utest1")
	}
	return address
}

func (self *cryptoRegtestZcash) tip(t testing.TB, ctx context.Context) uint64 {
	t.Helper()
	var height uint64
	if err := self.node.call(ctx, "getblockcount", []any{}, &height); err != nil {
		t.Fatalf("getblockcount: %s", err)
	}
	return height
}

// mine mines blocks (to the funder's transparent address) and waits until
// the wallet under test has scanned them. It returns the new tip.
func (self *cryptoRegtestZcash) mine(t testing.TB, ctx context.Context, blocks int) uint64 {
	t.Helper()
	if err := self.node.call(ctx, "generate", []any{blocks}, nil); err != nil {
		t.Fatalf("generate: %s", err)
	}
	tip := self.tip(t, ctx)
	self.waitScanned(t, ctx, tip)
	return tip
}

func (self *cryptoRegtestZcash) waitScanned(t testing.TB, ctx context.Context, height uint64) {
	t.Helper()
	cryptoRegtestEventually(t, 5*time.Minute, fmt.Sprintf("zecd to scan to %d", height), func() (bool, error) {
		var info struct {
			Blocks uint64 `json:"blocks"`
		}
		if err := self.view.call(ctx, "getblockchaininfo", []any{}, &info); err != nil {
			return false, err
		}
		return height <= info.Blocks, nil
	})
}

// zatoshiZec is an amount in zatoshi as zecd's 8-decimal ZEC number.
func cryptoRegtestZatoshiZec(zatoshi uint64) json.Number {
	return json.Number(fmt.Sprintf("%d.%08d", zatoshi/100_000_000, zatoshi%100_000_000))
}

// pay sends one transaction from the funder's shielded funds with these
// outputs (z_sendmany; zecd builds and proves it asynchronously).
func (self *cryptoRegtestZcash) pay(ctx context.Context, outputs ...cryptoRegtestOutput) (string, error) {
	amounts := []map[string]any{}
	for _, output := range outputs {
		amounts = append(amounts, map[string]any{"address": output.address, "amount": cryptoRegtestZatoshiZec(output.atomic)})
	}
	var opid string
	if err := self.funder.call(ctx, "z_sendmany", []any{self.harness.Zcash.FunderAddress, amounts, 1}, &opid); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var statuses []struct {
			Id     string `json:"id"`
			Status string `json:"status"`
			Result *struct {
				TxId string `json:"txid"`
			} `json:"result"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := self.funder.call(ctx, "z_getoperationstatus", []any{[]string{opid}}, &statuses); err != nil {
			return "", err
		}
		if len(statuses) == 1 {
			status := statuses[0]
			switch status.Status {
			case "success":
				// reap it
				self.funder.call(ctx, "z_getoperationresult", []any{[]string{opid}}, nil)
				if status.Result == nil || status.Result.TxId == "" {
					return "", fmt.Errorf("z_sendmany succeeded without a txid")
				}
				return strings.ToLower(status.Result.TxId), nil
			case "failed", "cancelled":
				self.funder.call(ctx, "z_getoperationresult", []any{[]string{opid}}, nil)
				if status.Error != nil {
					return "", fmt.Errorf("z_sendmany %s: %d %s", status.Status, status.Error.Code, status.Error.Message)
				}
				return "", fmt.Errorf("z_sendmany %s", status.Status)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("z_sendmany did not finish in 10 minutes")
		}
		time.Sleep(time.Second)
	}
}

// receipt waits until IncomingSince(minHeight) reports the transaction's
// outputs to the diversifier index and ready says it is what the step waits
// for.
func (self *cryptoRegtestZcash) receipt(
	t testing.TB,
	ctx context.Context,
	wallet cryptoWallet,
	minHeight uint64,
	txId string,
	index uint64,
	ready func(*cryptoWalletReceipt) bool,
) *cryptoWalletReceipt {
	t.Helper()
	outputKey := strconv.FormatUint(index, 10)
	var found *cryptoWalletReceipt
	cryptoRegtestEventually(t, 5*time.Minute, fmt.Sprintf("zec receipt %s", outputKey), func() (bool, error) {
		receipts, _, err := wallet.IncomingSince(ctx, minHeight)
		if err != nil {
			return false, err
		}
		found = cryptoRegtestFind(receipts, txId, outputKey)
		return found != nil && ready(found), nil
	})
	return found
}

func cryptoRegtestPoolHas(pools string, pool string) bool {
	return slices.Contains(strings.Split(pools, ","), pool)
}

func TestCryptoRegtestZcash(t *testing.T) {
	harness := cryptoRegtestLoad(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	zec := newCryptoRegtestZcash(t, harness)
	if zec.shimmed {
		t.Logf("NOTE: the zcash client has no regtest network (zcashChainName and zcashUnifiedAddressPrefix know main and test only); " +
			"its zec asset is configured as testnet and it reaches zecd through a shim that reads chain \"regtest\" as \"test\" and uregtest1 as utest1")
	}
	config := cryptoRegtestConfig(zec.network)
	cryptoRegtestPushVault(t, "zcash.yml", cryptoRegtestZcashYml(harness, zec.walletUrl, true))
	wallet := cryptoRegtestWalletFor(t, config, model.CryptoAssetZec).(*zcashWallet)

	t.Run("CheckNetwork", func(t *testing.T) {
		connect.AssertEqual(t, wallet.CheckNetwork(ctx), nil)
		// against zecd's own answer (no shim), a client configured for
		// mainnet or testnet refuses this regtest wallet
		pop := server.Vault.PushSimpleResource("zcash.yml", []byte(cryptoRegtestZcashYml(harness, harness.Zcash.ViewRpcUrl, true)))
		defer pop()
		for _, network := range []string{cryptoNetworkMainnet, cryptoNetworkTestnet} {
			other := cryptoRegtestConfig(network)
			otherWallet := cryptoRegtestWalletFor(t, other, model.CryptoAssetZec).(*zcashWallet)
			err := otherWallet.CheckNetwork(ctx)
			connect.AssertNotEqual(t, err, nil)
			t.Logf("%s refused: %s", network, err)
			// and so does every call that reads the chain
			_, _, err = otherWallet.IncomingSince(ctx, 0)
			connect.AssertNotEqual(t, err, nil)
		}
		if !zec.shimmed {
			direct := cryptoRegtestWalletFor(t, config, model.CryptoAssetZec).(*zcashWallet)
			connect.AssertEqual(t, direct.CheckNetwork(ctx), nil)
		}
	})

	t.Run("WrongCredentialsRefused", func(t *testing.T) {
		user, _ := cryptoRegtestLogin(harness.Zcash.ViewLogin)
		wrong := *harness
		wrong.Zcash.ViewLogin = user + ":wrong-regtest-password"
		pop := server.Vault.PushSimpleResource("zcash.yml", []byte(cryptoRegtestZcashYml(&wrong, zec.walletUrl, true)))
		refused := cryptoRegtestWalletFor(t, config, model.CryptoAssetZec).(*zcashWallet)
		pop()
		err := refused.CheckNetwork(ctx)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(err.Error(), "wrong-regtest-password"), false)
		t.Logf("refused: %s", err)
	})

	t.Run("Heights", func(t *testing.T) {
		tip := zec.tip(t, ctx)
		zec.waitScanned(t, ctx, tip)
		walletHeight, nodeHeight, networkHeight, err := wallet.Heights(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, walletHeight, tip)
		connect.AssertEqual(t, nodeHeight, tip)
		connect.AssertEqual(t, nodeHeight <= networkHeight, true)
		t.Logf("network height %d at tip %d: zebrad's estimatedheight, which regtest derives from the clock", networkHeight, tip)

		mined := zec.mine(t, ctx, 3)
		connect.AssertEqual(t, mined, tip+3)
		walletHeight, nodeHeight, _, err = wallet.Heights(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, walletHeight, tip+3)
		connect.AssertEqual(t, nodeHeight, tip+3)
		_, incomingHeight, err := wallet.IncomingSince(ctx, tip)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, incomingHeight, tip+3)
	})

	issued := []*cryptoRegtestIssued{}
	t.Run("NewAddress", func(t *testing.T) {
		var last uint64
		seen := map[string]bool{}
		for range 5 {
			address, index, err := wallet.NewAddress(ctx, cryptoPoolAddressLabel)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, last < index, true)
			real := zec.realAddress(address)
			connect.AssertEqual(t, strings.HasPrefix(real, "uregtest1"), true)
			connect.AssertEqual(t, seen[real], false)
			// a Sapling and an Orchard receiver, and no transparent one
			var receivers map[string]string
			connect.AssertEqual(t, zec.view.call(ctx, "z_listunifiedreceivers", []any{real}, &receivers), nil)
			connect.AssertNotEqual(t, receivers["orchard"], "")
			connect.AssertNotEqual(t, receivers["sapling"], "")
			connect.AssertEqual(t, receivers["p2pkh"], "")
			connect.AssertEqual(t, receivers["p2sh"], "")
			// the wallet owns it at that diversifier index
			var info struct {
				IsMine           bool        `json:"ismine"`
				DiversifierIndex json.Number `json:"diversifier_index"`
			}
			connect.AssertEqual(t, zec.view.call(ctx, "getaddressinfo", []any{real}, &info), nil)
			connect.AssertEqual(t, info.IsMine, true)
			connect.AssertEqual(t, info.DiversifierIndex.String(), strconv.FormatUint(index, 10))
			seen[real] = true
			last = index
			issued = append(issued, &cryptoRegtestIssued{address: real, index: index, orchard: receivers["orchard"], sapling: receivers["sapling"]})
		}
	})
	requireIssued := func(t *testing.T, n int) {
		if len(issued) < n {
			t.Skip("needs the addresses NewAddress issues")
		}
	}

	var firstTxId string
	var firstHeight uint64
	firstAmount := uint64(12_345_678)
	t.Run("MempoolThenConfirmed", func(t *testing.T) {
		requireIssued(t, 1)
		target := issued[0]
		// the whole unified address: the funder picks the receiver (Orchard)
		top := zec.tip(t, ctx)
		txId, err := zec.pay(ctx, cryptoRegtestOutput{target.address, firstAmount})
		connect.AssertEqual(t, err, nil)

		// asked from the tip, as the scanner does (its cursor less the reorg window)
		pool := zec.receipt(t, ctx, wallet, top, txId, target.index, cryptoRegtestPending)
		connect.AssertEqual(t, pool.BlockHeight, uint64(0))
		connect.AssertEqual(t, pool.Confirmations, uint64(0))
		connect.AssertEqual(t, pool.AmountAtomic, firstAmount)
		connect.AssertEqual(t, *pool.AddressIndex, target.index)
		t.Logf("mempool receipt: pool %q, address %.16s…", pool.Pool, pool.Address)
		receipts, found, err := wallet.Transaction(ctx, txId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, len(receipts), 1)
		connect.AssertEqual(t, receipts[0].AmountAtomic, firstAmount)
		connect.AssertEqual(t, receipts[0].BlockHeight, uint64(0))

		height := zec.mine(t, ctx, 1)
		mined := zec.receipt(t, ctx, wallet, height, txId, target.index, cryptoRegtestMined)
		connect.AssertEqual(t, mined.BlockHeight, height)
		connect.AssertEqual(t, mined.Confirmations, uint64(1))
		connect.AssertEqual(t, mined.AmountAtomic, firstAmount)
		connect.AssertEqual(t, *mined.AddressIndex, target.index)
		connect.AssertEqual(t, cryptoRegtestPoolHas(mined.Pool, cryptoZcashPoolTransparent), false)
		t.Logf("mined receipt: pool %q", mined.Pool)

		zec.mine(t, ctx, 2)
		deeper := zec.receipt(t, ctx, wallet, height, txId, target.index, func(receipt *cryptoWalletReceipt) bool {
			return receipt.Confirmations == 3
		})
		connect.AssertEqual(t, deeper.BlockHeight, height)
		receipts, found, err = wallet.Transaction(ctx, txId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, receipts[0].BlockHeight, height)
		connect.AssertEqual(t, receipts[0].Confirmations, uint64(3))
		firstTxId, firstHeight = txId, height
	})

	t.Run("ReceiversOfOneAddressAndTwoAddresses", func(t *testing.T) {
		requireIssued(t, 3)
		a, b := issued[1], issued[2]
		// the Orchard and the Sapling receiver of one unified address, and a
		// second address, in one transaction
		txId, err := zec.pay(ctx,
			cryptoRegtestOutput{a.orchard, 1_000_001},
			cryptoRegtestOutput{a.sapling, 2_000_002},
			cryptoRegtestOutput{b.address, 4_000_004},
		)
		connect.AssertEqual(t, err, nil)
		height := zec.mine(t, ctx, 1)
		receiptA := zec.receipt(t, ctx, wallet, height, txId, a.index, cryptoRegtestMined)
		receiptB := zec.receipt(t, ctx, wallet, height, txId, b.index, cryptoRegtestMined)
		// both receivers are keyed on the address's diversifier index, summed
		connect.AssertEqual(t, receiptA.AmountAtomic, uint64(3_000_003))
		connect.AssertEqual(t, *receiptA.AddressIndex, a.index)
		connect.AssertEqual(t, cryptoRegtestPoolHas(receiptA.Pool, cryptoZcashPoolSapling), true)
		connect.AssertEqual(t, cryptoRegtestPoolHas(receiptA.Pool, "orchard") || cryptoRegtestPoolHas(receiptA.Pool, "ironwood"), true)
		connect.AssertEqual(t, receiptB.AmountAtomic, uint64(4_000_004))
		connect.AssertEqual(t, *receiptB.AddressIndex, b.index)
		t.Logf("pools: %q and %q", receiptA.Pool, receiptB.Pool)

		receipts, _, err := wallet.IncomingSince(ctx, height)
		connect.AssertEqual(t, err, nil)
		ofTx := 0
		for _, receipt := range receipts {
			if receipt.TxId == txId {
				ofTx += 1
			}
		}
		connect.AssertEqual(t, ofTx, 2)
		reread, found, err := wallet.Transaction(ctx, txId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, len(reread), 2)
		connect.AssertEqual(t, cryptoRegtestFind(reread, txId, strconv.FormatUint(a.index, 10)).AmountAtomic, uint64(3_000_003))
		connect.AssertEqual(t, cryptoRegtestFind(reread, txId, strconv.FormatUint(b.index, 10)).AmountAtomic, uint64(4_000_004))
	})

	t.Run("OneAddressTwiceInOneTransaction", func(t *testing.T) {
		requireIssued(t, 4)
		c := issued[3]
		txId, err := zec.pay(ctx, cryptoRegtestOutput{c.address, 5_000_005}, cryptoRegtestOutput{c.address, 6_000_006})
		connect.AssertEqual(t, err, nil)
		height := zec.mine(t, ctx, 1)
		receipt := zec.receipt(t, ctx, wallet, height, txId, c.index, cryptoRegtestMined)
		connect.AssertEqual(t, receipt.AmountAtomic, uint64(11_000_011))
		connect.AssertEqual(t, *receipt.AddressIndex, c.index)
		t.Logf("one transaction paid diversifier index %d twice: one receipt of %d zatoshi, pool %q", c.index, receipt.AmountAtomic, receipt.Pool)
	})

	t.Run("SaplingReceiverOnly", func(t *testing.T) {
		requireIssued(t, 5)
		d := issued[4]
		txId, err := zec.pay(ctx, cryptoRegtestOutput{d.sapling, 7_000_007})
		connect.AssertEqual(t, err, nil)
		pool := zec.receipt(t, ctx, wallet, zec.tip(t, ctx), txId, d.index, cryptoRegtestPending)
		connect.AssertEqual(t, pool.AmountAtomic, uint64(7_000_007))
		height := zec.mine(t, ctx, 1)
		receipt := zec.receipt(t, ctx, wallet, height, txId, d.index, cryptoRegtestMined)
		connect.AssertEqual(t, receipt.AmountAtomic, uint64(7_000_007))
		connect.AssertEqual(t, *receipt.AddressIndex, d.index)
		connect.AssertEqual(t, receipt.Pool, cryptoZcashPoolSapling)
	})

	t.Run("TransactionUnknown", func(t *testing.T) {
		receipts, found, err := wallet.Transaction(ctx, cryptoRegtestRandomTxId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, found, false)
		connect.AssertEqual(t, len(receipts), 0)
	})

	t.Run("IncomingSinceHeights", func(t *testing.T) {
		if firstTxId == "" {
			t.Skip("needs MempoolThenConfirmed")
		}
		receipts, _, err := wallet.IncomingSince(ctx, firstHeight)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, cryptoRegtestFind(receipts, firstTxId, strconv.FormatUint(issued[0].index, 10)), nil)
		receipts, _, err = wallet.IncomingSince(ctx, firstHeight+1)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cryptoRegtestFind(receipts, firstTxId, strconv.FormatUint(issued[0].index, 10)), (*cryptoWalletReceipt)(nil))
	})
}

// ----- stage 2: the rail end to end, against the real daemons -----

// TestCryptoRegtestEndToEnd runs the pool refill, invoices from the
// controller, payments on the regtest chains, and the scanner against the
// real daemons and the test database: an exact payment is credited (the
// renewal and the Pro balance as the contract says), and an underpayment goes
// to review at expiry.
func TestCryptoRegtestEndToEnd(t *testing.T) {
	harness := cryptoRegtestLoad(t)
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		xmr := newCryptoRegtestMonero(harness)
		zec := newCryptoRegtestZcash(t, harness)

		config := cryptoRegtestConfig(zec.network)
		config.AddressPoolTarget = 3
		restoreConfig := setCryptoPayConfigForTest(config)
		t.Cleanup(restoreConfig)
		cryptoRegtestPushVault(t, "monero.yml", cryptoRegtestMoneroYml(harness, harness.Monero.PrimaryAddress))
		// no zebrad: on regtest its estimatedheight is the clock's guess,
		// millions of blocks ahead, and the lag check would stop sales. The
		// network height is then zecd's view of Zebra's tip.
		cryptoRegtestPushVault(t, "zcash.yml", cryptoRegtestZcashYml(harness, zec.walletUrl, false))
		resetCryptoAvailability()
		t.Cleanup(resetCryptoAvailability)

		taskSession := session.Testing_CreateClientSession(ctx, nil)
		for _, asset := range []string{model.CryptoAssetXmr, model.CryptoAssetZec} {
			refill, err := RunCryptoAddressRefill(taskSession, asset)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refill.Error, "")
			connect.AssertEqual(t, refill.Added, 3)
			scan, err := RunCryptoPaymentScan(taskSession, asset)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, scan.Error, "")
		}
		resetCryptoAvailability()

		xmrPay := func(address string, atomic uint64) (string, error) {
			return xmr.pay(ctx, cryptoRegtestOutput{address, atomic})
		}
		xmrMine := func(blocks int) {
			xmr.mine(t, ctx, blocks)
			connect.AssertEqual(t, xmr.refreshView(ctx), nil)
		}
		cryptoRegtestEndToEndAsset(t, ctx, taskSession, model.CryptoAssetXmr, func(address string) string { return address }, xmrPay, xmrMine)

		zecPay := func(address string, atomic uint64) (string, error) {
			return zec.pay(ctx, cryptoRegtestOutput{address, atomic})
		}
		zecMine := func(blocks int) {
			zec.mine(t, ctx, blocks)
		}
		cryptoRegtestEndToEndAsset(t, ctx, taskSession, model.CryptoAssetZec, zec.realAddress, zecPay, zecMine)
	})
}

func cryptoRegtestEndToEndAsset(
	t testing.TB,
	ctx context.Context,
	taskSession *session.ClientSession,
	asset string,
	realAddress func(string) string,
	pay func(address string, atomic uint64) (string, error),
	mine func(blocks int),
) {
	config := cryptoPayConfigFunc()
	creditConfirmations := int(config.Asset(asset).CreditConfirmations)
	networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoregtest"+asset)
	scan := func() *CryptoPaymentScanResult {
		result, err := RunCryptoPaymentScan(taskSession, asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		return result
	}
	status := func(result *CryptoPaymentIntentResult) *CryptoPaymentStatusResult {
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		return status
	}

	// a year of Pro, paid exactly
	result, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: asset, Plan: model.SolanaPlanYearly}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
	connect.AssertEqual(t, result.PriceUsd, float64(40))
	amountAtomic, err := strconv.ParseUint(result.AmountAtomic, 10, 64)
	connect.AssertEqual(t, err, nil)
	intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(result.IntentId))
	connect.AssertEqual(t, status(result).Status, CryptoPaymentStatusPending)

	_, err = pay(realAddress(result.Address), amountAtomic)
	connect.AssertEqual(t, err, nil)
	// seen unconfirmed: confirming, never credited at 0
	cryptoRegtestEventually(t, 5*time.Minute, asset+" payment seen by the scanner", func() (bool, error) {
		scan()
		return status(result).Status == CryptoPaymentStatusConfirming, nil
	})
	seen := status(result)
	connect.AssertEqual(t, seen.Received, result.Amount)
	connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, intent.IntentId).CreditedAt, (*time.Time)(nil))

	// the credit depth: paid
	mine(creditConfirmations)
	cryptoRegtestEventually(t, 5*time.Minute, asset+" payment credited", func() (bool, error) {
		scan()
		return status(result).Status == CryptoPaymentStatusPaid, nil
	})
	paid := status(result)
	connect.AssertEqual(t, paid.CreditedAt != nil, true)
	connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	renewal := cryptoDbRenewals(t, ctx, networkId, cryptoAssetMarket(asset))
	connect.AssertEqual(t, renewal.count, 1)
	connect.AssertEqual(t, renewal.transactionId, intent.IntentId.String())
	connect.AssertEqual(t, renewal.netRevenue, model.UsdToNanoCents(40))
	connect.AssertEqual(t, renewal.endTime.Sub(renewal.startTime), solanaPlanDuration(model.SolanaPlanYearly)+manualPaymentGracePeriod)
	proBalances := 0
	for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
		if balance.Pro {
			proBalances += 1
			connect.AssertEqual(t, balance.StartBalanceByteCount, RefreshSupporterTransferBalance)
		}
	}
	connect.AssertEqual(t, proBalances, 1)
	receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateCredited)
	connect.AssertEqual(t, receipts[0].AmountAtomic, int64(amountAtomic))
	connect.AssertEqual(t, *receipts[0].AddressIndex, *intent.AddressIndex)

	// a data pack paid half: short until its quote ends, then held as underpaid
	short, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: asset, ItemId: StripeItemData1Tib}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, short.Error, (*CryptoPaymentError)(nil))
	shortAtomic, err := strconv.ParseUint(short.AmountAtomic, 10, 64)
	connect.AssertEqual(t, err, nil)
	_, err = pay(realAddress(short.Address), shortAtomic/2)
	connect.AssertEqual(t, err, nil)
	mine(creditConfirmations)
	shortIntentId := server.RequireParseId(short.IntentId)
	cryptoRegtestEventually(t, 5*time.Minute, asset+" short payment recorded", func() (bool, error) {
		scan()
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, shortIntentId)
		return len(receipts) == 1 && receipts[0].Confirmations >= creditConfirmations, nil
	})
	connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, shortIntentId).CreditedAt, (*time.Time)(nil))
	// its quote ends (the half was seen ten minutes ago, the quote a minute ago)
	cryptoDbExec(ctx, `UPDATE crypto_payment_receipt SET first_seen_at = $2 WHERE intent_id = $1`, shortIntentId, server.NowUtc().Add(-10*time.Minute))
	cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET expires_at = $2 WHERE intent_id = $1`, shortIntentId, server.NowUtc().Add(-time.Minute))
	scan()
	held := status(short)
	connect.AssertEqual(t, held.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, held.ReviewReason, model.CryptoReviewReasonUnderpaid)
	for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, shortIntentId) {
		connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateReview)
		connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonUnderpaid)
	}
	// no data was granted for it
	for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
		connect.AssertEqual(t, balance.Pro, true)
	}
}
