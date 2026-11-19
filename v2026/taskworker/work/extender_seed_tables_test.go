package work

// The whole seed task on the real tables, with none of the publisher's seams
// replaced (connect/EXTENDER-UPDATE2.md §1.4, §2.2, §2.3): the open pool, each
// member's record inputs and the revoked members are read with the production
// queries and signed with the production signer, and what the task stored is
// checked as a client checks it. The other publisher tests stand in for the
// database; this is what ties their rules to it.

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Creates one active extender with a real-sized identity key, which the dns
// fixture's keys are not, and an active address per family given.
func createTestSeedTableExtender(ctx context.Context, index int, ipVersions ...int) (server.Id, []byte) {
	publicKey := testExtenderSeedPublicKey(3_000_000 + index)
	extenderId := server.NewId()
	now := server.NowUtc()
	addresses := []*model.NetworkExtenderAddress{}
	for _, ipVersion := range ipVersions {
		ip := fmt.Sprintf("198.51.100.%d", index+1)
		if ipVersion == 6 {
			ip = fmt.Sprintf("2001:db8:7::%x", index+1)
		}
		addresses = append(addresses, &model.NetworkExtenderAddress{
			IpVersion:    ipVersion,
			Ip:           netip.MustParseAddr(ip),
			Carriers:     []string{connect.ExtenderCarrierTcp},
			ActivateTime: now,
			Active:       true,
		})
	}
	model.Testing_CreateNetworkExtender(ctx, &model.NetworkExtender{
		ExtenderId:  extenderId,
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   publicKey,
		CreateTime:  now,
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "us",
		Active:      true,
	}, addresses)
	return extenderId, publicKey
}

