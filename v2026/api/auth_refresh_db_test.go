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
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
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
func (self *routeAccessDbFixture) agedNetworkToken() *session.ByJwt {
	byJwt := session.NewByJwt(self.networkId, self.userId, self.networkName, false, false)
	ageClaims(byJwt)
	return byJwt
}

func ageClaims(byJwt *session.ByJwt) {
	issued := server.NowUtc().Add(-time.Hour)
	byJwt.IssuedAt = gojwt.NewNumericDate(issued)
	byJwt.NotBefore = gojwt.NewNumericDate(issued)
	byJwt.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
}

// networkRefresh renews the token and returns the renewed token, parsed.
func (self *routeAccessDbFixture) networkRefresh(t testing.TB, presented string) *session.ByJwt {
	t.Helper()
	var result controller.NetworkRefreshTokenResult
	self.call(t, http.MethodPost, "/auth/network-refresh", "Bearer "+presented, nil, http.StatusOK, &result)
	if result.Error != nil || result.ByJwt == "" {
		t.Fatalf("network refresh = %+v", result)
	}
	renewed, err := session.ParseByJwt(self.ctx, result.ByJwt)
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
func requireFreshLifetime(t testing.TB, presented *session.ByJwt, refreshed *session.ByJwt, since time.Time) {
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
	if refreshed.Issuer != session.ByJwtIssuer || refreshed.Subject != refreshed.UserId.String() ||
		!slices.Contains(refreshed.Audience, session.ByJwtAudienceApi) || refreshed.NotBefore == nil {
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
		var renewed *session.ByJwt
		requireNetworkRefreshCounted(t, "renewed", func() {
			renewed = self.networkRefresh(t, presented.Testing_Sign())
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
		if err := session.ValidateByJwtState(ctx, renewed, false); err != nil {
			t.Fatal(err)
		}
		self.call(t, http.MethodGet, "/network/clients", "Bearer "+renewed.Testing_Sign(), nil, http.StatusOK, nil)
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
		clientByJwt, err := session.ParseByJwt(ctx, clientToken)
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

		requireStateInvalid := func(name string, byJwt *session.ByJwt) {
			requireNetworkRefreshCounted(t, "state_invalid", func() {
				staleSession := session.Testing_CreateClientSession(ctx, byJwt)
				defer staleSession.Cancel()
				result, err := controller.NetworkRefreshToken(staleSession)
				if err == nil || !strings.HasPrefix(err.Error(), "401 ") || result != nil {
					t.Fatalf("%s = %+v, %v; want 401", name, result, err)
				}
			})
		}
		requireStateInvalid("no network", session.NewByJwt(server.NewId(), server.NewId(), "gone", false, false))
		requireStateInvalid("not the admin", session.NewByJwt(self.networkId, server.NewId(), self.networkName, false, false))
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

		popExpired := session.Testing_SetRejectExpired(false)
		since := server.NowUtc()
		renewed := self.networkRefresh(t, expired.Testing_Sign())
		popExpired()
		requireFreshLifetime(t, expired, renewed, since)
		if !renewed.CreateTime.Equal(expired.CreateTime) {
			t.Fatalf("renewed create time = %v, want %v", renewed.CreateTime, expired.CreateTime)
		}

		popExpired = session.Testing_SetRejectExpired(true)
		if w := self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+expired.Testing_Sign(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("expired token with reject_expired = %d: %s", w.Code, w.Body.String())
		}
		// the token renewed before the flip is current
		self.call(t, http.MethodGet, "/network/clients", "Bearer "+renewed.Testing_Sign(), nil, http.StatusOK, nil)
		popExpired()

		legacy := &session.ByJwt{
			NetworkId:   self.networkId,
			UserId:      self.userId,
			NetworkName: self.networkName,
			CreateTime:  server.CodecTime(server.NowUtc()),
		}
		popMissing := session.Testing_SetRejectMissingExpiration(false)
		var result controller.NetworkRefreshTokenResult
		self.call(t, http.MethodPost, "/auth/network-refresh", "Bearer "+legacy.Testing_Sign(), nil, http.StatusOK, &result)
		popMissing()
		if result.Error != nil || result.ByJwt == "" {
			t.Fatalf("legacy refresh = %+v", result)
		}

		popMissing = session.Testing_SetRejectMissingExpiration(true)
		defer popMissing()
		if w := self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+legacy.Testing_Sign(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("legacy token with reject_missing_expiration = %d: %s", w.Code, w.Body.String())
		}
		// the renewed legacy token carries every registered claim
		migrated, err := session.ParseByJwt(ctx, result.ByJwt)
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
		if w := self.serve(http.MethodGet, "/network/clients", "Bearer "+renewed.Testing_Sign(), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("a token renewed before the reset = %d: %s", w.Code, w.Body.String())
		}
	})
}

// withRefreshMintHook runs f with hook between the router's state check and
// the mint of /auth/refresh and /auth/network-refresh, and restores the
// previous hook however f exits. A failed assertion in f exits with
// runtime.Goexit, so a hook restored only after f returned stayed installed,
// and every later refresh in the package ran the failed test's reset with its
// canceled context: a 503 for TestRealClientTokenActsOnlyForItsOwnClients.
func withRefreshMintHook(hook func(*session.ClientSession), f func()) {
	defer controller.Testing_SetRefreshMintHook(hook)()
	f()
}

// resetTimeInTheMintGap is a password reset's cutoff read in the mint gap,
// after the presented token's create time and before the mint.
func resetTimeInTheMintGap(presented *session.ByJwt) time.Time {
	resetTime := server.NowUtc()
	if !presented.CreateTime.Before(resetTime) {
		resetTime = presented.CreateTime.Add(time.Microsecond)
	}
	return resetTime
}

// requireRefreshExpiresWithAResetInTheMintGap lands a password reset whose
// cutoff is read between the router's state check and the mint, after the
// presented token's create time, and which commits after the refresh. The
// renewal's own check cannot see it. That is a reset on a host whose clock is
// behind the minting host's: the reset takes the session lifecycle lock and
// reads its cutoff after the renewal released the lock, and that clock still
// reads a time before the mint. The refreshed token must expire with it, and
// does because it keeps the presented create time. A refresh that stamps a
// fresh create time at the mint outlives it.
func requireRefreshExpiresWithAResetInTheMintGap(
	t testing.TB,
	self *routeAccessDbFixture,
	presented *session.ByJwt,
	refresh func() *session.ByJwt,
	requireClient bool,
) *session.ByJwt {
	t.Helper()
	hooked := false
	var resetTime time.Time
	since := server.NowUtc()
	var refreshed *session.ByJwt
	withRefreshMintHook(func(*session.ClientSession) {
		resetTime = resetTimeInTheMintGap(presented)
		hooked = true
	}, func() {
		refreshed = refresh()
	})
	if !hooked {
		t.Fatal("the refresh never reached its mint")
	}
	// the reset commits after the renewal
	self.setCredentialChangeTime(t, resetTime)

	if err := session.ValidateByJwtState(self.ctx, refreshed, requireClient); err == nil {
		t.Fatalf("the token refreshed in the mint gap outlived the reset (create time %v, presented %v, reset %v)", refreshed.CreateTime, presented.CreateTime, resetTime)
	}
	if w := self.serve(http.MethodGet, "/network/clients", "Bearer "+refreshed.Testing_Sign(), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("the token refreshed in the mint gap, after the reset = %d: %s", w.Code, w.Body.String())
	}
	if !refreshed.CreateTime.Equal(presented.CreateTime) {
		t.Fatalf("refreshed create time = %v, want the presented %v", refreshed.CreateTime, presented.CreateTime)
	}
	requireFreshLifetime(t, presented, refreshed, since)
	return refreshed
}

// requireRefreshRefusedForAResetCommittedInTheMintGap commits a password reset
// between the router's state check and the mint. The renewal checks the
// presented token again, under the session lifecycle lock that every reset
// holds exclusively (16675a430), so it refuses the token with the 401
// authentication gives it, and mints nothing that could outlive the reset.
func requireRefreshRefusedForAResetCommittedInTheMintGap(
	t testing.TB,
	self *routeAccessDbFixture,
	presented *session.ByJwt,
	method string,
	path string,
) {
	t.Helper()
	hooked := false
	var w *httptest.ResponseRecorder
	withRefreshMintHook(func(*session.ClientSession) {
		self.setCredentialChangeTime(t, resetTimeInTheMintGap(presented))
		hooked = true
	}, func() {
		w = self.serve(method, path, "Bearer "+presented.Testing_Sign(), nil)
	})
	if !hooked {
		t.Fatal("the refresh never reached its mint")
	}
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "by_jwt") {
		t.Fatalf("%s %s with a reset committed in the mint gap = %d: %s, want 401 and no token", method, path, w.Code, w.Body.String())
	}
	if w := self.serve(method, path, "Bearer "+presented.Testing_Sign(), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("%s %s after the reset = %d: %s, want the same 401", method, path, w.Code, w.Body.String())
	}
}

// agedClientToken mints a client and returns its client token as it is after
// some refreshes: the root's create time, an older lifetime.
func (self *routeAccessDbFixture) agedClientToken(t testing.TB) (clientId server.Id, minted *session.ByJwt, presented *session.ByJwt) {
	t.Helper()
	clientId, clientToken := self.mintClient(t, self.root(), nil)
	minted, err := session.ParseByJwt(self.ctx, clientToken)
	if err != nil {
		t.Fatal(err)
	}
	presented = session.NewByJwtWithCreateTime(
		self.networkId,
		self.userId,
		self.networkName,
		minted.CreateTime,
		false,
		false,
	).Client(*minted.DeviceId, clientId)
	ageClaims(presented)
	return clientId, minted, presented
}

// The network token renewed in the mint gap of a password reset that commits
// after the renewal expires with the reset.
func TestNetworkRefreshKeepsTheCreateTimeAcrossAResetInTheMintGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		presented := self.agedNetworkToken()
		requireRefreshExpiresWithAResetInTheMintGap(t, self, presented, func() *session.ByJwt {
			return self.networkRefresh(t, presented.Testing_Sign())
		}, false)
	})
}

