package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// `bringyourctl network trim-proxies` (owner, 2026-11-05: "Trim networks over
// the cap"; network_client_proxy_trim.go).

type proxyTrimTestNetwork struct {
	networkId server.Id
	session   *session.ClientSession
	proxies   []server.Id
	others    []server.Id
}

func newProxyTrimTestNetwork(t testing.TB, ctx context.Context, name string, proxies int, others int) *proxyTrimTestNetwork {
	t.Helper()
	network := &proxyTrimTestNetwork{networkId: server.NewId()}
	userId := server.NewId()
	Testing_CreateNetwork(ctx, network.networkId, name, userId)
	network.session = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: network.networkId,
		UserId:    userId,
	})
	for range proxies {
		network.proxies = append(network.proxies, proxyTrimTestCreateClient(t, network.session, true))
	}
	for range others {
		network.others = append(network.others, proxyTrimTestCreateClient(t, network.session, false))
	}
	return network
}

func proxyTrimTestCreateClient(t testing.TB, clientSession *session.ClientSession, proxy bool) server.Id {
	t.Helper()
	args := &AuthNetworkClientArgs{Description: "client", DeviceSpec: "client"}
	if proxy {
		args.Description, args.DeviceSpec = "proxy", "proxy"
		args.ProxyConfig = &ProxyConfig{
			InitialDeviceState: &ExtendedProxyDeviceState{
				ProxyDeviceState: ProxyDeviceState{
					Location: &sdk.ConnectLocation{
						ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
					},
				},
			},
		}
	}
	result, err := AuthNetworkClient(args, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil && result.Error == nil && result.ClientId != nil, true)
	return *result.ClientId
}

