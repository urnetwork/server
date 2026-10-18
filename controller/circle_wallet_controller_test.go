package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Circle users the synthetic Circle holds for the balance reads: one wallet
// without funds, and one wallet with one USDC.
var circleUserIdWithWallet = server.RequireParseId("018c4b12-1a76-aaca-acce-72ddae03f60d")
var circleUserIdWithWalletAndBalance = server.RequireParseId("018c3c7f-82f3-341b-6fd9-fe8d180c366c")

const circleUserTestApiToken = "synthetic-api-token"

// A synthetic Circle for the user-wallet calls, bound to one session. Each
// request must carry exactly Circle's endpoint, our credentials and body; it is
// recorded as "METHOD URI circle-user-id" and answered from the synthetic
// users, user tokens, wallets and balances. Nothing reaches Circle.
type circleUserTestCircle struct {
	t          testing.TB
	requests   []string
	closed     int
	users      map[server.Id]bool
	tokens     map[string]server.Id
	wallets    map[server.Id]map[string]any
	balances   map[string][]any
	challenges []string
}

func newCircleUserTestCircle(t testing.TB) *circleUserTestCircle {
	return &circleUserTestCircle{
		t:        t,
		users:    map[server.Id]bool{},
		tokens:   map[string]server.Id{},
		wallets:  map[server.Id]map[string]any{},
		balances: map[string][]any{},
	}
}

// A new network user's session, bound to this Circle with Polygon as the
// configured blockchain.
func (self *circleUserTestCircle) session() *session.ClientSession {
	client := &circleUserClient{
		core: CoreCircleApiClient{
			readHooks: paymentReadHooks{do: self.do},
			readToken: func() string { return circleUserTestApiToken },
		},
		defaultName:  "Polygon",
		defaultChain: model.MATIC.String(),
	}
	return session.Testing_CreateClientSession(context.WithValue(context.Background(), circleUserClientKey{}, client), &session.ByJwt{
		NetworkId:   server.NewId(),
		NetworkName: "test",
		UserId:      server.NewId(),
	})
}

// An existing Circle user with one Polygon wallet holding `balances`.
func (self *circleUserTestCircle) wallet(user server.Id, createDate string, balances ...any) string {
	id := server.NewId().String()
	self.users[user] = true
	self.wallets[user] = map[string]any{"id": id, "state": "LIVE", "custodyType": "ENDUSER", "accountType": "SCA", "userId": user.String(), "address": "0xsynthetic-" + id, "blockchain": "MATIC", "createDate": createDate, "updateDate": createDate}
	self.balances[id] = append([]any{}, balances...)
	return id
}

func (self *circleUserTestCircle) respond(status int, value any) *http.Response {
	body, err := json.Marshal(value)
	if err != nil {
		self.t.Fatal(err)
	}
	return paymentReadTestResponse(status, string(body), &self.closed)
}

// The body's exact {"userId"} of a user-scoped POST.
func (self *circleUserTestCircle) bodyUser(body map[string]any) server.Id {
	user, err := server.ParseId(fmt.Sprint(body["userId"]))
	if err != nil || len(body) != 1 {
		self.t.Fatalf("user request body changed: %v", body)
	}
	return user
}

