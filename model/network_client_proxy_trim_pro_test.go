package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// `bringyourctl network trim-proxies --include_pro --keep_described
// --keep_used_since` (owner, 2026-11-14, support item 8327: the 2026.10.27
// browser extension filled a Pro network to the Pro cap with hosted proxies it
// never released, and a trim never touched a Pro network).

func newProxyTrimTestProNetwork(t testing.TB, ctx context.Context, name string, proxies int) *proxyTrimTestNetwork {
	t.Helper()
	network := &proxyTrimTestNetwork{networkId: server.NewId()}
	userId := server.NewId()
	Testing_CreateNetwork(ctx, network.networkId, name, userId)
	proJwtGrantPro(t, ctx, network.networkId)
	network.session = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: network.networkId,
		UserId:    userId,
	})
	for range proxies {
		network.proxies = append(network.proxies, proxyTrimTestCreateClient(t, network.session, true))
	}
	return network
}

func proxyTrimTestSetUse(t testing.TB, ctx context.Context, clientIds []server.Id, createTime time.Time, authTime time.Time, contractTime *time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client
				SET create_time = $2, auth_time = $3, contract_time = $4
				WHERE client_id = ANY($1)
			`,
			clientIds,
			createTime,
			authTime,
			contractTime,
		))
	})
}

func proxyTrimTestSetDescription(t testing.TB, ctx context.Context, clientIds []server.Id, description *string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_client SET description = $2 WHERE client_id = ANY($1)`,
			clientIds,
			description,
		))
	})
}

func proxyTrimTestReasons(proxies []*NetworkProxyTrimProxy) map[server.Id]string {
	reasons := map[server.Id]string{}
	for _, proxy := range proxies {
		reasons[proxy.ClientId] = proxy.KeepReason
	}
	return reasons
}

