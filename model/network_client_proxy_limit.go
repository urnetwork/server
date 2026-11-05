package model

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The hosted proxy limits (owner, 2026-11-05). Every network has two:
//   - a cap on its active hosted proxies: free 5, Pro 1000, or its Embed plan
//     row's limit when it has one (bringyourctl network client-limit);
//   - a rate on its hosted proxy creates: free 5, Pro 20 a minute, counted in
//     one redis key per network and minute.
//
// Both apply to every hosted proxy create, top-level or derived, and always:
// they do not wait for enforce_concurrent_clients, which turns the rest of the
// client limits on with the app release. Only a new proxy is refused. A
// network's existing proxies keep working, and removing one makes room for
// the next.
//
// Why: on 2026-11-04/05 the two proxy hosts ran at 99.6% CPU with 91,000
// installed hosted devices. Every live hosted proxy is a device on a proxy
// host that keeps finding providers and reserving contracts. One network held
// 123,295 active hosted proxies; another created about 17,000 an hour and
// removed them within minutes, so a cap on active proxies alone never refused
// it.
//
// Free is 5 for now, not lower: browser extensions up to the current release
// create 5 hosted proxies per connect (src/utils/connection-manager.ts
// MULTI_IP_POOL_SIZE = 5, used only as an ordered failover list). The next
// extension release creates 1; lower free once it is out. The free tier's
// pro.yml concurrent_clients (2) is the dark device limit and is not used here.
//
// Variables only so tests can change them; 0 or less means no limit.
var (
	freeProxyClientLimit = 5
	proProxyClientLimit  = 1000

	freeProxyClientCreatesPerMinute = 5
	proProxyClientCreatesPerMinute  = 20
)

// proxyClientLimitFor is the most active hosted proxies a network may hold: its
// Embed plan row's limit when it has one (networkClientLimitOverride),
// otherwise its plan's cap.
func proxyClientLimitFor(isPro bool, overrideLimit int, override bool) int {
	if override {
		return overrideLimit
	}
	if isPro {
		return proProxyClientLimit
	}
	return freeProxyClientLimit
}

// proxyClientLimitReached reports whether a network with `activeCount` active
// hosted proxies has no room for one more under `limit`. A limit of 0 or less
// never refuses.
func proxyClientLimitReached(limit int, activeCount int) bool {
	if limit <= 0 {
		return false
	}
	return limit <= activeCount
}

// proxyClientCountBound is how many active proxies the count needs to find to
// decide `limit`: the limit itself, since proxyClientLimitReached only asks
// whether the count reaches it. Counting one more (the first release counted to
// limit+1) only makes a network that holds exactly its limit walk further.
func proxyClientCountBound(limit int) int {
	return limit
}

// proxyClientLimitScanBudget is the most clients each of the count's two walks
// visits on a proxy create (countNetworkActiveProxyClientsInTx).
//
// The count runs inside the auth-client transaction. Walking every active client
// cost one network with 324,000 active clients, few of them proxies, about 0.4 s
// warm and 3.6 s cold on Main (2026-11-05), on every proxy it tried to create.
// With the budget a create costs at most twice this many index entries and as
// many proxy_client probes.
//
// Beyond the budget the count can undercount, so the limit fails open. That
// happens only to a network with more top-level active clients than the budget,
// or one whose derived proxies are all older than its newest budget's worth of
// active clients. The trade is taken because the networks the limit is for hold
// recent proxies. One that churns them (creates and removes thousands an hour)
// holds only proxies younger than its other clients. One that is made mostly of
// proxies fills either window with them. Either way a window sees the limit
// long before the budget runs out.
var proxyClientLimitScanBudget = 50_000

// Testing_SetProxyClientLimitScanBudget sets the scan budget and returns a
// function that restores it. Tests that use it must not run in parallel.
func Testing_SetProxyClientLimitScanBudget(budget int) func() {
	prev := proxyClientLimitScanBudget
	proxyClientLimitScanBudget = budget
	return func() {
		proxyClientLimitScanBudget = prev
	}
}

