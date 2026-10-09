package api

// End to end through the router the API serves, against the database: a
// network token renews at POST /auth/network-refresh, a client token and an
// API key do not, and both refreshes keep the presented token's create time,
// so a password reset that expires the presented token expires the refreshed
// one too (AUTHZ1.md).

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// networkRefreshCount reads urnetwork_auth_network_refreshes_total for the
// outcome.
func networkRefreshCount(t testing.TB, outcome string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_auth_network_refreshes_total" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "outcome" && label.GetValue() == outcome {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("urnetwork_auth_network_refreshes_total{outcome=%q} is missing", outcome)
	return 0
}

// requireNetworkRefreshCounted requires that the outcome advanced by one and
// no other outcome moved.
func requireNetworkRefreshCounted(t testing.TB, outcome string, f func()) {
	t.Helper()
	outcomes := []string{"renewed", "refused_client", "refused_api_key", "state_invalid"}
	before := map[string]float64{}
	for _, o := range outcomes {
		before[o] = networkRefreshCount(t, o)
	}
	f()
	for _, o := range outcomes {
		want := before[o]
		if o == outcome {
			want += 1
		}
		if got := networkRefreshCount(t, o); got != want {
			t.Fatalf("network refreshes %s = %v, want %v", o, got, want)
		}
	}
}

// agedNetworkToken is a network token of the fixture's network issued an hour
// ago and expiring in an hour, so a renewal's fresh lifetime claims differ
// from its own whatever second the test runs in.
func (self *routeAccessDbFixture) agedNetworkToken() *jwt.ByJwt {
	byJwt := jwt.NewByJwt(self.networkId, self.userId, self.networkName, false, false)
	ageClaims(byJwt)
	return byJwt
}

func ageClaims(byJwt *jwt.ByJwt) {
	issued := server.NowUtc().Add(-time.Hour)
	byJwt.IssuedAt = gojwt.NewNumericDate(issued)
	byJwt.NotBefore = gojwt.NewNumericDate(issued)
	byJwt.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
}

// networkRefresh renews the token and returns the renewed token, parsed.
func (self *routeAccessDbFixture) networkRefresh(t testing.TB, presented string) *jwt.ByJwt {
	t.Helper()
	var result controller.NetworkRefreshTokenResult
	self.call(t, http.MethodPost, "/auth/network-refresh", "Bearer "+presented, nil, http.StatusOK, &result)
	if result.Error != nil || result.ByJwt == "" {
		t.Fatalf("network refresh = %+v", result)
	}
	renewed, err := jwt.ParseByJwt(self.ctx, result.ByJwt)
	if err != nil {
		t.Fatal(err)
	}
	return renewed
}

