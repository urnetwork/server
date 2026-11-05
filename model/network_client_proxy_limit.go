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

// countNetworkActiveProxyClientsInTx counts the network's active hosted proxies,
// stopping at `bound`, since only the threshold matters: the scan walks the
// network's active clients (network_client_network_id_active) and looks each up
// in proxy_client by its unique client_id, so a network with hundreds of
// thousands of clients costs at most `bound` rows.
func countNetworkActiveProxyClientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, bound int) (count int) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT count(*) FROM (
				SELECT 1
				FROM network_client
				INNER JOIN proxy_client ON proxy_client.client_id = network_client.client_id
				WHERE network_client.network_id = $1 AND network_client.active = true
				LIMIT $2
			) active_proxies
		`,
		networkId,
		bound,
	).Scan(&count))
	return
}

func proxyClientLimitMessage(limit int, isPro bool) string {
	if isPro {
		return fmt.Sprintf("Your plan's limit of %d active proxies is reached. Remove a proxy to create another.", limit)
	}
	return fmt.Sprintf("Your plan's limit of %d active proxies is reached. Remove a proxy to create another, or upgrade to UR Pro for more.", limit)
}
