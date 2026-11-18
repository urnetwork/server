package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/prometheus/client_golang/prometheus/testutil"
	promdto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server"
)

// A client credential of the network's own (a top-level client is its own
// root), with a fresh session.
func sessionClientFixtureCredential(networkId server.Id, now time.Time) *ByJwt {
	claims := sessionFixtureCredential(networkId, now)
	client, device := server.NewId(), server.NewId()
	claims.ClientId, claims.DeviceId, claims.RootClientId = &client, &device, &client
	return claims
}

func sessionLiveMembers(t testing.TB, ctx context.Context, networkId server.Id, suffix string) []string {
	t.Helper()
	var members []string
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		members, err = r.ZRange(ctx, SessionKey(networkId, suffix), 0, -1).Result()
		return err
	}))
	return members
}

func sessionContains(members []string, sid server.Id) bool {
	for _, member := range members {
		if member == sid.String() {
			return true
		}
	}
	return false
}

// R-dark: while pro.yml enforce_session_cap is false no mint is ever refused
// at a cap, live or retained. The pre-2026-11-18 script refused every
// network's 1,001st live session, and every new session of a network holding
// 10,000 retained ids, whatever any switch said.
func TestSessionCapDarkNeverRefusesAMint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		for i := 0; i < 1001; i++ {
			if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), "password", nil, false, now); err != nil {
				t.Fatalf("dark cap refused mint %d: %s", i+1, err)
			}
		}
		retained := server.NewId()
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Eval(ctx, `for i=1,10000 do redis.call('ZADD',KEYS[1],ARGV[1],'retained-'..i) end;return 1`, []string{SessionKey(retained, "held")}, now.Add(90*24*time.Hour).UnixMilli()).Err()
		}))
		if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(retained, now), "password", nil, false, now); err != nil {
			t.Fatal("dark retained cap refused a mint", err)
		}
	})
}

// R-dark: a mint over a cap while the cap is not enforced is admitted and
// counted by kind and cap; a refresh of a live session never is.
func TestSessionCapDarkCountsWouldRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetSessionCapPolicy(SessionCapPolicy{Live: 3, Retained: NetworkSessionRetainedLimit, Enforce: false})()
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		first := sessionFixtureCredential(network, now)
		for i, claims := range []*ByJwt{first, sessionFixtureCredential(network, now), sessionFixtureCredential(network, now)} {
			registered, err := RegisterNetworkSession(ctx, claims, "password", nil, false, now)
			if err != nil || registered.Would != "" {
				t.Fatal("under the cap", i, registered, err)
			}
		}
		signinBefore := testutil.ToFloat64(sessionCapWouldRefuse.WithLabelValues(SessionKindSignin, "live"))
		clientBefore := testutil.ToFloat64(sessionCapWouldRefuse.WithLabelValues(SessionKindClient, "live"))
		over, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), "password", nil, false, now)
		if err != nil || over.Would != "live" || !over.Created {
			t.Fatal("dark cap refused or did not report", over, err)
		}
		overClient, err := registerNetworkSession(ctx, sessionClientFixtureCredential(network, now), MintKindApiKeyClient, nil, false, now, sessionExemptNo)
		if err != nil || overClient.Would != "live" {
			t.Fatal("dark cap refused a client session", overClient, err)
		}
		if got := testutil.ToFloat64(sessionCapWouldRefuse.WithLabelValues(SessionKindSignin, "live")) - signinBefore; got != 1 {
			t.Fatal("signin would-refusals", got)
		}
		if got := testutil.ToFloat64(sessionCapWouldRefuse.WithLabelValues(SessionKindClient, "live")) - clientBefore; got != 1 {
			t.Fatal("client would-refusals", got)
		}
		refresh, err := RegisterNetworkSession(ctx, first, "password", nil, false, now)
		if err != nil || refresh.Would != "" || refresh.Created {
			t.Fatal("a refresh over the cap counted", refresh, err)
		}
		if live := sessionLiveMembers(t, ctx, network, "z"); len(live) != 5 {
			t.Fatal("dark admissions missing", len(live))
		}
	})
}

