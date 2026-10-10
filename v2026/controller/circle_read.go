package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

const circleReadPageSize = 50
const maximumCircleWallets = 256
const maximumCircleTokens = 512
const maximumCircleObservationPages = 128

// Circle's response Link, rather than a wallet/token id, carries the next
// cursor. Retain the endpoint and fixed filters before forwarding credentials.
func circleReadNext(first string, header http.Header) (string, error) {
	values := header.Values("Link")
	size := 0
	for _, value := range values {
		size += len(value)
	}
	if size > 16*1024 || len(values) > 16 {
		return "", fmt.Errorf("Circle pagination headers exceed capacity")
	}
	base, err := url.Parse(first)
	if err != nil {
		return "", err
	}
	var next string
	links := 0
	for _, value := range values {
		for value = strings.TrimSpace(value); value != ""; value = strings.TrimSpace(value) {
			links++
			if links > 16 || !strings.HasPrefix(value, "<") {
				return "", fmt.Errorf("invalid or excessive Circle pagination links")
			}
			end := strings.IndexByte(value, '>')
			if end < 1 {
				return "", fmt.Errorf("invalid Circle pagination target")
			}
			target := value[1:end]
			parameters := value[end+1:]
			quoted, escaped, split := false, false, len(parameters)
			for i, ch := range parameters {
				if escaped {
					escaped = false
				} else if ch == '\\' && quoted {
					escaped = true
				} else if ch == '"' {
					quoted = !quoted
				} else if ch == ',' && !quoted {
					split = i
					break
				}
			}
			if quoted || escaped {
				return "", fmt.Errorf("invalid Circle pagination parameters")
			}
			_, attrs, err := mime.ParseMediaType("application/link" + strings.TrimSpace(parameters[:split]))
			if err != nil || attrs["rel"] == "" {
				return "", fmt.Errorf("invalid Circle pagination relation")
			}
			value = ""
			if split < len(parameters) {
				value = parameters[split+1:]
			}
			for _, rel := range strings.Fields(attrs["rel"]) {
				if rel != "next" {
					continue
				}
				if next != "" || attrs["anchor"] != "" || len(target) > 4096 {
					return "", fmt.Errorf("ambiguous Circle pagination target")
				}
				uri, err := url.Parse(target)
				if err != nil || uri.Scheme != "https" || uri.Host != base.Host || uri.User != nil || uri.Fragment != "" || uri.Opaque != "" || uri.EscapedPath() != base.EscapedPath() {
					return "", fmt.Errorf("Circle pagination changed the authenticated endpoint")
				}
				query, err := url.ParseQuery(uri.RawQuery)
				if err != nil {
					return "", fmt.Errorf("invalid Circle pagination query")
				}
				for key, vs := range query {
					if len(vs) != 1 || vs[0] == "" {
						return "", fmt.Errorf("ambiguous Circle pagination query")
					}
					switch key {
					case "pageSize":
						count, err := strconv.Atoi(vs[0])
						if err != nil || count < 1 || count > circleReadPageSize {
							return "", fmt.Errorf("Circle pagination changed the page bound")
						}
					case "pageAfter", "pageBefore":
					default:
						if base.Query().Get(key) != vs[0] {
							return "", fmt.Errorf("Circle pagination changed fixed filters")
						}
					}
				}
				for key, vs := range base.Query() {
					if key != "pageSize" && query.Get(key) != vs[0] {
						return "", fmt.Errorf("Circle pagination lost fixed filters")
					}
				}
				if (query.Get("pageAfter") == "") == (query.Get("pageBefore") == "") {
					return "", fmt.Errorf("Circle pagination requires one opaque cursor")
				}
				// Query ordering has no meaning; normalize it only for cycle checks.
				uri.RawQuery = query.Encode()
				next = uri.String()
			}
		}
	}
	return next, nil
}

func (self *CoreCircleApiClient) beginReads(ctx context.Context) (context.Context, context.CancelFunc, error) {
	return beginPaymentReads(ctx, self.readSettings, self.readHooks)
}

