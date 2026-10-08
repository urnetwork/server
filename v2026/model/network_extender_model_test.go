// Pins the extender directory schema, address lifecycle, and publication writes.
package model

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
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

// The directory, activation history, and latency attestations exist on a fresh
// database with their read and upsert indexes. Identity and attestation unique
// keys keep reactivation and replay from creating duplicate rows.
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
			"network_extender_activation",
			"network_extender_address",
			"network_extender_latency",
			"network_extender_publish",
		}
		if !slices.Equal(tables, wantTables) {
			t.Fatalf("extender tables = %v, want %v", tables, wantTables)
		}
		for _, wantIndex := range []string{
			"network_extender_pkey",
			"network_extender_public_key_key",
			"network_extender_activation_pkey",
			"network_extender_activation_extender_id_activate_time",
			"network_extender_address_pkey",
			"network_extender_address_active_last_publish_time",
			"network_extender_latency_pkey",
			"network_extender_latency_extender_id_client_id_probe_nonce_key",
			"network_extender_latency_create_time",
			"network_extender_latency_extender_id_create_time",
			"network_extender_publish_pkey",
			"network_extender_publish_published_time_create_time",
		} {
			if !slices.Contains(indexes, wantIndex) {
				t.Fatalf("index %s is missing: %v", wantIndex, indexes)
			}
		}

		// the dns ports of an address (L2). The default is what every address
		// activated before the column has, and it is not nullable, so a reader
		// never has to tell an empty list from a missing one.
		isNullable := ""
		columnDefault := ""
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT is_nullable, coalesce(column_default, '')
				FROM information_schema.columns
				WHERE table_schema = 'public' AND
					table_name = 'network_extender_address' AND
					column_name = 'dns_ports'
				`,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&isNullable, &columnDefault))
				}
			})
		})
		connect.AssertEqual(t, isNullable, "NO")
		if !strings.HasPrefix(columnDefault, "''") {
			t.Fatalf("dns_ports default = %q, want the empty list", columnDefault)
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

// The dns ports of an address are stored ascending however the caller ordered
// them, because ascending is the order a client dials them in (L2) and the
// order the record lists them in. A value that is not a port is dropped on the
// way out rather than raised: the column is the operator's own data, and a
// record is better short one port than not signed at all.
func TestExtenderDnsPortsRoundTrip(t *testing.T) {
	cases := []struct {
		dnsPorts []int
		stored   string
		read     []int
	}{
		{dnsPorts: []int{53, 4053}, stored: "53,4053", read: []int{53, 4053}},
		{dnsPorts: []int{4053, 53}, stored: "53,4053", read: []int{53, 4053}},
		{dnsPorts: []int{4053, 53, 4053}, stored: "53,4053", read: []int{53, 4053}},
		{dnsPorts: []int{0, -1, 65536}, stored: "", read: []int{}},
		{dnsPorts: nil, stored: "", read: []int{}},
	}
	for _, c := range cases {
		stored := joinExtenderDnsPorts(c.dnsPorts)
		if stored != c.stored {
			t.Errorf("joinExtenderDnsPorts(%v) = %q, want %q", c.dnsPorts, stored, c.stored)
		}
		read := splitExtenderDnsPorts(stored)
		if !slices.Equal(read, c.read) {
			t.Errorf("splitExtenderDnsPorts(%q) = %v, want %v", stored, read, c.read)
		}
	}
	// a column no writer of ours produced, which a read must survive
	for _, c := range []struct {
		stored string
		read   []int
	}{
		{stored: "", read: []int{}},
		{stored: " 4053 , 53 ", read: []int{53, 4053}},
		{stored: "53,,x,70000,4053", read: []int{53, 4053}},
	} {
		read := splitExtenderDnsPorts(c.stored)
		if !slices.Equal(read, c.read) {
			t.Errorf("splitExtenderDnsPorts(%q) = %v, want %v", c.stored, read, c.read)
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

// The dns ports an activation reports are stored on the address it activated,
// ascending, and a re-activation replaces them rather than accumulating: the
// list is what the operator's probe found on THIS pass, so a port that stopped
// answering must leave the record on the next one.
func TestActivateNetworkExtenderStoresTheDnsPorts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-dnsports-001")

		sign, _ := testRecordSigner()
		activation := testExtenderActivation(publicKey, 4, "192.0.2.14")
		activation.Carriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns}
		activation.DnsPorts = []int{4053, 53}
		activated := ActivateNetworkExtender(ctx, activation, sign)
		if activated == nil {
			t.Fatal("the activation stored nothing")
		}
		// the addresses the record was signed over carry them too, since both
		// come out of the one transaction
		if !slices.Equal(activated.Addresses[0].DnsPorts, []int{53, 4053}) {
			t.Fatalf("signed dns ports = %v, want [53 4053]", activated.Addresses[0].DnsPorts)
		}

		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		if !slices.Equal(stored.Addresses[0].DnsPorts, []int{53, 4053}) {
			t.Fatalf("stored dns ports = %v, want [53 4053]", stored.Addresses[0].DnsPorts)
		}

		// 53 stopped answering, so it leaves the address
		activation = testExtenderActivation(publicKey, 4, "192.0.2.14")
		activation.Carriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns}
		activation.DnsPorts = []int{4053}
		ActivateNetworkExtender(ctx, activation, sign)
		stored = Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		if !slices.Equal(stored.Addresses[0].DnsPorts, []int{4053}) {
			t.Fatalf("stored dns ports = %v, want [4053]", stored.Addresses[0].DnsPorts)
		}

		// and an activation with no dns carrier leaves none, which is what the
		// sample reader and the drip then sign
		activation = testExtenderActivation(publicKey, 4, "192.0.2.14")
		ActivateNetworkExtender(ctx, activation, sign)
		stored = Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, len(stored.Addresses[0].DnsPorts), 0)
		sampled := GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})
		connect.AssertEqual(t, len(sampled), 1)
		connect.AssertEqual(t, len(sampled[0].Addresses[0].DnsPorts), 0)
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
		got := GetNetworkExtenderIdsForPublish(ctx, 4, testPublishStaleBefore())
		if !slices.Equal(got, want) {
			t.Fatalf("publish order = %v, want %v", got, want)
		}

		// publishing the head moves it to the back
		sign, _ := testRecordSigner()
		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, extenderIds[3], sign), true)
		got = GetNetworkExtenderIdsForPublish(ctx, 4, testPublishStaleBefore())
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

// The stale cut of the drip at a 12 hour rotation (GEOMAP §2.8), which is the
// work package's ExtenderPublishRotationTimeout.
func testPublishStaleBefore() time.Time {
	return server.NowUtc().Add(-12 * time.Hour)
}

// An active extender with one active v4 address stamped at lastPublishTime
// (nil for never drip-published) and a newest record issued at
// recordIssueTime (nil for never signed).
func testCreatePublishExtender(
	ctx context.Context,
	index int,
	active bool,
	lastPublishTime *time.Time,
	recordIssueTime *time.Time,
) server.Id {
	extenderId := server.NewId()
	createTime := server.NowUtc().Add(-48 * time.Hour)
	Testing_CreateNetworkExtender(
		ctx,
		&NetworkExtender{
			ExtenderId:      extenderId,
			NetworkId:       server.NewId(),
			ClientId:        server.NewId(),
			PublicKey:       []byte(fmt.Sprintf("extender-public-key-stale-%06d", index)),
			CreateTime:      createTime,
			TcpPort:         443,
			UdpPort:         443,
			DnsPort:         53,
			DnsTld:          connect.DefaultExtenderDnsTld,
			CountryCode:     "US",
			Active:          active,
			RecordIssueTime: recordIssueTime,
		},
		[]*NetworkExtenderAddress{
			{
				IpVersion:       4,
				Ip:              netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 150+index)),
				Carriers:        []string{connect.ExtenderCarrierTcp},
				ActivateTime:    createTime,
				Active:          active,
				LastPublishTime: lastPublishTime,
			},
		},
	)
	return extenderId
}

// Every active extender whose newest record is older than the stale cut is
// released whatever the batch (GEOMAP §2.8, D19), beside the oldest-first
// batch, each once and in batch order. The newest record is what clients
// hold, so an extender re-activated an hour ago is fresh even when its drip
// stamp is old -- it still takes its turn in the batch -- and one never
// signed for is always stale.
func TestGetNetworkExtenderIdsForPublishReleasesStaleExtendersBeyondTheBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		hoursAgo := func(hours int) *time.Time {
			return timePtr(now.Add(-time.Duration(hours) * time.Hour))
		}

		// drip published two hours ago: fresh, newest stamp
		fresh := testCreatePublishExtender(ctx, 1, true, hoursAgo(2), hoursAgo(2))
		// drip published thirteen hours ago and not since: stale
		stale := testCreatePublishExtender(ctx, 2, true, hoursAgo(13), hoursAgo(13))
		// drip stamp fourteen hours old, but re-activated an hour ago
		reactivated := testCreatePublishExtender(ctx, 3, true, hoursAgo(14), hoursAgo(1))
		// activated half an hour ago and never drip published
		activated := testCreatePublishExtender(ctx, 4, true, nil, timePtr(now.Add(-30*time.Minute)))
		// stale, but no longer active: never selected
		testCreatePublishExtender(ctx, 5, false, hoursAgo(30), hoursAgo(30))

		staleBefore := now.Add(-12 * time.Hour)
		for _, test := range []struct {
			limit int
			want  []server.Id
		}{
			// the stale extender goes even when the batch has no room for it
			{limit: 0, want: []server.Id{stale}},
			{limit: 1, want: []server.Id{activated, stale}},
			// the batch takes the drip order, and the stale one appears once
			{limit: 2, want: []server.Id{activated, reactivated, stale}},
			{limit: 3, want: []server.Id{activated, reactivated, stale}},
			{limit: 4, want: []server.Id{activated, reactivated, stale, fresh}},
			{limit: 8, want: []server.Id{activated, reactivated, stale, fresh}},
		} {
			got := GetNetworkExtenderIdsForPublish(ctx, test.limit, staleBefore)
			if !slices.Equal(got, test.want) {
				t.Fatalf("limit %d: selected %v, want %v", test.limit, got, test.want)
			}
		}

		// with a cut older than every record, only the batch goes
		got := GetNetworkExtenderIdsForPublish(ctx, 1, now.Add(-20*time.Hour))
		if !slices.Equal(got, []server.Id{activated}) {
			t.Fatalf("with nothing stale selected %v, want the batch of one", got)
		}

		// an extender never signed for has no newest record, and is stale
		unsigned := testCreatePublishExtender(ctx, 6, true, hoursAgo(1), nil)
		got = GetNetworkExtenderIdsForPublish(ctx, 0, staleBefore)
		if !slices.Equal(got, []server.Id{stale, unsigned}) {
			t.Fatalf("with an unsigned extender selected %v, want %v", got, []server.Id{stale, unsigned})
		}
	})
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

		got := GetNetworkExtenderIdsForPublish(ctx, 2, testPublishStaleBefore())
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

// The geo dns reader sees one row per active address of an active extender,
// carrying the country that decides which continent set it belongs in (C5).
// An extender that was revoked, and a family that ran out of probe attempts,
// are published nowhere -- a dns set is the one place a dead address costs a
// client a connection attempt with no fallback.
func TestGetActiveNetworkExtenderDnsAddressesExcludesTheInactive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		connect.AssertEqual(t, len(GetActiveNetworkExtenderDnsAddresses(ctx)), 0)

		create := func(index int, countryCode string, active bool, addresses ...*NetworkExtenderAddress) {
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  server.NewId(),
					NetworkId:   server.NewId(),
					ClientId:    server.NewId(),
					PublicKey:   []byte(fmt.Sprintf("extender-public-key-dnsread-%03d", index)),
					CreateTime:  server.NowUtc(),
					TcpPort:     443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      connect.DefaultExtenderDnsTld,
					CountryCode: countryCode,
					Active:      active,
				},
				addresses,
			)
		}
		address := func(ipVersion int, ip string, active bool) *NetworkExtenderAddress {
			return &NetworkExtenderAddress{
				IpVersion:    ipVersion,
				Ip:           netip.MustParseAddr(ip),
				Carriers:     []string{connect.ExtenderCarrierTcp},
				ActivateTime: server.NowUtc(),
				Active:       active,
			}
		}

		create(1, "de", true, address(4, "198.51.100.1", true), address(6, "2001:db8:2::1", true))
		// one family lost its attempts, the other still answers
		create(2, "jp", true, address(4, "198.51.100.2", false), address(6, "2001:db8:2::2", true))
		// revoked, so neither family is published
		create(3, "fr", false, address(4, "198.51.100.3", true))

		addresses := GetActiveNetworkExtenderDnsAddresses(ctx)
		connect.AssertEqual(t, len(addresses), 3)
		countryIps := map[string][]string{}
		for _, address := range addresses {
			countryIps[address.CountryCode] = append(countryIps[address.CountryCode], address.Ip.String())
		}
		connect.AssertEqual(t, countryIps, map[string][]string{
			"de": {"198.51.100.1", "2001:db8:2::1"},
			"jp": {"2001:db8:2::2"},
		})
	})
}

// The drip publishes nothing for an extender that went away between being
// selected and being signed, which is the expected race with a probe tick
// rather than a fault. A record signed here would promise an address the
// directory has already been told is gone, and a stamp without a record would
// skip the extender for a whole rotation.
func TestPublishNetworkExtenderRecordSkipsAnExtenderThatWentAway(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, signed := testRecordSigner()

		revoked := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-gone-0000001"), 4, "192.0.2.50"),
			sign,
		)
		for range ExtenderTestingMaxConsecutiveProbeFailures {
			RecordNetworkExtenderProbeResult(
				ctx,
				revoked.Extender.ExtenderId,
				4,
				false,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				testRevocationSigner(),
			)
		}

		// still marked active but with no address left, which is what a probe
		// worker that has just spent the last of its budget leaves behind
		addressless := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-gone-0000002"), 4, "192.0.2.51"),
			sign,
		)
		Testing_DeactivateNetworkExtenderAddress(ctx, addressless.Extender.ExtenderId, 4)

		signedCount := len(signed())
		publishCount := len(Testing_GetNetworkExtenderPublishes(ctx))
		for _, extenderId := range []server.Id{
			revoked.Extender.ExtenderId,
			addressless.Extender.ExtenderId,
			// an extender that never existed
			server.NewId(),
		} {
			if PublishNetworkExtenderRecord(ctx, extenderId, sign) {
				t.Fatalf("extender %s was published", extenderId)
			}
		}
		connect.AssertEqual(t, len(signed()), signedCount)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount)
		// and neither is ever selected for a batch in the first place
		connect.AssertEqual(t, len(GetNetworkExtenderIdsForPublish(ctx, 8, testPublishStaleBefore())), 0)

		// the stamps are untouched, so a later activation is still the oldest
		stored := Testing_GetNetworkExtender(ctx, addressless.Extender.ExtenderId)
		connect.AssertEqual(t, stored.Addresses[0].LastPublishTime == nil, true)
	})
}

// A probe result for an address that is gone, or for an extender that never
// existed, changes nothing and signs nothing. The task probes from a snapshot
// of the directory, so a result can always arrive for a row another worker has
// already removed.
func TestRecordNetworkExtenderProbeResultIgnoresAnUnknownAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		activated := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-unknown-0001"), 4, "192.0.2.52"),
			sign,
		)

		signCount := 0
		signRevocation := func(extender *NetworkExtender, issueTime time.Time) ([]byte, error) {
			signCount += 1
			return []byte("revocation"), nil
		}
		cases := []struct {
			name       string
			extenderId server.Id
			ipVersion  int
			success    bool
		}{
			{name: "a family that was never activated", extenderId: activated.Extender.ExtenderId, ipVersion: 6},
			{name: "a family that was never activated, success", extenderId: activated.Extender.ExtenderId, ipVersion: 6, success: true},
			{name: "an extender that never existed", extenderId: server.NewId(), ipVersion: 4},
		}
		for _, c := range cases {
			outcome := RecordNetworkExtenderProbeResult(
				ctx,
				c.extenderId,
				c.ipVersion,
				c.success,
				server.NowUtc(),
				ExtenderTestingMaxConsecutiveProbeFailures,
				signRevocation,
			)
			if outcome.AddressDeactivated || outcome.ExtenderRevoked {
				t.Errorf("%s: outcome = %+v, want nothing", c.name, outcome)
			}
		}
		connect.AssertEqual(t, signCount, 0)

		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, len(stored.Addresses), 1)
		connect.AssertEqual(t, stored.Addresses[0].Active, true)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeTime == nil, true)
		// only the activation record
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), 1)
	})
}

// A redelivery after a crash between the publish and the stamp must not move
// the stamp forward. The row is already delivered, and a second stamp would
// record the retry rather than the delivery.
func TestMarkExtenderPublishPublishedLeavesAStampedRowAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-stamp-00001"), 4, "192.0.2.53"),
			sign,
		)
		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 1)
		publishId := publishes[0].PublishId

		MarkExtenderPublishPublished(ctx, publishId)
		first := Testing_GetNetworkExtenderPublishes(ctx)[0].PublishedTime
		if first == nil {
			t.Fatal("the row was not stamped")
		}

		MarkExtenderPublishPublished(ctx, publishId)
		second := Testing_GetNetworkExtenderPublishes(ctx)[0].PublishedTime
		if second == nil || !second.Equal(*first) {
			t.Fatalf("the stamp moved from %s to %v", first, second)
		}
		// and a stamped row never comes back to a claim
		connect.AssertEqual(t, len(ClaimUnpublishedExtenderPublishes(ctx, 10)), 0)
	})
}

// Re-activating one identity key carries the new ports, tld, country and owner
// onto the same extender row. An operator that moved its extender to another
// client or another port must not leave the directory advertising the old one,
// and the record signed by the re-activation is what the directory keeps (B5).
func TestActivateNetworkExtenderUpdatesThePortsAndOwnerOfOneKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-moved-00001")
		sign, _ := testRecordSigner()

		first := testExtenderActivation(publicKey, 4, "192.0.2.54")
		activated := ActivateNetworkExtender(ctx, first, sign)

		second := testExtenderActivation(publicKey, 4, "192.0.2.55")
		second.TcpPort = 8443
		second.UdpPort = 8444
		second.DnsPort = 8453
		second.DnsTld = "moved.example."
		second.CountryCode = "DE"
		second.Carriers = []string{connect.ExtenderCarrierTcp}
		reactivated := ActivateNetworkExtender(ctx, second, sign)

		connect.AssertEqual(t, reactivated.Extender.ExtenderId, activated.Extender.ExtenderId)

		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, stored.Extender.NetworkId, second.NetworkId)
		connect.AssertEqual(t, stored.Extender.ClientId, second.ClientId)
		connect.AssertEqual(t, stored.Extender.TcpPort, 8443)
		connect.AssertEqual(t, stored.Extender.UdpPort, 8444)
		connect.AssertEqual(t, stored.Extender.DnsPort, 8453)
		connect.AssertEqual(t, stored.Extender.DnsTld, "moved.example.")
		connect.AssertEqual(t, stored.Extender.CountryCode, "DE")
		// the create time is the first activation's; only an update happened
		connect.AssertEqual(t, stored.Extender.CreateTime.Equal(activated.Extender.CreateTime), true)
		// one row per family, so the family's address moved rather than doubling
		connect.AssertEqual(t, len(stored.Addresses), 1)
		connect.AssertEqual(t, stored.Addresses[0].Ip.String(), "192.0.2.55")
		connect.AssertEqual(t, slices.Equal(stored.Addresses[0].Carriers, []string{"tcp"}), true)

		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 2)
		connect.AssertEqual(t, string(publishes[1].Message), "record:[192.0.2.55]")
	})
}

// The probe task reads never-probed addresses first and then the oldest probe,
// so a run that is cut short makes progress through the set rather than
// re-probing the same head forever. The target also carries everything one dial
// needs, so the task never goes back to the directory per address.
func TestGetActiveNetworkExtenderProbeTargetsTakeTheOldestProbeFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		baseTime := server.NowUtc().Add(-24 * time.Hour)

		type probeOrderCase struct {
			lastProbeAt *time.Time
			ip          string
		}
		// deliberately created newest-probe first, so the order under test
		// cannot be the insertion order
		cases := []probeOrderCase{
			{lastProbeAt: timePtr(baseTime.Add(2 * time.Hour)), ip: "192.0.2.60"},
			{lastProbeAt: timePtr(baseTime.Add(1 * time.Hour)), ip: "192.0.2.61"},
			{lastProbeAt: nil, ip: "192.0.2.62"},
			{lastProbeAt: timePtr(baseTime), ip: "192.0.2.63"},
		}
		for i, c := range cases {
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  server.NewId(),
					NetworkId:   networkId,
					ClientId:    clientId,
					PublicKey:   []byte(fmt.Sprintf("extender-public-key-probeord-%02d", i)),
					CreateTime:  baseTime,
					TcpPort:     8443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      "probe.example.",
					CountryCode: "US",
					Active:      true,
				},
				[]*NetworkExtenderAddress{
					{
						IpVersion:     4,
						Ip:            netip.MustParseAddr(c.ip),
						Carriers:      []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns},
						ActivateTime:  baseTime,
						Active:        true,
						LastProbeTime: c.lastProbeAt,
					},
				},
			)
		}
		// an inactive address of an active extender, and every address of a
		// revoked one, are out of the set entirely
		Testing_CreateNetworkExtender(
			ctx,
			&NetworkExtender{
				ExtenderId: server.NewId(),
				NetworkId:  networkId,
				ClientId:   clientId,
				PublicKey:  []byte("extender-public-key-probeord-90"),
				CreateTime: baseTime,
				DnsTld:     connect.DefaultExtenderDnsTld,
				Active:     true,
			},
			[]*NetworkExtenderAddress{
				{
					IpVersion:    4,
					Ip:           netip.MustParseAddr("192.0.2.64"),
					Carriers:     []string{connect.ExtenderCarrierTcp},
					ActivateTime: baseTime,
					Active:       false,
				},
			},
		)
		Testing_CreateNetworkExtender(
			ctx,
			&NetworkExtender{
				ExtenderId: server.NewId(),
				NetworkId:  networkId,
				ClientId:   clientId,
				PublicKey:  []byte("extender-public-key-probeord-91"),
				CreateTime: baseTime,
				DnsTld:     connect.DefaultExtenderDnsTld,
				Active:     false,
			},
			[]*NetworkExtenderAddress{
				{
					IpVersion:    4,
					Ip:           netip.MustParseAddr("192.0.2.65"),
					Carriers:     []string{connect.ExtenderCarrierTcp},
					ActivateTime: baseTime,
					Active:       true,
				},
			},
		)

		targets := GetActiveNetworkExtenderProbeTargets(ctx)
		ips := []string{}
		for _, target := range targets {
			ips = append(ips, target.Ip.String())
		}
		want := []string{"192.0.2.62", "192.0.2.63", "192.0.2.61", "192.0.2.60"}
		if !slices.Equal(ips, want) {
			t.Fatalf("probe order = %v, want %v", ips, want)
		}

		// every field one dial needs travels with the target
		head := targets[0]
		connect.AssertEqual(t, head.TcpPort, 8443)
		connect.AssertEqual(t, head.DnsTld, "probe.example.")
		connect.AssertEqual(t, head.IpVersion, 4)
		connect.AssertEqual(t, slices.Equal(head.Carriers, []string{"tcp", "dns"}), true)
		connect.AssertEqual(t, 0 < len(head.PublicKey), true)
	})
}
