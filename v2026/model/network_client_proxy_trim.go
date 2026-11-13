package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Trimming networks to the hosted proxy cap (owner, 2026-11-05: "Trim networks
// over the cap"; `bringyourctl network trim-proxies`).
//
// The cap (network_client_proxy_limit.go) refuses a network's next proxy but
// leaves the ones it already holds usable. On 11-05 the free network `hidden`
// held 123,295 active hosted proxies, and crisp and fireside stayed near 99%
// CPU with ~91k installed devices after every proxy block restarted: clients
// rotate across a network's proxy ids, and each id they touch installs a device
// for 45 minutes. A trim deactivates a free network's active hosted proxies
// beyond its newest `keep` (the free cap), through the deactivation
// /network/remove-clients uses, so the revocation records refuse each removed
// client and the post-commit removed notice reaches connect and the proxy
// hosts, which close what they hold.
//
// Never trimmed: Pro networks (listed when over the Pro cap) and networks with
// an Embed client allowance (their row is their cap, and the disposablenetwork
// customer was told on 11-05 that its existing proxies keep working).

const networkProxyTrimDigestVersion = "urnetwork network trim-proxies v1"

var ErrNetworkProxyTrimDigestMismatch = errors.New("network trim-proxies: --expect does not match the digest of the networks as they are now; nothing was removed; run the dry run again")

// Why a network over the keep count is not trimmed.
const (
	NetworkProxyTrimSkipPro      = "pro"
	NetworkProxyTrimSkipOverride = "override"
)

var (
	// proxy_client rows read per scan query (keyset pages by proxy_id)
	networkProxyTrimScanBatchSize = 50_000
	// clients deactivated per transaction, as /network/remove-clients batches
	networkProxyTrimRemoveBatchSize = 500
)

// Testing_SetNetworkProxyTrimBatchSizes overrides the scan page and removal
// batch sizes and returns a function that restores them.
func Testing_SetNetworkProxyTrimBatchSizes(scan int, remove int) func() {
	prevScan, prevRemove := networkProxyTrimScanBatchSize, networkProxyTrimRemoveBatchSize
	networkProxyTrimScanBatchSize, networkProxyTrimRemoveBatchSize = scan, remove
	return func() {
		networkProxyTrimScanBatchSize, networkProxyTrimRemoveBatchSize = prevScan, prevRemove
	}
}

// The keep count a trim uses by default: the free plan's proxy cap.
func DefaultNetworkProxyTrimKeep() int {
	return freeProxyClientLimit
}

type NetworkProxyTrimOptions struct {
	// the networks to trim; empty scans every network that holds a hosted proxy
	NetworkIds []server.Id
	// active hosted proxies a trimmed network keeps, newest by client id
	Keep int
	// removals a second during a commit; 0 or less is unpaced
	RatePerSecond float64
	// called while the scan reads proxy_client and after each removal batch;
	// may be nil
	Progress func(NetworkProxyTrimProgress)
}

type NetworkProxyTrimProgress struct {
	// "scan" or "remove"
	Stage string
	// proxy_client rows read so far (scan)
	Scanned int
	// clients deactivated so far, and the plan's total (remove)
	Removed int64
	Total   int
	// the network of the batch just removed (remove)
	NetworkId server.Id
}

type NetworkProxyTrimNetwork struct {
	NetworkId     server.Id
	NetworkName   string
	ActiveProxies int
	Pro           bool
	Override      bool
	OverrideLimit int
	// why the network is not trimmed; empty when it is
	Skip string
	// newest first
	KeepClientIds []server.Id
	// newest first
	RemoveClientIds []server.Id
}

type NetworkProxyTrimPlan struct {
	Keep int
	// the networks over the keep count, trimmed or skipped, in network id order.
	// A Pro network is listed only over the Pro cap.
	Networks    []*NetworkProxyTrimNetwork
	RemoveCount int
	// proxy_client rows the scan read; 0 when networks were named
	ScannedProxies int
	// binds the keep count and exactly the clients a commit removes
	Digest string
}

type NetworkProxyTrimResult struct {
	Plan             *NetworkProxyTrimPlan
	RemovedCount     int64
	RemovedByNetwork map[server.Id]int64
}

// PlanNetworkProxyTrim reads which hosted proxies a trim would deactivate. It
// writes nothing to the database (the Pro check refreshes the entitlement
// cache, which lives in Redis and process memory).
func PlanNetworkProxyTrim(ctx context.Context, options NetworkProxyTrimOptions) (*NetworkProxyTrimPlan, error) {
	if options.Keep < 0 {
		return nil, fmt.Errorf("network trim-proxies: --keep must be 0 or more, not %d", options.Keep)
	}
	activeProxies := map[server.Id][]server.Id{}
	scanned := 0
	if len(options.NetworkIds) == 0 {
		activeProxies, scanned = scanActiveHostedProxies(ctx, options.Progress)
	} else {
		networkIds := slices.Clone(options.NetworkIds)
		slices.SortFunc(networkIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		for _, networkId := range slices.Compact(networkIds) {
			activeProxies[networkId] = listNetworkActiveHostedProxies(ctx, networkId)
		}
	}

	overNetworkIds := []server.Id{}
	for networkId, clientIds := range activeProxies {
		if options.Keep < len(clientIds) {
			overNetworkIds = append(overNetworkIds, networkId)
		}
	}
	slices.SortFunc(overNetworkIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	names := networkProxyTrimNames(ctx, overNetworkIds)

	plan := &NetworkProxyTrimPlan{
		Keep:           options.Keep,
		ScannedProxies: scanned,
	}
	for _, networkId := range overNetworkIds {
		clientIds := slices.Clone(activeProxies[networkId])
		// client ids are time ordered: newest first
		slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return b.Cmp(a) })
		network := &NetworkProxyTrimNetwork{
			NetworkId:     networkId,
			NetworkName:   names[networkId],
			ActiveProxies: len(clientIds),
		}
		limit := GetNetworkTopLevelClientLimit(ctx, networkId)
		network.Override, network.OverrideLimit = limit.Override, limit.Limit
		network.Pro = IsProNetworkFresh(ctx, networkId)
		switch {
		case network.Override:
			network.Skip = NetworkProxyTrimSkipOverride
		case network.Pro:
			network.Skip = NetworkProxyTrimSkipPro
			if len(clientIds) <= proProxyClientLimit {
				// within the Pro cap: nothing to report
				continue
			}
		default:
			network.KeepClientIds = clientIds[:options.Keep]
			network.RemoveClientIds = clientIds[options.Keep:]
			plan.RemoveCount += len(network.RemoveClientIds)
		}
		plan.Networks = append(plan.Networks, network)
	}
	plan.Digest = networkProxyTrimDigest(plan)
	return plan, nil
}

// CommitNetworkProxyTrim plans the trim again and, when its digest is `expect`,
// deactivates the planned clients in batches. A client that became inactive
// after the plan is skipped, so an interrupted commit followed by a new dry
// run and commit removes exactly the rest.
func CommitNetworkProxyTrim(ctx context.Context, options NetworkProxyTrimOptions, expect string) (*NetworkProxyTrimResult, error) {
	plan, err := PlanNetworkProxyTrim(ctx, options)
	if err != nil {
		return nil, err
	}
	result := &NetworkProxyTrimResult{
		Plan:             plan,
		RemovedByNetwork: map[server.Id]int64{},
	}
	if plan.Digest != expect {
		return result, ErrNetworkProxyTrimDigestMismatch
	}

	start := time.Now()
	attempted := 0
	for _, network := range plan.Networks {
		if network.Skip != "" {
			continue
		}
		for i := 0; i < len(network.RemoveClientIds); i += networkProxyTrimRemoveBatchSize {
			batch := network.RemoveClientIds[i:min(i+networkProxyTrimRemoveBatchSize, len(network.RemoveClientIds))]
			var count int64
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				count, err = deactivateActiveNetworkClientsInTx(ctx, tx, batch, network.NetworkId)
				server.Raise(err)
			}, server.TxReadCommitted, server.TxLockTimeout(removeNetworkClientsBatchLockTimeout))
			result.RemovedCount += count
			result.RemovedByNetwork[network.NetworkId] += count
			attempted += len(batch)
			if options.Progress != nil {
				options.Progress(NetworkProxyTrimProgress{
					Stage:     "remove",
					Removed:   result.RemovedCount,
					Total:     plan.RemoveCount,
					NetworkId: network.NetworkId,
				})
			}
			if wait := networkProxyTrimPaceWait(start, time.Now(), attempted, options.RatePerSecond); 0 < wait {
				select {
				case <-ctx.Done():
					return result, ctx.Err()
				case <-time.After(wait):
				}
			}
		}
	}
	return result, nil
}

