// sn_substrate is the minimal substrate-side read the earnings flow needs:
// whether a coldkey exists on the subtensor chain (`POST /sn/wallet/validate`).
// The subtensor gateway serves the substrate JSON-RPC methods on the same
// endpoint as the EVM ones (server/st), so the read is one `state_getStorage`
// of the System.Account entry — no substrate client dependency.
package controller

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/crypto/blake2b"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	stconn "github.com/urnetwork/server/st"
)

const snSubstrateRequestTimeout = 6 * time.Second

// a per-key answer is stable for minutes: an address does not stop existing,
// and a brand-new one only needs to be re-asked once it is funded
const snWalletChainCacheTtl = 10 * time.Minute
const snWalletChainCacheMax = 4096

// the validate endpoint is unauthenticated; bound the chain lookups one
// source address can trigger per minute (cached answers are free)
const snWalletValidateIpLimitPerMinute = 30

var snSubstrateHttpClient = &http.Client{Timeout: snSubstrateRequestTimeout}

type snJsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type snJsonRpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *snJsonRpcError `json:"error"`
}

// twox128 is substrate's Twox128 hasher: xxhash64 with seeds 0 and 1,
// little-endian, concatenated.
func twox128(data []byte) []byte {
	out := make([]byte, 16)
	h0 := xxhash.NewWithSeed(0)
	h0.Write(data)
	binary.LittleEndian.PutUint64(out[0:8], h0.Sum64())
	h1 := xxhash.NewWithSeed(1)
	h1.Write(data)
	binary.LittleEndian.PutUint64(out[8:16], h1.Sum64())
	return out
}

// snSubstrateAccountKey is the System.Account storage key of a public key:
// twox128("System") ‖ twox128("Account") ‖ blake2_128(pubkey) ‖ pubkey.
func snSubstrateAccountKey(pubkey [32]byte) []byte {
	key := make([]byte, 0, 16+16+16+32)
	key = append(key, twox128([]byte("System"))...)
	key = append(key, twox128([]byte("Account"))...)
	h, err := blake2b.New(16, nil)
	if err != nil {
		panic(err)
	}
	h.Write(pubkey[:])
	key = append(key, h.Sum(nil)...)
	key = append(key, pubkey[:]...)
	return key
}

// snSubstrateRpcUrls lists the JSON-RPC endpoints to try, primary gateway
// first, then the lightnode. A deployment without the st connection (no
// st.yml, no vault) yields none — the caller fails open.
func snSubstrateRpcUrls() (urls []string) {
	defer func() {
		if r := recover(); r != nil {
			urls = nil
		}
	}()
	urls = append(urls, stconn.RpcUrls()...)
	return append(urls, stconn.LightnodeRpcUrls()...)
}

// snSubstrateStorageExists reports whether a storage key has a value on the
// chain, trying each configured endpoint in order.
func snSubstrateStorageExists(ctx context.Context, key []byte) (bool, error) {
	urls := snSubstrateRpcUrls()
	if len(urls) == 0 {
		return false, errors.New("subtensor rpc is not configured")
	}
	var lastErr error
	for _, url := range urls {
		exists, err := snSubstrateGetStorage(ctx, url, key)
		if err == nil {
			return exists, nil
		}
		lastErr = err
	}
	return false, lastErr
}

func snSubstrateGetStorage(ctx context.Context, url string, key []byte) (bool, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "state_getStorage",
		"params":  []string{"0x" + hex.EncodeToString(key)},
	})
	if err != nil {
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, snSubstrateRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := snSubstrateHttpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("substrate rpc %s: http %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	var out snJsonRpcResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, err
	}
	if out.Error != nil {
		return false, fmt.Errorf("substrate rpc %s: %s", url, out.Error.Message)
	}
	return snStorageValuePresent(out.Result), nil
}

// snStorageValuePresent decodes a `state_getStorage` result: null (or an
// empty hex string) means the key has no value.
func snStorageValuePresent(result json.RawMessage) bool {
	trimmed := bytes.TrimSpace(result)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	return 2 < len(value)
}

type snWalletChainEntry struct {
	exists bool
	expiry time.Time
}

var snWalletChainCache = struct {
	lock    sync.Mutex
	entries map[[32]byte]snWalletChainEntry
}{entries: map[[32]byte]snWalletChainEntry{}}

// SnWalletExistsOnChain reports whether the coldkey has a System.Account
// entry on the subtensor chain (funded above the existential deposit or has
// transacted). Answers are cached per key; an rpc failure is returned as an
// error so callers can fail open.
func SnWalletExistsOnChain(ctx context.Context, pubkey [32]byte) (bool, error) {
	now := server.NowUtc()
	cached := func() (snWalletChainEntry, bool) {
		snWalletChainCache.lock.Lock()
		defer snWalletChainCache.lock.Unlock()
		entry, ok := snWalletChainCache.entries[pubkey]
		if ok && now.Before(entry.expiry) {
			return entry, true
		}
		return snWalletChainEntry{}, false
	}
	if entry, ok := cached(); ok {
		return entry.exists, nil
	}
	exists, err := snSubstrateStorageExists(ctx, snSubstrateAccountKey(pubkey))
	if err != nil {
		return false, err
	}
	snWalletChainCache.lock.Lock()
	defer snWalletChainCache.lock.Unlock()
	if snWalletChainCacheMax <= len(snWalletChainCache.entries) {
		snWalletChainCache.entries = map[[32]byte]snWalletChainEntry{}
	}
	snWalletChainCache.entries[pubkey] = snWalletChainEntry{exists: exists, expiry: now.Add(snWalletChainCacheTtl)}
	return exists, nil
}

var snWalletValidateLimiter = struct {
	lock   sync.Mutex
	window time.Time
	counts map[[32]byte]int
}{counts: map[[32]byte]int{}}

// snWalletValidateAllow is a fixed one-minute window per source address for
// the unauthenticated validate endpoint's chain lookups. Unknown source
// addresses (tests, local calls) are allowed.
func snWalletValidateAllow(clientSession *session.ClientSession) (allow bool) {
	// the address hash needs the client secret; a deployment without it
	// (tests, local runs) is not rate limited rather than broken
	defer func() {
		if r := recover(); r != nil {
			allow = true
		}
	}()
	if clientSession.ClientAddress == "" {
		return true
	}
	clientAddressHash, _, err := clientSession.ClientAddressHashPort()
	if err != nil {
		return true
	}
	now := server.NowUtc()
	snWalletValidateLimiter.lock.Lock()
	defer snWalletValidateLimiter.lock.Unlock()
	if snWalletValidateLimiter.window.IsZero() || time.Minute <= now.Sub(snWalletValidateLimiter.window) {
		snWalletValidateLimiter.window = now
		snWalletValidateLimiter.counts = map[[32]byte]int{}
	}
	snWalletValidateLimiter.counts[clientAddressHash] += 1
	return snWalletValidateLimiter.counts[clientAddressHash] <= snWalletValidateIpLimitPerMinute
}
