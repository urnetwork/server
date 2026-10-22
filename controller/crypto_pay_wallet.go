package controller

// The wallet daemons behind the crypto payment rail
// (mmm/ZCASH-CONTRACT.md §3.4, mmm/ZCASH.md §4.4).
//
// Each scanned coin has one daemon holding only a viewing key:
// monero-wallet-rpc restored view-only, and zecd watch-only from the unified
// full viewing key. Neither can spend. The server talks to them through one
// interface, so the scanner, the address refill and the reconcile legs are
// written once and tested against an in-memory fake.
//
// Only the taskworker calls a daemon. A client request never does: addresses
// come from the pre-derived pool, and status is read from Postgres.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// cryptoWallet is one coin's view-only wallet daemon.
type cryptoWallet interface {
	// a fresh receive address for an invoice; label is the pool's own name
	// for it, never user data
	NewAddress(ctx context.Context, label string) (address string, index uint64, err error)
	// every incoming transfer (pool and confirmed) at or above minHeight, plus
	// the wallet's own height
	IncomingSince(ctx context.Context, minHeight uint64) (receipts []*cryptoWalletReceipt, walletHeight uint64, err error)
	// one transaction's incoming outputs, for re-verification; found = false
	// when the wallet no longer has it
	Transaction(ctx context.Context, txId string) (receipts []*cryptoWalletReceipt, found bool, err error)
	// the wallet's height, the node's height and the network's best known
	// height, for the lag check
	Heights(ctx context.Context) (walletHeight uint64, nodeHeight uint64, networkHeight uint64, err error)
}

// cryptoWalletReceipt is what the daemon reports one transaction paid one of
// the wallet's addresses: every output of the transaction to that address,
// summed (monero-wallet-rpc already sums them per subaddress; zecd lists each
// output, and the client sums them per diversifier index).
type cryptoWalletReceipt struct {
	TxId string
	// xmr: "<major>.<minor>" (the subaddress); zec: the diversifier index, or
	// "t:<vout>" for a transparent output
	OutputKey string
	// the address as the daemon reports it (zecd: the single receiver that
	// was paid, not necessarily the unified address handed out)
	Address string
	// the index that identifies the address in the wallet: the Monero
	// subaddress minor index in account 0, the Zcash diversifier index. nil
	// when the payment reached no address of ours that an index names (another
	// Monero account, a transparent output)
	AddressIndex *uint64
	AmountAtomic uint64
	// 0 while in the mempool
	BlockHeight   uint64
	Confirmations uint64
	// nonzero: the output is time-locked, and never counted
	UnlockTime uint64
	// the daemon flagged the transaction as a double spend
	DoubleSpend bool
	// zec: the pool the output landed in (orchard, sapling, transparent);
	// "" for xmr
	Pool string
	// the block's time, or when the daemon first saw a mempool transfer
	Timestamp time.Time
}

// Zcash pools as the receipts name them.
const (
	cryptoZcashPoolOrchard     = "orchard"
	cryptoZcashPoolSapling     = "sapling"
	cryptoZcashPoolTransparent = "transparent"
)

// cryptoWalletFor is the daemon client for a coin. Replaceable only by tests,
// which run the scanner, the refill and the reconcile legs against a fake.
var cryptoWalletFor = func(asset string, settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) (cryptoWallet, error) {
	switch asset {
	case model.CryptoAssetXmr:
		return newMoneroWallet(settings, assetConfig), nil
	case model.CryptoAssetZec:
		return newZcashWallet(settings, assetConfig), nil
	}
	return nil, fmt.Errorf("%s has no wallet daemon", asset)
}

// cryptoWalletForAsset builds the coin's daemon client from the vault.
func cryptoWalletForAsset(config *CryptoPayConfig, asset string) (cryptoWallet, *cryptoWalletSettings, error) {
	assetConfig := config.Asset(asset)
	if assetConfig == nil {
		return nil, nil, fmt.Errorf("%s is not configured", asset)
	}
	settings, err := cryptoWalletSettingsFunc(asset)
	if err != nil {
		return nil, nil, err
	}
	wallet, err := cryptoWalletFor(asset, settings, assetConfig)
	if err != nil {
		return nil, nil, err
	}
	return wallet, settings, nil
}

// ----- JSON-RPC over HTTP, with digest (monero-wallet-rpc) or basic (zecd,
// zebrad) authentication -----

// A daemon's answer is read up to this many bytes.
const cryptoRpcMaxResponseBytes = 64 * 1024 * 1024

// One RPC call is abandoned after this long.
const cryptoRpcTimeout = 60 * time.Second

type cryptoRpcAuth int

const (
	cryptoRpcAuthBasic cryptoRpcAuth = iota
	cryptoRpcAuthDigest
)

// cryptoRpcError is an error the daemon answered (not a transport failure).
type cryptoRpcError struct {
	Method  string
	Code    int
	Message string
}

func (self *cryptoRpcError) Error() string {
	return fmt.Sprintf("%s: rpc error %d: %s", self.Method, self.Code, self.Message)
}