func TestExtenderSeedTaskOnTheTables(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1<<20)
		rootPublicKey := installTestExtenderWorkConfig(t, "seed:", "  enabled: true")
		rootKeys := connect.NewExtenderRootKeySet(rootPublicKey)
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// the open pool
		open := map[string]server.Id{}
		for index := range 12 {
			extenderId, publicKey := createTestSeedTableExtender(ctx, index, 4, 6)
			open[string(publicKey)] = extenderId
		}
		// and what must stay out of a public bundle, with every other reason to
		// be chosen
		restricted := map[string]string{}
		gatedId, gatedKey := createTestSeedTableExtender(ctx, 20, 4, 6)
		if err := model.SetNetworkExtenderDirectoryTier(ctx, gatedId, connect.ExtenderDirectoryTierGated, false); err != nil {
			t.Fatal(err)
		}
		restricted[string(gatedKey)] = "gated"
		gatedCanaryId, gatedCanaryKey := createTestSeedTableExtender(ctx, 21, 4, 6)
		if err := model.SetNetworkExtenderDirectoryTier(ctx, gatedCanaryId, connect.ExtenderDirectoryTierGated, false); err != nil {
			t.Fatal(err)
		}
		if err := model.SetNetworkExtenderCanaryChannel(ctx, gatedCanaryId, connect.ExtenderChannelGated, false); err != nil {
			t.Fatal(err)
		}
		restricted[string(gatedCanaryKey)] = "gated canary"
		dnsCanaryId, dnsCanaryKey := createTestSeedTableExtender(ctx, 22, 4, 6)
		if err := model.SetNetworkExtenderCanaryChannel(ctx, dnsCanaryId, connect.ExtenderChannelDns, false); err != nil {
			t.Fatal(err)
		}
		restricted[string(dnsCanaryKey)] = "dns canary"

		pass := func() *ExtenderSeedResult {
			t.Helper()
			result, err := ExtenderSeed(&ExtenderSeedArgs{}, clientSession)
			if err != nil {
				t.Fatalf("ExtenderSeed: %v", err)
			}
			return result
		}
		// what the origin holds, checked as a client checks it, and the invariants
		stored := func() *connect.ExtenderSeedBundle {
			t.Helper()
			store, ok := server.LoadBlobStore()
			if !ok {
				t.Fatal("no blob store")
			}
			artifact, err := controller.ReadExtenderSeedLatest(ctx, store, testExtenderWorkNetworkHost)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := connect.ParseExtenderSeedEnvelope(artifact)
			if err != nil {
				t.Fatal(err)
			}
			now := server.NowUtc()
			bundle, err := connect.VerifyExtenderSeed(envelope, rootKeys, []string{testExtenderWorkNetworkHost}, now)
			if err != nil {
				t.Fatalf("the stored bundle does not verify: %s", err)
			}
			assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
			for _, record := range bundle.Records {
				if name, ok := restricted[string(record.Body.PublicKey)]; ok {
					t.Fatalf("the bundle carries the %s extender's record", name)
				}
				if _, ok := open[string(record.Body.PublicKey)]; !ok {
					t.Fatalf("the bundle carries a record of no open extender")
				}
				if record.Revoked {
					t.Fatalf("the bundle carries a record its own revocation dominates")
				}
			}
			return bundle
		}

		first := pass()
		if !first.Enabled || first.Outcome != extenderSeedOutcomePublished || first.Reason != "initial" {
			t.Fatalf("the first pass: %+v", first)
		}
		bundle := stored()
		if len(bundle.Records) != ExtenderSeedMaxRecordCount {
			t.Fatalf("%d records from a pool of 12, want a full cohort", len(bundle.Records))
		}
		if first.Sequence != bundle.Sequence {
			t.Fatalf("the pass reported sequence %d, the origin holds %d", first.Sequence, bundle.Sequence)
		}
		for _, record := range bundle.Records {
			if len(record.Body.Addresses) != 2 {
				t.Fatalf("a dual-stack member's record lists %d addresses", len(record.Body.Addresses))
			}
			if record.Body.ExpireTimeMs < uint64(bundle.ExpireTime.UnixMilli()) {
				t.Fatalf("a record expires before the bundle")
			}
		}

		// nothing changed, nothing is due: no new sequence
		if result := pass(); result.Outcome != extenderSeedOutcomeCurrent || result.Sequence != bundle.Sequence {
			t.Fatalf("a pass with nothing due: %+v", result)
		}

		// the probe's revocation of a member, as RecordNetworkExtenderProbeResult
		// leaves the rows: the next pass replaces the bundle and carries it
		revokedKey := bundle.Records[0].Body.PublicKey
		revokeTime := server.NowUtc().Truncate(time.Millisecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender SET active = false, revoke_time = $2 WHERE extender_id = $1`,
				open[string(revokedKey)],
				revokeTime,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_address SET active = false WHERE extender_id = $1`,
				open[string(revokedKey)],
			))
		})
		result := pass()
		if result.Outcome != extenderSeedOutcomePublished || result.Reason != "revoked" || result.Sequence <= bundle.Sequence {
			t.Fatalf("a revoked member: %+v after sequence %d", result, bundle.Sequence)
		}
		replaced := stored()
		carried := false
		for _, revocation := range replaced.Revocations {
			carried = carried || string(revocation.Body.PublicKey) == string(revokedKey)
		}
		if !carried {
			t.Fatalf("the revoked member's revocation is not in the bundle")
		}
		for _, record := range replaced.Records {
			if string(record.Body.PublicKey) == string(revokedKey) {
				t.Fatalf("the revoked member is still published")
			}
		}
		if len(replaced.Records) != ExtenderSeedMaxRecordCount {
			t.Fatalf("%d records after a vacancy, want it filled", len(replaced.Records))
		}

		// the control: off, the task stores nothing and reports only that
		installTestExtenderWorkConfig(t, "seed:", "  enabled: false")
		if result := pass(); result.Enabled || result.Outcome != "" || result.Sequence != 0 {
			t.Fatalf("an off seed: %+v", result)
		}
	})
}
