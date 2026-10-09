package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Per-network Embed enablement (EMBED1.md). Pure tests: no database or redis,
// so a gate that queried would fail here.

// primeNetworkEmbedCache sets this process's cached flag for a network, so a
// pure test exercises the Embed gate without a database. An enabled network
// was ever enabled too.
func primeNetworkEmbedCache(t testing.TB, networkId server.Id, enabled bool) {
	t.Helper()
	primeNetworkEmbedCacheState(t, networkId, enabled, enabled)
}

// primeNetworkEmbedCacheState sets both cached meanings for a network: enabled
// now, and ever enabled.
func primeNetworkEmbedCacheState(t testing.TB, networkId server.Id, enabled bool, everEnabled bool) {
	t.Helper()
	networkEmbedLocal.Put(networkId, networkEmbedLocalEntry{
		enabled:     enabled,
		everEnabled: everEnabled,
		expiry:      server.NowUtc().Add(time.Hour),
	})
	t.Cleanup(func() {
		networkEmbedLocal.Remove(networkId)
	})
}

// primeNetworkClientLimitCache sets this process's cached client allowance for
// a network, so a pure test reads it without a database.
func primeNetworkClientLimitCache(t testing.TB, networkId server.Id, limit int, override bool) {
	t.Helper()
	networkClientLimitLocal.Put(networkId, networkClientLimitLocalEntry{
		limit:    limit,
		override: override,
		expiry:   server.NowUtc().Add(time.Hour),
	})
	t.Cleanup(func() {
		networkClientLimitLocal.Remove(networkId)
	})
}

func TestNetworkEmbedLocalCache(t *testing.T) {
	cache := newNetworkEmbedLocalCache()
	networkId := server.NewId()
	now := server.NowUtc()

	_, ok := cache.Get(networkId, now)
	connect.AssertEqual(t, ok, false)

	cache.Put(networkId, networkEmbedLocalEntry{enabled: true, everEnabled: true, expiry: now.Add(time.Minute)})
	entry, ok := cache.Get(networkId, now)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, entry.enabled, true)
	connect.AssertEqual(t, entry.everEnabled, true)

	// a disabled network that was enabled keeps both meanings apart
	cache.Put(networkId, networkEmbedLocalEntry{enabled: false, everEnabled: true, expiry: now.Add(time.Minute)})
	entry, ok = cache.Get(networkId, now)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, entry.enabled, false)
	connect.AssertEqual(t, entry.everEnabled, true)

	// an expired entry is a miss, and is dropped
	_, ok = cache.Get(networkId, now.Add(time.Minute))
	connect.AssertEqual(t, ok, false)
	connect.AssertEqual(t, len(cache.entries), 0)

	// remove and clear
	cache.Put(networkId, networkEmbedLocalEntry{enabled: false, expiry: now.Add(time.Minute)})
	cache.Remove(networkId)
	_, ok = cache.Get(networkId, now)
	connect.AssertEqual(t, ok, false)
	cache.Put(networkId, networkEmbedLocalEntry{enabled: false, expiry: now.Add(time.Minute)})
	cache.Clear()
	connect.AssertEqual(t, len(cache.entries), 0)
}

// A full cache restarts rather than growing past its bound.
func TestNetworkEmbedLocalCacheBounded(t *testing.T) {
	cache := newNetworkEmbedLocalCache()
	expiry := server.NowUtc().Add(time.Minute)
	for range networkEmbedLocalCacheMaxSize {
		cache.Put(server.NewId(), networkEmbedLocalEntry{expiry: expiry})
	}
	connect.AssertEqual(t, len(cache.entries), networkEmbedLocalCacheMaxSize)
	cache.Put(server.NewId(), networkEmbedLocalEntry{expiry: expiry})
	connect.AssertEqual(t, len(cache.entries), 1)
}

// The cached flag answers without a database.
func TestNetworkEmbedEnabledUsesTheCache(t *testing.T) {
	enabledNetworkId := server.NewId()
	disabledNetworkId := server.NewId()
	primeNetworkEmbedCache(t, enabledNetworkId, true)
	primeNetworkEmbedCache(t, disabledNetworkId, false)
	connect.AssertEqual(t, NetworkEmbedEnabled(context.Background(), enabledNetworkId), true)
	connect.AssertEqual(t, NetworkEmbedEnabled(context.Background(), disabledNetworkId), false)
}

