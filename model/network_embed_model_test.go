package model

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Per-network Embed enablement (network_embed_model.go). DB-backed tests
// (postgres + redis). Under the owner rule these run only after the branch,
// with its migrations, is merged to main.

// assertEmbedApisRefused calls every gated route with a valid request for the
// client and asserts the Embed refusal.
func assertEmbedApisRefused(t testing.TB, clientSession *session.ClientSession, clientId server.Id) {
	t.Helper()
	setResult := setDataCapJson(t, clientSession, `{"client_id":"`+clientId.String()+`","monthly_byte_limit":0}`)
	connect.AssertEqual(t, setResult.ClientDataCap, (*ClientDataCap)(nil))
	connect.AssertEqual(t, setResult.Error.Message, NetworkEmbedNotEnabledMessage)
	getResult, err := GetClientDataCap(&GetClientDataCapArgs{ClientId: clientId.String()}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, getResult.ClientDataCap, (*ClientDataCap)(nil))
	connect.AssertEqual(t, getResult.Error.Message, NetworkEmbedNotEnabledMessage)
	listResult, err := ListClientDataCaps(&ListClientDataCapsArgs{}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(listResult.Clients), 0)
	connect.AssertEqual(t, listResult.Error.Message, NetworkEmbedNotEnabledMessage)
	setAclResult := setAclGroup(t, clientSession, clientId, NetworkClientAclGroupIsolated)
	connect.AssertEqual(t, setAclResult.NetworkClientAclGroup, (*NetworkClientAclGroup)(nil))
	connect.AssertEqual(t, setAclResult.Error.Message, NetworkEmbedNotEnabledMessage)
	getAclResult := getAclGroup(t, clientSession, clientId.String())
	connect.AssertEqual(t, getAclResult.NetworkClientAclGroup, (*NetworkClientAclGroup)(nil))
	connect.AssertEqual(t, getAclResult.Error.Message, NetworkEmbedNotEnabledMessage)
}

func countDataCapRows(ctx context.Context, clientId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM network_client_data_cap WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

func readNetworkEmbedEnableTime(ctx context.Context, networkId server.Id) (enableTime time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT enable_time FROM network_embed WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&enableTime))
			}
		})
	})
	return
}

// readNetworkEmbedDisableTime reads the network's disable time: nil without a
// row, or while the network is enabled.
func readNetworkEmbedDisableTime(ctx context.Context, networkId server.Id) (disableTime *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT disable_time FROM network_embed WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&disableTime))
			}
		})
	})
	return
}

