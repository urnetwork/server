package model

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The extender directory writes (connect/EXTENDER.md C1 to C4).
//
// Nothing here signs anything real: the signer callbacks return a recognizable
// synthetic message, because what these tests are about is WHEN a message is
// produced and over which rows, not what is in it. The signature itself is
// proved end to end by the controller and taskworker tests.

// A signer that records what it was asked to sign and answers with a message
// naming the addresses, so a test can assert the address set a record covered
// without decoding a protobuf.
func testRecordSigner() (
	sign NetworkExtenderRecordSigner,
	signed func() []string,
) {
	messages := []string{}
	sign = func(
		extender *NetworkExtender,
		addresses []*NetworkExtenderAddress,
		issueTime time.Time,
	) ([]byte, error) {
		ips := []string{}
		for _, address := range addresses {
			ips = append(ips, address.Ip.String())
		}
		message := fmt.Sprintf("record:%s", ips)
		messages = append(messages, message)
		return []byte(message), nil
	}
	return sign, func() []string { return messages }
}

func testRevocationSigner() NetworkExtenderRevocationSigner {
	return func(extender *NetworkExtender, issueTime time.Time) ([]byte, error) {
		return []byte("revocation"), nil
	}
}

func testExtenderActivation(publicKey []byte, ipVersion int, ip string) *NetworkExtenderActivation {
	return &NetworkExtenderActivation{
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   publicKey,
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "US",
		IpVersion:   ipVersion,
		Ip:          netip.MustParseAddr(ip),
		Carriers:    []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic},
	}
}