func (self *circleUserTestCircle) do(request *http.Request) (*http.Response, error) {
	t := self.t
	header := map[string]string{}
	for key, values := range request.Header {
		if len(values) != 1 {
			t.Fatalf("repeated header %s: %v", key, values)
		}
		header[key] = values[0]
	}
	expected := map[string]string{"Accept": "application/json", "Authorization": "Bearer " + circleUserTestApiToken}
	var body map[string]any
	switch request.Method {
	case http.MethodPost:
		expected["Content-Type"] = "application/json"
		encoded, err := io.ReadAll(request.Body)
		if err != nil || json.Unmarshal(encoded, &body) != nil {
			t.Fatalf("POST body is not JSON: %q %v", encoded, err)
		}
	case http.MethodGet:
		if request.Body != nil {
			t.Fatal("GET has a body")
		}
	default:
		t.Fatalf("unexpected method %s", request.Method)
	}
	userToken := header["X-User-Token"]
	user, issued := self.tokens[userToken]
	if userToken != "" {
		if !issued {
			t.Fatalf("user token %q was not issued", userToken)
		}
		expected["X-User-Token"] = userToken
	}
	if request.URL.Scheme != "https" || request.URL.Host != "api.circle.com" || request.URL.User != nil || request.URL.Fragment != "" || !maps.Equal(header, expected) {
		t.Fatalf("request left Circle or changed its headers: %s %v", request.URL, header)
	}
	uri := request.URL.RequestURI()
	wallet := self.wallets[user]
	switch {
	case request.Method == http.MethodPost && uri == "/v1/w3s/users" && userToken == "":
		user = self.bodyUser(body)
		self.requests = append(self.requests, fmt.Sprintf("POST %s %s", uri, user))
		if self.users[user] {
			return self.respond(http.StatusConflict, map[string]any{"code": 155101, "message": "Existing user already created with the provided userId."}), nil
		}
		self.users[user] = true
		return self.respond(http.StatusCreated, map[string]any{"data": map[string]any{}}), nil
	case request.Method == http.MethodPost && uri == "/v1/w3s/users/token" && userToken == "":
		user = self.bodyUser(body)
		if !self.users[user] {
			t.Fatalf("user token requested for a user Circle does not hold: %s", user)
		}
		self.requests = append(self.requests, fmt.Sprintf("POST %s %s", uri, user))
		n := len(self.tokens) + 1
		token := fmt.Sprintf("synthetic-user-token-%d", n)
		self.tokens[token] = user
		return self.respond(http.StatusOK, map[string]any{"data": map[string]any{"userToken": token, "encryptionKey": fmt.Sprintf("synthetic-encryption-key-%d", n)}}), nil
	case request.Method == http.MethodPost && uri == "/v1/w3s/user/initialize" && issued:
		if _, err := server.ParseId(fmt.Sprint(body["idempotencyKey"])); err != nil || len(body) != 3 || body["accountType"] != "SCA" || !reflect.DeepEqual(body["blockchains"], []any{"MATIC"}) {
			t.Fatalf("initialize body changed: %v", body)
		}
		self.requests = append(self.requests, fmt.Sprintf("POST %s %s", uri, user))
		challenge := server.NewId().String()
		self.challenges = append(self.challenges, challenge)
		return self.respond(http.StatusCreated, map[string]any{"data": map[string]any{"challengeId": challenge}}), nil
	case request.Method == http.MethodGet && uri == "/v1/w3s/wallets?pageSize=50" && issued:
		self.requests = append(self.requests, fmt.Sprintf("GET %s %s", uri, user))
		wallets := []any{}
		if wallet != nil {
			wallets = append(wallets, wallet)
		}
		return self.respond(http.StatusOK, map[string]any{"data": map[string]any{"wallets": wallets}}), nil
	case request.Method == http.MethodGet && wallet != nil && uri == fmt.Sprintf("/v1/w3s/wallets/%s/balances?includeAll=true&pageSize=50", wallet["id"]):
		self.requests = append(self.requests, fmt.Sprintf("GET %s %s", uri, user))
		return self.respond(http.StatusOK, map[string]any{"data": map[string]any{"tokenBalances": self.balances[wallet["id"].(string)]}}), nil
	}
	t.Fatalf("unexpected Circle request: %s %s", request.Method, uri)
	return nil, nil
}

// Every request was exactly the expected one, and its response was closed.
func (self *circleUserTestCircle) requireRequests(expected ...string) {
	if !slices.Equal(self.requests, expected) || self.closed != len(self.requests) {
		self.t.Fatalf("Circle requests changed: %q closed=%d, want %q", self.requests, self.closed, expected)
	}
}

// The requests of one wallet initialization by the Circle user `user`.
func circleUserTestInit(user server.Id) []string {
	return []string{
		"POST /v1/w3s/users " + user.String(),
		"POST /v1/w3s/users/token " + user.String(),
		"POST /v1/w3s/user/initialize " + user.String(),
	}
}

