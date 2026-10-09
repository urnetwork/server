package api

// End to end through the router the API serves, against the database: a real
// client token, minted with POST /network/auth-client by the network's root
// token or an API key exactly as an embed customer's backend mints one, is
// refused on every admin route and changes nothing, while the network
// credential administers the network as before (AUTHZ1.md).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

type routeAccessDbFixture struct {
	ctx         context.Context
	router      *router.Router
	networkId   server.Id
	userId      server.Id
	networkName string
	rootToken   string
	apiKey      string
	apiKeyId    server.Id
}

func newRouteAccessDbFixture(t testing.TB, ctx context.Context) *routeAccessDbFixture {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	networkName := fmt.Sprintf("authz%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	self := &routeAccessDbFixture{
		ctx:         ctx,
		router:      router.NewRouter(ctx, Routes()),
		networkId:   networkId,
		userId:      userId,
		networkName: networkName,
		rootToken:   jwt.NewByJwt(networkId, userId, networkName, false, false).Sign(),
	}

	// the root token creates the backend's API key
	var created model.CreateApiKeyResult
	self.call(t, http.MethodPost, "/account/api-key", self.root(), map[string]any{"name": "backend"}, http.StatusOK, &created)
	if created.Error != nil || !strings.HasPrefix(created.ApiKey, "urn_") {
		t.Fatalf("api key = %+v", created)
	}
	self.apiKey = created.ApiKey
	self.apiKeyId = created.Id
	return self
}

// enableEmbed puts the network on the Embed plan, which hands its client
// tokens to a third party's users (model.NetworkRefusesClientAdmin).
func (self *routeAccessDbFixture) enableEmbed(t testing.TB) {
	t.Helper()
	if err := model.SetNetworkTopLevelClientLimit(self.ctx, self.networkId, 1000); err != nil {
		t.Fatal(err)
	}
	if !model.NetworkRefusesClientAdmin(self.ctx, self.networkId) {
		t.Fatal("the Embed network serves its client tokens on the app admin routes")
	}
}

func (self *routeAccessDbFixture) root() string {
	return "Bearer " + self.rootToken
}

func (self *routeAccessDbFixture) key() string {
	return "Bearer " + self.apiKey
}

func (self *routeAccessDbFixture) serve(method string, path string, authorization string, body any) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		server.Raise(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	r.RemoteAddr = "198.51.100.42:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	return w
}

// call serves the request and requires the status, decoding the body into
// result when one is given.
func (self *routeAccessDbFixture) call(t testing.TB, method string, path string, authorization string, body any, status int, result any) {
	t.Helper()
	w := self.serve(method, path, authorization, body)
	if w.Code != status {
		t.Fatalf("%s %s: status = %d, want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatalf("%s %s: %s: %s", method, path, err, w.Body.String())
		}
	}
}

// mintClient creates a top-level client with POST /network/auth-client, as a
// backend mints one for each installation, and returns its client token.
func (self *routeAccessDbFixture) mintClient(t testing.TB, authorization string, sourceClientId *server.Id) (server.Id, string) {
	t.Helper()
	body := map[string]any{"description": "embedded app", "device_spec": "embed"}
	if sourceClientId != nil {
		body["source_client_id"] = *sourceClientId
	}
	var result model.AuthNetworkClientResult
	self.call(t, http.MethodPost, "/network/auth-client", authorization, body, http.StatusOK, &result)
	if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		t.Fatalf("auth-client = %+v", result)
	}
	return *result.ClientId, *result.ByClientJwt
}

// The tables an admin route writes, by network and by user.
var routeAccessNetworkTables = []string{
	"network",
	"network_client",
	"device",
	"account_api_key",
	"auth_code",
	"account_preferences",
	"account_wallet",
	"payout_wallet",
	"exclude_network_client_location",
	"network_referral",
	"network_points_leaderboard",
	"network_client_acl_group",
	"network_client_data_cap",
	"network_top_level_client_limit",
	"device_add_history",
	"device_association_name",
	"oauth_authorization_code",
	"oauth_consent",
	"st_wallet",
	"test_balance_drain",
	"stripe_customer",
}

