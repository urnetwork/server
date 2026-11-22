package controller

// A proxied or bridged address counts the same as an owned one (owner
// decision 2026-11-21). That reverted judging a provider by its probed exit
// (2026-11-20), the SN25 exit rule from epoch 7 (2026-11-20) and the
// proxied-egress mark (2026-11-19). State the reverted code wrote before the
// release outlives it: a judgement per client ({pexit_<client>}), the mark
// ({vproxy_<client>}), the connect announce's record of a probed provider's
// network systems ({pnet_<client>}), and provider_egress_exit_day, a table
// Main keeps, unread. These tests write that state as the reverted code did
// and check that nothing reads it, and that a probe writes none of it:
//   - a probe stores the probed location and nothing else, and leaves the
//     provider's live connections as they were;
//   - a provider with a differing day in an epoch's window earns its usage;
//   - an extender whose provider client is marked is active in the census.
//
// Each test fails on the code before the revert. Addresses are RFC 5737
// documentation addresses, except the exit, a MaxMind test address this suite
// already places (provider_egress_location_controller_test.go).

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One connection's place and ARIN facts.
type proxyCountsLocationRow struct {
	cityLocationId      server.Id
	countryLocationId   server.Id
	arinRisk            bool
	arinNonQuality      bool
	arinQualityVerified bool
	arinEpoch           int64
	arinOldNonQuality   bool
	arinOldEpoch        int64
}

func proxyCountsConnectionLocation(t testing.TB, ctx context.Context, connectionId server.Id) proxyCountsLocationRow {
	t.Helper()
	row := proxyCountsLocationRow{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				city_location_id,
				country_location_id,
				arin_risk,
				arin_non_quality,
				arin_quality_verified,
				arin_database_build_epoch,
				arin_old_non_quality,
				arin_old_database_build_epoch
			FROM network_client_location
			WHERE connection_id = $1
			`,
			connectionId,
		).Scan(
			&row.cityLocationId,
			&row.countryLocationId,
			&row.arinRisk,
			&row.arinNonQuality,
			&row.arinQualityVerified,
			&row.arinEpoch,
			&row.arinOldNonQuality,
			&row.arinOldEpoch,
		))
	})
	return row
}

// The days provider_egress_exit_day holds for the client.
func proxyCountsExitDays(t testing.TB, ctx context.Context, clientId server.Id) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM provider_egress_exit_day WHERE client_id = $1`,
			clientId,
		).Scan(&count))
	})
	return count
}

// Writes the mark as the reverted MarkVerifyProxiedEgress wrote it.
func proxyCountsWriteMark(ctx context.Context, clientId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, fmt.Sprintf("{vproxy_%s}", clientId), "1", 8*24*time.Hour).Err())
	})
}