// Both cached meanings through a network's Embed life, enabled, disabled and
// enabled again, beside a network that never was: the Embed APIs follow
// "enabled now" and the client token gate follows "ever enabled", so a disable
// closes the APIs without re-opening the admin routes to the network's client
// tokens. Each answer comes from the cache, without a database.
func TestNetworkEmbedStateMeaningsThroughEnableDisableReenable(t *testing.T) {
	ctx := context.Background()
	userId := server.NewId()
	clientId := server.NewId()
	type stage struct {
		name             string
		enabled          bool
		everEnabled      bool
		wantRefusesAdmin bool
		wantApisRefused  bool
	}
	for _, s := range []stage{
		{name: "never enabled", enabled: false, everEnabled: false, wantRefusesAdmin: false, wantApisRefused: true},
		{name: "enabled", enabled: true, everEnabled: true, wantRefusesAdmin: true, wantApisRefused: false},
		{name: "disabled (was enabled)", enabled: false, everEnabled: true, wantRefusesAdmin: true, wantApisRefused: true},
		{name: "enabled again", enabled: true, everEnabled: true, wantRefusesAdmin: true, wantApisRefused: false},
	} {
		t.Run(s.name, func(t *testing.T) {
			networkId := server.NewId()
			primeNetworkEmbedCacheState(t, networkId, s.enabled, s.everEnabled)
			// the default allowance, so only the Embed state decides
			primeNetworkClientLimitCache(t, networkId, LimitTopLevelClientIdsPerNetwork, false)

			connect.AssertEqual(t, NetworkEmbedEnabled(ctx, networkId), s.enabled)
			connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, networkId), s.everEnabled)
			connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, networkId), s.wantRefusesAdmin)

			for _, clientSession := range []*session.ClientSession{
				{Ctx: ctx, ByJwt: session.NewByJwt(networkId, userId, "embed", false, false)},
				{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId, UserId: userId, ClientId: &clientId}},
			} {
				connect.AssertEqual(t, networkEmbedRefused(clientSession), s.wantApisRefused)
				if s.wantApisRefused {
					result, err := SetClientDataCap(&SetClientDataCapArgs{ClientId: clientId}, clientSession)
					connect.AssertEqual(t, err, nil)
					connect.AssertEqual(t, result.Error.Message, NetworkEmbedNotEnabledMessage)
					aclResult, err := GetNetworkClientAclGroup(&GetNetworkClientAclGroupArgs{ClientId: clientId.String()}, clientSession)
					connect.AssertEqual(t, err, nil)
					connect.AssertEqual(t, aclResult.Error.Message, NetworkEmbedNotEnabledMessage)
				}
			}
		})
	}
}

// The client token gate's Embed test is "ever enabled" or the Embed plan's
// client allowance: the allowance alone refuses, and a network with neither
// does not. An ever-enabled network is refused without reading the allowance.
func TestNetworkRefusesClientAdminUsesEverEnabledOrTheAllowance(t *testing.T) {
	ctx := context.Background()

	allowanceNetworkId := server.NewId()
	primeNetworkEmbedCacheState(t, allowanceNetworkId, false, false)
	primeNetworkClientLimitCache(t, allowanceNetworkId, 5000, true)
	connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, allowanceNetworkId), true)

	ordinaryNetworkId := server.NewId()
	primeNetworkEmbedCacheState(t, ordinaryNetworkId, false, false)
	primeNetworkClientLimitCache(t, ordinaryNetworkId, LimitTopLevelClientIdsPerNetwork, false)
	connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, ordinaryNetworkId), false)

	// no allowance cached: reading it would need a database, so the answer
	// comes from the Embed state alone
	wasEnabledNetworkId := server.NewId()
	primeNetworkEmbedCacheState(t, wasEnabledNetworkId, false, true)
	connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, wasEnabledNetworkId), true)
	_, cached := networkClientLimitLocal.Get(wasEnabledNetworkId, server.NowUtc())
	connect.AssertEqual(t, cached, false)
}

// GET /network/embed serves "enabled now" and keeps the was-enabled state to
// the ctl: the response has exactly its three documented fields.
func TestNetworkEmbedJsonOmitsEverEnabled(t *testing.T) {
	for _, embed := range []*NetworkEmbed{
		{Enabled: false, EverEnabled: true, ClientLimit: 100, ActiveClientCount: 3},
		{Enabled: false, EverEnabled: false, ClientLimit: 100, ActiveClientCount: 3},
	} {
		body, err := json.Marshal(&NetworkEmbedResult{NetworkEmbed: embed})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, string(body), `{"enabled":false,"client_limit":100,"active_client_count":3}`)
	}
	body, err := json.Marshal(&NetworkEmbedResult{NetworkEmbed: &NetworkEmbed{Enabled: true, EverEnabled: true, ClientLimit: 5000, ActiveClientCount: 12}})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(body), `{"enabled":true,"client_limit":5000,"active_client_count":12}`)
}

