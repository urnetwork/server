package model

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

func setAclGroup(t testing.TB, clientSession *session.ClientSession, clientId server.Id, aclGroup string) *NetworkClientAclGroupResult {
	result, err := SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{ClientId: clientId, AclGroup: aclGroup}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result
}

func getAclGroup(t testing.TB, clientSession *session.ClientSession, clientId string) *NetworkClientAclGroupResult {
	result, err := GetNetworkClientAclGroup(&GetNetworkClientAclGroupArgs{ClientId: clientId}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result
}

func countAclGroupRows(ctx context.Context, clientId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM network_client_acl_group WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

func TestNetworkClientAclGroupAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice:laptop", nil)
		otherClientId := network.provisionClient(t, "user:bob:phone", nil)
		childId := network.provisionClient(t, "user:alice:laptop:window", &clientId)

		// a client never set is in the default group, with no row
		result := getAclGroup(t, network.rootSession, clientId.String())
		connect.AssertEqual(t, result.Error, (*NetworkClientAclGroupError)(nil))
		connect.AssertEqual(t, result.ClientId, clientId)
		connect.AssertEqual(t, result.AclGroup, NetworkClientAclGroupDefault)

		// the root token and an API key both set and read
		result = setAclGroup(t, network.rootSession, clientId, NetworkClientAclGroupIsolated)
		connect.AssertEqual(t, result.Error, (*NetworkClientAclGroupError)(nil))
		connect.AssertEqual(t, result.ClientId, clientId)
		connect.AssertEqual(t, result.AclGroup, NetworkClientAclGroupIsolated)
		connect.AssertEqual(t, countAclGroupRows(ctx, clientId), 1)
		for _, networkSession := range []*session.ClientSession{network.rootSession, network.apiKeySession} {
			connect.AssertEqual(t, getAclGroup(t, networkSession, clientId.String()).AclGroup, NetworkClientAclGroupIsolated)
		}

		// setting the same group again is a no-op success
		connect.AssertEqual(t, setAclGroup(t, network.apiKeySession, clientId, NetworkClientAclGroupIsolated).AclGroup, NetworkClientAclGroupIsolated)
		connect.AssertEqual(t, countAclGroupRows(ctx, clientId), 1)

		// a client token reads its own group, with or without naming itself
		clientSession := network.clientSession(ctx, clientId)
		connect.AssertEqual(t, getAclGroup(t, clientSession, "").AclGroup, NetworkClientAclGroupIsolated)
		connect.AssertEqual(t, getAclGroup(t, clientSession, clientId.String()).AclGroup, NetworkClientAclGroupIsolated)
		// ... but not another client's
		connect.AssertEqual(t, getAclGroup(t, clientSession, otherClientId.String()).Error.Message, "A client token can only read its own ACL group.")
		// and may not set any group, its own included
		connect.AssertEqual(t, setAclGroup(t, clientSession, clientId, NetworkClientAclGroupDefault).Error.Message, networkClientAclGroupSessionMessage)
		connect.AssertEqual(t, getAclGroup(t, network.rootSession, clientId.String()).AclGroup, NetworkClientAclGroupIsolated)

		// a child client's token reads its top-level client's group
		childResult := getAclGroup(t, network.clientSession(ctx, childId), "")
		connect.AssertEqual(t, childResult.ClientId, clientId)
		connect.AssertEqual(t, childResult.AclGroup, NetworkClientAclGroupIsolated)

		// a group applies to top-level clients only
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, childId, NetworkClientAclGroupIsolated).Error.Message, "ACL groups apply to top-level clients.")
		connect.AssertEqual(t, getAclGroup(t, network.rootSession, childId.String()).Error.Message, "ACL groups apply to top-level clients.")

		// a network session needs a client id to read
		connect.AssertEqual(t, getAclGroup(t, network.rootSession, "").Error.Message, "client_id is required.")

		// a client of another network is not found, for set and get
		foreign := newDataCapTestNetwork(ctx, "other")
		foreignClientId := foreign.provisionClient(t, "user:mallory", nil)
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, foreignClientId, NetworkClientAclGroupIsolated).Error.Message, "Client not found in this network.")
		connect.AssertEqual(t, getAclGroup(t, network.apiKeySession, foreignClientId.String()).Error.Message, "Client not found in this network.")
		connect.AssertEqual(t, getAclGroup(t, foreign.rootSession, foreignClientId.String()).AclGroup, NetworkClientAclGroupDefault)
		connect.AssertEqual(t, countAclGroupRows(ctx, foreignClientId), 0)
		// and an unknown client likewise
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, server.NewId(), NetworkClientAclGroupIsolated).Error.Message, "Client not found in this network.")

		// default removes the row
		result = setAclGroup(t, network.apiKeySession, clientId, NetworkClientAclGroupDefault)
		connect.AssertEqual(t, result.Error, (*NetworkClientAclGroupError)(nil))
		connect.AssertEqual(t, result.AclGroup, NetworkClientAclGroupDefault)
		connect.AssertEqual(t, countAclGroupRows(ctx, clientId), 0)
		connect.AssertEqual(t, GetNetworkClientAclGroupForClient(ctx, clientId), NetworkClientAclGroupDefault)
	})
}

