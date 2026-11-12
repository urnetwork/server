// The SN25 extender amendment's platform endpoint registry on a real database
// (sn WHITEPAPER.md §8.6.2): registration narrowly, sidecar's three edges on the
// operator's own networks, never a measurement network, from the moment it is
// made; revocation ends it; and the amended reader classifies a contract by its
// create time, so a share is intra-platform exactly when the contract was created
// while the endpoint was registered.
package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestStPlatformEndpointRegistry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		operator, measurement, customer := server.NewId(), server.NewId(), server.NewId()
		edge, probe, provider := server.NewId(), server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, operator, server.NewId(), edge, "tlsedge", "tlsedge")
		Testing_CreateDevice(ctx, measurement, server.NewId(), probe, "probe", "probe")
		Testing_CreateDevice(ctx, customer, server.NewId(), provider, "provider", "provider")
		networks := StPlatformEndpointNetworks{OperatorNetworkIds: map[server.Id]bool{operator: true, measurement: true}, MeasurementNetworkIds: map[server.Id]bool{measurement: true}}

		if set := GetStPlatformEndpoints(ctx); set.Count() != 0 {
			t.Fatalf("a new registry holds %d registrations", set.Count())
		}
		for name, c := range map[string]struct {
			clientId server.Id
			kind     string
			by       string
		}{
			"another kind":          {edge, "egress", "operator@example"},
			"no one":                {edge, StPlatformEndpointTlsEdge, ""},
			"an unknown client":     {server.NewId(), StPlatformEndpointTlsEdge, "operator@example"},
			"a customer's client":   {provider, StPlatformEndpointSmtpEdge, "operator@example"},
			"a measurement network": {probe, StPlatformEndpointMxEdge, "operator@example"},
		} {
			if registered, err := RegisterStPlatformEndpoint(ctx, c.clientId, c.kind, c.by, networks); !errors.Is(err, errStPlatformEndpoint) || registered != nil {
				t.Fatalf("%s: %+v, %v", name, registered, err)
			}
		}
		if set := GetStPlatformEndpoints(ctx); set.Count() != 0 {
			t.Fatalf("a refused registration was kept: %+v", set)
		}

		before := server.NowUtc()
		registered, err := RegisterStPlatformEndpoint(ctx, edge, StPlatformEndpointTlsEdge, "operator@example", networks)
		if err != nil {
			t.Fatal(err)
		}
		if registered.NetworkId != operator || registered.Kind != StPlatformEndpointTlsEdge || registered.RegisterTime.Before(before) || registered.RevokeTime != nil {
			t.Fatalf("registered %+v", registered)
		}
		if _, err := RegisterStPlatformEndpoint(ctx, edge, StPlatformEndpointSmtpEdge, "operator@example", networks); !errors.Is(err, errStPlatformEndpoint) {
			t.Fatalf("a second registration in force: %v", err)
		}
		set := GetStPlatformEndpoints(ctx)
		if set.Count() != 1 || set.InForce(server.NowUtc()) != 1 || !set.holds(edge, server.NowUtc()) {
			t.Fatalf("registry %+v", set)
		}
		// never backdated: a contract created before the registration is not
		// intra-platform
		if set.holds(edge, registered.RegisterTime.Add(-time.Microsecond)) || !set.holds(edge, registered.RegisterTime) {
			t.Fatal("the registration does not start when it was made")
		}

		time.Sleep(2 * time.Millisecond)
		if err := RevokeStPlatformEndpoint(ctx, edge, ""); !errors.Is(err, errStPlatformEndpoint) {
			t.Fatalf("a revocation by no one: %v", err)
		}
		if err := RevokeStPlatformEndpoint(ctx, edge, "operator@example"); err != nil {
			t.Fatal(err)
		}
		if err := RevokeStPlatformEndpoint(ctx, edge, "operator@example"); !errors.Is(err, errStPlatformEndpoint) {
			t.Fatalf("a second revocation: %v", err)
		}
		set = GetStPlatformEndpoints(ctx)
		revoked := set[edge][0]
		if set.Count() != 1 || revoked.RevokeTime == nil || revoked.RevokedBy != "operator@example" || set.InForce(server.NowUtc()) != 0 {
			t.Fatalf("registry after the revocation %+v", set)
		}
		// contracts created while it was in force keep their class
		if !set.holds(edge, registered.RegisterTime) || set.holds(edge, *revoked.RevokeTime) {
			t.Fatal("the revocation moved the registration's window")
		}

		// registered again, it is a second registration beside the first
		time.Sleep(2 * time.Millisecond)
		if _, err := RegisterStPlatformEndpoint(ctx, edge, StPlatformEndpointMxEdge, "operator@example", networks); err != nil {
			t.Fatal(err)
		}
		if set := GetStPlatformEndpoints(ctx); set.Count() != 2 || set.InForce(server.NowUtc()) != 1 || set[edge][1].Kind != StPlatformEndpointMxEdge {
			t.Fatalf("registry %+v", set)
		}
	})
}