// The root cause of item 8327: a trim never touched a Pro network, so a Pro
// network the extension had filled to its cap could not be trimmed. Named with
// IncludePro, a Pro network at its cap keeps its newest proxies, the ones with a
// description and the ones used since the bound (by their last contract or
// their last auth), and loses the rest through the removal path, with each
// removed client's revocation record and removed flag. A Pro network the trim
// does not name and a network with an Embed allowance are untouched; without
// IncludePro, a named Pro network at its cap is not trimmed and a free network
// is trimmed as before, its described proxies included.
func TestNetworkProxyTrimIncludeProTrimsANamedProNetworkToItsKeepSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetNetworkProxyTrimBatchSizes(3, 2)()

		lift := Testing_LiftProxyClientLimits()
		pro := newProxyTrimTestProNetwork(t, ctx, "trimpronamed", 12)
		proOther := newProxyTrimTestProNetwork(t, ctx, "trimproother", 8)
		free := newProxyTrimTestNetwork(t, ctx, "trimfreeplain", 8, 0)
		allowance := newProxyTrimTestNetwork(t, ctx, "trimallowancepro", 8, 0)
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, allowance.networkId, 100), nil)
		lift()
		// the named Pro network holds exactly the Pro cap, as the customer's did
		defer Testing_SetProxyClientLimits(5, 12)()

		now := server.NowUtc()
		since := now.Add(-7 * 24 * time.Hour)
		old := now.Add(-20 * 24 * time.Hour)
		recentContract := now.Add(-24 * time.Hour)
		recentAuth := now.Add(-48 * time.Hour)
		justBefore := since.Add(-time.Second)

		// oldest first by client id; the last 5 are the newest
		sorted := slices.Clone(pro.proxies)
		slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		// the extension's proxies: no description, last used 20 days ago
		empty := ""
		proxyTrimTestSetDescription(t, ctx, sorted, &empty)
		proxyTrimTestSetUse(t, ctx, sorted, old, old, &old)
		// never used, and a NULL description
		proxyTrimTestSetDescription(t, ctx, sorted[1:2], nil)
		proxyTrimTestSetUse(t, ctx, sorted[1:2], old, old, nil)
		// named on ur.io
		home := "home"
		proxyTrimTestSetDescription(t, ctx, sorted[2:3], &home)
		// an auth an hour before the bound, and a contract a second before it
		proxyTrimTestSetUse(t, ctx, sorted[3:4], old, since.Add(-time.Hour), &old)
		proxyTrimTestSetUse(t, ctx, sorted[5:6], old, old, &justBefore)
		// a contract a day ago, and an auth two days ago (a client's connect)
		proxyTrimTestSetUse(t, ctx, sorted[4:5], old, old, &recentContract)
		proxyTrimTestSetUse(t, ctx, sorted[6:7], old, recentAuth, &old)

		wantKeep := []server.Id{sorted[11], sorted[10], sorted[9], sorted[8], sorted[7], sorted[6], sorted[4], sorted[2]}
		wantReasons := map[server.Id]string{
			sorted[11]: NetworkProxyTrimKeepNewest,
			sorted[10]: NetworkProxyTrimKeepNewest,
			sorted[9]:  NetworkProxyTrimKeepNewest,
			sorted[8]:  NetworkProxyTrimKeepNewest,
			sorted[7]:  NetworkProxyTrimKeepNewest,
			sorted[6]:  NetworkProxyTrimKeepUsed,
			sorted[4]:  NetworkProxyTrimKeepUsed,
			sorted[2]:  NetworkProxyTrimKeepDescribed,
		}
		wantRemove := []server.Id{sorted[5], sorted[3], sorted[1], sorted[0]}

		// Without IncludePro: the named Pro network at its cap is not listed,
		// and the free network is trimmed to its newest 5 as before, though
		// every one of its proxies has a description.
		defaults := NetworkProxyTrimOptions{
			NetworkIds:    []server.Id{pro.networkId, proOther.networkId, free.networkId, allowance.networkId},
			Keep:          DefaultNetworkProxyTrimKeep(),
			RatePerSecond: 1_000_000,
		}
		defaultPlan, err := PlanNetworkProxyTrim(ctx, defaults)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, proxyTrimTestPlanNetwork(defaultPlan, pro.networkId) == nil, true)
		connect.AssertEqual(t, proxyTrimTestPlanNetwork(defaultPlan, proOther.networkId) == nil, true)
		freePlan := proxyTrimTestPlanNetwork(defaultPlan, free.networkId)
		connect.AssertEqual(t, freePlan != nil && freePlan.Skip == "" && len(freePlan.RemoveClientIds) == 3, true)
		allowancePlan := proxyTrimTestPlanNetwork(defaultPlan, allowance.networkId)
		connect.AssertEqual(t, allowancePlan != nil && allowancePlan.Skip == NetworkProxyTrimSkipOverride, true)
		connect.AssertEqual(t, defaultPlan.RemoveCount, 3)
		defaultResult, err := CommitNetworkProxyTrim(ctx, defaults, defaultPlan.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, defaultResult.RemovedCount, int64(3))
		connect.AssertEqual(t, proxyTrimTestActive(t, ctx, free.proxies), proxyTrimTestNewest(free.proxies, 5))
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, pro.proxies)), 12)

		// IncludePro trims only named networks: a scan with it is refused and
		// removes nothing
		_, err = PlanNetworkProxyTrim(ctx, NetworkProxyTrimOptions{Keep: 5, IncludePro: true})
		connect.AssertEqual(t, errors.Is(err, ErrNetworkProxyTrimProNeedsNetworkIds), true)
		_, err = CommitNetworkProxyTrim(ctx, NetworkProxyTrimOptions{Keep: 5, IncludePro: true}, "")
		connect.AssertEqual(t, errors.Is(err, ErrNetworkProxyTrimProNeedsNetworkIds), true)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, pro.proxies)), 12)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, proOther.proxies)), 8)

		// The named Pro network, with the keep rules; the other Pro network is
		// not named, and the allowance network is named
		options := NetworkProxyTrimOptions{
			NetworkIds:    []server.Id{pro.networkId, allowance.networkId},
			Keep:          DefaultNetworkProxyTrimKeep(),
			IncludePro:    true,
			KeepDescribed: true,
			KeepUsedSince: &since,
			RatePerSecond: 1_000_000,
		}
		plan, err := PlanNetworkProxyTrim(ctx, options)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(plan.Networks), 2)
		connect.AssertEqual(t, plan.RemoveCount, 4)
		proPlan := proxyTrimTestPlanNetwork(plan, pro.networkId)
		connect.AssertEqual(t, proPlan != nil, true)
		connect.AssertEqual(t, proPlan.Skip, "")
		connect.AssertEqual(t, proPlan.Pro, true)
		connect.AssertEqual(t, proPlan.ActiveProxies, 12)
		connect.AssertEqual(t, proPlan.KeepClientIds, wantKeep)
		connect.AssertEqual(t, proxyTrimTestReasons(proPlan.KeepProxies), wantReasons)
		connect.AssertEqual(t, proPlan.RemoveClientIds, wantRemove)
		for _, proxy := range proPlan.RemoveProxies {
			connect.AssertEqual(t, proxy.Description, "")
			connect.AssertEqual(t, proxy.KeepReason, "")
			connect.AssertEqual(t, proxy.LastUseTime().Before(since), true)
		}
		allowancePlan = proxyTrimTestPlanNetwork(plan, allowance.networkId)
		connect.AssertEqual(t, allowancePlan != nil && allowancePlan.Skip == NetworkProxyTrimSkipOverride && len(allowancePlan.RemoveClientIds) == 0, true)
		connect.AssertEqual(t, proxyTrimTestPlanNetwork(plan, proOther.networkId) == nil, true)

		// the options bind the digest: without them, the same networks plan
		// another one
		unbound := options
		unbound.IncludePro, unbound.KeepDescribed, unbound.KeepUsedSince = false, false, nil
		unboundPlan, err := PlanNetworkProxyTrim(ctx, unbound)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, unboundPlan.Digest != plan.Digest, true)

		_, err = CommitNetworkProxyTrim(ctx, options, unboundPlan.Digest)
		connect.AssertEqual(t, errors.Is(err, ErrNetworkProxyTrimDigestMismatch), true)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, pro.proxies)), 12)

		result, err := CommitNetworkProxyTrim(ctx, options, plan.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.RemovedCount, int64(4))
		connect.AssertEqual(t, result.RemovedByNetwork[pro.networkId], int64(4))

		keepSorted := slices.Clone(wantKeep)
		slices.SortFunc(keepSorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, proxyTrimTestActive(t, ctx, pro.proxies), keepSorted)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, proOther.proxies)), 8)
		connect.AssertEqual(t, len(proxyTrimTestActive(t, ctx, allowance.proxies)), 8)
		// each removed client is refused by its revocation record (a hosted
		// proxy is its own root) and closed by its removed flag; a kept one is
		// neither
		for _, clientId := range wantRemove {
			key := session.Testing_RevocationRecordKey(pro.networkId, clientId)
			assertRevocationField(t, ctx, key, session.Testing_RevocationClientField(clientId), "x")
			assertRevocationField(t, ctx, key, session.Testing_RevocationRootField(clientId), "x")
			connect.AssertEqual(t, IsClientRemoved(ctx, clientId), true)
		}
		for _, clientId := range wantKeep {
			connect.AssertEqual(t, IsClientRemoved(ctx, clientId), false)
		}

		// a second trim finds nothing more to remove
		rerun, err := PlanNetworkProxyTrim(ctx, options)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rerun.RemoveCount, 0)
	})
}