// countNetworkActiveProxyClientsInTx counts the network's active hosted
// proxies, stopping at `bound`, since only the threshold matters. It takes the
// larger of two bounded walks. Each is a lower bound of the true count, so the
// larger never refuses a network that is under its limit.
//
//  1. Top-level proxies, created with the network token and no source client.
//     The walk reads network_client_network_id_top_level, a partial index of
//     active clients with no source_client_id. It skips the derived clients
//     that make up most of a proxy network's active clients: the window
//     identities its hosted devices mint. The index is keyed on network_id
//     alone, so it cannot order by client_id, and each entry costs one heap
//     fetch for its client_id. The count is exact while the network has at most
//     proxyClientLimitScanBudget top-level active clients.
//  2. Derived proxies, with a source client. A client token can create only
//     children of its own client (AUTHZ1.md), so every proxy an app creates is
//     derived, and a network token may pass a source_client_id too. The partial
//     index cannot see them, so this walk reads the network's newest
//     proxyClientLimitScanBudget active clients of either kind. It walks
//     network_client_network_id_active_client_id backward, index-only: client
//     ids are time ordered, so this is newest first.
//
// Each client a walk visits is looked up in proxy_client by its unique
// client_id. The lookup is a LATERAL ... LIMIT 1 subquery, which the planner
// cannot turn into a join, so it stays one index probe per client. A hash join
// would read all of proxy_client before returning a row.
func countNetworkActiveProxyClientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, bound int) (count int) {
	count = countNetworkTopLevelProxyClientsInTx(ctx, tx, networkId, bound)
	if bound <= count {
		return
	}
	return max(count, countNetworkRecentProxyClientsInTx(ctx, tx, networkId, bound))
}

func countNetworkTopLevelProxyClientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, bound int) (count int) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT count(*) FROM (
				SELECT 1
				FROM (
					SELECT client_id
					FROM network_client
					WHERE network_id = $1 AND active = true AND source_client_id IS NULL
					LIMIT $3
				) top_level
				CROSS JOIN LATERAL (
					SELECT 1
					FROM proxy_client
					WHERE proxy_client.client_id = top_level.client_id
					LIMIT 1
				) proxy
				LIMIT $2
			) top_level_proxies
		`,
		networkId,
		bound,
		proxyClientLimitScanBudget,
	).Scan(&count))
	return
}

func countNetworkRecentProxyClientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, bound int) (count int) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT count(*) FROM (
				SELECT 1
				FROM (
					SELECT client_id
					FROM network_client
					WHERE network_id = $1 AND active = true
					ORDER BY client_id DESC
					LIMIT $3
				) recent
				CROSS JOIN LATERAL (
					SELECT 1
					FROM proxy_client
					WHERE proxy_client.client_id = recent.client_id
					LIMIT 1
				) proxy
				LIMIT $2
			) recent_proxies
		`,
		networkId,
		bound,
		proxyClientLimitScanBudget,
	).Scan(&count))
	return
}

func proxyClientLimitMessage(limit int, isPro bool) string {
	if isPro {
		return fmt.Sprintf("Your plan's limit of %d active proxies is reached. Remove a proxy to create another.", limit)
	}
	return fmt.Sprintf("Your plan's limit of %d active proxies is reached. Remove a proxy to create another, or upgrade to UR Pro for more.", limit)
}

// The create rate counts every hosted proxy create attempt in a redis counter
// per network and unix minute, `cl-<network_id>-<block>`. It is a single key,
// so a single cluster slot. Each create refreshes the key's ttl, which outlives
// its minute.
const proxyClientCreateKeyTtl = 120 * time.Second

// proxyClientCreateNow is the clock the rate's minutes are read from.
var proxyClientCreateNow = time.Now

// proxyClientCreateBlock is the unix minute a create at `now` counts in.
func proxyClientCreateBlock(now time.Time) int64 {
	return now.Unix() / 60
}

func proxyClientCreateKey(networkId server.Id, block int64) string {
	return fmt.Sprintf("cl-%s-%d", networkId, block)
}

