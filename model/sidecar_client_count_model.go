package model

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Counting sidecar clients (DESIGN.md 20.8, X10): §20.8's table, row by row.
//
//	client                                   concurrent_clients   100 create cap, peer valve   sidecar_clients
//	app device client                        counts               counts                       -
//	sidecar child of a device client         exempt               -                            counts
//	standalone, declared no-egress           exempt               exempt                       counts
//	standalone, with ur proxy or public exits counts              counts                       counts
//
// The sidecar mark is network_client_owner.strict_exp (§7.4): app device clients are
// minted admin_refused and carry none, so they never count here. A sidecar child is in no
// peer registry, since only top-level clients register (peer_model.go), so every marked
// client, child or top-level, whatever its category, joins one zset of its own,
// {sq_<network_id>}connected_sidecar, scored by its expiry in ms as the other connected
// zsets are, written and refreshed on its resident's poll, the key living 48 h past its
// last write (plan-QUOTA-sidecar-clients-store; §2.8 api-13's {sq_} family, which §19.7
// keeps out of the rdb bundles). sidecar_clients is one ZCOUNT of it.
//
// A declared no-egress top-level client takes NetworkPeerCategorySidecar: unlisted, no
// peer subscription, in no {np_} zset, so GetNetworkEnforceableConnectedCount leaves it
// out, and the create cap's and the peer valve's counts skip it (the NOT EXISTS on
// declares_no_egress, run only where MG0 is linked). A standalone sidecar with egress
// stays an ordinary client and joins the zset too, so it counts in all three columns.
//
// The limit is dark behind enforce_sidecar_limits, with no I/O while off: no mark is read
// at a resident, no zset is written, and both checks pass. Turned on, a client counts from
// its resident's next construction, and clients already connected stay connected (§20.7's
// "connected clients stay").

// zset: every marked client of the network, scored by its expiry in unix ms.
func networkSidecarConnectedKey(networkId server.Id) string {
	return fmt.Sprintf("{sq_%s}connected_sidecar", networkId)
}

// How the connected sidecar set is kept.
type SidecarClientCountSettings struct {
	// The key's ttl past its last write, which §2.8 gives every {sq_} key (48 h). A network
	// with no connected sidecar clears within it.
	KeyTtl time.Duration
	// The most expired entries one write removes, so a write's work stays bounded.
	PruneLimit int64
}

func DefaultSidecarClientCountSettings() *SidecarClientCountSettings {
	return &SidecarClientCountSettings{
		KeyTtl:     48 * time.Hour,
		PruneLimit: 64,
	}
}

var sidecarClientCountSettings = DefaultSidecarClientCountSettings()

// Whether sidecar clients are counted now: the limit is enforced and this binary links
// MG0, whose owner row holds the mark. False means no mark is read and nothing is written.
func SidecarClientCountingOn() bool {
	return Pro().EnforceSidecarLimits && server.SidecarMg0Linked()
}

// Registers or refreshes one marked client as connected until ttl from now, and ages out
// a bounded batch of expired ones. The resident's poll calls it, as it heartbeats the peer
// registry.
func AddNetworkSidecarClient(ctx context.Context, networkId server.Id, clientId server.Id, ttl time.Duration) {
	member := string(clientId.Bytes())
	now := server.NowUtc()
	key := networkSidecarConnectedKey(networkId)
	settings := sidecarClientCountSettings
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		pipe.ZAdd(ctx, key, redis.Z{
			Score:  float64(now.Add(ttl).UnixMilli()),
			Member: member,
		})
		pipe.Expire(ctx, key, settings.KeyTtl)
		_, err := pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}
		expired, err := r.ZRangeByScore(ctx, key, &redis.ZRangeBy{
			Min:   "-inf",
			Max:   strconv.FormatInt(now.UnixMilli(), 10),
			Count: settings.PruneLimit,
		}).Result()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		if 0 < len(expired) {
			members := make([]any, len(expired))
			for i, expiredMember := range expired {
				members[i] = expiredMember
			}
			if err := r.ZRem(ctx, key, members...).Err(); err != nil && err != server.RedisNil {
				panic(err)
			}
		}
	})
}

// Removes one marked client, as its resident closes.
func RemoveNetworkSidecarClient(ctx context.Context, networkId server.Id, clientId server.Id) {
	member := string(clientId.Bytes())
	server.Redis(ctx, func(r server.RedisClient) {
		if err := r.ZRem(ctx, networkSidecarConnectedKey(networkId), member).Err(); err != nil && err != server.RedisNil {
			panic(err)
		}
	})
}

// The network's connected sidecar clients: the zset's entries whose expiry has not
// passed. One ZCOUNT.
func GetNetworkSidecarConnectedCount(ctx context.Context, networkId server.Id) (count int64) {
	liveMin := strconv.FormatInt(server.NowUtc().UnixMilli()+1, 10)
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		count, err = r.ZCount(ctx, networkSidecarConnectedKey(networkId), liveMin, "+inf").Result()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
	})
	return
}