// R-refuse: an enforced cap refuses a new session with 409
// session_limit_reached and its message, evicts nothing, and still renews a
// live session.
func TestSessionCapEnforcedRefusesWithoutEviction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetSessionCapPolicy(SessionCapPolicy{Live: 3, Retained: NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		first := sessionFixtureCredential(network, now)
		for _, claims := range []*ByJwt{first, sessionFixtureCredential(network, now), sessionFixtureCredential(network, now)} {
			if _, err := RegisterNetworkSession(ctx, claims, "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		before := sessionLiveMembers(t, ctx, network, "z")
		refusedBefore := testutil.ToFloat64(sessionCapRefused.WithLabelValues(SessionKindCode, "live"))
		_, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), MintKindAuthCode, nil, false, now)
		var refusal *SessionError
		if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict || refusal.Code != "session_limit_reached" {
			t.Fatal("enforced cap admitted", err)
		}
		body, _ := json.Marshal(refusal.HttpErrorResultBody())
		if string(body) != `{"code":"session_limit_reached","error":"Too many active sessions. Sign out of sessions you no longer use in Settings → Sessions, then try again."}` {
			t.Fatal("refusal body", string(body))
		}
		if got := testutil.ToFloat64(sessionCapRefused.WithLabelValues(SessionKindCode, "live")) - refusedBefore; got != 1 {
			t.Fatal("refusals counted", got)
		}
		after := sessionLiveMembers(t, ctx, network, "z")
		if len(after) != len(before) {
			t.Fatal("refusal changed the live set", before, after)
		}
		for _, member := range before {
			sid, err := server.ParseId(member)
			if err != nil || !sessionContains(after, sid) {
				t.Fatal("refusal evicted", member)
			}
		}
		if _, err := RegisterNetworkSession(ctx, first, "password", nil, false, now); err != nil {
			t.Fatal("live session refresh refused at the cap", err)
		}
	})
}

// R-cap: a client session whose top-level client provides publicly is not
// counted, and is admitted at the cap; the exemption follows the client's
// provide state.
func TestSessionCapExcludesProviderClientSessions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetSessionCapPolicy(SessionCapPolicy{Live: 2, Retained: NetworkSessionRetainedLimit, Enforce: true})()
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		providers := []*ByJwt{}
		for i := 0; i < 3; i++ {
			provider := sessionClientFixtureCredential(network, now)
			if _, err := registerNetworkSession(ctx, provider, MintKindApiKeyClient, nil, false, now, sessionExemptYes); err != nil {
				t.Fatal("provider session refused", err)
			}
			providers = append(providers, provider)
		}
		for i := 0; i < 2; i++ {
			if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), "password", nil, false, now); err != nil {
				t.Fatal("providers were counted", err)
			}
		}
		if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), "password", nil, false, now); err == nil {
			t.Fatal("the cap admitted a third counted session")
		}
		// a provider session is admitted at the cap
		late := sessionClientFixtureCredential(network, now)
		if _, err := registerNetworkSession(ctx, late, MintKindApiKeyClient, nil, false, now, sessionExemptYes); err != nil {
			t.Fatal("a provider session was refused at the cap", err)
		}
		// a child's mint keeps the exemption its root decided
		child := *providers[0]
		childId := server.NewId()
		child.ClientId = &childId
		if _, err := RegisterNetworkSession(ctx, &child, MintKindLegacy, nil, false, now); err != nil {
			t.Fatal(err)
		}
		if !sessionContains(sessionLiveMembers(t, ctx, network, "xz"), *providers[0].SessionId) {
			t.Fatal("a child's mint cleared its root's exemption")
		}
		// the client stops providing: its session counts again
		if err := UpdateSessionProvideExemption(ctx, network, *providers[0].SessionId, false); err != nil {
			t.Fatal(err)
		}
		if sessionContains(sessionLiveMembers(t, ctx, network, "xz"), *providers[0].SessionId) {
			t.Fatal("the exemption outlived provide")
		}
		// a sign-in session is never exempt
		other := server.NewId()
		signin := sessionFixtureCredential(other, now)
		if _, err := RegisterNetworkSession(ctx, signin, "password", nil, false, now); err != nil {
			t.Fatal(err)
		}
		if err := UpdateSessionProvideExemption(ctx, other, *signin.SessionId, true); err != nil {
			t.Fatal(err)
		}
		if sessionContains(sessionLiveMembers(t, ctx, other, "xz"), *signin.SessionId) {
			t.Fatal("a sign-in session became exempt")
		}
	})
}