var routeAccessUserTables = []string{
	"network_user",
	"network_user_auth_password",
	"network_user_auth_seedphrase",
	"network_user_auth_sso",
	"network_user_auth_wallet",
}

// snapshot fingerprints every row of the network and its admin user in the
// tables an admin route writes.
func (self *routeAccessDbFixture) snapshot(t testing.TB) map[string]string {
	t.Helper()
	fingerprints := map[string]string{}
	server.Db(self.ctx, func(conn server.PgConn) {
		fingerprint := func(table string, column string, id server.Id) {
			var exists bool
			server.Raise(conn.QueryRow(self.ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists))
			if !exists {
				t.Fatalf("snapshot table %s does not exist", table)
			}
			var value string
			server.Raise(conn.QueryRow(
				self.ctx,
				fmt.Sprintf(
					`SELECT coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM %s t WHERE t.%s = $1`,
					table,
					column,
				),
				id,
			).Scan(&value))
			fingerprints[table] = value
		}
		for _, table := range routeAccessNetworkTables {
			fingerprint(table, "network_id", self.networkId)
		}
		for _, table := range routeAccessUserTables {
			fingerprint(table, "user_id", self.userId)
		}
	})
	return fingerprints
}

func requireRouteAccessSnapshot(t testing.TB, before map[string]string, after map[string]string, what string) {
	t.Helper()
	changed := []string{}
	for table, value := range before {
		if after[table] != value {
			changed = append(changed, table)
		}
	}
	slices.Sort(changed)
	if 0 < len(changed) {
		t.Fatalf("%s changed %v", what, changed)
	}
}

type routeAccessDbRequest struct {
	path string
	body any
}

