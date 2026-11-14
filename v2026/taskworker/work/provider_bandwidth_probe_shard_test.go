// The bandwidth probe's prober shard against the database: its slot beside
// the URL and capability probes' shards, its own identity, and its cleanup.
package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
	"github.com/urnetwork/server/v2026/session"
)

// A bandwidth pass runs in prober shard slot 254 beside an active URL probe
// shard and an active capability probe shard, in a network and client of its
// own. Its tunnels carry that shard's identity -- a credential that
// authenticates as the shard's own client, with its credentials, control,
// placement and retirements -- and never the URL shard's, so a provider that
// recognizes a bandwidth tunnel cannot link it to any quality tunnel's
// identity. Both other shards keep their owners and identities, and the
// pass's own shard is drained when it returns.
func TestProviderBandwidthProbeShardRunsApartFromTheQualityShards(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := providerBandwidthProbeSettingsFrom(defaultProviderEgressProbeSettings("probe.example"))
		credit, err := providerBandwidthProbeShardCredit(settings, 5)
		if err != nil {
			t.Fatal(err)
		}
		urlKey := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: defaultProviderEgressProbeShardCount}
		urlOwner, err := model.BeginProberShard(ctx, urlKey, 64*1024, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		capabilityKey := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: retiredCapabilityProbeShardIndex, ShardCount: retiredCapabilityProbeShardCount}
		capabilityOwner, err := model.BeginProberShard(ctx, capabilityKey, 64*1024, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: providerBandwidthProbeShardIndex, ShardCount: providerBandwidthProbeShardCount}
		finished := false
		var networkId server.Id
		pass := &ProviderBandwidthProbeResult{Started: 1, Measured: 1}
		result, err := runProviderBandwidthProbeShard(ctx, key, settings, credit, time.Hour, func(tunnelConfig providertunnel.Config) (*ProviderBandwidthProbeResult, error) {
			finished = true
			if tunnelConfig.ClientCredentials == nil || tunnelConfig.ClientControl == nil || tunnelConfig.ByJwt == "" ||
				tunnelConfig.PlatformPlacement == nil || tunnelConfig.Retirements == nil || tunnelConfig.ApiUrl != "https://api.probe.example" {
				t.Errorf("the pass's tunnels lack the shard's identity, credentials, control, placement or retirements: %+v", tunnelConfig)
			}
			networkId = proberShardTestNetworkId(t, ctx, key)
			if networkId == urlOwner.NetworkId || networkId == capabilityOwner.NetworkId {
				t.Error("the bandwidth pass shares a network with a quality shard")
			}
			clientId := server.Id(tunnelConfig.ClientId)
			if clientId == urlOwner.ClientId || clientId == capabilityOwner.ClientId {
				t.Error("the bandwidth pass's tunnels carry a quality shard's client")
			}
			claims, err := session.ParseByJwtForAudience(ctx, tunnelConfig.ByJwt, session.ByJwtAudienceApi)
			if err != nil {
				t.Fatalf("the pass's credential does not parse: %v", err)
			}
			if claims.NetworkId != networkId || claims.ClientId == nil || *claims.ClientId != clientId {
				t.Errorf("the pass's credential is not its own shard's: network %s client %v, want %s and %s", claims.NetworkId, claims.ClientId, networkId, clientId)
			}
			for _, c := range []struct {
				name string
				key  model.ProberShardKey
			}{
				{name: "bandwidth", key: key},
				{name: "URL", key: urlKey},
				{name: "capability", key: capabilityKey},
			} {
				if state := proberShardTestState(t, ctx, c.key); state != "active" {
					t.Errorf("the %s shard is %q while the bandwidth pass runs", c.name, state)
				}
			}
			return pass, nil
		})
		if err != nil || !finished || result != pass {
			t.Fatalf("the pass returned %+v, %v (finished %t)", result, err, finished)
		}
		if state := proberShardTestState(t, ctx, key); state != "draining" {
			t.Fatalf("the pass's shard is %q after it, want draining until its generation drops", state)
		}
		for _, owner := range []*model.ProberShardOwner{urlOwner, capabilityOwner} {
			if _, err := model.ProberShardIdentity(ctx, owner); err != nil {
				t.Fatalf("a quality shard lost its identity beside the bandwidth pass: %v", err)
			}
		}
		proberShardTestCleanup(t, ctx, key)
		proberShardTestCleanup(t, ctx, urlKey)
		proberShardTestCleanup(t, ctx, capabilityKey)
	})
}
