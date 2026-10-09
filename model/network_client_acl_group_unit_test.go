package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// Pure tests: no database or redis.

func TestParseNetworkClientAclGroup(t *testing.T) {
	for _, value := range []string{"default", "isolated"} {
		aclGroup, ok := parseNetworkClientAclGroup(value)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, aclGroup, value)
	}
	// exact names only: no trimming, no case folding, no empty default
	for _, value := range []string{"", " isolated", "Isolated", "ISOLATED", "isolate", "public", "default "} {
		aclGroup, ok := parseNetworkClientAclGroup(value)
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, aclGroup, "")
	}
}

// A hosted proxy device and a provider install keep their own categories (both
// are already never listed); the ACL group only changes an ordinary client.
func TestNetworkPeerCategory(t *testing.T) {
	connect.AssertEqual(t, networkPeerCategory(false, false, false), NetworkPeerCategoryClient)
	connect.AssertEqual(t, networkPeerCategory(false, false, true), NetworkPeerCategoryIsolated)
	connect.AssertEqual(t, networkPeerCategory(true, false, false), NetworkPeerCategoryProxy)
	connect.AssertEqual(t, networkPeerCategory(true, false, true), NetworkPeerCategoryProxy)
	connect.AssertEqual(t, networkPeerCategory(false, true, false), NetworkPeerCategoryProvider)
	connect.AssertEqual(t, networkPeerCategory(false, true, true), NetworkPeerCategoryProvider)
	connect.AssertEqual(t, networkPeerCategory(true, true, true), NetworkPeerCategoryProxy)

	// the categories are distinct values the resident branches on
	connect.AssertNotEqual(t, NetworkPeerCategoryIsolated, NetworkPeerCategoryClient)
	connect.AssertNotEqual(t, NetworkPeerCategoryIsolated, NetworkPeerCategoryProxy)
	connect.AssertNotEqual(t, NetworkPeerCategoryIsolated, NetworkPeerCategoryProvider)
}

// Every refusal that needs no lookup answers 200 with error.message before any
// query runs.
func TestSetNetworkClientAclGroupRefusalsBeforeAnyQuery(t *testing.T) {
	ctx := context.Background()
	networkId := server.NewId()
	userId := server.NewId()
	clientId := server.NewId()

	clientSession := &session.ClientSession{Ctx: ctx, ByJwt: &jwt.ByJwt{NetworkId: networkId, UserId: userId, ClientId: &clientId}}
	rootSession := &session.ClientSession{Ctx: ctx, ByJwt: &jwt.ByJwt{NetworkId: networkId, UserId: userId}}

	// a client token may not set a group, not even its own
	result, err := SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{ClientId: clientId, AclGroup: "isolated"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.NetworkClientAclGroup, (*NetworkClientAclGroup)(nil))
	connect.AssertEqual(t, result.Error.Message, networkClientAclGroupSessionMessage)

	for _, refusedSession := range []*session.ClientSession{nil, {}, {ByJwt: &jwt.ByJwt{}}} {
		result, err = SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{ClientId: clientId, AclGroup: "isolated"}, refusedSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Message, networkClientAclGroupSessionMessage)
	}

	result, err = SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{AclGroup: "isolated"}, rootSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "client_id is required.")

	for _, aclGroup := range []string{"", "Isolated", "public"} {
		result, err = SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{ClientId: clientId, AclGroup: aclGroup}, rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Message, `acl_group must be "default" or "isolated".`)
	}

	// an API key session (no client id, pro mode off) passes the session check:
	// the next refusal is the group name, not the credential
	apiKeySession := &session.ClientSession{Ctx: ctx, ByJwt: jwt.NewByJwt(networkId, userId, "embed", false, false)}
	result, err = SetNetworkClientAclGroup(&SetNetworkClientAclGroupArgs{ClientId: clientId, AclGroup: "nope"}, apiKeySession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, `acl_group must be "default" or "isolated".`)
}

func TestGetNetworkClientAclGroupRefusalsBeforeAnyQuery(t *testing.T) {
	ctx := context.Background()
	networkId := server.NewId()
	rootSession := &session.ClientSession{Ctx: ctx, ByJwt: &jwt.ByJwt{NetworkId: networkId, UserId: server.NewId()}}

	for _, refusedSession := range []*session.ClientSession{nil, {}, {ByJwt: &jwt.ByJwt{}}} {
		result, err := GetNetworkClientAclGroup(&GetNetworkClientAclGroupArgs{}, refusedSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Message, networkClientAclGroupSessionMessage)
	}

	result, err := GetNetworkClientAclGroup(&GetNetworkClientAclGroupArgs{ClientId: "not-a-uuid"}, rootSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.NetworkClientAclGroup, (*NetworkClientAclGroup)(nil))
	connect.AssertEqual(t, result.Error.Message, "Invalid client_id.")
}

// The valve cache entry is dropped for one network, so a group change re-counts
// it at once, while other networks keep their cached decision.
func TestPeersEnabledCacheRemove(t *testing.T) {
	cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
	networkIdA := server.NewId()
	networkIdB := server.NewId()
	cache.Put(networkIdA, true)
	cache.Put(networkIdB, false)

	cache.Remove(networkIdA)
	_, ok := cache.Get(networkIdA)
	connect.AssertEqual(t, ok, false)
	enabled, ok := cache.Get(networkIdB)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, enabled, false)

	// removing an absent network is a no-op
	cache.Remove(server.NewId())
	_, ok = cache.Get(networkIdB)
	connect.AssertEqual(t, ok, true)
}