// A network token whose password reset commits in the mint gap is refused with
// 401, counted as a stale state, not a 500.
func TestNetworkRefreshRefusesAResetCommittedInTheMintGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		presented := self.agedNetworkToken()
		requireNetworkRefreshCounted(t, "state_invalid", func() {
			requireRefreshRefusedForAResetCommittedInTheMintGap(t, self, presented, http.MethodPost, "/auth/network-refresh")
		})
	})
}

// A client token whose password reset commits in the mint gap is refused with
// 401, not a 500.
func TestClientRefreshRefusesAResetCommittedInTheMintGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		_, _, presented := self.agedClientToken(t)
		requireRefreshRefusedForAResetCommittedInTheMintGap(t, self, presented, http.MethodGet, "/auth/refresh")
	})
}

// A network token whose session is revoked in the mint gap is refused with the
// structured 401 session_revoked authentication gives a revoked session, which
// the apps read as a sign-out from another device, not a 500.
func TestNetworkRefreshRefusesASessionRevokedInTheMintGap(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		// a tracked sign-in: the renewal registers the root token's session
		tracked := self.networkRefresh(t, self.rootToken)
		if tracked.SessionId == nil {
			t.Fatal("the renewed network token carries no session")
		}
		hooked := false
		var w *httptest.ResponseRecorder
		requireNetworkRefreshCounted(t, "state_invalid", func() {
			withRefreshMintHook(func(*session.ClientSession) {
				// the network's backend revokes the sign-in
				self.call(t, http.MethodPost, "/network/revoke-session", self.key(), map[string]any{"session_id": *tracked.SessionId, "operation_id": server.NewId()}, http.StatusOK, nil)
				hooked = true
			}, func() {
				w = self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+tracked.Testing_Sign(), nil)
			})
		})
		if !hooked {
			t.Fatal("the refresh never reached its mint")
		}
		var refusal struct {
			Code string `json:"code"`
		}
		if w.Code != http.StatusUnauthorized || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Code != "session_revoked" {
			t.Fatalf("network refresh of a session revoked in the mint gap = %d: %s, want 401 session_revoked", w.Code, w.Body.String())
		}
	})
}