// The three tables of C1 exist on a fresh database with the indexes the reads
// and the upsert depend on. The unique key on public_key is not decoration:
// the activation upsert is keyed by it, which is what makes a second family
// add an address instead of creating a second extender.
func TestExtenderMigrationsApply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		tables := []string{}
		indexes := []string{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT table_name
				FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name LIKE 'network_extender%'
				ORDER BY table_name
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var tableName string
					server.Raise(result.Scan(&tableName))
					tables = append(tables, tableName)
				}
			})

			result, err = conn.Query(
				ctx,
				`
				SELECT indexname
				FROM pg_indexes
				WHERE schemaname = 'public' AND tablename LIKE 'network_extender%'
				ORDER BY indexname
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var indexName string
					server.Raise(result.Scan(&indexName))
					indexes = append(indexes, indexName)
				}
			})
		})

		wantTables := []string{
			"network_extender",
			"network_extender_address",
			"network_extender_publish",
		}
		if !slices.Equal(tables, wantTables) {
			t.Fatalf("extender tables = %v, want %v", tables, wantTables)
		}
		for _, wantIndex := range []string{
			"network_extender_pkey",
			"network_extender_public_key_key",
			"network_extender_address_pkey",
			"network_extender_address_active_last_publish_time",
			"network_extender_publish_pkey",
			"network_extender_publish_published_time_create_time",
		} {
			if !slices.Contains(indexes, wantIndex) {
				t.Fatalf("index %s is missing: %v", wantIndex, indexes)
			}
		}
	})
}

func TestExtenderCarriersRoundTrip(t *testing.T) {
	cases := []struct {
		carriers []string
		stored   string
		read     []string
	}{
		{carriers: []string{"tcp", "quic", "dns"}, stored: "tcp,quic,dns", read: []string{"tcp", "quic", "dns"}},
		{carriers: []string{"tcp", "tcp", " quic "}, stored: "tcp,quic", read: []string{"tcp", "quic"}},
		{carriers: []string{"", "  "}, stored: "", read: []string{}},
		{carriers: nil, stored: "", read: []string{}},
	}
	for _, c := range cases {
		stored := joinExtenderCarriers(c.carriers)
		if stored != c.stored {
			t.Errorf("joinExtenderCarriers(%v) = %q, want %q", c.carriers, stored, c.stored)
		}
		read := splitExtenderCarriers(stored)
		if !slices.Equal(read, c.read) {
			t.Errorf("splitExtenderCarriers(%q) = %v, want %v", stored, read, c.read)
		}
	}
}

// One activation writes the extender, the family address and one record
// publish row, and the record it signed covers the address that was stored.
func TestActivateNetworkExtenderStoresTheAddressAndPublishesTheRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-activate-0001")

		sign, signed := testRecordSigner()
		activated := ActivateNetworkExtender(
			ctx,
			testExtenderActivation(publicKey, 4, "192.0.2.10"),
			sign,
		)
		if activated == nil {
			t.Fatal("the activation stored nothing")
		}

		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		if stored == nil {
			t.Fatal("the extender row is missing")
		}
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, stored.Extender.RevokeTime == nil, true)
		connect.AssertEqual(t, stored.Extender.RecordIssueTime != nil, true)
		connect.AssertEqual(t, stored.Extender.DnsTld, connect.DefaultExtenderDnsTld)
		connect.AssertEqual(t, stored.Extender.CountryCode, "US")
		connect.AssertEqual(t, len(stored.Addresses), 1)
		connect.AssertEqual(t, stored.Addresses[0].IpVersion, 4)
		connect.AssertEqual(t, stored.Addresses[0].Ip.String(), "192.0.2.10")
		connect.AssertEqual(t, stored.Addresses[0].Active, true)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
		connect.AssertEqual(t, stored.Addresses[0].LastPublishTime == nil, true)
		connect.AssertEqual(
			t,
			slices.Equal(stored.Addresses[0].Carriers, []string{"tcp", "quic"}),
			true,
		)

		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 1)
		connect.AssertEqual(t, publishes[0].Kind, NetworkExtenderPublishKindRecord)
		connect.AssertEqual(t, publishes[0].ExtenderId, activated.Extender.ExtenderId)
		connect.AssertEqual(t, publishes[0].PublishedTime == nil, true)
		connect.AssertEqual(t, string(publishes[0].Message), "record:[192.0.2.10]")
		connect.AssertEqual(t, len(signed()), 1)
	})
}

// Re-activating the same identity key on the other family adds a second
// address to the SAME extender and the record then lists both, which is what
// makes a dual-stack extender one directory entry rather than two.
func TestActivateNetworkExtenderAddsTheSecondFamilyToTheSameExtender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-dualstack-001")

		sign, _ := testRecordSigner()
		first := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.11"), sign)
		second := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 6, "2001:db8::11"), sign)

		connect.AssertEqual(t, first.Extender.ExtenderId, second.Extender.ExtenderId)
		connect.AssertEqual(t, len(second.Addresses), 2)

		stored := Testing_GetNetworkExtender(ctx, second.Extender.ExtenderId)
		connect.AssertEqual(t, len(stored.Addresses), 2)
		connect.AssertEqual(t, stored.Addresses[0].IpVersion, 4)
		connect.AssertEqual(t, stored.Addresses[1].IpVersion, 6)

		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 2)
		connect.AssertEqual(t, string(publishes[1].Message), "record:[192.0.2.11 2001:db8::11]")

		// the second record must be newer than the first, or a directory that
		// keeps the newest record would keep the single family one (B5)
		connect.AssertEqual(
			t,
			!second.Extender.RecordIssueTime.Before(*first.Extender.RecordIssueTime),
			true,
		)
	})
}

// A re-activation of an extender that had been revoked brings it back with a
// record issued after the revocation, which is what lets a directory accept it
// again (B5).
func TestActivateNetworkExtenderClearsARevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-reactivate-01")

		sign, _ := testRecordSigner()
		activated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.12"), sign)

		for range ExtenderTestingMaxConsecutiveProbeFailures {
			RecordNetworkExtenderProbeResult(
				ctx,
				activated.Extender.ExtenderId,
				4,
				false,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				testRevocationSigner(),
			)
		}
		revoked := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, revoked.Extender.Active, false)
		connect.AssertEqual(t, revoked.Extender.RevokeTime != nil, true)

		reactivated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.12"), sign)
		connect.AssertEqual(t, reactivated.Extender.ExtenderId, activated.Extender.ExtenderId)

		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, stored.Extender.RevokeTime == nil, true)
		connect.AssertEqual(t, stored.Addresses[0].Active, true)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
		connect.AssertEqual(
			t,
			revoked.Extender.RevokeTime.After(*revoked.Extender.RecordIssueTime) ||
				stored.Extender.RecordIssueTime.After(*revoked.Extender.RevokeTime),
			true,
		)
	})
}

// The failure budget used by the model tests. The task's own constant is in
// the taskworker package, which the model must not depend on.
const ExtenderTestingMaxConsecutiveProbeFailures = 6

// The budget is consecutive: a success anywhere in the run clears it, and only
// the sixth failure IN A ROW deactivates the address.
func TestRecordNetworkExtenderProbeResultSpendsAConsecutiveBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-probebudget-1")

		sign, _ := testRecordSigner()
		activated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.13"), sign)
		extenderId := activated.Extender.ExtenderId

		probe := func(success bool) *NetworkExtenderProbeOutcome {
			return RecordNetworkExtenderProbeResult(
				ctx,
				extenderId,
				4,
				success,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				testRevocationSigner(),
			)
		}

		for i := range ExtenderTestingMaxConsecutiveProbeFailures - 1 {
			outcome := probe(false)
			if outcome.AddressDeactivated {
				t.Fatalf("the address was deactivated after %d failures", i+1)
			}
		}
		stored := Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 5)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeTime != nil, true)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeSuccessTime == nil, true)

		// one success in the middle puts the whole budget back
		probe(true)
		stored = Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeSuccessTime != nil, true)
		connect.AssertEqual(t, stored.Addresses[0].Active, true)

		for i := range ExtenderTestingMaxConsecutiveProbeFailures {
			outcome := probe(false)
			last := i == ExtenderTestingMaxConsecutiveProbeFailures-1
			if outcome.AddressDeactivated != last {
				t.Fatalf(
					"failure %d: deactivated = %v, want %v",
					i+1,
					outcome.AddressDeactivated,
					last,
				)
			}
		}

		stored = Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].Active, false)
		connect.AssertEqual(t, stored.Extender.Active, false)
		connect.AssertEqual(t, stored.Extender.RevokeTime != nil, true)

		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 2)
		connect.AssertEqual(t, publishes[1].Kind, NetworkExtenderPublishKindRevocation)
		connect.AssertEqual(t, string(publishes[1].Message), "revocation")

		// a deactivated address is out of the probe set entirely; only a new
		// activation puts it back
		targets := GetActiveNetworkExtenderProbeTargets(ctx)
		connect.AssertEqual(t, len(targets), 0)

		// and a further result for it changes nothing
		outcome := probe(false)
		connect.AssertEqual(t, outcome.AddressDeactivated, false)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), 2)
	})
}

// Two probe workers of one tick losing the two families of one extender at the
// same instant must still revoke it.
//
// The interleaving is forced with the probe barrier: the sibling family is
// deactivated and committed while this transaction is already open, so the
// remaining-address read sees a snapshot in which the sibling is still active.
// A read that did not lock would believe that snapshot, decide the extender is
// still reachable, and leave it active with no address and no revocation --
// permanently, since nothing probes a deactivated address again.
func TestRecordNetworkExtenderProbeResultRevokesWhenBothFamiliesAreLostAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-bothlost-001")

		sign, _ := testRecordSigner()
		activated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.15"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 6, "2001:db8::15"), sign)
		extenderId := activated.Extender.ExtenderId

		probe := func(ipVersion int) *NetworkExtenderProbeOutcome {
			return RecordNetworkExtenderProbeResult(
				ctx,
				extenderId,
				ipVersion,
				false,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				testRevocationSigner(),
			)
		}
		// both families one failure short of the budget
		for range ExtenderTestingMaxConsecutiveProbeFailures - 1 {
			probe(4)
			probe(6)
		}

		// the other worker commits its own deactivation while this transaction
		// is open; the barrier runs once, so the retry this provokes does not
		// repeat it
		barrierDone := false
		Testing_NetworkExtenderProbeBarrier = func() {
			if barrierDone {
				return
			}
			barrierDone = true
			Testing_DeactivateNetworkExtenderAddress(ctx, extenderId, 4)
		}
		t.Cleanup(func() {
			Testing_NetworkExtenderProbeBarrier = nil
		})

		outcome := probe(6)
		Testing_NetworkExtenderProbeBarrier = nil

		connect.AssertEqual(t, barrierDone, true)
		connect.AssertEqual(t, outcome.AddressDeactivated, true)
		connect.AssertEqual(t, outcome.ExtenderRevoked, true)

		stored := Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].Active, false)
		connect.AssertEqual(t, stored.Addresses[1].Active, false)
		connect.AssertEqual(t, stored.Extender.Active, false)
		connect.AssertEqual(t, stored.Extender.RevokeTime != nil, true)

		revocations := 0
		for _, publish := range Testing_GetNetworkExtenderPublishes(ctx) {
			if publish.Kind == NetworkExtenderPublishKindRevocation {
				revocations += 1
			}
		}
		connect.AssertEqual(t, revocations, 1)
	})
}

// Losing one family of a dual-stack extender deactivates that address only.
// The extender stays active and nothing is revoked, because it is still
// reachable -- revoking here would remove a working extender from every
// directory.
func TestRecordNetworkExtenderProbeResultKeepsTheExtenderWhileAFamilyRemains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-onefamily-001")

		sign, _ := testRecordSigner()
		activated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.14"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 6, "2001:db8::14"), sign)
		extenderId := activated.Extender.ExtenderId

		var outcome *NetworkExtenderProbeOutcome
		for range ExtenderTestingMaxConsecutiveProbeFailures {
			outcome = RecordNetworkExtenderProbeResult(
				ctx,
				extenderId,
				4,
				false,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				testRevocationSigner(),
			)
		}
		connect.AssertEqual(t, outcome.AddressDeactivated, true)
		connect.AssertEqual(t, outcome.ExtenderRevoked, false)

		stored := Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, stored.Extender.RevokeTime == nil, true)
		connect.AssertEqual(t, stored.Addresses[0].Active, false)
		connect.AssertEqual(t, stored.Addresses[1].Active, true)

		// only the record and no revocation
		for _, publish := range Testing_GetNetworkExtenderPublishes(ctx) {
			connect.AssertEqual(t, publish.Kind, NetworkExtenderPublishKindRecord)
		}

		// the surviving family is still probed, the lost one is not
		targets := GetActiveNetworkExtenderProbeTargets(ctx)
		connect.AssertEqual(t, len(targets), 1)
		connect.AssertEqual(t, targets[0].IpVersion, 6)
	})
}

// The drip takes the oldest publish first and a stamped extender goes to the
// back, so the whole set rotates rather than the head being republished.
func TestGetNetworkExtenderIdsForPublishOrdersOldestFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		baseTime := server.NowUtc().Add(-24 * time.Hour)

		// three published at known times, oldest stamp first, and one never
		// published
		extenderIds := []server.Id{}
		for i, lastPublishAt := range []*time.Time{
			timePtr(baseTime),
			timePtr(baseTime.Add(1 * time.Hour)),
			timePtr(baseTime.Add(2 * time.Hour)),
			nil,
		} {
			extenderId := server.NewId()
			extenderIds = append(extenderIds, extenderId)
			var lastPublishTime *time.Time
			if lastPublishAt != nil {
				stamped := *lastPublishAt
				lastPublishTime = &stamped
			}
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  extenderId,
					NetworkId:   networkId,
					ClientId:    clientId,
					PublicKey:   []byte(fmt.Sprintf("extender-public-key-order-%05d", i)),
					CreateTime:  baseTime,
					TcpPort:     443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      connect.DefaultExtenderDnsTld,
					CountryCode: "US",
					Active:      true,
				},
				[]*NetworkExtenderAddress{
					{
						IpVersion:       4,
						Ip:              netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 100+i)),
						Carriers:        []string{connect.ExtenderCarrierTcp},
						ActivateTime:    baseTime,
						Active:          true,
						LastPublishTime: lastPublishTime,
					},
				},
			)
		}

		// never published first, then oldest stamp to newest
		want := []server.Id{extenderIds[3], extenderIds[0], extenderIds[1], extenderIds[2]}
		got := GetNetworkExtenderIdsForPublish(ctx, 4)
		if !slices.Equal(got, want) {
			t.Fatalf("publish order = %v, want %v", got, want)
		}

		// publishing the head moves it to the back
		sign, _ := testRecordSigner()
		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, extenderIds[3], sign), true)
		got = GetNetworkExtenderIdsForPublish(ctx, 4)
		connect.AssertEqual(t, got[0], extenderIds[0])
		connect.AssertEqual(t, got[3], extenderIds[3])

		stored := Testing_GetNetworkExtender(ctx, extenderIds[3])
		connect.AssertEqual(t, stored.Addresses[0].LastPublishTime != nil, true)
		connect.AssertEqual(t, stored.Extender.RecordIssueTime != nil, true)

		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 1)
		connect.AssertEqual(t, publishes[0].Kind, NetworkExtenderPublishKindRecord)
		connect.AssertEqual(t, publishes[0].ExtenderId, extenderIds[3])
	})
}

func timePtr(t time.Time) *time.Time {
	return &t
}

// An extender with one never-published address sorts with the unpublished, not
// with the address that happens to be fresh. MIN over a null set would rank it
// by the fresh one and the never-published address would never be covered.
func TestGetNetworkExtenderIdsForPublishRanksAPartlyUnpublishedExtenderFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		baseTime := server.NowUtc().Add(-24 * time.Hour)
		oldPublishTime := baseTime.Add(time.Hour)

		publishedId := server.NewId()
		Testing_CreateNetworkExtender(
			ctx,
			&NetworkExtender{
				ExtenderId: publishedId,
				NetworkId:  networkId,
				ClientId:   clientId,
				PublicKey:  []byte("extender-public-key-partial-0001"),
				CreateTime: baseTime,
				DnsTld:     connect.DefaultExtenderDnsTld,
				Active:     true,
			},
			[]*NetworkExtenderAddress{
				{
					IpVersion:       4,
					Ip:              netip.MustParseAddr("192.0.2.200"),
					Carriers:        []string{connect.ExtenderCarrierTcp},
					ActivateTime:    baseTime,
					Active:          true,
					LastPublishTime: &oldPublishTime,
				},
			},
		)

		freshTime := server.NowUtc()
		partialId := server.NewId()
		Testing_CreateNetworkExtender(
			ctx,
			&NetworkExtender{
				ExtenderId: partialId,
				NetworkId:  networkId,
				ClientId:   clientId,
				PublicKey:  []byte("extender-public-key-partial-0002"),
				CreateTime: baseTime,
				DnsTld:     connect.DefaultExtenderDnsTld,
				Active:     true,
			},
			[]*NetworkExtenderAddress{
				{
					IpVersion:       4,
					Ip:              netip.MustParseAddr("192.0.2.201"),
					Carriers:        []string{connect.ExtenderCarrierTcp},
					ActivateTime:    baseTime,
					Active:          true,
					LastPublishTime: &freshTime,
				},
				{
					IpVersion:    6,
					Ip:           netip.MustParseAddr("2001:db8::201"),
					Carriers:     []string{connect.ExtenderCarrierTcp},
					ActivateTime: baseTime,
					Active:       true,
				},
			},
		)

		got := GetNetworkExtenderIdsForPublish(ctx, 2)
		if !slices.Equal(got, []server.Id{partialId, publishedId}) {
			t.Fatalf(
				"publish order = %v, want the partly unpublished extender %v first",
				got,
				partialId,
			)
		}
	})
}

// The bootstrap sample never contains the extender that asked for it and never
// contains an inactive one.
func TestGetRandomActiveNetworkExtendersExcludesTheCallerAndTheInactive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		self := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-sample-self1"), 4, "192.0.2.20"),
			sign,
		)
		other := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-sample-othr1"), 6, "2001:db8::20"),
			sign,
		)
		gone := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-sample-gone1"), 4, "192.0.2.21"),
			sign,
		)
		for range ExtenderTestingMaxConsecutiveProbeFailures {
			RecordNetworkExtenderProbeResult(
				ctx,
				gone.Extender.ExtenderId,
				4,
				false,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				testRevocationSigner(),
			)
		}

		sample := GetRandomActiveNetworkExtenders(ctx, 8, self.Extender.ExtenderId)
		connect.AssertEqual(t, len(sample), 1)
		connect.AssertEqual(t, sample[0].Extender.ExtenderId, other.Extender.ExtenderId)
		connect.AssertEqual(t, len(sample[0].Addresses), 1)
		connect.AssertEqual(t, sample[0].Addresses[0].Ip.String(), "2001:db8::20")

		// with nothing excluded both active extenders are candidates
		sample = GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})
		connect.AssertEqual(t, len(sample), 2)
	})
}

// A row another claim holds is skipped rather than waited for, and a stamped
// row is never claimed again.
func TestClaimUnpublishedExtenderPublishesSkipsHeldAndStampedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		extenderIds := []server.Id{}
		for i := range 4 {
			activated := ActivateNetworkExtender(
				ctx,
				testExtenderActivation(
					[]byte(fmt.Sprintf("extender-public-key-claim-%06d", i)),
					4,
					fmt.Sprintf("192.0.2.%d", 30+i),
				),
				sign,
			)
			extenderIds = append(extenderIds, activated.Extender.ExtenderId)
		}

		all := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(all), 4)

		heldIds := []server.Id{all[0].PublishId, all[1].PublishId}
		Testing_HoldExtenderPublishLock(ctx, heldIds, func() {
			claimed := ClaimUnpublishedExtenderPublishes(ctx, 10)
			claimedIds := []server.Id{}
			for _, publish := range claimed {
				claimedIds = append(claimedIds, publish.PublishId)
			}
			for _, heldId := range heldIds {
				if slices.Contains(claimedIds, heldId) {
					t.Fatalf("a held row %s was claimed as well", heldId)
				}
			}
			if !slices.Equal(claimedIds, []server.Id{all[2].PublishId, all[3].PublishId}) {
				t.Fatalf("claimed %v, want the two rows that were not held", claimedIds)
			}
		})

		// nothing was consumed by the held claim, and the queue is intact
		claimed := ClaimUnpublishedExtenderPublishes(ctx, 10)
		connect.AssertEqual(t, len(claimed), 4)

		MarkExtenderPublishPublished(ctx, all[0].PublishId)
		MarkExtenderPublishPublished(ctx, all[1].PublishId)

		claimed = ClaimUnpublishedExtenderPublishes(ctx, 10)
		connect.AssertEqual(t, len(claimed), 2)
		connect.AssertEqual(t, claimed[0].PublishId, all[2].PublishId)
		connect.AssertEqual(t, claimed[1].PublishId, all[3].PublishId)

		stamped := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, stamped[0].PublishedTime != nil, true)
		connect.AssertEqual(t, stamped[2].PublishedTime == nil, true)
		// a claimed message must arrive intact; the gossip service publishes
		// exactly these bytes
		connect.AssertEqual(t, string(claimed[0].Message), "record:[192.0.2.32]")
	})
}

func TestCountActiveNetworkExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		connect.AssertEqual(t, CountActiveNetworkExtenders(ctx), 0)

		Testing_CreateNetworkExtenderPopulation(ctx, server.NewId(), server.NewId(), 25)
		connect.AssertEqual(t, CountActiveNetworkExtenders(ctx), 25)

		// a dual-stack extender is one extender, not two
		sign, _ := testRecordSigner()
		publicKey := []byte("extender-public-key-countpair-01")
		ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.40"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 6, "2001:db8::40"), sign)
		connect.AssertEqual(t, CountActiveNetworkExtenders(ctx), 26)
	})
}