func (self *CoreCircleApiClient) readHeaders(userToken string) func(http.Header) {
	token := self.readToken
	if token == nil {
		token = func() string { return fmt.Sprint(circleConfig()["api_token"]) }
	}
	apiToken := token()
	return func(header http.Header) {
		header.Set("Accept", "application/json")
		header.Set("Authorization", "Bearer "+apiToken)
		if userToken != "" {
			header.Set("X-User-Token", userToken)
		}
	}
}

func circleReadData(data []byte, key string) (json.RawMessage, error) {
	var result struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	value := result.Data[key]
	if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, fmt.Errorf("Circle GET is missing %s", key)
	}
	return value, nil
}

// Refuse oversized pages before allocating a server-declared array length.
// Short pages are not an end marker: Circle may cap below the requested size.
func circleReadPage[R any](data []byte, key string) ([]R, error) {
	value, err := circleReadData(data, key)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	if opening, err := decoder.Token(); err != nil || opening != json.Delim('[') {
		return nil, fmt.Errorf("Circle GET %s is not an array", key)
	}
	items := make([]R, 0, circleReadPageSize)
	for decoder.More() {
		if len(items) == circleReadPageSize {
			return nil, fmt.Errorf("Circle GET %s exceeds requested page bound", key)
		}
		var item R
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("Circle GET page has trailing data")
	}
	return items, nil
}

func validCircleReadId(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "/\\?#\x00\r\n")
}