// A slot connects from a residential proxy, stored at a city of its own in
// Canada as a verified subscriber, and the probe sees its users' traffic leave
// from another country. The connect announce of the reverted connect build
// recorded the system of the network it connects from. Two probes store the
// probed location, and nothing else: no judgement, no mark and no recorded
// day, and the live connection keeps its own facts and place.
func TestSubmitProviderEgressLocationJudgesNoExit(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// documentation addresses resolve to no place, whatever the config overrides
		server.UseNoIpOverridesForTest(t)
		const residentialIp = "198.51.100.10"

		clientId := testEgressProvider(ctx)
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, net.JoinHostPort(residentialIp, "40000"), handlerId)
		if err != nil {
			t.Fatal(err)
		}
		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "city-" + clientId.String(),
			Region:       "region-" + clientId.String(),
			Country:      "Canada",
			CountryCode:  "ca",
		}
		model.CreateLocation(ctx, city)
		lookupAt := server.NowUtc()
		if err := model.SetConnectionLocation(ctx, connectionId, city.LocationId, &model.ConnectionLocationScores{
			ArinQualityVerified:       true,
			ArinLookupAt:              &lookupAt,
			ArinDatabaseBuildEpoch:    1800086400,
			ArinOldQualityVerified:    true,
			ArinOldDatabaseBuildEpoch: 1800000000,
		}); err != nil {
			t.Fatal(err)
		}
		before := proxyCountsConnectionLocation(t, ctx, connectionId)
		if before.countryLocationId != city.CountryLocationId || !before.arinQualityVerified || before.arinNonQuality {
			t.Fatalf("the fixture's residential connection %+v", before)
		}

		// the network system record the reverted connect announce wrote
		addressHash, err := server.ClientIpHash(residentialIp)
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, fmt.Sprintf("{pnet_%s}", clientId), hex.EncodeToString(addressHash[:]), "64496").Err())
		})

		for range 2 {
			if _, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId:   clientId,
				ExitIp:     testEgressCountryExitIp,
				ObservedAt: server.NowUtc(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		egress := model.GetProviderEgressLocation(ctx, clientId)
		if egress == nil {
			t.Fatal("the probe stored no location")
		}
		if location := model.GetLocation(ctx, egress.LocationId); location == nil || location.CountryLocationId == city.CountryLocationId {
			t.Fatalf("the fixture's exit %+v is in the connection's country", location)
		}

		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{fmt.Sprintf("{pexit_%s}", clientId), fmt.Sprintf("{vproxy_%s}", clientId)} {
				count, err := r.Exists(ctx, key).Result()
				server.Raise(err)
				if count != 0 {
					t.Fatalf("the probe wrote %s", key)
				}
			}
		})
		if days := proxyCountsExitDays(t, ctx, clientId); days != 0 {
			t.Fatalf("the probe recorded %d differing days", days)
		}
		if after := proxyCountsConnectionLocation(t, ctx, connectionId); after != before {
			t.Fatalf("the probe rewrote the live connection: %+v, was %+v", after, before)
		}
	})
}

// A provider alone in its network, connected from one address, whose network
// consents to a coldkey of its own. The caller flushes the consents.
func (self *ledgerV2BuildFixture) proxyCountsProvider(ip string, seed uint32) (server.Id, server.Id, [32]byte) {
	t := self.t
	t.Helper()
	clientId, networkId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(self.ctx, networkId, server.NewId(), clientId, "proxy-counts-provider", "test")
	if _, _, _, _, err := model.ConnectNetworkClient(self.ctx, clientId, net.JoinHostPort(ip, "40000"), model.CreateNetworkClientHandler(self.ctx)); err != nil {
		t.Fatal(err)
	}
	self.consents.networkChain(t, networkId, seed, 1)
	return clientId, networkId, self.consents.public(t, seed)
}