// How long to wait so that `done` removals since `start` keep to `rate` a
// second.
func networkProxyTrimPaceWait(start time.Time, now time.Time, done int, rate float64) time.Duration {
	if rate <= 0 || done <= 0 {
		return 0
	}
	due := start.Add(time.Duration(float64(done) / rate * float64(time.Second)))
	return due.Sub(now)
}

// Deactivates those of `clientIds` that are still active clients of the
// network, through the deactivation /network/remove-clients uses. Only active
// rows are locked and written, so a client deactivated earlier keeps its first
// deactivate_time.
func deactivateActiveNetworkClientsInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id, networkId server.Id) (int64, error) {
	orderedClientIds := slices.Clone(clientIds)
	slices.SortFunc(orderedClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	orderedClientIds = slices.Compact(orderedClientIds)

	lockedClientIds := []server.Id{}
	result, err := tx.Query(
		ctx,
		`
			/* network_client_deactivation_write_boundary */
			SELECT client_id
			FROM network_client
			WHERE
				client_id = ANY($1) AND
				network_id = $2 AND
				active = true
			ORDER BY client_id
			FOR UPDATE
		`,
		orderedClientIds,
		networkId,
	)
	if err != nil {
		return 0, err
	}
	defer result.Close()
	for result.Next() {
		var clientId server.Id
		if err := result.Scan(&clientId); err != nil {
			return 0, err
		}
		lockedClientIds = append(lockedClientIds, clientId)
	}
	if err := result.Err(); err != nil {
		return 0, err
	}
	result.Close()
	if len(lockedClientIds) == 0 {
		return 0, nil
	}
	return deactivateLockedNetworkClientsInTx(ctx, tx, lockedClientIds, networkId)
}

// The network's active hosted proxies, through its active clients
// (network_client_network_id_active_client_id) and proxy_client's unique
// client_id. A proxy can be a derived client (one an app created with its client
// token), so source_client_id is not a filter.
func listNetworkActiveHostedProxies(ctx context.Context, networkId server.Id) (clientIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_client.client_id
				FROM network_client
				INNER JOIN proxy_client ON proxy_client.client_id = network_client.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return
}

// Every network's active hosted proxies, read in keyset pages of proxy_client
// by proxy_id, each page's clients looked up by network_client's primary key.
// network_client has hundreds of millions of rows, so it is never joined
// unbounded.
func scanActiveHostedProxies(ctx context.Context, progress func(NetworkProxyTrimProgress)) (activeProxies map[server.Id][]server.Id, scanned int) {
	activeProxies = map[server.Id][]server.Id{}
	var after *server.Id
	for {
		pageClientIds := []server.Id{}
		var lastProxyId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			var result server.PgResult
			var err error
			if after == nil {
				result, err = conn.Query(
					ctx,
					`SELECT proxy_id, client_id FROM proxy_client ORDER BY proxy_id LIMIT $1`,
					networkProxyTrimScanBatchSize,
				)
			} else {
				result, err = conn.Query(
					ctx,
					`SELECT proxy_id, client_id FROM proxy_client WHERE $1 < proxy_id ORDER BY proxy_id LIMIT $2`,
					*after,
					networkProxyTrimScanBatchSize,
				)
			}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var proxyId, clientId server.Id
					server.Raise(result.Scan(&proxyId, &clientId))
					pageClientIds = append(pageClientIds, clientId)
					lastProxyId = proxyId
				}
			})
		})
		if len(pageClientIds) == 0 {
			return
		}
		scanned += len(pageClientIds)
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT client_id, network_id
					FROM network_client
					WHERE
						client_id = ANY($1) AND
						active = true
				`,
				pageClientIds,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId, networkId server.Id
					server.Raise(result.Scan(&clientId, &networkId))
					activeProxies[networkId] = append(activeProxies[networkId], clientId)
				}
			})
		})
		if progress != nil {
			progress(NetworkProxyTrimProgress{Stage: "scan", Scanned: scanned})
		}
		if len(pageClientIds) < networkProxyTrimScanBatchSize {
			return
		}
		after = &lastProxyId
	}
}

func networkProxyTrimNames(ctx context.Context, networkIds []server.Id) map[server.Id]string {
	names := map[server.Id]string{}
	if len(networkIds) == 0 {
		return names
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, network_name FROM network WHERE network_id = ANY($1)`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var name string
				server.Raise(result.Scan(&networkId, &name))
				names[networkId] = name
			}
		})
	})
	return names
}

// The digest binds the keep count and exactly the clients a commit removes, per
// network. A skipped network is not bound: its own changes do not refuse a trim
// of the others.
func networkProxyTrimDigest(plan *NetworkProxyTrimPlan) string {
	lines := []string{networkProxyTrimDigestVersion, "keep " + strconv.Itoa(plan.Keep)}
	for _, network := range plan.Networks {
		if network.Skip != "" {
			continue
		}
		removeClientIds := slices.Clone(network.RemoveClientIds)
		slices.SortFunc(removeClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		lines = append(lines, fmt.Sprintf("network %s remove %d", network.NetworkId, len(removeClientIds)))
		for _, clientId := range removeClientIds {
			lines = append(lines, clientId.String())
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}