// A network that is not Embed-enabled is refused on every gated route — for
// the root token, an API key and a client token — and nothing is written.
// Enabling opens the routes; disabling closes them again.
func TestNetworkEmbedGatesTheEmbedApis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()
		defer Testing_ClearNetworkEmbedCache()

		network := newDataCapTestNetworkWithoutEmbed(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		clientSession := network.clientSession(ctx, clientId)
		sessions := []*session.ClientSession{network.rootSession, network.apiKeySession, clientSession}
		for _, refusedSession := range sessions {
			assertEmbedApisRefused(t, refusedSession, clientId)
		}
		// the refused writes stored nothing
		connect.AssertEqual(t, countDataCapRows(ctx, clientId), 0)
		connect.AssertEqual(t, countAclGroupRows(ctx, clientId), 0)

		// enabling opens every route, for both network credentials
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, nil), nil)
		for i, networkSession := range []*session.ClientSession{network.rootSession, network.apiKeySession} {
			limit := ByteCount(1000 + i)
			setResult := setDataCapJson(t, networkSession, `{"client_id":"`+clientId.String()+`","monthly_byte_limit":`+strconv.FormatInt(limit, 10)+`}`)
			connect.AssertEqual(t, setResult.Error, (*ClientDataCapError)(nil))
			connect.AssertEqual(t, *setResult.MonthlyByteLimit, limit)
			getResult, err := GetClientDataCap(&GetClientDataCapArgs{ClientId: clientId.String()}, networkSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, getResult.Error, (*ClientDataCapError)(nil))
			connect.AssertEqual(t, *getResult.MonthlyByteLimit, limit)
			listResult, err := ListClientDataCaps(&ListClientDataCapsArgs{}, networkSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, listResult.Error, (*ClientDataCapError)(nil))
			connect.AssertEqual(t, len(listResult.Clients), 1)
			connect.AssertEqual(t, listResult.Clients[0].ClientId, clientId)
			connect.AssertEqual(t, setAclGroup(t, networkSession, clientId, NetworkClientAclGroupIsolated).Error, (*NetworkClientAclGroupError)(nil))
			connect.AssertEqual(t, getAclGroup(t, networkSession, clientId.String()).AclGroup, NetworkClientAclGroupIsolated)
		}
		// a client token reads its own cap and group
		getResult, err := GetClientDataCap(&GetClientDataCapArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, getResult.Error, (*ClientDataCapError)(nil))
		connect.AssertEqual(t, *getResult.MonthlyByteLimit, ByteCount(1001))
		connect.AssertEqual(t, getAclGroup(t, clientSession, "").AclGroup, NetworkClientAclGroupIsolated)

		// disabling closes every route again
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		for _, refusedSession := range sessions {
			assertEmbedApisRefused(t, refusedSession, clientId)
		}

		// enabling again opens them, with the cap and group stored before
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, nil), nil)
		getResult, err = GetClientDataCap(&GetClientDataCapArgs{ClientId: clientId.String()}, network.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, getResult.Error, (*ClientDataCapError)(nil))
		connect.AssertEqual(t, *getResult.MonthlyByteLimit, ByteCount(1001))
		connect.AssertEqual(t, getAclGroup(t, clientSession, "").AclGroup, NetworkClientAclGroupIsolated)
	})
}

// GET /network/embed: a network credential reads the flag, the effective
// client allowance and exactly the count the create cap enforces. A client
// token is refused.
func TestGetNetworkEmbedStatus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		// no connected client limit, so only the top-level cap can refuse
		defer Testing_SetConcurrentClientsLimit(0, 0)()
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_ClearNetworkEmbedCache()

		network := newDataCapTestNetworkWithoutEmbed(ctx, "embed")
		status := func(clientSession *session.ClientSession) *NetworkEmbedResult {
			result, err := GetNetworkEmbedStatus(clientSession)
			connect.AssertEqual(t, err, nil)
			return result
		}
		provision := func() *AuthNetworkClientResult {
			result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "user"}, network.rootSession)
			connect.AssertEqual(t, err, nil)
			return result
		}

		// not enabled: still readable, by the root token and an API key
		for _, networkSession := range []*session.ClientSession{network.rootSession, network.apiKeySession} {
			result := status(networkSession)
			connect.AssertEqual(t, result.Error, (*NetworkEmbedError)(nil))
			connect.AssertEqual(t, result.NetworkEmbed, &NetworkEmbed{
				Enabled:           false,
				ClientLimit:       LimitTopLevelClientIdsPerNetwork,
				ActiveClientCount: 0,
			})
		}

		// three top-level clients count; a child client and a provider install
		// do not, as for the create cap
		clientIds := []server.Id{}
		for range 3 {
			clientIds = append(clientIds, network.provisionClient(t, "user:install", nil))
		}
		network.provisionClient(t, "user:install:window", &clientIds[0])
		providerResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "provider", ProvideIntent: true}, network.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, providerResult.Error, (*AuthNetworkClientError)(nil))
		connect.AssertEqual(t, status(network.rootSession).ActiveClientCount, 3)

		// a client token is refused
		result := status(network.clientSession(ctx, clientIds[0]))
		connect.AssertEqual(t, result.NetworkEmbed, (*NetworkEmbed)(nil))
		connect.AssertEqual(t, result.Error.Message, networkEmbedSessionMessage)

		// enabled with an allowance equal to the count: the create cap refuses
		// the next client, so the count is the cap's own
		allowance := 3
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, &allowance), nil)
		connect.AssertEqual(t, status(network.apiKeySession).NetworkEmbed, &NetworkEmbed{
			Enabled:           true,
			EverEnabled:       true,
			ClientLimit:       3,
			ActiveClientCount: 3,
		})
		if result := provision(); result.Error == nil || !result.Error.ClientLimitExceeded {
			t.Fatal("the create cap did not refuse at the reported count")
		}
		// one more allowance admits exactly one more client
		allowance = 4
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, &allowance), nil)
		connect.AssertEqual(t, provision().Error, (*AuthNetworkClientError)(nil))
		connect.AssertEqual(t, status(network.rootSession).NetworkEmbed, &NetworkEmbed{
			Enabled:           true,
			EverEnabled:       true,
			ClientLimit:       4,
			ActiveClientCount: 4,
		})
		if result := provision(); result.Error == nil || !result.Error.ClientLimitExceeded {
			t.Fatal("the create cap did not refuse at the reported count")
		}

		// a removed client leaves the count and frees a slot
		removeResult, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientIds[1]}, network.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Error, (*RemoveNetworkClientError)(nil))
		connect.AssertEqual(t, status(network.rootSession).ActiveClientCount, 3)
		connect.AssertEqual(t, provision().Error, (*AuthNetworkClientError)(nil))
		connect.AssertEqual(t, status(network.rootSession).ActiveClientCount, 4)
		// the create cap's bounded scan agrees at its threshold
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, countNetworkActiveTopLevelClients(ctx, tx, network.networkId, 4+1), 4)
		})

		// `network client-limit` sets the same allowance
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 250), nil)
		connect.AssertEqual(t, status(network.rootSession).ClientLimit, 250)

		// disabled: enabled=false and the default allowance again, while the
		// network stays known as one that was enabled (not served)
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		connect.AssertEqual(t, status(network.rootSession).NetworkEmbed, &NetworkEmbed{
			Enabled:           false,
			EverEnabled:       true,
			ClientLimit:       LimitTopLevelClientIdsPerNetwork,
			ActiveClientCount: 4,
		})
	})
}