// Every gated route refuses a network that is not Embed-enabled first: before
// its session checks, its argument checks and any query. Each credential is
// covered — the root JWT, an API key and a client JWT.
func TestNetworkEmbedRefusedBeforeAnyQuery(t *testing.T) {
	ctx := context.Background()
	networkId := server.NewId()
	userId := server.NewId()
	clientId := server.NewId()
	primeNetworkEmbedCache(t, networkId, false)

	sessions := map[string]*session.ClientSession{
		"root":    {Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId, UserId: userId}},
		"api key": {Ctx: ctx, ByJwt: session.NewByJwt(networkId, userId, "embed", false, false)},
		"client":  {Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId, UserId: userId, ClientId: &clientId}},
	}
	negative := ByteCount(-5)
	for name, clientSession := range sessions {
		t.Run(name, func(t *testing.T) {
			// valid and invalid arguments alike: the gate answers first
			for _, args := range []*SetClientDataCapArgs{{ClientId: clientId}, {}, {ClientId: clientId, MonthlyByteLimit: &negative}} {
				result, err := SetClientDataCap(args, clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.ClientDataCap, (*ClientDataCap)(nil))
				connect.AssertEqual(t, result.Error.Message, NetworkEmbedNotEnabledMessage)
			}
			for _, args := range []*GetClientDataCapArgs{{ClientId: clientId.String()}, {}, {ClientId: "not-a-uuid"}} {
				result, err := GetClientDataCap(args, clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.Error.Message, NetworkEmbedNotEnabledMessage)
			}
			for _, args := range []*ListClientDataCapsArgs{{}, {Limit: "0"}, {Cursor: "%%%"}} {
				result, err := ListClientDataCaps(args, clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.Clients, ([]*ClientDataCapResult)(nil))
				connect.AssertEqual(t, result.Error.Message, NetworkEmbedNotEnabledMessage)
			}
			for _, args := range []*SetNetworkClientAclGroupArgs{{ClientId: clientId, AclGroup: "isolated"}, {}, {ClientId: clientId, AclGroup: "nope"}} {
				result, err := SetNetworkClientAclGroup(args, clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.NetworkClientAclGroup, (*NetworkClientAclGroup)(nil))
				connect.AssertEqual(t, result.Error.Message, NetworkEmbedNotEnabledMessage)
			}
			for _, args := range []*GetNetworkClientAclGroupArgs{{ClientId: clientId.String()}, {}, {ClientId: "not-a-uuid"}} {
				result, err := GetNetworkClientAclGroup(args, clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.Error.Message, NetworkEmbedNotEnabledMessage)
			}
		})
	}
}

// A session without a network cannot be gated: the route's own session check
// answers, as before.
func TestNetworkEmbedGateLeavesSessionChecks(t *testing.T) {
	for _, clientSession := range []*session.ClientSession{nil, {}, {ByJwt: &session.ByJwt{}}} {
		connect.AssertEqual(t, networkEmbedRefused(clientSession), false)

		setResult, err := SetClientDataCap(&SetClientDataCapArgs{ClientId: server.NewId()}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, setResult.Error.Message, clientDataCapNetworkSessionMessage)

		aclResult, err := SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{ClientId: server.NewId(), AclGroup: "isolated"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, aclResult.Error.Message, networkClientAclGroupSessionMessage)
	}
}

// GET /network/embed takes only a network credential, and refuses a client JWT
// before any query.
func TestGetNetworkEmbedStatusRefusesNonNetworkSessions(t *testing.T) {
	networkId := server.NewId()
	clientId := server.NewId()
	for _, clientSession := range []*session.ClientSession{
		nil,
		{},
		{ByJwt: &session.ByJwt{}},
		{Ctx: context.Background(), ByJwt: &session.ByJwt{NetworkId: networkId, UserId: server.NewId(), ClientId: &clientId}},
	} {
		result, err := GetNetworkEmbedStatus(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.NetworkEmbed, (*NetworkEmbed)(nil))
		connect.AssertEqual(t, result.Error.Message, networkEmbedSessionMessage)
	}
}

// The allowance bounds shared by `network client-limit` and `network embed`.
func TestNetworkTopLevelClientLimitMessage(t *testing.T) {
	for _, limit := range []int{1, 100, MaxNetworkTopLevelClientLimit} {
		connect.AssertEqual(t, networkTopLevelClientLimitMessage(limit), "")
	}
	for _, limit := range []int{0, -1, MaxNetworkTopLevelClientLimit + 1} {
		connect.AssertEqual(t, networkTopLevelClientLimitMessage(limit), fmt.Sprintf("The limit must be between 1 and %d.", MaxNetworkTopLevelClientLimit))
	}
	// an invalid limit is refused before any query
	err := EnableNetworkEmbed(context.Background(), server.NewId(), new(int))
	connect.AssertNotEqual(t, err, nil)
	err = SetNetworkTopLevelClientLimit(context.Background(), server.NewId(), MaxNetworkTopLevelClientLimit+1)
	connect.AssertNotEqual(t, err, nil)
}