func TestWalletValidateAddressMatchesDeclaredChain(t *testing.T) {
	tests := []struct {
		name    string
		chain   string
		address string
		valid   bool
	}{
		{
			name:    "solana",
			chain:   "SOL",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   true,
		},
		{
			name:    "polygon",
			chain:   "MATIC",
			address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
			valid:   true,
		},
		{
			name:    "production regression - solana key labeled polygon",
			chain:   "MATIC",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   false,
		},
		{
			name:    "polygon address labeled solana",
			chain:   "SOL",
			address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
			valid:   false,
		},
		{
			name:    "zero polygon address",
			chain:   "MATIC",
			address: "0x0000000000000000000000000000000000000000",
			valid:   false,
		},
		{
			name:    "solana usdc mint is not a wallet",
			chain:   "SOL",
			address: solanaUsdcMint,
			valid:   false,
		},
		{
			name:    "unknown chain",
			chain:   "BTC",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   false,
		},
		// TAO addresses are recorded, not paid out. The prefix is pinned to
		// 42, so the same public key under another network prefix is a
		// different -- and rejected -- address. These two are the well known
		// substrate dev key at prefix 42 and prefix 0 respectively.
		{
			name:    "bittensor prefix 42",
			chain:   "TAO",
			address: "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
			valid:   true,
		},
		{
			name:    "bittensor key at the polkadot prefix",
			chain:   "TAO",
			address: "15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5",
			valid:   false,
		},
		{
			name:    "bittensor bad checksum",
			chain:   "TAO",
			address: "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ",
			valid:   false,
		},
		{
			name:    "solana address labeled bittensor",
			chain:   "TAO",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := WalletValidateAddress(&WalletValidateAddressArgs{
				Address: test.address,
				Chain:   test.chain,
			}, nil)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Valid, test.valid)
		})
	}
}

func TestCircleInvalidDestinationErrorClassification(t *testing.T) {
	invalidDestination := &server.HttpStatusError{
		StatusCode:   http.StatusBadRequest,
		Status:       "400 Bad Request",
		ResponseBody: `{"code":155219,"message":"Invalid destination address."}`,
	}
	tests := []struct {
		name  string
		err   error
		match bool
	}{
		{name: "exact", err: invalidDestination, match: true},
		{name: "wrapped", err: fmt.Errorf("submit: %w", invalidDestination), match: true},
		{name: "rate limit", err: &server.HttpStatusError{StatusCode: http.StatusTooManyRequests, ResponseBody: invalidDestination.ResponseBody}, match: false},
		{name: "other bad request", err: &server.HttpStatusError{StatusCode: http.StatusBadRequest, ResponseBody: `{"code":123,"message":"bad amount"}`}, match: false},
		{name: "matching text without typed status", err: errors.New("Invalid destination address."), match: false},
		{name: "malformed body", err: &server.HttpStatusError{StatusCode: http.StatusBadRequest, ResponseBody: `not-json`}, match: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connect.AssertEqual(t, isCircleInvalidDestinationError(test.err), test.match)
		})
	}
}

func TestWalletCircleInit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		circle := newCircleUserTestCircle(t)
		clientSession := circle.session()
		result, err := WalletCircleInit(clientSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")
		connect.AssertEqual(t, result.UserToken.UserToken, "synthetic-user-token-1")
		connect.AssertEqual(t, result.UserToken.EncryptionKey, "synthetic-encryption-key-1")
		connect.AssertEqual(t, result.ChallengeId, circle.challenges[0])

		// a second init should not create an error
		// (Circle answers the repeated user creation with code 155101)
		result, err = WalletCircleInit(clientSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")
		connect.AssertEqual(t, result.UserToken.UserToken, "synthetic-user-token-2")
		connect.AssertEqual(t, result.UserToken.EncryptionKey, "synthetic-encryption-key-2")
		connect.AssertEqual(t, result.ChallengeId, circle.challenges[1])

		circleUserId := model.GetOrCreateCircleUserId(ctx, clientSession.ByJwt.NetworkId, clientSession.ByJwt.UserId)
		circle.requireRequests(slices.Concat(circleUserTestInit(circleUserId), circleUserTestInit(circleUserId))...)
	})
}

func TestWalletValidateAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		circle := newCircleUserTestCircle(t)
		clientSession := circle.session()
		result, err := WalletCircleInit(clientSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")
		circleUserId := model.GetOrCreateCircleUserId(ctx, clientSession.ByJwt.NetworkId, clientSession.ByJwt.UserId)
		circle.requireRequests(circleUserTestInit(circleUserId)...)

		// test valid SOL address
		validateResult, err := WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
				Chain:   model.SOL.String(),
			},
			clientSession,
		)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateResult.Valid, true)

		// test invalid address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				// BringYour USDC Polygon
				Address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
				Chain:   model.SOL.String(),
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateResult.Valid, false)

		// test passing USDC mint address as wallet address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: solanaUsdcMint,
				Chain:   model.SOL.String(),
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateResult.Valid, false)

		// validation itself asks Circle nothing
		circle.requireRequests(circleUserTestInit(circleUserId)...)
	})
}

func TestWalletBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		circle := newCircleUserTestCircle(t)
		emptyWallet := circle.wallet(circleUserIdWithWallet, "2023-12-08T00:00:00Z")
		usdcToken := server.NewId().String()
		fundedWallet := circle.wallet(circleUserIdWithWalletAndBalance, "2023-12-06T00:00:00Z", map[string]any{
			"amount":     "1",
			"updateDate": "2023-12-07T00:00:00Z",
			"token":      map[string]any{"id": usdcToken, "blockchain": "MATIC", "isNative": false, "name": "USD Coin", "symbol": "USDC", "decimals": 6},
		})
		clientSession := circle.session()

		model.SetCircleUserId(
			ctx,
			clientSession.ByJwt.NetworkId,
			clientSession.ByJwt.UserId,
			circleUserIdWithWallet,
		)

		result, err := WalletBalance(clientSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.WalletInfo, nil)
		connect.AssertNotEqual(t, result.WalletInfo.WalletId, "")
		connect.AssertNotEqual(t, result.WalletInfo.CreateDate, time.Time{})
		// the wallet is empty so these are the defaults
		connect.AssertEqual(t, result.WalletInfo.Blockchain, "Polygon")
		connect.AssertEqual(t, result.WalletInfo.BlockchainSymbol, model.MATIC.String())
		connect.AssertEqual(t, result.WalletInfo.TokenId, "")
		connect.AssertEqual(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(0.0))
		connect.AssertEqual(t, result.WalletInfo.WalletId, emptyWallet)
		connect.AssertEqual(t, result.WalletInfo.CreateDate, time.Date(2023, 12, 8, 0, 0, 0, 0, time.UTC))

		model.SetCircleUserId(
			ctx,
			clientSession.ByJwt.NetworkId,
			clientSession.ByJwt.UserId,
			circleUserIdWithWalletAndBalance,
		)

		result, err = WalletBalance(clientSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.WalletInfo, nil)
		connect.AssertNotEqual(t, result.WalletInfo.WalletId, "")
		connect.AssertNotEqual(t, result.WalletInfo.CreateDate, time.Time{})
		connect.AssertEqual(t, result.WalletInfo.Blockchain, model.MATIC.String())
		connect.AssertEqual(t, result.WalletInfo.BlockchainSymbol, "USDC")
		connect.AssertNotEqual(t, result.WalletInfo.TokenId, "")
		connect.AssertEqual(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(1.0))
		connect.AssertEqual(t, result.WalletInfo.WalletId, fundedWallet)
		connect.AssertEqual(t, result.WalletInfo.TokenId, usdcToken)
		connect.AssertEqual(t, result.WalletInfo.CreateDate, time.Date(2023, 12, 6, 0, 0, 0, 0, time.UTC))

		// each read creates a user token for the mapped Circle user, lists its
		// wallets and reads every balance page
		balanceRead := func(user server.Id, wallet string) []string {
			return []string{
				"POST /v1/w3s/users/token " + user.String(),
				"GET /v1/w3s/wallets?pageSize=50 " + user.String(),
				"GET /v1/w3s/wallets/" + wallet + "/balances?includeAll=true&pageSize=50 " + user.String(),
			}
		}
		circle.requireRequests(slices.Concat(balanceRead(circleUserIdWithWallet, emptyWallet), balanceRead(circleUserIdWithWalletAndBalance, fundedWallet))...)
	})
}