// Enable, disable and enable again: unknown networks and invalid limits are
// refused, enable is idempotent and keeps its first time, and the allowance is
// set with the flag and cleared with it. A disable keeps the row with its
// disable time, so the network stays ever-enabled and its client tokens stay
// refused on the admin routes; enabling again clears the disable time.
func TestNetworkEmbedEnableDisable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_ClearNetworkEmbedCache()

		connect.AssertEqual(t, EnableNetworkEmbed(ctx, server.NewId(), nil), ErrNetworkNotFound)
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, server.NewId()), ErrNetworkNotFound)

		network := newDataCapTestNetworkWithoutEmbed(ctx, "embed")
		for _, invalid := range []int{0, -1, MaxNetworkTopLevelClientLimit + 1} {
			connect.AssertNotEqual(t, EnableNetworkEmbed(ctx, network.networkId, &invalid), nil)
		}
		// a refused enable changes nothing
		connect.AssertEqual(t, GetNetworkEmbed(ctx, network.networkId), &NetworkEmbed{
			Enabled:     false,
			ClientLimit: LimitTopLevelClientIdsPerNetwork,
		})

		// enable without a limit keeps the default allowance
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, nil), nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, GetNetworkTopLevelClientLimit(ctx, network.networkId).Override, false)
		enableTime := readNetworkEmbedEnableTime(ctx, network.networkId)
		connect.AssertEqual(t, enableTime.IsZero(), false)

		// enabling again keeps the first enable time and sets the allowance,
		// which this process applies to the concurrent limit at once
		limit := 5000
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, &limit), nil)
		if again := readNetworkEmbedEnableTime(ctx, network.networkId); !again.Equal(enableTime) {
			t.Fatalf("enable time moved from %s to %s", enableTime, again)
		}
		clientLimit := GetNetworkTopLevelClientLimit(ctx, network.networkId)
		connect.AssertEqual(t, clientLimit.Limit, 5000)
		connect.AssertEqual(t, clientLimit.Override, true)
		connect.AssertEqual(t, networkConcurrentClientLimit(ctx, network.networkId), 5000)

		connect.AssertEqual(t, readNetworkEmbedDisableTime(ctx, network.networkId), (*time.Time)(nil))
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)

		// disable clears the flag and the allowance override, and keeps the row:
		// the first enable time and the disable time
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, network.networkId), true)
		if again := readNetworkEmbedEnableTime(ctx, network.networkId); !again.Equal(enableTime) {
			t.Fatalf("enable time moved from %s to %s", enableTime, again)
		}
		disableTime := readNetworkEmbedDisableTime(ctx, network.networkId)
		if disableTime == nil || disableTime.Before(enableTime) {
			t.Fatalf("disable time = %v after enable time %s", disableTime, enableTime)
		}
		connect.AssertEqual(t, GetNetworkTopLevelClientLimit(ctx, network.networkId).Override, false)
		_, override := networkClientLimitOverride(ctx, network.networkId)
		connect.AssertEqual(t, override, false)
		connect.AssertEqual(t, GetNetworkEmbed(ctx, network.networkId), &NetworkEmbed{
			Enabled:     false,
			EverEnabled: true,
			ClientLimit: LimitTopLevelClientIdsPerNetwork,
		})

		// disabling a disabled network succeeds and keeps its first disable time
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		if again := readNetworkEmbedDisableTime(ctx, network.networkId); again == nil || !again.Equal(*disableTime) {
			t.Fatalf("disable time moved from %s to %v", disableTime, again)
		}

		// enabling again clears the disable time and keeps the first enable time
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, nil), nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, readNetworkEmbedDisableTime(ctx, network.networkId), (*time.Time)(nil))
		if again := readNetworkEmbedEnableTime(ctx, network.networkId); !again.Equal(enableTime) {
			t.Fatalf("enable time moved from %s to %s", enableTime, again)
		}
		connect.AssertEqual(t, GetNetworkEmbed(ctx, network.networkId), &NetworkEmbed{
			Enabled:     true,
			EverEnabled: true,
			ClientLimit: LimitTopLevelClientIdsPerNetwork,
		})

		// and disabling again sets a new disable time
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)
		if again := readNetworkEmbedDisableTime(ctx, network.networkId); again == nil || again.Before(*disableTime) {
			t.Fatalf("second disable time = %v, before the first %s", again, disableTime)
		}

		// a disable of a network that was never enabled writes no row
		neverNetwork := newDataCapTestNetworkWithoutEmbed(ctx, "never")
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, neverNetwork.networkId), nil)
		connect.AssertEqual(t, readNetworkEmbedEnableTime(ctx, neverNetwork.networkId).IsZero(), true)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, neverNetwork.networkId), false)
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, neverNetwork.networkId), false)
	})
}