func nominateTestResident(t testing.TB, ctx context.Context, clientId server.Id) *NetworkClientResident {
	resident := &NetworkClientResident{
		ClientId:              clientId,
		InstanceId:            server.NewId(),
		ResidentId:            server.NewId(),
		ResidentHost:          "host0",
		ResidentService:       "connect",
		ResidentBlock:         "test",
		ResidentInternalPorts: []int{5080},
	}
	if !NominateResident(ctx, nil, resident, time.Minute) {
		t.Fatalf("nominate resident for %s", clientId)
	}
	return resident
}

func registerTestPeer(t testing.TB, ctx context.Context, networkId server.Id, clientId server.Id) *NetworkClientResident {
	resident := nominateTestResident(t, ctx, clientId)
	_, topLevel, category, profile, _ := GetNetworkPeerProfile(ctx, clientId)
	connect.AssertEqual(t, topLevel, true)
	connect.AssertEqual(t, category, NetworkPeerCategoryClient)
	AddNetworkPeer(ctx, networkId, profile, resident.ResidentId, time.Minute)
	return resident
}

func networkPeerIds(peers []*NetworkPeer) (connected map[server.Id]bool, disconnected map[server.Id]bool) {
	connected = map[server.Id]bool{}
	disconnected = map[server.Id]bool{}
	for _, peer := range peers {
		if peer.DisconnectTime != nil {
			disconnected[peer.ClientId] = true
		} else {
			connected[peer.ClientId] = true
		}
	}
	return
}

func networkIsolatedZsetScore(ctx context.Context, networkId server.Id, clientId server.Id) (score float64, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.ZScore(ctx, networkPeerConnectedProxyKey(networkId), string(clientId.Bytes())).Result()
		if err == nil {
			score = value
			ok = true
		} else if err != redis.Nil {
			panic(err)
		}
	})
	return
}

// Isolating a connected client drops it from the peer list for every reader,
// with no disconnect marker, keeps it counted, empties its own peer read, and
// retires its resident so its next connection registers as isolated. Going
// back to default undoes each.
func TestNetworkClientAclGroupPeerExclusion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ClearNetworkPeersEnabledCache()

		network := newDataCapTestNetwork(ctx, "embed")
		clientIdA := network.provisionClient(t, "user:alice:laptop", nil)
		clientIdB := network.provisionClient(t, "user:bob:phone", nil)
		residentA := registerTestPeer(t, ctx, network.networkId, clientIdA)
		residentB := registerTestPeer(t, ctx, network.networkId, clientIdB)

		_, peers := GetNetworkPeers(ctx, network.networkId)
		connected, _ := networkPeerIds(peers)
		connect.AssertEqual(t, connected[clientIdA], true)
		connect.AssertEqual(t, connected[clientIdB], true)
		connectedCount := GetNetworkConnectedCount(ctx, network.networkId)
		connect.AssertEqual(t, connectedCount, 2)
		eventIdBefore := GetNetworkPeerEventId(ctx, network.networkId)

		// isolate b
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, clientIdB, NetworkClientAclGroupIsolated).Error, (*NetworkClientAclGroupError)(nil))

		eventId, peers := GetNetworkPeers(ctx, network.networkId)
		connected, disconnected := networkPeerIds(peers)
		connect.AssertEqual(t, connected[clientIdA], true)
		connect.AssertEqual(t, connected[clientIdB], false)
		// reported neither connected nor recently disconnected
		connect.AssertEqual(t, disconnected[clientIdB], false)
		// listeners full-read on the bumped version
		if eventId <= eventIdBefore {
			t.Fatalf("event id %d did not advance past %d", eventId, eventIdBefore)
		}
		// still counted, now in the isolated (proxy) zset
		connect.AssertEqual(t, GetNetworkConnectedCount(ctx, network.networkId), connectedCount)
		_, isolatedRegistered := networkIsolatedZsetScore(ctx, network.networkId, clientIdB)
		connect.AssertEqual(t, isolatedRegistered, true)
		connect.AssertEqual(t, GetNetworkPeerMember(ctx, network.networkId, clientIdB), (*NetworkPeer)(nil))

		// b's resident is retired; a's is untouched
		connect.AssertEqual(t, GetResidentForClient(ctx, clientIdB, 0), (*NetworkClientResident)(nil))
		residentAfter := GetResidentForClient(ctx, clientIdA, 0)
		connect.AssertNotEqual(t, residentAfter, (*NetworkClientResident)(nil))
		connect.AssertEqual(t, residentAfter.ResidentId, residentA.ResidentId)
		_ = residentB

		// b's next connection registers as isolated
		_, topLevel, category, _, _ := GetNetworkPeerProfile(ctx, clientIdB)
		connect.AssertEqual(t, topLevel, true)
		connect.AssertEqual(t, category, NetworkPeerCategoryIsolated)

		// an isolated client receives no peer list; others do not see it
		peersB, err := GetNetworkPeersForSession(network.clientSession(ctx, clientIdB))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, peersB.Error, (*NetworkPeersError)(nil))
		connect.AssertEqual(t, len(peersB.Peers), 0)
		connect.AssertEqual(t, peersB.DisconnectedCount, 0)
		peersA, err := GetNetworkPeersForSession(network.clientSession(ctx, clientIdA))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersA.Peers), 0)
		peersRoot, err := GetNetworkPeersForSession(network.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersRoot.Peers), 1)
		connect.AssertEqual(t, peersRoot.Peers[0].ClientId, clientIdA)

		// back to default: dropped from the isolated zset (its next connection
		// registers as a listed peer), and it reads the peer list again
		residentB2 := nominateTestResident(t, ctx, clientIdB)
		connect.AssertEqual(t, setAclGroup(t, network.apiKeySession, clientIdB, NetworkClientAclGroupDefault).Error, (*NetworkClientAclGroupError)(nil))
		_, isolatedRegistered = networkIsolatedZsetScore(ctx, network.networkId, clientIdB)
		connect.AssertEqual(t, isolatedRegistered, false)
		connect.AssertEqual(t, GetResidentForClient(ctx, clientIdB, 0), (*NetworkClientResident)(nil))
		_ = residentB2
		_, _, category, _, _ = GetNetworkPeerProfile(ctx, clientIdB)
		connect.AssertEqual(t, category, NetworkPeerCategoryClient)
		peersB, err = GetNetworkPeersForSession(network.clientSession(ctx, clientIdB))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersB.Peers), 1)
		connect.AssertEqual(t, peersB.Peers[0].ClientId, clientIdA)
	})
}