// A slot whose probed exit the reverted ingest judged different, inside epoch
// 7's window, as provider_egress_exit_day recorded it, earns its usage in
// epoch 7: its row keeps its selected coldkey, the epoch's amount splits by
// usage, both resolutions are retained, and the validator's audit approves the
// root.
func TestStLedgerV2ProviderWhoseExitDifferedEarnsItsUsage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		slot, slotNetwork, slotKey := f.proxyCountsProvider("198.51.100.8", 21)
		direct, directNetwork, directKey := f.proxyCountsProvider("192.0.2.17", 22)
		f.consents.flush(t)
		seven := ledgerV2BuildAuthority(7)

		// the day the reverted ingest recorded, as recordProviderEgressExitDay wrote it
		observedAt := seven.StartTime.Add(time.Minute)
		day := time.Date(observedAt.Year(), observedAt.Month(), observedAt.Day(), 0, 0, 0, 0, time.UTC)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				f.ctx,
				`
				INSERT INTO provider_egress_exit_day (
					day, client_id, network_id, differs_count, cross_family_count, first_observed_at, last_observed_at
				) VALUES ($1, $2, $3, 2, 0, $4, $5)
				`,
				day, slot, slotNetwork, observedAt, observedAt.Add(time.Minute),
			))
		})
		f.contract(seven.StartTime.Add(2*time.Minute),
			ledgerV2BuildUsage{clientId: slot, networkId: slotNetwork, bytes: 300},
			ledgerV2BuildUsage{clientId: direct, networkId: directNetwork, bytes: 100})

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 7, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(7, 1_000, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(ledgerV2Row(slot, slotKey, 300, false), ledgerV2Row(direct, directKey, 100, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 7 rows %+v\nwant %+v", got, want)
		}
		want := map[string]uint64{
			fmt.Sprintf("7 %x", slotKey[:4]):   750,
			fmt.Sprintf("7 %x", directKey[:4]): 250,
		}
		if got := ledgerV2Amounts(build.Amounts); !reflect.DeepEqual(got, want) {
			t.Fatalf("amounts %v, want %v", got, want)
		}
		if got, want := f.resolutionModes(7), map[server.Id]string{slot: protocol.EarningWalletModeNetwork, direct: protocol.EarningWalletModeNetwork}; !reflect.DeepEqual(got, want) {
			t.Fatalf("retained resolutions %v, want %v", got, want)
		}
		if result := f.audit(input, root, ledgerV2Heads{7: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// From E_x = 2, an extender whose provider client carries the mark, as a proxy
// slot that declared its proxy, or whose probed exit differed, was marked, is
// active in the census and eligible with its own unit.
func TestStLedgerV2ExtenderCensusCountsAMarkedProxySlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		// the census keys units as the head's egress prefix, under verify.yml's
		// key, which the test vault does not hold
		settings := model.DefaultVerifySettings()
		settings.EgressHashKey = []byte("sn25 extender census proxy slot test key")
		SetVerifySettings(settings)
		t.Cleanup(func() { SetVerifySettings(nil) })
		line, err := amendment.NewExtenders(f.cfg.ChainId, uint16(f.cfg.Netuid), f.cfg.NoId, 2).Line()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.cfg.Amendments.Encode([]string{line}); err != nil {
			t.Fatal(err)
		}
		if err := f.cfg.ExtenderPool.Encode(map[string][]string{"measurement_network_ids": {server.NewId().String()}}); err != nil {
			t.Fatal(err)
		}

		slot, receiver := server.NewId(), server.NewId()
		slotNetwork, origin := server.NewId(), server.NewId()
		f.consents.providerChain(t, slot, slotNetwork, [2]uint64{16, 1})
		f.consents.flush(t)
		slotKey := f.consents.public(t, 16)
		const slotIp = "198.51.100.9"
		created := ledgerV2BuildTime(150)
		for _, record := range []struct {
			client, network server.Id
			ip              string
		}{{slot, slotNetwork, slotIp}, {receiver, origin, "203.0.113.7"}} {
			extenderId := server.NewId()
			model.Testing_CreateNetworkExtender(f.ctx, &model.NetworkExtender{
				ExtenderId: extenderId, NetworkId: record.network, ClientId: record.client, PublicKey: extenderId.Bytes(), CreateTime: created,
				TcpPort: 443, UdpPort: 443, DnsPort: 53, DnsTld: "ur.xyz.", CountryCode: "us", Active: true,
			}, []*model.NetworkExtenderAddress{{ExtenderId: extenderId, IpVersion: 4, Ip: netip.MustParseAddr(record.ip), Carriers: []string{"tcp"}, ActivateTime: created, Active: true}})
		}
		proxyCountsWriteMark(f.ctx, slot)

		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed || !strings.Contains(outcome.Reason, "2 rows, 1 eligible extenders in 1 units") {
			t.Fatalf("census capture %+v, want the marked slot eligible", outcome)
		}
		captured := model.GetStLedgerV2ExtenderCensus(f.ctx, f.cfg.DeploymentKey(), f.cfg.NoId, 2)
		if captured == nil {
			t.Fatal("no census captured")
		}
		census, err := model.DecodeStExtenderCensus(captured.Census)
		if err != nil || len(census) != 2 {
			t.Fatalf("census %+v %v", census, err)
		}
		found := false
		for _, row := range census {
			if server.Id(row.ClientID) != slot {
				continue
			}
			found = true
			if !row.Eligible() || row.Coldkey != slotKey || row.Family != ledger.UnitIPv4 || row.Unit != model.VerifyEgressIpHashWithSettings(netip.MustParseAddr(slotIp), VerifySettings()) {
				t.Fatalf("the marked slot's census row %+v, want eligible at its own unit", row)
			}
		}
		if !found {
			t.Fatal("the census has no row for the marked slot")
		}
	})
}