func (self *routeAccessDbFixture) setCredentialChangeTime(t testing.TB, changeTime time.Time) {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`,
			self.userId,
			changeTime,
		))
	})
}

// requireFreshLifetime requires the refreshed token's registered claims to be
// its own: issued at or after since, expiring later than the presented token,
// with a new id.
func requireFreshLifetime(t testing.TB, presented *jwt.ByJwt, refreshed *jwt.ByJwt, since time.Time) {
	t.Helper()
	if refreshed.IssuedAt == nil || refreshed.IssuedAt.Time.Before(since.Truncate(time.Second)) {
		t.Fatalf("refreshed iat = %v, want at or after %v", refreshed.IssuedAt, since)
	}
	if refreshed.ExpiresAt == nil || !refreshed.ExpiresAt.Time.After(presented.ExpiresAt.Time) ||
		refreshed.ExpiresAt.Time.Before(since.Add(29*24*time.Hour)) {
		t.Fatalf("refreshed exp = %v, want a new 30 day lifetime (presented exp %v)", refreshed.ExpiresAt, presented.ExpiresAt)
	}
	if refreshed.ID == "" || refreshed.ID == presented.ID {
		t.Fatalf("refreshed jti = %q, presented jti = %q", refreshed.ID, presented.ID)
	}
	if refreshed.Issuer != jwt.ByJwtIssuer || refreshed.Subject != refreshed.UserId.String() ||
		!slices.Contains(refreshed.Audience, jwt.ByJwtAudienceApi) || refreshed.NotBefore == nil {
		t.Fatalf("refreshed registered claims = %+v", refreshed.RegisteredClaims)
	}
}

// A network token renews: the same network, user, guest mode, roles and
// principal, the network's current name and Pro state, the presented create
// time, and a new lifetime. The renewed token administers the network.
func TestNetworkRefreshRenewsTheNetworkToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		presented := self.agedNetworkToken()
		presented.GuestMode = true
		presented.Roles = []string{"operator", "validator"}
		presented.Principal = "principal-1"

		// renamed and Pro since the presented token was minted
		renamed := self.networkName + "r"
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, self.networkId, renamed))
			if err := model.AddProTransferBalanceInTx(tx, ctx, self.networkId, model.ByteCount(1024*1024*1024), now, now.Add(30*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
		})
		model.UpdateProNetwork(ctx, self.networkId)

		since := server.NowUtc()
		var renewed *jwt.ByJwt
		requireNetworkRefreshCounted(t, "renewed", func() {
			renewed = self.networkRefresh(t, presented.Sign())
		})

		if renewed.ClientId != nil || renewed.DeviceId != nil {
			t.Fatalf("renewed a network token as a client token: client=%v device=%v", renewed.ClientId, renewed.DeviceId)
		}
		if renewed.NetworkId != self.networkId || renewed.UserId != self.userId {
			t.Fatalf("renewed identity = %s/%s, want %s/%s", renewed.NetworkId, renewed.UserId, self.networkId, self.userId)
		}
		if renewed.NetworkName != renamed || !renewed.Pro {
			t.Fatalf("renewed name = %q pro = %t, want %q true", renewed.NetworkName, renewed.Pro, renamed)
		}
		if !renewed.GuestMode || !slices.Equal(renewed.Roles, presented.Roles) || renewed.Principal != presented.Principal {
			t.Fatalf("renewed guest = %t roles = %v principal = %q", renewed.GuestMode, renewed.Roles, renewed.Principal)
		}
		if !renewed.CreateTime.Equal(presented.CreateTime) {
			t.Fatalf("renewed create time = %v, want the presented %v", renewed.CreateTime, presented.CreateTime)
		}
		requireFreshLifetime(t, presented, renewed, since)

		// the renewed token is live and administers the network
		if err := jwt.ValidateByJwtState(ctx, renewed, false); err != nil {
			t.Fatal(err)
		}
		self.call(t, http.MethodGet, "/network/clients", "Bearer "+renewed.Sign(), nil, http.StatusOK, nil)
	})
}

// A client token never obtains a network token: the router refuses it with
// 403 before the handler runs, and the handler refuses one that reaches it.
// An API key does not expire and is not refreshed: signing its identity would
// turn a key that can be removed into a token that outlives the removal.
// /auth/refresh still refuses a network token.
func TestNetworkRefreshRefusesClientTokensAndApiKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		_, clientToken := self.mintClient(t, self.root(), nil)

		// the client token, through the router
		requireNetworkRefreshCounted(t, "", func() {
			w := self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+clientToken, nil)
			if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
				t.Fatalf("client token status = %d body = %q, want 403 refused", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "by_jwt") {
				t.Fatalf("client token got a token: %s", w.Body.String())
			}
		})

		// the client token, reaching the handler
		clientByJwt, err := jwt.ParseByJwt(ctx, clientToken)
		if err != nil {
			t.Fatal(err)
		}
		requireNetworkRefreshCounted(t, "refused_client", func() {
			clientSession := session.Testing_CreateClientSession(ctx, clientByJwt)
			defer clientSession.Cancel()
			result, err := controller.NetworkRefreshToken(clientSession)
			if err != nil || result.Error == nil || result.ByJwt != "" {
				t.Fatalf("client token in the handler = %+v, %v", result, err)
			}
		})

		// the API key, through the router
		requireNetworkRefreshCounted(t, "refused_api_key", func() {
			var result controller.NetworkRefreshTokenResult
			self.call(t, http.MethodPost, "/auth/network-refresh", self.key(), nil, http.StatusOK, &result)
			if result.Error == nil || result.ByJwt != "" {
				t.Fatalf("API key refresh = %+v", result)
			}
		})

		// /auth/refresh renews only client tokens
		var refreshResult controller.RefreshTokenResult
		self.call(t, http.MethodGet, "/auth/refresh", self.root(), nil, http.StatusOK, &refreshResult)
		if refreshResult.Error == nil || refreshResult.ByJwt != "" {
			t.Fatalf("network token at /auth/refresh = %+v", refreshResult)
		}
	})
}

// A network token whose network is gone, or whose user no longer administers
// it, by the time the handler runs is refused with 401 and counted.
func TestNetworkRefreshRefusesAStaleIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		requireStateInvalid := func(name string, byJwt *jwt.ByJwt) {
			requireNetworkRefreshCounted(t, "state_invalid", func() {
				staleSession := session.Testing_CreateClientSession(ctx, byJwt)
				defer staleSession.Cancel()
				result, err := controller.NetworkRefreshToken(staleSession)
				if err == nil || !strings.HasPrefix(err.Error(), "401 ") || result != nil {
					t.Fatalf("%s = %+v, %v; want 401", name, result, err)
				}
			})
		}
		requireStateInvalid("no network", jwt.NewByJwt(server.NewId(), server.NewId(), "gone", false, false))
		requireStateInvalid("not the admin", jwt.NewByJwt(self.networkId, server.NewId(), self.networkName, false, false))
	})
}

// An expired network token renews while reject_expired is off, which lets the
// installed base renew before the gate flips, and is refused once it is on. A
// legacy network token without registered claims renews while
// reject_missing_expiration is off, into a token the gate accepts.
func TestNetworkRefreshFollowsTheExpiryGates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		expired := self.agedNetworkToken()
		expired.IssuedAt = gojwt.NewNumericDate(server.NowUtc().Add(-31 * 24 * time.Hour))
		expired.NotBefore = expired.IssuedAt
		expired.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-24 * time.Hour))

		popExpired := jwt.Testing_SetRejectExpired(false)
		since := server.NowUtc()
		renewed := self.networkRefresh(t, expired.Sign())
		popExpired()
		requireFreshLifetime(t, expired, renewed, since)
		if !renewed.CreateTime.Equal(expired.CreateTime) {
			t.Fatalf("renewed create time = %v, want %v", renewed.CreateTime, expired.CreateTime)
		}

		popExpired = jwt.Testing_SetRejectExpired(true)
		if w := self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+expired.Sign(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("expired token with reject_expired = %d: %s", w.Code, w.Body.String())
		}
		// the token renewed before the flip is current
		self.call(t, http.MethodGet, "/network/clients", "Bearer "+renewed.Sign(), nil, http.StatusOK, nil)
		popExpired()

		legacy := &jwt.ByJwt{
			NetworkId:   self.networkId,
			UserId:      self.userId,
			NetworkName: self.networkName,
			CreateTime:  server.CodecTime(server.NowUtc()),
		}
		popMissing := jwt.Testing_SetRejectMissingExpiration(false)
		var result controller.NetworkRefreshTokenResult
		self.call(t, http.MethodPost, "/auth/network-refresh", "Bearer "+legacy.Sign(), nil, http.StatusOK, &result)
		popMissing()
		if result.Error != nil || result.ByJwt == "" {
			t.Fatalf("legacy refresh = %+v", result)
		}

		popMissing = jwt.Testing_SetRejectMissingExpiration(true)
		defer popMissing()
		if w := self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+legacy.Sign(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("legacy token with reject_missing_expiration = %d: %s", w.Code, w.Body.String())
		}
		// the renewed legacy token carries every registered claim
		migrated, err := jwt.ParseByJwt(ctx, result.ByJwt)
		if err != nil {
			t.Fatal(err)
		}
		if migrated.ExpiresAt == nil || migrated.IssuedAt == nil || migrated.ID == "" || !migrated.CreateTime.Equal(legacy.CreateTime) {
			t.Fatalf("migrated legacy token = %+v", migrated)
		}
		self.call(t, http.MethodGet, "/network/clients", "Bearer "+result.ByJwt, nil, http.StatusOK, nil)
	})
}

// A token created before the last password reset does not renew, and a
// renewed token stops working at the next reset.
func TestNetworkRefreshFollowsCredentialRotation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		renewed := self.networkRefresh(t, self.rootToken)
		self.setCredentialChangeTime(t, server.NowUtc().Add(time.Second))

		if w := self.serve(http.MethodPost, "/auth/network-refresh", self.root(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("a token created before the reset = %d: %s", w.Code, w.Body.String())
		}
		if w := self.serve(http.MethodGet, "/network/clients", "Bearer "+renewed.Sign(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("a token renewed before the reset = %d: %s", w.Code, w.Body.String())
		}
	})
}

// requireRefreshExpiresWithAResetInTheMintGap lands a password reset between
// the router's state check and the mint, stamped after the presented token's
// create time and no later than the mint. The reset's transaction started
// before the mint although it committed after the check: the refreshed token
// must expire with it. A refresh that stamps a fresh create time outlives it.
func requireRefreshExpiresWithAResetInTheMintGap(
	t testing.TB,
	self *routeAccessDbFixture,
	presented *jwt.ByJwt,
	refresh func() *jwt.ByJwt,
	requireClient bool,
) *jwt.ByJwt {
	t.Helper()
	hooked := false
	popHook := controller.Testing_SetRefreshMintHook(func(*session.ClientSession) {
		resetTime := server.NowUtc()
		if !presented.CreateTime.Before(resetTime) {
			resetTime = presented.CreateTime.Add(time.Microsecond)
		}
		self.setCredentialChangeTime(t, resetTime)
		hooked = true
	})
	since := server.NowUtc()
	refreshed := refresh()
	popHook()
	if !hooked {
		t.Fatal("the refresh never reached its mint")
	}

	if err := jwt.ValidateByJwtState(self.ctx, refreshed, requireClient); err == nil {
		t.Fatalf("the token refreshed while the reset committed outlived the reset (create time %v, presented %v)", refreshed.CreateTime, presented.CreateTime)
	}
	if w := self.serve(http.MethodGet, "/network/clients", "Bearer "+refreshed.Sign(), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("the token refreshed while the reset committed = %d: %s", w.Code, w.Body.String())
	}
	if !refreshed.CreateTime.Equal(presented.CreateTime) {
		t.Fatalf("refreshed create time = %v, want the presented %v", refreshed.CreateTime, presented.CreateTime)
	}
	requireFreshLifetime(t, presented, refreshed, since)
	return refreshed
}

// The network token renewed while a password reset commits expires with the
// reset.
func TestNetworkRefreshKeepsTheCreateTimeAcrossAResetInTheMintGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		presented := self.agedNetworkToken()
		requireRefreshExpiresWithAResetInTheMintGap(t, self, presented, func() *jwt.ByJwt {
			return self.networkRefresh(t, presented.Sign())
		}, false)
	})
}

// The client token refreshed while a password reset commits expires with the
// reset, and keeps its client.
func TestClientRefreshKeepsTheCreateTimeAcrossAResetInTheMintGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		clientId, clientToken := self.mintClient(t, self.root(), nil)
		minted, err := jwt.ParseByJwt(ctx, clientToken)
		if err != nil {
			t.Fatal(err)
		}
		// the client token as it is after some refreshes: the root's create
		// time, an older lifetime
		presented := jwt.NewByJwtWithCreateTime(
			self.networkId,
			self.userId,
			self.networkName,
			minted.CreateTime,
			false,
			false,
		).Client(*minted.DeviceId, clientId)
		ageClaims(presented)

		refreshed := requireRefreshExpiresWithAResetInTheMintGap(t, self, presented, func() *jwt.ByJwt {
			var result controller.RefreshTokenResult
			self.call(t, http.MethodGet, "/auth/refresh", "Bearer "+presented.Sign(), nil, http.StatusOK, &result)
			if result.Error != nil || result.ByJwt == "" {
				t.Fatalf("client refresh = %+v", result)
			}
			refreshed, err := jwt.ParseByJwt(ctx, result.ByJwt)
			if err != nil {
				t.Fatal(err)
			}
			return refreshed
		}, true)

		if refreshed.ClientId == nil || *refreshed.ClientId != clientId ||
			refreshed.DeviceId == nil || *refreshed.DeviceId != *minted.DeviceId ||
			refreshed.NetworkId != self.networkId || refreshed.UserId != self.userId {
			body, _ := json.Marshal(refreshed)
			t.Fatalf("refreshed client token = %s", body)
		}
	})
}