// A session without a bound client uses Circle itself: the default transport,
// the configured API token and blockchain. A bound client replaces them.
func TestCircleUserClientDefaultsToCircle(t *testing.T) {
	unbound := []context.Context{
		context.Background(),
		context.WithValue(context.Background(), circleUserClientKey{}, (*circleUserClient)(nil)),
	}
	for _, ctx := range unbound {
		client := circleUserClientOf(ctx)
		if client == nil || client.core.readHooks.do != nil || client.core.readToken != nil || client.tokenSource != nil || client.defaultName != "" || client.defaultChain != "" {
			t.Fatal("an unbound session did not use Circle's transport, token and configuration")
		}
	}
	bound := &circleUserClient{defaultName: "Synthetic native", defaultChain: "SYNTHETIC"}
	if circleUserClientOf(context.WithValue(context.Background(), circleUserClientKey{}, bound)) != bound {
		t.Fatal("the bound client was not used")
	}
}

// Without a bound transport a user-wallet POST goes through the default HTTP
// client, as server.HttpPost sends it; here to a loopback server.
func TestCircleUserPostDefaultsToTheHttpClient(t *testing.T) {
	type observed struct {
		method, path, body string
		header             http.Header
	}
	received := make(chan observed, 1)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		received <- observed{method: request.Method, path: request.URL.RequestURI(), body: string(body), header: request.Header.Clone()}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"data":{"challengeId":"synthetic-challenge"}}`))
	}))
	defer listener.Close()
	client := &CoreCircleApiClient{readToken: func() string { return circleUserTestApiToken }}
	status := 0
	challenge, err := circleUserPost(context.Background(), client, listener.URL+"/v1/w3s/user/initialize", map[string]any{"accountType": "SCA"}, "synthetic-user-token", func(response *http.Response, body []byte) (string, error) {
		status = response.StatusCode
		return parseCircleChallengeId(body)
	})
	if err != nil || challenge != "synthetic-challenge" || status != http.StatusCreated {
		t.Fatal("the default transport did not exchange the POST", challenge, status, err)
	}
	request := <-received
	if request.method != http.MethodPost || request.path != "/v1/w3s/user/initialize" || request.body != `{"accountType":"SCA"}` ||
		request.header.Get("Content-Type") != "application/json" || request.header.Get("Accept") != "application/json" ||
		request.header.Get("Authorization") != "Bearer "+circleUserTestApiToken || request.header.Get("X-User-Token") != "synthetic-user-token" {
		t.Fatalf("the default transport changed the request: %+v", request)
	}
}

// Each failure before or during the exchange is returned unchanged, and an
// exchange that did not complete never reaches the response decoder.
func TestCircleUserPostReturnsEachFailure(t *testing.T) {
	sendFailure := errors.New("synthetic send failure")
	readFailure := errors.New("synthetic body read failure")
	for _, test := range []struct {
		name     string
		uri      string
		body     map[string]any
		response *http.Response
		sent     bool
		expected string
	}{
		{name: "body", uri: "https://api.circle.com/v1/w3s/users", body: map[string]any{"userId": func() {}}, expected: "json: unsupported type"},
		{name: "uri", uri: "https://api.circle.com/\x7f", expected: "invalid control character in URL"},
		{name: "send", uri: "https://api.circle.com/v1/w3s/users", sent: true, expected: sendFailure.Error()},
		{name: "read", uri: "https://api.circle.com/v1/w3s/users", sent: true, response: &http.Response{StatusCode: http.StatusOK, Body: &paymentReadTestBody{reader: iotest.ErrReader(readFailure)}}, expected: readFailure.Error()},
	} {
		sends := 0
		client := &CoreCircleApiClient{
			readToken: func() string { return circleUserTestApiToken },
			readHooks: paymentReadHooks{do: func(*http.Request) (*http.Response, error) {
				sends++
				if test.response == nil {
					return nil, sendFailure
				}
				return test.response, nil
			}},
		}
		closed := 0
		if test.response != nil {
			test.response.Body.(*paymentReadTestBody).closed = &closed
		}
		_, err := circleUserPost(context.Background(), client, test.uri, test.body, "", func(*http.Response, []byte) (any, error) {
			t.Fatal("an incomplete exchange was decoded", test.name)
			return nil, nil
		})
		if err == nil || !strings.Contains(err.Error(), test.expected) || (sends == 1) != test.sent || (test.response != nil && closed != 1) {
			t.Fatal("failure was hidden, changed or sent", test.name, err, sends, closed)
		}
	}
}

// Historical one-dollar transfers retain the caller request id and accepted
// challenge, with synthetic Circle responses.
func TestWalletCircleTransferOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var submissionCount int
		var fixture *customerTransferFixture
		fixture = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || request.URL.Path != "/v1/w3s/user/transactions/transfer" {
				return nil, fmt.Errorf("unexpected Circle request: %s %s", request.Method, request.URL.Path)
			}
			submissionCount++
			return customerTransferResponse(http.StatusCreated, map[string]any{
				"data": map[string]any{"challengeId": fixture.challenge},
			}), nil
		})
		fixture.args.AmountUsdcNanoCents = model.UsdToNanoCents(1.0)

		result, err := WalletCircleTransferOut(fixture.args, fixture.owner)
		if err != nil || result == nil {
			t.Fatal("historical transfer did not create a challenge", err)
		}
		if result.RequestId == nil || *result.RequestId != *fixture.args.RequestId {
			t.Fatal("transfer response lost the caller request id")
		}
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertEqual(t, result.ChallengeId, fixture.challenge)
		connect.AssertEqual(t, result.ChallengeStatus, "PENDING")
		connect.AssertEqual(t, submissionCount, 1)

		retained := fixture.retained(t)
		if retained.ChallengeId == nil || *retained.ChallengeId != result.ChallengeId {
			t.Fatal("accepted challenge was not retained for the caller request")
		}
		connect.AssertEqual(t, retained.RequestId, *result.RequestId)
		connect.AssertEqual(t, retained.Basis.Destination, fixture.args.ToAddress)
		connect.AssertEqual(t, retained.Basis.Amount, fixture.args.AmountUsdcNanoCents)
		connect.AssertEqual(t, retained.ChallengeStatus, result.ChallengeStatus)
		connect.AssertEqual(t, retained.SubmissionCount, int64(1))
		connect.AssertEqual(t, retained.ReviewRequired, false)
	})
}

func TestCircleWalletIdParsing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		circleWalletId := "02201362-9c27-5793-ad74-994c8bac4ccf" // this is an ID generated by Circle
		walletId, err := server.ParseId("02201362-9c27-5793-ad74-994c8bac4ccf")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, circleWalletId, walletId.String())
	})
}

func TestCircleWebhookVerifySignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// values pulled from example docs at https://developers.circle.com/w3s/docs/web3-services-notifications-quickstart
		publicKeyBase64 := "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAESl76SZPBJemW0mJNN4KTvYkLT8bOT4UGhFhzNk3fJqf6iuPlLQLq533FelXwczJbjg2U1PHTvQTK7qOQnDL2Tg=="
		signatureBase64 := "MEQCIBlJPX7t0FDOcozsRK6qIQwik5Fq6mhAtCSSgIB/yQO7AiB9U5lVpdufKvPhk3cz4TH2f5MP7ArnmPRBmhPztpsIFQ=="
		responseBodyBytes := []byte("{\n\"subscriptionId\":\"00000000-0000-0000-0000-000000000000\",\"notificationId\":\"00000000-0000-0000-0000-000000000000\",\"notificationType\":\"webhooks.test\",\"notification\":{\"hello\":\"world\"},\"timestamp\":\"2024-01-26T18:22:19.779834211Z\",\"version\":2}")

		err := verifySignature(
			publicKeyBase64,
			signatureBase64,
			responseBodyBytes,
		)

		connect.AssertEqual(t, err, nil)

	})
}