// The mint hook of a refresh that fails does not run in a later refresh: a
// failed assertion exits its goroutine with runtime.Goexit, past any restore
// that waits for the refresh to return.
func TestTheRefreshMintHookDoesNotOutliveAFailedRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		ran := false
		exited := make(chan struct{})
		go func() {
			defer close(exited)
			withRefreshMintHook(func(*session.ClientSession) {
				ran = true
			}, func() {
				// how t.Fatalf leaves a failed refresh
				runtime.Goexit()
			})
		}()
		<-exited

		self.networkRefresh(t, self.rootToken)
		if ran {
			t.Fatal("the mint hook of a failed refresh ran in a later refresh")
		}
	})
}

// The client token refreshed in the mint gap of a password reset that commits
// after the refresh expires with the reset, and keeps its client.
func TestClientRefreshKeepsTheCreateTimeAcrossAResetInTheMintGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		clientId, minted, presented := self.agedClientToken(t)

		refreshed := requireRefreshExpiresWithAResetInTheMintGap(t, self, presented, func() *session.ByJwt {
			var result controller.RefreshTokenResult
			self.call(t, http.MethodGet, "/auth/refresh", "Bearer "+presented.Testing_Sign(), nil, http.StatusOK, &result)
			if result.Error != nil || result.ByJwt == "" {
				t.Fatalf("client refresh = %+v", result)
			}
			refreshed, err := session.ParseByJwt(ctx, result.ByJwt)
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