// R-free: freeing removes only live client-owned sessions that no network
// credential carries, with no marker; it never frees a sign-in, code or
// revoked session, nor a per-client lineage a network credential took up.
func TestSessionFreeClientSessionsFreesOnlyClientOwned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		apiKey := sessionClientFixtureCredential(network, now)
		proxy := sessionClientFixtureCredential(network, now)
		lineage := sessionClientFixtureCredential(network, now)
		shared := sessionClientFixtureCredential(network, now)
		revoked := sessionClientFixtureCredential(network, now)
		signin := sessionFixtureCredential(network, now)
		code := sessionFixtureCredential(network, now)
		for _, mint := range []struct {
			claims *ByJwt
			kind   string
		}{{apiKey, MintKindApiKeyClient}, {proxy, MintKindHostedProxy}, {lineage, MintKindLegacy}, {shared, MintKindLegacy}, {revoked, MintKindDeviceAdopt}, {signin, "password"}, {code, MintKindAuthCode}} {
			if _, err := RegisterNetworkSession(ctx, mint.claims, mint.kind, nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		// the network credential of the lineage's sign-in takes it up
		network2 := *shared
		network2.ClientId, network2.DeviceId, network2.RootClientId = nil, nil, nil
		if _, err := RegisterNetworkSession(ctx, &network2, MintKindLegacy, nil, false, now); err != nil {
			t.Fatal(err)
		}
		if _, err := enforceSessionRevoke(ctx, network, server.NewId(), "single", revoked.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		before := testutil.ToFloat64(sessionFreedOnRemoval.WithLabelValues(SessionFreePathRemoveClient))
		freed, err := FreeClientSessions(ctx, network, []server.Id{*apiKey.SessionId, *proxy.SessionId, *lineage.SessionId, *shared.SessionId, *revoked.SessionId, *signin.SessionId, *code.SessionId}, SessionFreePathRemoveClient)
		if err != nil || freed != 3 {
			t.Fatal("freed", freed, err)
		}
		if got := testutil.ToFloat64(sessionFreedOnRemoval.WithLabelValues(SessionFreePathRemoveClient)) - before; got != 3 {
			t.Fatal("frees counted", got)
		}
		live, held := sessionLiveMembers(t, ctx, network, "z"), sessionLiveMembers(t, ctx, network, "held")
		for _, sid := range []server.Id{*apiKey.SessionId, *proxy.SessionId, *lineage.SessionId} {
			if sessionContains(live, sid) || sessionContains(held, sid) {
				t.Fatal("a freed session stayed", sid)
			}
			if err := CheckSession(ctx, network, sid); err != nil {
				t.Fatal("a free wrote a marker", err)
			}
		}
		for _, sid := range []server.Id{*shared.SessionId, *signin.SessionId, *code.SessionId} {
			if !sessionContains(live, sid) {
				t.Fatal("a session a network credential carries was freed", sid)
			}
		}
		if !errors.Is(CheckSession(ctx, network, *revoked.SessionId), ErrSessionRevoked) || !sessionContains(held, *revoked.SessionId) {
			t.Fatal("a free dropped a revoked session's marker")
		}
	})
}