// Whether the client holds a live entry: a re-nomination of a connected client is no new
// connection.
func isNetworkSidecarClientConnected(ctx context.Context, networkId server.Id, clientId server.Id) (connected bool) {
	member := string(clientId.Bytes())
	server.Redis(ctx, func(r server.RedisClient) {
		expiryMs, err := r.ZScore(ctx, networkSidecarConnectedKey(networkId), member).Result()
		if err == nil && server.NowUtc().UnixMilli() < int64(expiryMs) {
			connected = true
		}
	})
	return
}

// The refusal for one more connected sidecar client of the network, or nil when it has
// room: what TEAM-M1-04 calls at enrollment. Nil, with no I/O, while the limit is dark. A
// limit of -1 passes, and the refusal's Upgrade says whether a higher tier lifts it.
func SidecarClientsExceeded(ctx context.Context, networkId server.Id) *sidecar.QuotaRefusal {
	return sidecarClientsExceeded(ctx, nil, networkId)
}

// SidecarClientsExceeded for a caller holding a transaction, such as an enrollment that
// mints the client in it: the limits read through tx where their caches miss.
func SidecarClientsExceededInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *sidecar.QuotaRefusal {
	return sidecarClientsExceeded(ctx, tx, networkId)
}

func sidecarClientsExceeded(ctx context.Context, tx server.PgTx, networkId server.Id) *sidecar.QuotaRefusal {
	if !SidecarClientCountingOn() {
		return nil
	}
	tier, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
	limit := limits.Limit("sidecar_clients")
	if limit == proSidecarLimitNone {
		return nil
	}
	usedCount := GetNetworkSidecarConnectedCount(ctx, networkId)
	if usedCount < limit {
		return nil
	}
	return &sidecar.QuotaRefusal{
		LimitKey: "sidecar_clients",
		Limit:    limit,
		Used:     usedCount,
		Upgrade:  quotaRefusalUpgrade(tier, "sidecar_clients", usedCount, networkSidecarLimitHasRow(ctx, tx, networkId, "sidecar_clients")),
	}
}

// Whether the client may become a connected sidecar client of its network: the
// connection-activation gate beside CanConnectNetworkPeer at a resident's nomination,
// whose refusal closes the connection with 4001 as the concurrent limit's does. Always
// true, with no I/O, while the limit is dark; true for a client with no sidecar mark, for
// one already connected (a re-nomination), and while the network has room. Clients
// already connected stay.
func CanConnectSidecarClient(ctx context.Context, clientId server.Id) bool {
	if !SidecarClientCountingOn() {
		return true
	}
	networkId, marked := getSidecarClientMark(ctx, clientId)
	if !marked {
		return true
	}
	if isNetworkSidecarClientConnected(ctx, networkId, clientId) {
		return true
	}
	return SidecarClientsExceeded(ctx, networkId) == nil
}

// A client's network and sidecar mark. Both are written with the client and never
// change, so a process keeps each answer it read, bounded as ClientRefusesAdmin's are,
// and a re-nomination asks no query.
type sidecarClientMark struct {
	networkId server.Id
	marked    bool
}

var sidecarClientMarkAnswers = newBoundedClientMap[sidecarClientMark](DefaultClientAdminSettings().CachedClientMaxCount)

// The client's network and whether its owner row marks it strict_exp. An inactive or
// unknown client is unmarked, and that answer is not kept. Only while MG0 is linked.
func getSidecarClientMark(ctx context.Context, clientId server.Id) (networkId server.Id, marked bool) {
	if answer, ok := sidecarClientMarkAnswers.Load(clientId); ok {
		return answer.networkId, answer.marked
	}
	found := false
	defer func() {
		if found {
			sidecarClientMarkAnswers.Store(clientId, sidecarClientMark{networkId: networkId, marked: marked})
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client.network_id,
					COALESCE(network_client_owner.strict_exp, false)
				FROM network_client
				LEFT JOIN network_client_owner ON
					network_client_owner.client_id = network_client.client_id
				WHERE
					network_client.client_id = $1 AND
					network_client.active = true
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId, &marked))
				found = true
			}
		})
	})
	return
}

// Drops this process's kept marks, for a test that writes a client's owner row after the
// client was asked about.
func Testing_ForgetSidecarClientMarks() {
	sidecarClientMarkAnswers.Clear()
}

// Replaces the zset's settings, returning a restore function, for tests.
func Testing_SetSidecarClientCountSettings(settings *SidecarClientCountSettings) func() {
	prev := sidecarClientCountSettings
	sidecarClientCountSettings = settings
	return func() { sidecarClientCountSettings = prev }
}