// The state is cached per process, both meanings from one read. A row written
// outside this process (ops on another host) is seen once the entry expires,
// or at once with a cleared cache; Enable and Disable refresh this process at
// once. GET /network/embed reads the db. Through a disable the ever-enabled
// answer stays true: in the stale entry, after a reload and after a local
// disable.
func TestNetworkEmbedCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_ClearNetworkEmbedCache()

		network := newDataCapTestNetworkWithoutEmbed(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), false)

		// another host enables directly: this process keeps its cached refusal,
		// on the gated routes too
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_embed (network_id, enable_time) VALUES ($1, $2)`,
				network.networkId,
				server.NowUtc(),
			))
		})
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, getAclGroup(t, network.rootSession, clientId.String()).Error.Message, NetworkEmbedNotEnabledMessage)
		connect.AssertEqual(t, GetNetworkEmbed(ctx, network.networkId).Enabled, true)

		// an expired entry reloads both meanings
		networkEmbedLocal.Put(network.networkId, networkEmbedLocalEntry{enabled: false, everEnabled: false, expiry: server.NowUtc()})
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, getAclGroup(t, network.rootSession, clientId.String()).AclGroup, NetworkClientAclGroupDefault)
		// and is cached for the ttl
		now := server.NowUtc()
		entry, ok := networkEmbedLocal.Get(network.networkId, now)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, entry.enabled, true)
		connect.AssertEqual(t, entry.everEnabled, true)
		if !now.Before(entry.expiry) || now.Add(networkEmbedLocalCacheTtl).Before(entry.expiry) {
			t.Fatalf("expiry %s is not within the ttl of %s", entry.expiry, now)
		}

		// another host disables directly: this process keeps its stale entry,
		// and a cleared cache sees the disable at once, while the network stays
		// ever-enabled and its client tokens refused throughout
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_embed SET disable_time = $2 WHERE network_id = $1`,
				network.networkId,
				server.NowUtc(),
			))
		})
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, network.networkId), true)
		Testing_ClearNetworkEmbedCache()
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, network.networkId), true)
		connect.AssertEqual(t, getAclGroup(t, network.rootSession, clientId.String()).Error.Message, NetworkEmbedNotEnabledMessage)
		entry, ok = networkEmbedLocal.Get(network.networkId, server.NowUtc())
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, entry.enabled, false)
		connect.AssertEqual(t, entry.everEnabled, true)

		// Enable and Disable refresh this process at once
		connect.AssertEqual(t, EnableNetworkEmbed(ctx, network.networkId, nil), nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)
		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		connect.AssertEqual(t, NetworkEmbedEnabled(ctx, network.networkId), false)
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), true)

		// only deleting the row, which no code path does, forgets the network
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_embed WHERE network_id = $1`, network.networkId))
		})
		Testing_ClearNetworkEmbedCache()
		connect.AssertEqual(t, NetworkEmbedEverEnabled(ctx, network.networkId), false)
	})
}

// Disabling closes the APIs, not enforcement: a stored cap still refuses
// escrow for the client and its child, and an isolated client stays isolated.
func TestNetworkEmbedDisableKeepsEnforcement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()
		defer Testing_ResetClientDataCapState()
		Testing_ClearNetworkPeersEnabledCache()
		defer Testing_ClearNetworkEmbedCache()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		childId := network.provisionClient(t, "user:alice:window", &clientId)
		otherClientId := network.provisionClient(t, "user:bob", nil)
		registerTestPeer(t, ctx, network.networkId, clientId)
		registerTestPeer(t, ctx, network.networkId, otherClientId)

		paused := setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`","monthly_byte_limit":0}`)
		connect.AssertEqual(t, paused.Error, (*ClientDataCapError)(nil))
		connect.AssertEqual(t, paused.Capped, true)
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, clientId, NetworkClientAclGroupIsolated).Error, (*NetworkClientAclGroupError)(nil))

		connect.AssertEqual(t, DisableNetworkEmbed(ctx, network.networkId), nil)
		// the APIs are closed, and the refused calls changed nothing
		assertEmbedApisRefused(t, network.apiKeySession, otherClientId)
		connect.AssertEqual(t, countDataCapRows(ctx, otherClientId), 0)
		connect.AssertEqual(t, countAclGroupRows(ctx, otherClientId), 0)

		// the stored cap still refuses escrow, for the client and its child,
		// from this host's snapshot and from a reloaded one
		admission := func(payerClientId server.Id) (err error) {
			server.Db(ctx, func(conn server.PgConn) {
				err = clientDataCapEscrowError(ctx, conn, network.networkId, payerClientId, 1024, server.NowUtc())
			})
			return
		}
		for range 2 {
			err := admission(clientId)
			connect.AssertNotEqual(t, err, nil)
			connect.AssertEqual(t, err.Error(), "Insufficient balance (0).")
			connect.AssertNotEqual(t, admission(childId), nil)
			connect.AssertEqual(t, admission(otherClientId), nil)
			Testing_ResetClientDataCapState()
		}

		// the client stays isolated: unlisted, receiving no peer list, and its
		// next connection registers as isolated
		connect.AssertEqual(t, GetNetworkClientAclGroupForClient(ctx, clientId), NetworkClientAclGroupIsolated)
		_, _, category, _, _ := GetNetworkPeerProfile(ctx, clientId)
		connect.AssertEqual(t, category, NetworkPeerCategoryIsolated)
		peersIsolated, err := GetNetworkPeersForSession(network.clientSession(ctx, clientId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersIsolated.Peers), 0)
		peersOther, err := GetNetworkPeersForSession(network.clientSession(ctx, otherClientId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersOther.Peers), 0)
		peersRoot, err := GetNetworkPeersForSession(network.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersRoot.Peers), 1)
		connect.AssertEqual(t, peersRoot.Peers[0].ClientId, otherClientId)
	})
}