func proxyTrimTestActive(t testing.TB, ctx context.Context, clientIds []server.Id) (active []server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT client_id FROM network_client WHERE client_id = ANY($1) AND active = true`, clientIds)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				active = append(active, clientId)
			}
		})
	})
	slices.SortFunc(active, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return
}

func proxyTrimTestNewest(clientIds []server.Id, n int) []server.Id {
	sorted := slices.Clone(clientIds)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return b.Cmp(a) })
	newest := slices.Clone(sorted[:n])
	slices.SortFunc(newest, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return newest
}

func proxyTrimTestPlanNetwork(plan *NetworkProxyTrimPlan, networkId server.Id) *NetworkProxyTrimNetwork {
	for _, network := range plan.Networks {
		if network.NetworkId == networkId {
			return network
		}
	}
	return nil
}

// A free network over the cap keeps its newest proxies and its other clients;
// Pro networks within the Pro cap are not listed, Pro networks over it and
// networks with a client allowance are listed and never trimmed; a commit
// needs the dry run's digest, sends each removed client's removed notice, and a
// second trim removes nothing.
func TestNetworkProxyTrimKeepsTheNewestProxiesOfFreeNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// small pages and batches exercise the keyset scan and the batching
		defer Testing_SetNetworkProxyTrimBatchSizes(3, 2)()

		lift := Testing_LiftProxyClientLimits()
		free := newProxyTrimTestNetwork(t, ctx, "trimfree", 8, 2)
		under := newProxyTrimTestNetwork(t, ctx, "trimunder", 4, 0)
		proNetworkId := server.NewId()
		pro := func() *proxyTrimTestNetwork {
			network := &proxyTrimTestNetwork{networkId: proNetworkId}
			userId := server.NewId()
			Testing_CreateNetwork(ctx, proNetworkId, "trimpro", userId)
			proJwtGrantPro(t, ctx, proNetworkId)
			network.session = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: proNetworkId, UserId: userId})
			for range 8 {
				network.proxies = append(network.proxies, proxyTrimTestCreateClient(t, network.session, true))
			}
			return network
		}()
		proUnderNetworkId := server.NewId()
		proUnder := func() *proxyTrimTestNetwork {
			network := &proxyTrimTestNetwork{networkId: proUnderNetworkId}
			userId := server.NewId()
			Testing_CreateNetwork(ctx, proUnderNetworkId, "trimprounder", userId)
			proJwtGrantPro(t, ctx, proUnderNetworkId)
			network.session = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: proUnderNetworkId, UserId: userId})
			for range 6 {
				network.proxies = append(network.proxies, proxyTrimTestCreateClient(t, network.session, true))
			}
			return network
		}()
		allowance := newProxyTrimTestNetwork(t, ctx, "trimallowance", 8, 0)
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, allowance.networkId, 100), nil)
		lift()
		// the free cap of 5 and a Pro cap of 7: the 8-proxy Pro network is over
		// it, the 6-proxy one within it
		defer Testing_SetProxyClientLimits(5, 7)()
		connect.AssertEqual(t, DefaultNetworkProxyTrimKeep(), 5)

		options := NetworkProxyTrimOptions{Keep: DefaultNetworkProxyTrimKeep(), RatePerSecond: 1_000_000}
		scans := 0
		options.Progress = func(progress NetworkProxyTrimProgress) {
			if progress.Stage == "scan" {
				scans += 1
			}
		}
		plan, err := PlanNetworkProxyTrim(ctx, options)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.ScannedProxies, 8+4+8+6+8)
		// pages of 3 proxy_client rows
		connect.AssertEqual(t, 10 <= scans, true)
		connect.AssertEqual(t, plan.RemoveCount, 3)

		freePlan := proxyTrimTestPlanNetwork(plan, free.networkId)
		connect.AssertEqual(t, freePlan != nil, true)
		connect.AssertEqual(t, freePlan.Skip, "")
		connect.AssertEqual(t, freePlan.ActiveProxies, 8)
		kept := slices.Clone(freePlan.KeepClientIds)
		slices.SortFunc(kept, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, kept, proxyTrimTestNewest(free.proxies, 5))
		connect.AssertEqual(t, len(freePlan.RemoveClientIds), 3)

		connect.AssertEqual(t, proxyTrimTestPlanNetwork(plan, under.networkId) == nil, true)
		connect.AssertEqual(t, proxyTrimTestPlanNetwork(plan, proUnder.networkId) == nil, true)
		proPlan := proxyTrimTestPlanNetwork(plan, pro.networkId)
		connect.AssertEqual(t, proPlan != nil && proPlan.Skip == NetworkProxyTrimSkipPro && len(proPlan.RemoveClientIds) == 0, true)
		allowancePlan := proxyTrimTestPlanNetwork(plan, allowance.networkId)
		connect.AssertEqual(t, allowancePlan != nil && allowancePlan.Skip == NetworkProxyTrimSkipOverride && allowancePlan.OverrideLimit == 100 && len(allowancePlan.RemoveClientIds) == 0, true)

		// the digest is stable, and naming the networks plans the same removals
		again, err := PlanNetworkProxyTrim(ctx, options)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Digest, plan.Digest)
		named := options
		named.NetworkIds = []server.Id{allowance.networkId, free.networkId, pro.networkId, under.networkId}
		namedPlan, err := PlanNetworkProxyTrim(ctx, named)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, namedPlan.Digest, plan.Digest)

		// a commit with another digest removes nothing
		_, err = CommitNetworkProxyTrim(ctx, options, "0000")
		connect.AssertEqual(t, errors.Is(err, ErrNetworkProxyTrimDigestMismatch), true)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, free.proxies)), 8)

		removeBatches := 0
		options.Progress = func(progress NetworkProxyTrimProgress) {
			if progress.Stage == "remove" {
				removeBatches += 1
				connect.AssertEqual(t, progress.NetworkId, free.networkId)
				connect.AssertEqual(t, progress.Total, 3)
			}
		}
		result, err := CommitNetworkProxyTrim(ctx, options, plan.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.RemovedCount, int64(3))
		connect.AssertEqual(t, result.RemovedByNetwork[free.networkId], int64(3))
		// batches of 2
		connect.AssertEqual(t, removeBatches, 2)

		connect.AssertEqual(t, proxyTrimTestActive(t, ctx, free.proxies), proxyTrimTestNewest(free.proxies, 5))
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, free.others)), 2)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, under.proxies)), 4)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, pro.proxies)), 8)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, proUnder.proxies)), 6)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, allowance.proxies)), 8)

		// each removed client's removed flag, which connect and the proxy hosts
		// read, is set; a kept one's is not
		for _, clientId := range freePlan.RemoveClientIds {
			connect.AssertEqual(t, IsClientRemoved(ctx, clientId), true)
		}
		for _, clientId := range freePlan.KeepClientIds {
			connect.AssertEqual(t, IsClientRemoved(ctx, clientId), false)
		}

		// a second trim finds nothing to remove
		rerun, err := PlanNetworkProxyTrim(ctx, options)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rerun.RemoveCount, 0)
		connect.AssertEqual(t, proxyTrimTestPlanNetwork(rerun, free.networkId) == nil, true)
		rerunResult, err := CommitNetworkProxyTrim(ctx, options, rerun.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rerunResult.RemovedCount, int64(0))
	})
}

// The deactivation writes only clients that are still active, so a client
// removed after the plan keeps its first deactivate_time and is not counted.
func TestNetworkProxyTrimDeactivatesOnlyActiveClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_LiftProxyClientLimits()()
		network := newProxyTrimTestNetwork(t, ctx, "trimactive", 3, 0)

		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: network.proxies[0]}, network.session)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		readDeactivateTime := func(clientId server.Id) (deactivateTime *time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, clientId).Scan(&deactivateTime))
			})
			return
		}
		first := readDeactivateTime(network.proxies[0])
		connect.AssertEqual(t, first != nil, true)

		var count int64
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			count, err = deactivateActiveNetworkClientsInTx(ctx, tx, network.proxies, network.networkId)
			server.Raise(err)
		}, server.TxReadCommitted, server.TxLockTimeout(removeNetworkClientsBatchLockTimeout))
		connect.AssertEqual(t, count, int64(2))
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, network.proxies)), 0)
		connect.AssertEqual(t, readDeactivateTime(network.proxies[0]).Equal(*first), true)
	})
}

// Pure: the pace keeps a commit's removals to the rate.
func TestNetworkProxyTrimPaceWait(t *testing.T) {
	start := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, networkProxyTrimPaceWait(start, start, 500, 0), time.Duration(0))
	connect.AssertEqual(t, networkProxyTrimPaceWait(start, start, 0, 200), time.Duration(0))
	// 500 removals at 200 a second are due 2.5 s after the start
	connect.AssertEqual(t, networkProxyTrimPaceWait(start, start.Add(time.Second), 500, 200), 1500*time.Millisecond)
	// already behind the rate: no wait
	connect.AssertEqual(t, networkProxyTrimPaceWait(start, start.Add(3*time.Second), 500, 200) <= 0, true)
}