// R-reset: a password reset, sign-out-all (reset) and sign out other sessions
// (others) retire every session but a hosted proxy's own; a single revoke of
// the proxy's own session still retires it.
func TestSessionResetAndOthersSkipOnlyHostedProxySessions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		type mint struct {
			claims *ByJwt
			kind   string
		}
		register := func(mints ...mint) {
			for _, m := range mints {
				if _, err := RegisterNetworkSession(ctx, m.claims, m.kind, nil, false, now); err != nil {
					t.Fatal(err)
				}
			}
		}
		bootstrapped, created := sessionClientFixtureCredential(network, now), sessionClientFixtureCredential(network, now)
		apiKey, device, lineage := sessionClientFixtureCredential(network, now), sessionClientFixtureCredential(network, now), sessionClientFixtureCredential(network, now)
		signin, code := sessionFixtureCredential(network, now), sessionFixtureCredential(network, now)
		register(mint{bootstrapped, MintKindLegacyProxy}, mint{created, MintKindHostedProxy}, mint{apiKey, MintKindApiKeyClient}, mint{device, MintKindDeviceAdopt}, mint{lineage, MintKindLegacy}, mint{signin, "password"}, mint{code, MintKindAuthCode})
		reset, err := enforceSessionRevoke(ctx, network, server.NewId(), "reset", nil, nil, now.Add(time.Second))
		if err != nil || reset.RevokedCount != 5 {
			t.Fatal("reset", reset, err)
		}
		for _, claims := range []*ByJwt{apiKey, device, lineage, signin, code} {
			if !errors.Is(CheckSession(ctx, network, *claims.SessionId), ErrSessionRevoked) {
				t.Fatal("reset spared a non-proxy session", *claims.SessionId)
			}
		}
		for _, claims := range []*ByJwt{bootstrapped, created} {
			if err := CheckSession(ctx, network, *claims.SessionId); err != nil {
				t.Fatal("reset retired a hosted proxy session", err)
			}
		}
		keep, other := sessionFixtureCredential(network, now), sessionClientFixtureCredential(network, now)
		register(mint{keep, "password"}, mint{other, MintKindApiKeyClient})
		others, err := enforceSessionRevoke(ctx, network, server.NewId(), "others", nil, keep.SessionId)
		if err != nil || others.RevokedCount != 1 || !errors.Is(CheckSession(ctx, network, *other.SessionId), ErrSessionRevoked) {
			t.Fatal("others", others, err)
		}
		for _, claims := range []*ByJwt{bootstrapped, created, keep} {
			if err := CheckSession(ctx, network, *claims.SessionId); err != nil {
				t.Fatal("others retired a hosted proxy or the kept session", err)
			}
		}
		single, err := enforceSessionRevoke(ctx, network, server.NewId(), "single", created.SessionId, nil)
		if err != nil || single.RevokedCount != 1 || !errors.Is(CheckSession(ctx, network, *created.SessionId), ErrSessionRevoked) {
			t.Fatal("a hosted proxy's own revoke", single, err)
		}
		if sessionContains(sessionLiveMembers(t, ctx, network, "zh"), *created.SessionId) {
			t.Fatal("a revoked session stayed in its class index")
		}
	})
}