func proxyClientCreatesPerMinute(isPro bool) int {
	if isPro {
		return proProxyClientCreatesPerMinute
	}
	return freeProxyClientCreatesPerMinute
}

// proxyClientCreateRateExceeded reports whether the create that brought its
// minute's count to `count` is over `perMinute`. 0 or less never refuses.
func proxyClientCreateRateExceeded(perMinute int, count int64) bool {
	return 0 < perMinute && int64(perMinute) < count
}

// countProxyClientCreate counts one hosted proxy create in the network's minute
// and returns the minute's count. A redis failure is logged and returns counted
// false. The create then goes ahead (the rate fails open), and the active cap
// still applies.
func countProxyClientCreate(ctx context.Context, networkId server.Id, now time.Time) (count int64, counted bool) {
	key := proxyClientCreateKey(networkId, proxyClientCreateBlock(now))
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			var incr *redis.IntCmd
			_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				incr = pipe.Incr(ctx, key)
				pipe.Expire(ctx, key, proxyClientCreateKeyTtl)
				return nil
			})
			server.Raise(err)
			count, counted = incr.Val(), true
		})
	}, func(err error) {
		count, counted = 0, false
		glog.Warningf("[nc]proxy create rate not counted for network %s; the create goes ahead: %s\n", networkId, err)
	})
	return
}

func proxyClientCreateRateMessage(perMinute int, isPro bool) string {
	proxies := "proxies"
	if perMinute == 1 {
		proxies = "proxy"
	}
	if isPro {
		return fmt.Sprintf("Your plan allows %d new %s a minute. Try again in a minute.", perMinute, proxies)
	}
	return fmt.Sprintf("Your plan allows %d new %s a minute. Try again in a minute, or upgrade to UR Pro for more.", perMinute, proxies)
}

func proxyClientCreateRateRefusal(isPro bool) *AuthNetworkClientResult {
	return &AuthNetworkClientResult{
		Error: &AuthNetworkClientError{
			ClientLimitExceeded: true,
			UpgradeRequired:     !isPro,
			Message:             proxyClientCreateRateMessage(proxyClientCreatesPerMinute(isPro), isPro),
		},
	}
}

// Testing_SetProxyClientLimits sets the active hosted proxy caps (0 or less: no
// cap) and returns a function that restores them. Tests that use it must not
// run in parallel.
func Testing_SetProxyClientLimits(free int, pro int) func() {
	prevFree, prevPro := freeProxyClientLimit, proProxyClientLimit
	freeProxyClientLimit, proProxyClientLimit = free, pro
	return func() {
		freeProxyClientLimit, proProxyClientLimit = prevFree, prevPro
	}
}

// Testing_SetProxyClientCreatesPerMinute sets the hosted proxy create rates (0
// or less: no rate) and returns a function that restores them.
func Testing_SetProxyClientCreatesPerMinute(free int, pro int) func() {
	prevFree, prevPro := freeProxyClientCreatesPerMinute, proProxyClientCreatesPerMinute
	freeProxyClientCreatesPerMinute, proProxyClientCreatesPerMinute = free, pro
	return func() {
		freeProxyClientCreatesPerMinute, proProxyClientCreatesPerMinute = prevFree, prevPro
	}
}

// Testing_SetProxyClientCreateNow replaces the rate's clock and returns a
// function that restores it.
func Testing_SetProxyClientCreateNow(now func() time.Time) func() {
	prev := proxyClientCreateNow
	proxyClientCreateNow = now
	return func() {
		proxyClientCreateNow = prev
	}
}

// Testing_LiftProxyClientLimits lifts the hosted proxy cap and rate, for a test
// about something else that creates many proxies in one network.
func Testing_LiftProxyClientLimits() func() {
	restoreLimits := Testing_SetProxyClientLimits(0, 0)
	restoreRates := Testing_SetProxyClientCreatesPerMinute(0, 0)
	return func() {
		restoreRates()
		restoreLimits()
	}
}