// routeAccessAdminRequests is a request for every admin route, aimed at the
// victim client and the network, that would change state if the route served
// it. A route classified admin with no request here fails the test.
func routeAccessAdminRequests(self *routeAccessDbFixture, victimClientId server.Id) map[string]routeAccessDbRequest {
	walletId := server.NewId()
	locationId := server.NewId()
	oauthRequest := map[string]any{
		"client_id":             "attacker",
		"redirect_uri":          "https://attacker.example/callback",
		"response_type":         "code",
		"scope":                 "openid",
		"code_challenge":        strings.Repeat("a", 43),
		"code_challenge_method": "S256",
	}
	return map[string]routeAccessDbRequest{
		// network only
		"GET /stats/providers":                  {path: "/stats/providers"},
		"POST /stats/providers-last-n":          {path: "/stats/providers-last-n", body: map[string]any{"last_n": 7}},
		"POST /stats/provider-last-n":           {path: "/stats/provider-last-n", body: map[string]any{"client_id": victimClientId, "last_n": 7}},
		"POST /stats/providers-overview-last-n": {path: "/stats/providers-overview-last-n", body: map[string]any{"last_n": 7}},
		"GET /stats/providers-overview-last-90": {path: "/stats/providers-overview-last-90"},
		"POST /stats/provider-last-90":          {path: "/stats/provider-last-90", body: map[string]any{"client_id": victimClientId}},
		"POST /network/register-client-v1":      {path: "/network/register-client-v1", body: map[string]any{"description": "attacker", "device_spec": "attacker"}},
		"POST /network/remove-clients":          {path: "/network/remove-clients", body: map[string]any{"client_ids": []server.Id{victimClientId}}},
		"GET /network/proxies":                  {path: "/network/proxies"},
		"POST /network/user/update":             {path: "/network/user/update", body: map[string]any{"network_name": "attacker" + self.networkName}},
		"POST /network/client-data-cap":         {path: "/network/client-data-cap", body: map[string]any{"client_id": victimClientId, "monthly_byte_limit": 0}},
		"GET /network/client-data-caps":         {path: "/network/client-data-caps?limit=100"},
		"POST /network/client-acl-group":        {path: "/network/client-acl-group", body: map[string]any{"client_id": victimClientId, "acl_group": "default"}},
		"GET /wallet/balance":                   {path: "/wallet/balance"},
		"POST /wallet/circle-init":              {path: "/wallet/circle-init"},
		"POST /wallet/circle-transfer-out":      {path: "/wallet/circle-transfer-out", body: map[string]any{"to_address": "attacker", "amount_usdc_nano_cents": 1, "terms": true}},
		"POST /test/balance-drain":              {path: "/test/balance-drain", body: map[string]any{"duration_seconds": 60}},
		"POST /test/balance-restore":            {path: "/test/balance-restore", body: map[string]any{}},
		"GET /subscription/details":             {path: "/subscription/details"},
		"POST /subscription/cancel":             {path: "/subscription/cancel", body: map[string]any{"store": "stripe"}},
		"POST /subscription/resume":             {path: "/subscription/resume", body: map[string]any{"store": "stripe"}},
		"POST /device/add":                      {path: "/device/add", body: map[string]any{"code": "attacker"}},
		"POST /device/create-share-code":        {path: "/device/create-share-code", body: map[string]any{"client_id": victimClientId}},
		"GET /device/share-code/([^/]+)/qr.png": {path: "/device/share-code/attacker/qr.png"},
		"POST /device/share-status":             {path: "/device/share-status", body: map[string]any{"share_code": "attacker"}},
		"POST /device/confirm-share":            {path: "/device/confirm-share", body: map[string]any{"share_code": "attacker"}},
		"GET /device/associations":              {path: "/device/associations"},
		"POST /device/remove-association":       {path: "/device/remove-association", body: map[string]any{"code": "attacker"}},
		"POST /device/set-association-name":     {path: "/device/set-association-name", body: map[string]any{"code": "attacker", "device_name": "attacker"}},
		"POST /sn/wallet/network-consent":       {path: "/sn/wallet/network-consent", body: map[string]any{"coldkey_ss58": "attacker"}},
		"POST /sn/wallet/hotkey-consent":        {path: "/sn/wallet/hotkey-consent", body: map[string]any{"coldkey_ss58": "attacker"}},
		"POST /sn/wallet/hotkey-delegation":     {path: "/sn/wallet/hotkey-delegation", body: map[string]any{"coldkey_ss58": "attacker"}},
		"POST /account/api-key":                 {path: "/account/api-key", body: map[string]any{"name": "attacker"}},
		"POST /account/api-key/remove":          {path: "/account/api-key/remove", body: map[string]any{"id": self.apiKeyId}},
		"GET /account/api-keys":                 {path: "/account/api-keys"},
		"POST /oauth/authorize":                 {path: "/oauth/authorize", body: oauthRequest},
		"POST /oauth/consent":                   {path: "/oauth/consent", body: oauthRequest},
		// app admin
		"POST /auth/network-delete":               {path: "/auth/network-delete"},
		"POST /auth/code-create":                  {path: "/auth/code-create", body: map[string]any{"uses": 1, "duration_minutes": 60}},
		"POST /auth/add-auth":                     {path: "/auth/add-auth", body: map[string]any{"user_auth": "attacker@example.com", "password": "attacker-password-1"}},
		"POST /auth/remove-auth":                  {path: "/auth/remove-auth", body: map[string]any{"auth_type": "password"}},
		"POST /auth/regenerate-seedphrase":        {path: "/auth/regenerate-seedphrase", body: map[string]any{}},
		"POST /auth/generate-seedphrase":          {path: "/auth/generate-seedphrase", body: map[string]any{}},
		"GET /network/clients":                    {path: "/network/clients"},
		"GET /network/user":                       {path: "/network/user"},
		"POST /network/ranking-visibility":        {path: "/network/ranking-visibility", body: map[string]any{"is_public": true}},
		"POST /network/points-ranking-visibility": {path: "/network/points-ranking-visibility", body: map[string]any{"is_public": true}},
		"POST /network/emoji":                     {path: "/network/emoji", body: map[string]any{"emoji_tag": "attacker"}},
		"POST /network/block-location":            {path: "/network/block-location", body: map[string]any{"location_id": locationId}},
		"POST /network/unblock-location":          {path: "/network/unblock-location", body: map[string]any{"location_id": locationId}},
		"POST /preferences/set-preferences":       {path: "/preferences/set-preferences", body: map[string]any{"product_updates": true}},
		"POST /stripe/customer-portal":            {path: "/stripe/customer-portal", body: map[string]any{}},
		"POST /account/payout-wallet":             {path: "/account/payout-wallet", body: map[string]any{"wallet_id": walletId}},
		"GET /account/payout-wallet":              {path: "/account/payout-wallet"},
		"POST /account/wallet":                    {path: "/account/wallet", body: map[string]any{"network_id": self.networkId, "blockchain": "SOL", "wallet_address": "attacker", "default_token_type": "USDC"}},
		"GET /account/wallets":                    {path: "/account/wallets"},
		"POST /account/wallets/remove":            {path: "/account/wallets/remove", body: map[string]any{"wallet_id": walletId.String()}},
		"POST /account/wallets/verify-seeker":     {path: "/account/wallets/verify-seeker", body: map[string]any{}},
		"GET /account/payments":                   {path: "/account/payments"},
		"GET /account/unlink-referral-network":    {path: "/account/unlink-referral-network"},
		"POST /account/set-referral":              {path: "/account/set-referral", body: map[string]any{"referral_code": "attacker"}},
		"POST /account/change-name":               {path: "/account/change-name", body: map[string]any{"network_name": self.networkName, "new_name": "attacker"}},
		"POST /account/claim-name":                {path: "/account/claim-name", body: map[string]any{"network_name": self.networkName, "new_name": "attacker"}},
		"GET /account/balance-codes":              {path: "/account/balance-codes"},
	}
}