// The list: session_kind and client_kind for every row, counts for the whole
// network, and cursor pages that walk every live session once, sign-ins
// first.
func TestSessionListPagesByKind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		actor := recoverySessionActor(t)
		network := actor.ByJwt.NetworkId
		kinds := map[string]string{}
		register := func(claims *ByJwt, kind string, exempt string) {
			if _, err := registerNetworkSession(ctx, claims, kind, nil, false, now, exempt); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 3; i++ {
			claims := sessionFixtureCredential(network, now)
			register(claims, "google", sessionExemptKeep)
			kinds[claims.SessionId.String()] = "signin/"
		}
		codeClaims := sessionFixtureCredential(network, now)
		register(codeClaims, MintKindAuthCode, sessionExemptKeep)
		kinds[codeClaims.SessionId.String()] = "code/"
		for i := 0; i < 4; i++ {
			claims := sessionClientFixtureCredential(network, now)
			register(claims, MintKindHostedProxy, sessionExemptNo)
			kinds[claims.SessionId.String()] = "client/hosted_proxy"
		}
		provider := sessionClientFixtureCredential(network, now)
		register(provider, MintKindApiKeyClient, sessionExemptYes)
		kinds[provider.SessionId.String()] = "client/api_key"
		adopted := sessionClientFixtureCredential(network, now)
		register(adopted, MintKindDeviceAdopt, sessionExemptNo)
		kinds[adopted.SessionId.String()] = "client/device"
		lineage := sessionClientFixtureCredential(network, now)
		register(lineage, MintKindLegacy, sessionExemptNo)
		kinds[lineage.SessionId.String()] = "client/legacy"
		kinds[actor.ByJwt.SessionId.String()] = "signin/"

		// every page size walks every live session once: the current session
		// first, then the sign-ins and codes, then the client sessions
		for _, limit := range []int{1, 3, NetworkSessionsPageLimit} {
			seen := map[string]bool{}
			order := []string{}
			var cursor string
			for page := 0; ; page++ {
				result, err := GetNetworkSessionsPage(actor, &NetworkSessionsArgs{Cursor: cursor, Limit: limit})
				if err != nil {
					t.Fatal(limit, err)
				}
				if page == 0 {
					counts := *result.Counts
					if counts.Live != 12 || counts.Capped != 11 || counts.Provider != 1 || counts.Signin != 4 || counts.Code != 1 || counts.HostedProxy != 4 || counts.ApiKey != 1 || counts.Device != 1 || counts.Legacy != 1 {
						t.Fatalf("counts %+v", counts)
					}
					if len(result.Sessions) == 0 || !result.Sessions[0].Current {
						t.Fatal("the current session does not lead the first page", limit)
					}
				}
				if limit < len(result.Sessions) {
					t.Fatal("page over its limit", limit, len(result.Sessions))
				}
				for _, row := range result.Sessions {
					id := row.SessionId.String()
					if seen[id] {
						t.Fatal("a page repeated a session", limit, id)
					}
					seen[id] = true
					clientKind := ""
					if row.ClientKind != nil {
						clientKind = *row.ClientKind
					}
					if got := row.SessionKind + "/" + clientKind; got != kinds[id] {
						t.Fatalf("%s: %s, want %s", id, got, kinds[id])
					}
					if row.Provider != (id == provider.SessionId.String()) {
						t.Fatal("provider flag", id)
					}
					order = append(order, row.SessionKind)
				}
				if result.NextCursor == nil {
					break
				}
				cursor = *result.NextCursor
			}
			if len(seen) != len(kinds) {
				t.Fatal("pages missed sessions", limit, len(seen), len(kinds))
			}
			for i := 0; i < 5; i++ {
				if order[i] == SessionKindClient {
					t.Fatal("a client session came before the sign-ins", limit, order)
				}
			}
		}
		filtered, err := GetNetworkSessionsPage(actor, &NetworkSessionsArgs{SessionKind: SessionKindClient, ClientKind: ClientKindHostedProxy})
		if err != nil || len(filtered.Sessions) != 4 || filtered.NextCursor != nil {
			t.Fatal("client_kind filter", err)
		}
		codes, err := GetNetworkSessionsPage(actor, &NetworkSessionsArgs{SessionKind: SessionKindCode})
		if err != nil || len(codes.Sessions) != 1 || codes.Sessions[0].SessionId != *codeClaims.SessionId {
			t.Fatal("code filter", err)
		}
		signins, err := GetNetworkSessionsPage(actor, &NetworkSessionsArgs{SessionKind: SessionKindSignin})
		if err != nil || len(signins.Sessions) != 4 {
			t.Fatal("signin filter", err)
		}
		for _, args := range []*NetworkSessionsArgs{{SessionKind: "other"}, {ClientKind: "other"}, {SessionKind: SessionKindSignin, ClientKind: ClientKindApiKey}, {Limit: NetworkSessionsPageLimit + 1}, {Cursor: "not-a-cursor"}} {
			if _, err := GetNetworkSessionsPage(actor, args); err == nil {
				t.Fatalf("invalid request admitted %+v", args)
			}
		}
		// a cursor read with other filters is refused
		first, err := GetNetworkSessionsPage(actor, &NetworkSessionsArgs{Limit: 1})
		if err != nil || first.NextCursor == nil {
			t.Fatal(err)
		}
		if _, err := GetNetworkSessionsPage(actor, &NetworkSessionsArgs{Cursor: *first.NextCursor, SessionKind: SessionKindClient}); err == nil {
			t.Fatal("a cursor crossed filters")
		}
	})
}