func (self *CoreCircleApiClient) getPublicKey(ctx context.Context) (*string, error) {
	ctx, cancel, err := self.beginReads(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return paymentHttpGet(ctx, self.readSettings, self.readHooks, "https://api.circle.com/v1/w3s/config/entity/publicKey", self.readHeaders(""), func(_ *http.Response, body []byte) (*string, error) {
		value, err := circleReadData(body, "publicKey")
		if err != nil {
			return nil, err
		}
		var key string
		if err := json.Unmarshal(value, &key); err != nil || len(key) == 0 || len(key) > 16*1024 {
			return nil, fmt.Errorf("Circle GET has an invalid entity public key")
		}
		if _, err := parseRsaPublicKeyFromPem([]byte(key)); err != nil {
			return nil, err
		}
		return &key, nil
	})
}

func (self *CoreCircleApiClient) verifyCircleAuth(ctx context.Context, keyId, signature string, body []byte) error {
	ctx, cancel, err := self.beginReads(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if !validCircleReadId(keyId) || signature == "" || len(signature) > 16*1024 {
		return fmt.Errorf("invalid Circle notification key/signature")
	}
	key, err := paymentHttpGet(ctx, self.readSettings, self.readHooks, "https://api.circle.com/v2/notifications/publicKey/"+url.PathEscape(keyId), self.readHeaders(""), func(_ *http.Response, data []byte) (*string, error) {
		var result struct {
			Data *struct {
				Id        string `json:"id"`
				Algorithm string `json:"algorithm"`
				PublicKey string `json:"publicKey"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		if result.Data == nil || result.Data.Id != keyId || result.Data.Algorithm != "ECDSA_SHA_256" || len(result.Data.PublicKey) == 0 || len(result.Data.PublicKey) > 16*1024 {
			return nil, fmt.Errorf("Circle notification key identity/algorithm mismatch")
		}
		if err := verifySignature(result.Data.PublicKey, signature, body); err != nil {
			return nil, err
		}
		return &result.Data.PublicKey, nil
	})
	if err != nil {
		return err
	}
	if key == nil {
		return fmt.Errorf("Circle notification returned no verified key")
	}
	return nil
}

func validCircleReadWallet(wallet *CircleWallet) bool {
	return wallet != nil && validCircleReadId(wallet.ID) && wallet.Address != "" && len(wallet.Address) <= 512 &&
		wallet.Blockchain != "" && len(wallet.Blockchain) <= 64 && !wallet.CreateDate.IsZero()
}

func (self *CoreCircleApiClient) getCircleWallet(ctx context.Context, id string) (*CircleWallet, error) {
	ctx, cancel, err := self.beginReads(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if !validCircleReadId(id) {
		return nil, fmt.Errorf("invalid Circle wallet observation id")
	}
	return paymentHttpGet(ctx, self.readSettings, self.readHooks, "https://api.circle.com/v1/w3s/wallets/"+url.PathEscape(id), self.readHeaders(""), func(_ *http.Response, body []byte) (*CircleWallet, error) {
		value, err := circleReadData(body, "wallet")
		if err != nil {
			return nil, err
		}
		var wallet CircleWallet
		if err := json.Unmarshal(value, &wallet); err != nil {
			return nil, err
		}
		if !validCircleReadWallet(&wallet) || wallet.ID != id {
			return nil, fmt.Errorf("Circle wallet response has missing fields or a different identity")
		}
		return &wallet, nil
	})
}

// The existing user-token POST is invoked once under the logical operation's
// deadline. Only the subsequent GETs may retry; pages never create more tokens.
func (self *CoreCircleApiClient) findCircleWallets(owner *session.ClientSession, tokenSource func(*session.ClientSession) (*CircleUserToken, error), defaultName, defaultChain string) ([]*CircleWalletInfo, error) {
	if owner == nil {
		return nil, fmt.Errorf("Circle wallet observation requires an owner")
	}
	ctx, cancel, err := self.beginReads(owner.Ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	borrowed := *owner
	borrowed.Ctx = ctx
	userToken, err := tokenSource(&borrowed)
	if err != nil {
		return nil, err
	}
	if userToken == nil || userToken.UserToken == "" || len(userToken.UserToken) > 16*1024 {
		return nil, fmt.Errorf("Circle wallet observation returned no bounded user token")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	headers := self.readHeaders(userToken.UserToken)
	seenWallets := map[string]bool{}
	wallets := []*CircleWallet{}
	pages := 0
	first := "https://api.circle.com/v1/w3s/wallets?pageSize=50"
	seenPages := map[string]bool{}
	for uri := first; uri != ""; {
		if pages == maximumCircleObservationPages {
			return nil, fmt.Errorf("Circle wallet observation page capacity exhausted")
		}
		pages++
		if seenPages[uri] {
			return nil, fmt.Errorf("Circle wallet pagination repeated a page")
		}
		seenPages[uri] = true
		var next string
		page, err := paymentHttpGet(ctx, self.readSettings, self.readHooks, uri, headers, func(response *http.Response, body []byte) ([]*CircleWallet, error) {
			var err error
			if next, err = circleReadNext(first, response.Header); err != nil {
				return nil, err
			}
			return circleReadPage[*CircleWallet](body, "wallets")
		})
		if err != nil {
			return nil, err
		}
		for _, wallet := range page {
			if !validCircleReadWallet(wallet) || seenWallets[wallet.ID] || len(wallets) == maximumCircleWallets {
				return nil, fmt.Errorf("Circle wallet census is invalid, repeated or exceeds capacity")
			}
			if wallet.UserId != "" && wallet.UserId != userToken.circleUserId.String() {
				return nil, fmt.Errorf("Circle wallet belongs to a different user")
			}
			seenWallets[wallet.ID] = true
			wallets = append(wallets, wallet)
		}
		uri = next
	}
	result := make([]*CircleWalletInfo, 0, len(wallets))
	for _, wallet := range wallets {
		info, err := self.readCircleWalletBalances(ctx, wallet, headers, &pages, defaultName, defaultChain)
		if err != nil {
			return nil, err
		}
		result = append(result, info)
	}
	return result, nil
}

type circleObservedBalance struct {
	Amount string `json:"amount"`
	Token  *struct {
		Id         string `json:"id"`
		Blockchain string `json:"blockchain"`
		IsNative   *bool  `json:"isNative"`
		Name       string `json:"name"`
		Symbol     string `json:"symbol"`
	} `json:"token"`
}

func circleObservedAmount(value string) (*big.Rat, error) {
	if len(value) == 0 || len(value) > 128 || strings.Count(value, ".") > 1 || value[0] == '.' || value[len(value)-1] == '.' {
		return nil, fmt.Errorf("Circle balance has an invalid decimal amount")
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && ch != '.' {
			return nil, fmt.Errorf("Circle balance is not a nonnegative decimal")
		}
	}
	amount, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("Circle balance has an invalid decimal amount")
	}
	return amount, nil
}

// The page counter belongs to the serial list coordinator, never to the client
// or process. A partial or malformed census is not a zero-balance response.
func (self *CoreCircleApiClient) readCircleWalletBalances(ctx context.Context, wallet *CircleWallet, headers func(http.Header), pages *int, defaultName, defaultChain string) (*CircleWalletInfo, error) {
	info := &CircleWalletInfo{WalletId: wallet.ID, Address: wallet.Address, CreateDate: wallet.CreateDate, Blockchain: wallet.Blockchain, BlockchainSymbol: wallet.Blockchain}
	if wallet.Blockchain == defaultChain {
		info.Blockchain, info.BlockchainSymbol = defaultName, defaultChain
	}
	seen := map[string]bool{}
	first := "https://api.circle.com/v1/w3s/wallets/" + url.PathEscape(wallet.ID) + "/balances?includeAll=true&pageSize=50"
	seenPages := map[string]bool{}
	nativeSeen := false
	for uri := first; uri != ""; {
		if *pages == maximumCircleObservationPages {
			return nil, fmt.Errorf("Circle balance observation page capacity exhausted")
		}
		*pages++
		if seenPages[uri] {
			return nil, fmt.Errorf("Circle balance pagination repeated a page")
		}
		seenPages[uri] = true
		var next string
		page, err := paymentHttpGet(ctx, self.readSettings, self.readHooks, uri, headers, func(response *http.Response, body []byte) ([]circleObservedBalance, error) {
			var err error
			if next, err = circleReadNext(first, response.Header); err != nil {
				return nil, err
			}
			return circleReadPage[circleObservedBalance](body, "tokenBalances")
		})
		if err != nil {
			return nil, err
		}
		for _, balance := range page {
			token := balance.Token
			if token == nil || !validCircleReadId(token.Id) || token.IsNative == nil || token.Symbol == "" || len(token.Symbol) > 64 || token.Blockchain != wallet.Blockchain || seen[token.Id] || len(seen) == maximumCircleTokens {
				return nil, fmt.Errorf("Circle balance census has missing/mismatched identity, repetition or capacity loss")
			}
			amount, err := circleObservedAmount(balance.Amount)
			if err != nil {
				return nil, err
			}
			seen[token.Id] = true
			if *token.IsNative {
				if nativeSeen || token.Name == "" || token.Symbol == "" || len(token.Name) > 256 || len(token.Symbol) > 64 {
					return nil, fmt.Errorf("Circle native token metadata is missing or ambiguous")
				}
				nativeSeen = true
				if info.TokenId == "" {
					info.Blockchain, info.BlockchainSymbol = token.Name, token.Symbol
				}
			} else if token.Symbol == "USDC" {
				if info.TokenId != "" {
					return nil, fmt.Errorf("Circle USDC balance is ambiguous")
				}
				scaled := new(big.Rat).Mul(amount, big.NewRat(1_000_000_000, 1))
				if !scaled.IsInt() || !scaled.Num().IsInt64() {
					return nil, fmt.Errorf("Circle USDC balance exceeds exact supported precision/range")
				}
				info.TokenId, info.Blockchain, info.BlockchainSymbol = token.Id, token.Blockchain, token.Symbol
				info.BalanceUsdcNanoCents = model.NanoCents(scaled.Num().Int64())
			}
		}
		uri = next
	}
	return info, nil
}