// requireClientTokenRefused calls every route of the access classes with the
// client token and requires 403 before the handler and no change to the
// network.
func requireClientTokenRefused(t testing.TB, self *routeAccessDbFixture, clientToken string, victimClientId server.Id, classes ...routeAccess) int {
	t.Helper()
	requests := routeAccessAdminRequests(self, victimClientId)
	before := self.snapshot(t)
	refusedCount := 0
	for _, route := range Routes() {
		if !slices.Contains(classes, routeAccessFor(route)) {
			continue
		}
		key := routeAccessKey(route)
		request, ok := requests[key]
		if !ok {
			t.Fatalf("%s has no request in routeAccessAdminRequests", key)
		}
		w := self.serve(route.Method(), request.path, "Bearer "+clientToken, request.body)
		if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
			t.Fatalf("%s: client token status = %d body = %q, want 403 refused", key, w.Code, w.Body.String())
		}
		refusedCount += 1
	}
	requireRouteAccessSnapshot(t, before, self.snapshot(t), "a refused client token")
	return refusedCount
}

// The client token an embed backend mints for an installation is refused on
// every network-only route of an ordinary network, and changes nothing. The
// URnetwork apps still call the app admin routes with theirs (AUTHZ1.md,
// decision 1).
func TestRealClientTokenIsRefusedOnEveryNetworkRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		_, clientToken := self.mintClient(t, self.root(), nil)
		victimClientId, _ := self.mintClient(t, self.root(), nil)

		if refusedCount := requireClientTokenRefused(t, self, clientToken, victimClientId, routeAccessNetwork); refusedCount == 0 {
			t.Fatal("no network-only routes")
		}

		// an app admin route still serves an ordinary network's client token
		w := self.serve(http.MethodGet, "/network/clients", "Bearer "+clientToken, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /network/clients with the client token of an ordinary network = %d: %s", w.Code, w.Body.String())
		}
	})
}