// The isolate transition removes every listed trace in one step and moves a
// connected client to the counted zset with its remaining ttl; repeating it on
// a client that is not listed changes nothing visible.
func TestIsolateNetworkPeerMovesTheRegistration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:carol", nil)
		registerTestPeer(t, ctx, network.networkId, clientId)

		var connectedScore float64
		server.Redis(ctx, func(r server.RedisClient) {
			score, err := r.ZScore(ctx, networkPeerConnectedKey(network.networkId), string(clientId.Bytes())).Result()
			if err != nil {
				panic(err)
			}
			connectedScore = score
		})

		isolateNetworkPeer(ctx, network.networkId, clientId)
		server.Redis(ctx, func(r server.RedisClient) {
			member := string(clientId.Bytes())
			exists, err := r.HExists(ctx, networkPeerMetaKey(network.networkId), member).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, exists, false)
			memberKeyCount, err := r.Exists(ctx, networkPeerMemberKey(network.networkId, clientId)).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, memberKeyCount, int64(0))
			_, err = r.ZScore(ctx, networkPeerConnectedKey(network.networkId), member).Result()
			connect.AssertEqual(t, err, redis.Nil)
			_, err = r.ZScore(ctx, networkPeerDisconnectedKey(network.networkId), member).Result()
			connect.AssertEqual(t, err, redis.Nil)
		})
		isolatedScore, ok := networkIsolatedZsetScore(ctx, network.networkId, clientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, isolatedScore, connectedScore)

		eventId := GetNetworkPeerEventId(ctx, network.networkId)
		isolateNetworkPeer(ctx, network.networkId, clientId)
		connect.AssertEqual(t, GetNetworkPeerEventId(ctx, network.networkId), eventId)
	})
}

// The peer valve does not count isolated clients: a network one client over the
// limit keeps its peer list once that client is isolated, and loses it again
// when the client returns to default.
func TestNetworkPeersEnabledExcludesIsolatedClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// creation is uncapped while enforcement is dark
		defer Testing_SetEnforceConcurrentClients(false)()
		Testing_ClearNetworkPeersEnabledCache()
		defer Testing_ClearNetworkPeersEnabledCache()

		network := newDataCapTestNetwork(ctx, "embed")
		clientIds := []server.Id{}
		for i := range LimitTopLevelClientIdsPerNetwork + 1 {
			clientIds = append(clientIds, network.provisionClient(t, "user:install:"+string(rune('a'+i%26)), nil))
		}
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, network.networkId), false)

		// SetNetworkClientAclGroup drops the network's valve cache entry
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, clientIds[0], NetworkClientAclGroupIsolated).Error, (*NetworkClientAclGroupError)(nil))
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, network.networkId), true)

		connect.AssertEqual(t, setAclGroup(t, network.rootSession, clientIds[0], NetworkClientAclGroupDefault).Error, (*NetworkClientAclGroupError)(nil))
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, network.networkId), false)
	})
}