// S1 W7: a new session publishes its network's review at once and queues no
// outbox row; a refresh touches neither; only a failed publication queues the
// outbox. Every mint used to upsert the network's one outbox row, which only
// the 64-row repair published.
func TestSessionMintPublishesNewMemberIndexWithoutOutbox(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		actor := recoverySessionActor(t)
		network := actor.ByJwt.NetworkId
		outboxRows := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_session_index_outbox WHERE network_id=$1`, network).Scan(&count))
			})
			return
		}
		indexScore := func() (float64, bool) {
			keys := sessionIndexKeys(network)
			var score float64
			err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
				var err error
				score, err = r.ZScore(ctx, keys[0], network.String()).Result()
				return err
			})
			if errors.Is(err, server.RedisNil) {
				return 0, false
			}
			server.Raise(err)
			return score, true
		}
		claims := NewByJwt(network, actor.ByJwt.UserId, "session-index", false, false)
		sessionId := server.NewId()
		claims.SessionId = &sessionId
		if _, err := MintNetworkSession(ctx, claims, "password"); err != nil {
			t.Fatal(err)
		}
		score, published := indexScore()
		if !published || int64(score) != DeadlineMillis(claims.AcceptUntil()) {
			t.Fatal("the new session's network was not published", score, published)
		}
		if outboxRows() != 0 {
			t.Fatal("a published mint queued the outbox")
		}
		renewed := claims.Renew()
		server.Raise(sessionTx(ctx, func(tx server.PgTx) error {
			_, err := RegisterAndSignInTx(ctx, tx, renewed, "password", nil, false)
			return err
		}))
		if outboxRows() != 0 {
			t.Fatal("a refresh queued the outbox")
		}
		defer Testing_FailSessionIndexPublishOnMint()()
		failed := NewByJwt(network, actor.ByJwt.UserId, "session-index", false, false)
		failedId := server.NewId()
		failed.SessionId = &failedId
		if _, err := MintNetworkSession(ctx, failed, "password"); err != nil {
			t.Fatal(err)
		}
		if outboxRows() != 1 {
			t.Fatal("a failed publication did not queue the outbox")
		}
	})
}

// S1 W11 (the session half): the revision a hint reads carries no members.
func TestSessionRevisionReadCarriesNoMembers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		for i := 0; i < 3; i++ {
			if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		settings := defaultSessionInventorySettings()
		var raw json.RawMessage
		server.Raise(sessionEval(ctx, sessionRevisionLua, sessionSharedKeys(network), []any{now.UnixMilli(), server.NewId().String(), settings.retention.Milliseconds(), settings.retentionHeadroom.Milliseconds()}, &raw))
		var fields map[string]json.RawMessage
		server.Raise(json.Unmarshal(raw, &fields))
		if _, ok := fields["members"]; ok || len(fields) != 2 {
			t.Fatal("the revision read returned more than the revision", string(raw))
		}
		generation, eventId, err := NetworkSessionRevision(ctx, network)
		inventory, inventoryErr := readSessionInventory(ctx, network, server.NowUtc())
		if err != nil || inventoryErr != nil || generation != inventory.Generation || eventId != inventory.EventId {
			t.Fatal("the revision read differs from the inventory's", err, inventoryErr)
		}
	})
}

// The level sample observes every network of the maintenance index that has
// a live set, once, with what the cap counts.
func TestSessionLevelSampleObservesEveryLiveNetworkOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		withLive, providersOnly, retainedOnly := server.NewId(), server.NewId(), server.NewId()
		for i := 0; i < 12; i++ {
			if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(withLive, now), "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := registerNetworkSession(ctx, sessionClientFixtureCredential(providersOnly, now), MintKindApiKeyClient, nil, false, now, sessionExemptYes); err != nil {
			t.Fatal(err)
		}
		revoked := sessionFixtureCredential(retainedOnly, now)
		if _, err := RegisterNetworkSession(ctx, revoked, "password", nil, false, now); err != nil {
			t.Fatal(err)
		}
		if _, err := enforceSessionRevoke(ctx, retainedOnly, server.NewId(), "single", revoked.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		for _, network := range []server.Id{withLive, providersOnly, retainedOnly} {
			server.Raise(publishSessionIndex(ctx, network, now.Add(time.Hour), server.NewId().String()))
		}
		// each test has its own redis db: the index holds this test's networks
		count, sum := levelHistogram(t)
		complete := testutil.ToFloat64(sessionLiveSamplePasses.WithLabelValues("complete"))
		observed, done, err := SampleSessionLevels(ctx, now)
		if err != nil || !done || observed != 2 {
			t.Fatal("pass", observed, done, err)
		}
		afterCount, afterSum := levelHistogram(t)
		// the network with 12 live sessions, and the one whose only session a
		// provider's is (0 counted); the retained-only network has no live set
		if afterCount-count != 2 || afterSum-sum != 12 {
			t.Fatal("histogram", afterCount-count, afterSum-sum)
		}
		if testutil.ToFloat64(sessionLiveSamplePasses.WithLabelValues("complete"))-complete != 1 {
			t.Fatal("pass not counted")
		}
	})
}

func levelHistogram(t testing.TB) (uint64, float64) {
	t.Helper()
	metric := &promdto.Metric{}
	server.Raise(sessionLivePerNetwork.Write(metric))
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}

// The new session metrics push under the stats pusher's grouping labels
// (grafana.go): the push client refuses every push of a process whose metric
// declares a grouping label as its own.
func TestSessionCapMetricsPushUnderTheStatsGrouping(t *testing.T) {
	grouping := map[string]bool{"env": true, "service": true, "block": true, "host": true, "instance": true}
	collectors := []prometheus.Collector{sessionCapWouldRefuse, sessionCapRefused, sessionFreedOnRemoval, sessionLivePerNetwork, sessionLiveSamplePasses, sessionOperationRecoveryFailures}
	registry := prometheus.NewRegistry()
	for _, collector := range collectors {
		descriptions := make(chan *prometheus.Desc, 16)
		collector.Describe(descriptions)
		close(descriptions)
		for description := range descriptions {
			for label := range grouping {
				if containsLabel(description.String(), label) {
					t.Fatalf("%s declares the grouping label %s", description, label)
				}
			}
		}
		registry.MustRegister(collector)
	}
	var pushCount atomic.Int32
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer pushServer.Close()
	pusher := push.New(pushServer.URL, "api").
		Gatherer(registry).
		Grouping("env", "test").
		Grouping("service", "api").
		Grouping("block", "push-test").
		Grouping("host", "push-test").
		Grouping("instance", "push-test")
	for range 2 {
		if err := pusher.Push(); err != nil {
			t.Fatal(err)
		}
	}
	if pushCount.Load() != 2 {
		t.Fatal("pushes", pushCount.Load())
	}
}

// Whether a metric description declares the label, as variableLabels lists
// them: {label} or a label among others.
func containsLabel(description string, label string) bool {
	for _, form := range []string{"variableLabels: {" + label + "}", "variableLabels: {" + label + ",", "," + label + ",", "," + label + "}"} {
		if contains(description, form) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// S2 N1: a sign out of other sessions whose kept session ended before it was
// enforced fails for good and releases its quota. Left prepared, the journal
// held every other session of the network unavailable, and recovery could
// never apply it.
func TestRevokeOthersWhoseKeptSessionEndedFailsAndReleasesTheNetwork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		other := NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "session-recovery", false, false)
		sid := server.NewId()
		other.SessionId = &sid
		if _, err := RegisterNetworkSession(ctx, other, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		op := server.NewId()
		if err := prepareSessionOperation(actor, op, "others", nil, actor.ByJwt.SessionId); err != nil {
			t.Fatal(err)
		}
		// the kept session is revoked from another device before enforcement
		if _, err := enforceSessionRevoke(ctx, actor.ByJwt.NetworkId, server.NewId(), "single", actor.ByJwt.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		_, err := EnforceSessionOperation(ctx, actor.ByJwt.NetworkId, op)
		var refusal *SessionError
		if !errors.As(err, &refusal) || refusal.Code != "current_session_required" {
			t.Fatal("enforcement", err)
		}
		var status string
		var reserved bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT status,quota_reserved FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`, actor.ByJwt.NetworkId, op).Scan(&status, &reserved))
		})
		if status != "failed" || reserved {
			t.Fatal("the operation stayed", status, reserved)
		}
		server.Raise(sessionTx(ctx, func(tx server.PgTx) error {
			if err := sessionJournalFence(ctx, tx, actor.ByJwt.NetworkId, sid, &other.CreateTime); err != nil {
				t.Fatal("the failed operation still fences the network", err)
			}
			return nil
		}))
	})
}