// The amended reader reads each contract's create time and classifies a
// platform endpoint's share by it: a contract created while the endpoint was
// registered is intra-platform, one created before the registration or after
// its revocation is not, and its share stays provider usage.
func TestStExtenderDbAmendedUsageClassifiesPlatformEndpointsByCreateTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 11, 27, 18, 25, 0, 0, time.UTC)
		end := start.Add(time.Hour)
		id := contractPayoutTestId
		origin, edge := ContractParticipant{ClientId: id(1), NetworkId: id(101)}, ContractParticipant{ClientId: id(2), NetworkId: id(102)}
		registeredAt, revokedAt := start.Add(10*time.Minute), start.Add(20*time.Minute)
		set := StPlatformEndpointSet{edge.ClientId: {{ClientId: edge.ClientId, NetworkId: edge.NetworkId, Kind: StPlatformEndpointTlsEdge, RegisterTime: registeredAt, RegisteredBy: "test", RevokeTime: &revokedAt, RevokedBy: "test"}}}
		created := map[string]time.Time{"before": start.Add(5 * time.Minute), "during": start.Add(15 * time.Minute), "after": start.Add(25 * time.Minute)}
		bytes := map[string]ByteCount{"before": 100, "during": 1_000, "after": 10_000}
		for name, createTime := range created {
			snapshot := stAmendedDbSnapshot(t, bytes[name], edge)
			contractId := addStAmendedDbContract(t, ctx, stAmendedDbContract{closeTime: start.Add(30 * time.Minute), sourceId: origin.ClientId, sourceNetworkId: origin.NetworkId,
				destinationId: edge.ClientId, destinationNetworkId: edge.NetworkId, originIsSource: stAmendedDbBool(true), payerNetworkId: &origin.NetworkId, snapshot: snapshot})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time = $2 WHERE contract_id = $1`, contractId, createTime))
			})
		}
		usages, pool, err := GetStEpochAmendedUsageAtEpoch(ctx, 8, start, end, StExtenderAmendment{FirstEpoch: 8, PlatformEndpoints: set}, map[server.Id]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if got := stAmendedDbUsages(usages); len(got) != 1 || got[edge.ClientId] != 100+10_000 {
			t.Fatalf("usages %v, want the edge's shares of the contracts created outside its registration", got)
		}
		if pool.IntraPlatform != 1_000 || pool.IntraNetwork != 0 || pool.Contracts != 1 || pool.Payers != 1 || pool.ClientIdPooledBytes[edge.ClientId] != 1_000 {
			t.Fatalf("pool %+v", pool)
		}
		// with no registration, nothing is intra-platform
		usages, pool, err = GetStEpochAmendedUsageAtEpoch(ctx, 8, start, end, StExtenderAmendment{FirstEpoch: 8, PlatformEndpoints: StPlatformEndpointSet{}}, map[server.Id]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if got := stAmendedDbUsages(usages); got[edge.ClientId] != 11_100 || pool.Bytes() != 0 {
			t.Fatalf("usages %v, pool %+v", got, pool)
		}
	})
}
