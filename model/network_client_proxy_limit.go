package model

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
)

// The hosted proxy limit (owner, 2026-11-05).
//
// A network holds at most its plan's number of active hosted proxies: the
// plan's concurrent client count (pro.yml: free 2, Pro 1000), or an Embed
// plan's override (networkConcurrentClientLimit). Unlike the concurrent client
// limit it is always enforced: it does not wait for enforce_concurrent_clients,
// which turns the rest of the client limits on with the app release.
//
// Why: on 2026-11-04/05 one network held 300,006 active hosted proxies and
// created 14-22k more an hour. Every live hosted proxy is a device on a proxy
// host that keeps finding providers and reserving contracts, so the two proxy
// hosts ran at 100% CPU from about 00:00Z and their hosted devices, other
// networks' among them, lost their connections, while that network moved about
// 9.7 GB in the month. Only a new proxy is refused; a network's existing proxies
// keep working, and removing one makes room for the next.

// networkProxyClientLimit is the most active hosted proxies the network may hold.
// 0 (no pro.yml) means no limit.
func networkProxyClientLimit(ctx context.Context, networkId server.Id) int {
	return networkConcurrentClientLimit(ctx, networkId)
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