// On a network that hands its client tokens to a third party (the Embed plan),
// a client token minted with the backend's API key is refused on every admin
// route, the ones the URnetwork apps call included, and changes nothing.
func TestRealClientTokenOfAnEmbedNetworkIsRefusedOnEveryAdminRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		self.enableEmbed(t)

		_, clientToken := self.mintClient(t, self.key(), nil)
		victimClientId, _ := self.mintClient(t, self.key(), nil)

		refusedCount := requireClientTokenRefused(t, self, clientToken, victimClientId, routeAccessNetwork, routeAccessAppAdmin)
		appAdminCount := 0
		for _, access := range routeAccessByRoute {
			if access == routeAccessNetwork || access == routeAccessAppAdmin {
				appAdminCount += 1
			}
		}
		if refusedCount != appAdminCount {
			t.Fatalf("refused %d admin routes, want all %d", refusedCount, appAdminCount)
		}
	})
}

// The routes a client token keeps act only for its own client and the clients
// it created: through the router it cannot mint a top-level client, create a
// child of another client, reissue another client's token, remove another
// client or rename another device, and each refusal changes nothing. Its own
// child it creates, reissues and removes, and its token refreshes.
func TestRealClientTokenActsOnlyForItsOwnClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		self.enableEmbed(t)

		clientId, clientToken := self.mintClient(t, self.key(), nil)
		victimClientId, _ := self.mintClient(t, self.key(), nil)
		var victimDeviceId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT device_id FROM network_client WHERE client_id = $1`, victimClientId).Scan(&victimDeviceId))
		})
		client := "Bearer " + clientToken

		before := self.snapshot(t)
		for _, c := range []struct {
			name    string
			path    string
			body    map[string]any
			message string
		}{
			{"a top-level client", "/network/auth-client", map[string]any{"description": "attacker", "device_spec": "attacker"}, "A client token cannot create a top-level client."},
			{"a child of another client", "/network/auth-client", map[string]any{"source_client_id": victimClientId, "description": "attacker", "device_spec": "attacker"}, "Client does not exist."},
			{"another client's token", "/network/auth-client", map[string]any{"client_id": victimClientId, "description": "attacker", "device_spec": "attacker"}, "Client does not exist."},
			{"removing another client", "/network/remove-client", map[string]any{"client_id": victimClientId}, "Client does not exist."},
			{"renaming another device", "/device/set-name", map[string]any{"device_id": victimDeviceId, "device_name": "attacker"}, "Device does not exist."},
		} {
			var result struct {
				ByClientJwt *string `json:"by_client_jwt"`
				Error       *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			self.call(t, http.MethodPost, c.path, client, c.body, http.StatusOK, &result)
			if result.Error == nil || result.Error.Message != c.message || result.ByClientJwt != nil {
				t.Fatalf("%s: answered %+v, want the refusal %q", c.name, result, c.message)
			}
		}
		requireRouteAccessSnapshot(t, before, self.snapshot(t), "a client token acting for another client")

		// its own child: create, reissue, remove
		childClientId, _ := self.mintClient(t, client, &clientId)
		var reissued model.AuthNetworkClientResult
		self.call(t, http.MethodPost, "/network/auth-client", client, map[string]any{"client_id": childClientId, "description": "window", "device_spec": "window"}, http.StatusOK, &reissued)
		if reissued.Error != nil || reissued.ByClientJwt == nil {
			t.Fatalf("reissue of its own child = %+v", reissued)
		}
		var removed model.RemoveNetworkClientResult
		self.call(t, http.MethodPost, "/network/remove-client", client, map[string]any{"client_id": childClientId}, http.StatusOK, &removed)
		if removed.Error != nil {
			t.Fatalf("removing its own child = %+v", removed.Error)
		}

		// and its token refreshes
		var refreshed struct {
			ByJwt string `json:"by_jwt"`
		}
		self.call(t, http.MethodGet, "/auth/refresh", client, nil, http.StatusOK, &refreshed)
		if refreshedJwt, err := jwt.ParseByJwtUnverified(ctx, refreshed.ByJwt); err != nil || refreshedJwt.ClientId == nil || *refreshedJwt.ClientId != clientId {
			t.Fatalf("refresh = %v %v", refreshedJwt, err)
		}
	})
}

// The network credential, the root token and an API key, still administers
// the network through the same routes: what a client token was refused, the
// network credential does.
func TestNetworkCredentialAdministersTheNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		self.enableEmbed(t)

		clientId, _ := self.mintClient(t, self.key(), nil)
		otherClientId, _ := self.mintClient(t, self.root(), nil)

		for _, credential := range []string{self.root(), self.key()} {
			// another client's token
			var reissued model.AuthNetworkClientResult
			self.call(t, http.MethodPost, "/network/auth-client", credential, map[string]any{"client_id": clientId, "description": "device", "device_spec": "device"}, http.StatusOK, &reissued)
			if reissued.Error != nil || reissued.ByClientJwt == nil {
				t.Fatalf("reissue = %+v", reissued)
			}

			// ACL group and data cap
			var acl model.NetworkClientAclGroupResult
			self.call(t, http.MethodPost, "/network/client-acl-group", credential, map[string]any{"client_id": clientId, "acl_group": "isolated"}, http.StatusOK, &acl)
			if acl.Error != nil {
				t.Fatalf("acl group = %+v", acl.Error)
			}
			var capResult model.ClientDataCapResult
			self.call(t, http.MethodPost, "/network/client-data-cap", credential, map[string]any{"client_id": clientId, "monthly_byte_limit": 1024 * 1024}, http.StatusOK, &capResult)
			if capResult.Error != nil {
				t.Fatalf("data cap = %+v", capResult.Error)
			}

			// an auth code and the account's preferences
			var code model.AuthCodeCreateResult
			self.call(t, http.MethodPost, "/auth/code-create", credential, map[string]any{"uses": 1, "duration_minutes": 5}, http.StatusOK, &code)
			if code.Error != nil || code.AuthCode == "" {
				t.Fatalf("auth code = %+v", code)
			}
			self.call(t, http.MethodPost, "/preferences/set-preferences", credential, map[string]any{"product_updates": true}, http.StatusOK, nil)

			// the clients list and the API keys
			self.call(t, http.MethodGet, "/network/clients", credential, nil, http.StatusOK, nil)
			self.call(t, http.MethodGet, "/account/api-keys", credential, nil, http.StatusOK, nil)
		}

		var aclGroup string
		var monthlyByteLimit int64
		var authCodeCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT acl_group FROM network_client_acl_group WHERE client_id = $1`, clientId).Scan(&aclGroup))
			server.Raise(conn.QueryRow(ctx, `SELECT monthly_byte_limit FROM network_client_data_cap WHERE client_id = $1`, clientId).Scan(&monthlyByteLimit))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM auth_code WHERE network_id = $1`, self.networkId).Scan(&authCodeCount))
		})
		if aclGroup != "isolated" || monthlyByteLimit != 1024*1024 || authCodeCount != 2 {
			t.Fatalf("acl group = %q, monthly limit = %d, auth codes = %d", aclGroup, monthlyByteLimit, authCodeCount)
		}

		// the bulk removal, then a new API key and the old one's removal
		var removed model.RemoveNetworkClientsResult
		self.call(t, http.MethodPost, "/network/remove-clients", self.root(), map[string]any{"client_ids": []server.Id{otherClientId}}, http.StatusOK, &removed)
		if !removed.Scheduled {
			var active bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, otherClientId).Scan(&active))
			})
			if active {
				t.Fatal("the bulk removal left the client active")
			}
		}
		var created model.CreateApiKeyResult
		self.call(t, http.MethodPost, "/account/api-key", self.key(), map[string]any{"name": "rotated"}, http.StatusOK, &created)
		if created.Error != nil || created.ApiKey == "" {
			t.Fatalf("api key = %+v", created)
		}
		self.call(t, http.MethodPost, "/account/api-key/remove", "Bearer "+created.ApiKey, map[string]any{"id": self.apiKeyId}, http.StatusOK, nil)
		if w := self.serve(http.MethodGet, "/account/api-keys", self.key(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("the removed API key = %d, want 401", w.Code)
		}
	})
}