// Pure: the keep rule. The newest `keep` stay whatever else holds; past them a
// proxy stays for a description only when the options keep described ones,
// and for use only when its last contract or its last auth is at or after the
// bound. An empty description is none, and a use a second before the bound is
// not recent.
func TestNetworkProxyTrimKeepReason(t *testing.T) {
	since := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	old := since.Add(-20 * 24 * time.Hour)
	before := since.Add(-time.Second)
	proxy := func(description string, authTime time.Time, contractTime *time.Time) *NetworkProxyTrimProxy {
		return &NetworkProxyTrimProxy{
			ClientId:     server.NewId(),
			Description:  description,
			CreateTime:   old,
			AuthTime:     authTime,
			ContractTime: contractTime,
		}
	}

	unused := proxy("", old, nil)
	connect.AssertEqual(t, networkProxyTrimKeepReason(0, unused, 5, true, &since), NetworkProxyTrimKeepNewest)
	connect.AssertEqual(t, networkProxyTrimKeepReason(4, unused, 5, true, &since), NetworkProxyTrimKeepNewest)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, unused, 5, true, &since), "")
	connect.AssertEqual(t, networkProxyTrimKeepReason(0, unused, 0, true, &since), "")

	named := proxy("home", old, &old)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, named, 5, true, nil), NetworkProxyTrimKeepDescribed)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, named, 5, false, &since), "")
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, named, 5, false, nil), "")

	contracted := proxy("", old, &since)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, contracted, 5, false, &since), NetworkProxyTrimKeepUsed)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, contracted, 5, true, nil), "")
	authed := proxy("", since, &old)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, authed, 5, false, &since), NetworkProxyTrimKeepUsed)
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, proxy("", before, &before), 5, true, &since), "")
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, proxy("", before, nil), 5, true, &since), "")

	// the first reason that holds is the one named
	connect.AssertEqual(t, networkProxyTrimKeepReason(5, proxy("home", since, &since), 5, true, &since), NetworkProxyTrimKeepDescribed)
	connect.AssertEqual(t, networkProxyTrimKeepReason(1, proxy("home", since, &since), 5, true, &since), NetworkProxyTrimKeepNewest)

	// the last use is the later of the two markers
	connect.AssertEqual(t, proxy("", old, &since).LastUseTime(), since)
	connect.AssertEqual(t, proxy("", since, &old).LastUseTime(), since)
	connect.AssertEqual(t, proxy("", since, nil).LastUseTime(), since)
}