// S2 R5 (the release-1 invariant, pinned): with new session creation off, a
// credential that already carries a session still registers and extends it
// before signing, and a revoked one is refused.
func TestSessionDarkCreationStillExtendsTaggedRenewals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(false)()
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		network := actor.ByJwt.NetworkId
		score := func() float64 {
			var value float64
			server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
				var err error
				value, err = r.ZScore(ctx, SessionKey(network, "z"), actor.ByJwt.SessionId.String()).Result()
				return err
			}))
			return value
		}
		before := score()
		renewed := actor.ByJwt.Renew()
		renewed.ExpiresAt = gojwt.NewNumericDate(actor.ByJwt.ExpiresAt.Time.Add(24 * time.Hour))
		server.Raise(sessionTx(ctx, func(tx server.PgTx) error {
			_, err := RegisterAndSignInTx(ctx, tx, renewed, "password", nil, false)
			return err
		}))
		if score() <= before {
			t.Fatal("a dark tagged renewal did not extend its session")
		}
		if _, err := enforceSessionRevoke(ctx, network, server.NewId(), "single", actor.ByJwt.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		err := sessionTx(ctx, func(tx server.PgTx) error {
			_, err := RegisterAndSignInTx(ctx, tx, actor.ByJwt.Renew(), "password", nil, false)
			return err
		})
		if !errors.Is(err, ErrSessionRevoked) && !isSessionCode(err, "session_revoked") {
			t.Fatal("a dark renewal of a revoked session was signed", err)
		}
	})
}

func isSessionCode(err error, code string) bool {
	var refusal *SessionError
	return errors.As(err, &refusal) && refusal.Code == code
}