// cryptoRpcClient calls one daemon. It never logs or returns the credentials
// or the url: errors name the method and the status only.
type cryptoRpcClient struct {
	url      string
	user     string
	password string
	auth     cryptoRpcAuth
	// "2.0" for monero-wallet-rpc and monerod, "1.0" for the bitcoind dialect
	// (zecd, zebrad)
	jsonRpcVersion string
	httpClient     *http.Client

	digestLock sync.Mutex
	digest     *cryptoDigestChallenge
	digestNc   int
}

func newCryptoRpcClient(rpcUrl string, user string, password string, auth cryptoRpcAuth, jsonRpcVersion string) *cryptoRpcClient {
	httpClient := server.DefaultHttpClient()
	if auth == cryptoRpcAuthDigest {
		// monero-wallet-rpc keeps the digest nonce and its counter per TCP
		// connection, and serves one request at a time anyway: one pinned
		// keep-alive connection keeps the challenge valid between calls
		transport := &http.Transport{
			DialContext:         server.NewDialer(server.DefaultHttpConnectTimeout).DialContext,
			TLSHandshakeTimeout: server.DefaultHttpTlsTimeout,
			MaxConnsPerHost:     1,
			MaxIdleConnsPerHost: 1,
		}
		httpClient = &http.Client{Transport: transport, Timeout: server.DefaultHttpTimeout}
	}
	return &cryptoRpcClient{
		url:            rpcUrl,
		user:           user,
		password:       password,
		auth:           auth,
		jsonRpcVersion: jsonRpcVersion,
		httpClient:     httpClient,
	}
}

type cryptoRpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call makes one JSON-RPC call and decodes its result into result (nil to
// ignore it).
func (self *cryptoRpcClient) call(ctx context.Context, method string, params any, result any) error {
	callCtx, cancel := context.WithTimeout(ctx, cryptoRpcTimeout)
	defer cancel()
	request := map[string]any{
		"jsonrpc": self.jsonRpcVersion,
		"id":      "0",
		"method":  method,
	}
	if params != nil {
		request["params"] = params
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	response, err := self.post(callCtx, body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	// monero-wallet-rpc answers an rpc error with a 200; zecd with a 400, 404
	// or 500, and the same json envelope in the body. Anything that does not
	// decode as an envelope (a 503 queue-full text, an html 404) is the status.
	var rpcResponse cryptoRpcResponse
	decoder := json.NewDecoder(bytes.NewReader(response.body))
	decoder.UseNumber()
	if err := decoder.Decode(&rpcResponse); err != nil {
		return fmt.Errorf("%s: undecodable answer (http status %d)", method, response.status)
	}
	if rpcResponse.Error != nil {
		return &cryptoRpcError{Method: method, Code: rpcResponse.Error.Code, Message: rpcResponse.Error.Message}
	}
	if response.status != http.StatusOK {
		return fmt.Errorf("%s: http status %d", method, response.status)
	}
	if result == nil {
		return nil
	}
	resultDecoder := json.NewDecoder(bytes.NewReader(rpcResponse.Result))
	// amounts and indices stay exact: a json.Number field decodes as text
	resultDecoder.UseNumber()
	if err := resultDecoder.Decode(result); err != nil {
		return fmt.Errorf("%s: undecodable result: %w", method, err)
	}
	return nil
}

type cryptoRpcHttpResponse struct {
	status int
	body   []byte
}

func (self *cryptoRpcClient) post(ctx context.Context, body []byte) (*cryptoRpcHttpResponse, error) {
	switch self.auth {
	case cryptoRpcAuthDigest:
		// with the challenge in hand, one round trip; a stale or missing one
		// costs a second
		if authorization, ok := self.digestAuthorization(http.MethodPost); ok {
			response, challenge, err := self.postOnce(ctx, body, authorization)
			if err != nil {
				return nil, err
			}
			if challenge == "" {
				return response, nil
			}
			self.setDigestChallenge(challenge)
		} else {
			response, challenge, err := self.postOnce(ctx, body, "")
			if err != nil {
				return nil, err
			}
			if challenge == "" {
				return response, nil
			}
			self.setDigestChallenge(challenge)
		}
		authorization, ok := self.digestAuthorization(http.MethodPost)
		if !ok {
			return nil, errors.New("unusable digest challenge")
		}
		response, challenge, err := self.postOnce(ctx, body, authorization)
		if err != nil {
			return nil, err
		}
		if challenge != "" {
			return nil, errors.New("digest authentication refused")
		}
		return response, nil
	default:
		response, challenge, err := self.postOnce(ctx, body, "Basic "+basicAuthValue(self.user, self.password))
		if err != nil {
			return nil, err
		}
		if challenge != "" {
			return nil, errors.New("basic authentication refused")
		}
		return response, nil
	}
}

// postOnce sends the request with the authorization given. challenge is the
// WWW-Authenticate value of a 401, "" otherwise.
func (self *cryptoRpcClient) postOnce(ctx context.Context, body []byte, authorization string) (*cryptoRpcHttpResponse, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.url, bytes.NewReader(body))
	if err != nil {
		return nil, "", errors.New("invalid rpc url")
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := self.httpClient.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// the url can carry credentials; report only what failed
			return nil, "", fmt.Errorf("transport: %w", urlErr.Err)
		}
		return nil, "", fmt.Errorf("transport: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, cryptoRpcMaxResponseBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("transport: %w", err)
	}
	if cryptoRpcMaxResponseBytes < len(responseBody) {
		return nil, "", errors.New("answer too large")
	}
	if response.StatusCode == http.StatusUnauthorized {
		// monero-wallet-rpc offers two challenges, MD5 and MD5-sess; this
		// client answers the MD5 one
		challenges := response.Header.Values("WWW-Authenticate")
		challenge := "unauthorized"
		for _, candidate := range challenges {
			if _, ok := parseCryptoDigestChallenge(candidate); ok {
				challenge = candidate
				break
			}
		}
		if challenge == "unauthorized" && 0 < len(challenges) {
			challenge = challenges[0]
		}
		return nil, challenge, nil
	}
	return &cryptoRpcHttpResponse{status: response.StatusCode, body: responseBody}, "", nil
}

func basicAuthValue(user string, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// ----- HTTP digest (RFC 7616, MD5, qop=auth), as monero-wallet-rpc speaks it -----

type cryptoDigestChallenge struct {
	realm     string
	nonce     string
	opaque    string
	qop       string
	algorithm string
}

// parseCryptoDigestChallenge reads a `Digest k="v", ...` challenge. ok =
// false for anything that is not a usable MD5 digest challenge.
func parseCryptoDigestChallenge(header string) (*cryptoDigestChallenge, bool) {
	header = strings.TrimSpace(header)
	if len(header) < 7 || !strings.EqualFold(header[:7], "digest ") {
		return nil, false
	}
	challenge := &cryptoDigestChallenge{}
	for _, part := range splitCryptoDigestParams(header[7:]) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch key {
		case "realm":
			challenge.realm = value
		case "nonce":
			challenge.nonce = value
		case "opaque":
			challenge.opaque = value
		case "qop":
			// offered as a list; this client speaks auth
			for _, qop := range strings.Split(value, ",") {
				if strings.TrimSpace(qop) == "auth" {
					challenge.qop = "auth"
				}
			}
		case "algorithm":
			challenge.algorithm = value
		}
	}
	if challenge.nonce == "" {
		return nil, false
	}
	if challenge.algorithm != "" && !strings.EqualFold(challenge.algorithm, "MD5") {
		return nil, false
	}
	return challenge, true
}

// splitCryptoDigestParams splits on commas outside quotes.
func splitCryptoDigestParams(s string) []string {
	parts := []string{}
	quoted := false
	start := 0
	for i, r := range s {
		switch r {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func (self *cryptoRpcClient) setDigestChallenge(header string) {
	self.digestLock.Lock()
	defer self.digestLock.Unlock()
	if challenge, ok := parseCryptoDigestChallenge(header); ok {
		self.digest = challenge
		self.digestNc = 0
	} else {
		self.digest = nil
	}
}

// digestAuthorization is the Authorization value for the next request under
// the current challenge, or ok = false without one.
func (self *cryptoRpcClient) digestAuthorization(method string) (string, bool) {
	self.digestLock.Lock()
	defer self.digestLock.Unlock()
	if self.digest == nil {
		return "", false
	}
	self.digestNc += 1
	requestUrl, err := url.Parse(self.url)
	if err != nil {
		return "", false
	}
	uri := requestUrl.RequestURI()
	return cryptoDigestAuthorization(self.digest, self.user, self.password, method, uri, self.digestNc, cryptoDigestCnonce()), true
}

// cryptoDigestAuthorization computes one digest Authorization value.
func cryptoDigestAuthorization(challenge *cryptoDigestChallenge, user string, password string, method string, uri string, nc int, cnonce string) string {
	ha1 := md5Hex(user + ":" + challenge.realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	ncText := fmt.Sprintf("%08x", nc)
	var response string
	if challenge.qop == "auth" {
		response = md5Hex(ha1 + ":" + challenge.nonce + ":" + ncText + ":" + cnonce + ":auth:" + ha2)
	} else {
		response = md5Hex(ha1 + ":" + challenge.nonce + ":" + ha2)
	}
	parts := []string{
		fmt.Sprintf(`username="%s"`, user),
		fmt.Sprintf(`realm="%s"`, challenge.realm),
		fmt.Sprintf(`nonce="%s"`, challenge.nonce),
		fmt.Sprintf(`uri="%s"`, uri),
		`algorithm=MD5`,
		fmt.Sprintf(`response="%s"`, response),
	}
	if challenge.qop == "auth" {
		parts = append(parts, `qop=auth`, fmt.Sprintf(`nc=%s`, ncText), fmt.Sprintf(`cnonce="%s"`, cnonce))
	}
	if challenge.opaque != "" {
		parts = append(parts, fmt.Sprintf(`opaque="%s"`, challenge.opaque))
	}
	return "Digest " + strings.Join(parts, ", ")
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cryptoDigestCnonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