// Pure: a trim without the options added on 2026-11-14 keeps the v1 digest
// byte for byte (the keep count, then each trimmed network's removals in client
// id order; a skipped network is not bound), so free networks plan and commit
// as before. Each added option, and another bound, changes the digest.
func TestNetworkProxyTrimDigestWithoutTheProOptionsIsUnchanged(t *testing.T) {
	networkId, err := server.ParseId("019c6280-38a0-9123-374f-a630b59efa36")
	connect.AssertEqual(t, err, nil)
	skippedNetworkId, err := server.ParseId("01969f18-0000-0000-0000-000000000001")
	connect.AssertEqual(t, err, nil)
	a, err := server.ParseId("019a0000-0000-0000-0000-00000000000a")
	connect.AssertEqual(t, err, nil)
	b, err := server.ParseId("019a0000-0000-0000-0000-00000000000b")
	connect.AssertEqual(t, err, nil)
	newPlan := func() *NetworkProxyTrimPlan {
		return &NetworkProxyTrimPlan{
			Keep: 5,
			Networks: []*NetworkProxyTrimNetwork{
				{NetworkId: skippedNetworkId, Skip: NetworkProxyTrimSkipPro},
				{NetworkId: networkId, RemoveClientIds: []server.Id{b, a}},
			},
		}
	}
	v1 := sha256.Sum256([]byte(
		"urnetwork network trim-proxies v1\n" +
			"keep 5\n" +
			"network " + networkId.String() + " remove 2\n" +
			a.String() + "\n" +
			b.String() + "\n",
	))
	connect.AssertEqual(t, networkProxyTrimDigest(newPlan()), hex.EncodeToString(v1[:]))

	since := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	later := since.Add(time.Second)
	digests := map[string]bool{networkProxyTrimDigest(newPlan()): true}
	for _, set := range []func(plan *NetworkProxyTrimPlan){
		func(plan *NetworkProxyTrimPlan) { plan.IncludePro = true },
		func(plan *NetworkProxyTrimPlan) { plan.KeepDescribed = true },
		func(plan *NetworkProxyTrimPlan) { plan.KeepUsedSince = &since },
		func(plan *NetworkProxyTrimPlan) { plan.KeepUsedSince = &later },
	} {
		plan := newPlan()
		set(plan)
		digest := networkProxyTrimDigest(plan)
		connect.AssertEqual(t, digests[digest], false)
		digests[digest] = true
	}
}
