// Contract endpoint rechecks share one fresh read without weakening lifecycle
// rejection, source-error precedence, or the transaction's final active locks.
package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Reads the existing collector without opening or acquiring a connection.
// These fixtures run serially inside their own database environment.
func contractPairAcquireCount(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_pg_pool_acquires_total" {
			continue
		}
		for _, metric := range family.Metric {
			pool, outcome := "", ""
			for _, label := range metric.Label {
				switch label.GetName() {
				case "pool":
					pool = label.GetValue()
				case "outcome":
					outcome = label.GetValue()
				}
			}
			if pool == "default" && outcome == "acquired" {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatal("default pool acquire counter unavailable after fixture setup")
	return 0
}

// Creates synthetic independent endpoint identities, with no connection rows.
func contractPairTestClients(ctx context.Context) (server.Id, server.Id, server.Id) {
	networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("contract-pair-%s", networkId), server.NewId())
	for _, clientId := range []server.Id{sourceId, destinationId} {
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	}
	return networkId, sourceId, destinationId
}

// An already inactive destination must not take two turns through a busy pool.
// The error exits before any write or optional post-commit work can affect the
// acquisition counter; no timing or scheduler assumption proves the reduction.
func TestNewContractChecksInactiveDestinationWithOneAcquisition(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		_, sourceId, destinationId := contractPairTestClients(ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, destinationId))
		})
		before := contractPairAcquireCount(t)
		contractId, _, _, _, err := newContract(ctx, sourceId, destinationId, nil, false, true, 1024,
			model.ProvideModeNetwork, false, 0, connect.DefaultContractManagerSettings())
		after := contractPairAcquireCount(t)
		if !errors.Is(err, errContractDestinationInactive) || contractResultError(err) != protocol.ContractError_Reliability || contractId != (server.Id{}) {
			t.Fatalf("inactive destination changed rejection: contract=%t err=%v", contractId != (server.Id{}), err)
		}
		if delta := after - before; delta != 1 {
			t.Fatalf("write-boundary endpoint recheck acquired %.0f connections, want exactly 1", delta)
		}
	})
}

// Source invalidity remains an account error, even when both endpoints are
// unusable. A destination-only failure remains a selected-route rejection.
func TestNewContractPairReadPreservesRejectionPrecedence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		_, sourceId, destinationId := contractPairTestClients(ctx)
		for _, test := range []struct {
			name              string
			sourceId          server.Id
			destinationId     server.Id
			sourceActive      bool
			destinationActive bool
			want              error
			wantResult        protocol.ContractError
		}{
			{name: "inactive source", sourceId: sourceId, destinationId: destinationId, destinationActive: true, want: model.ErrActiveClientNotFound, wantResult: protocol.ContractError_NoPermission},
			{name: "both inactive", sourceId: sourceId, destinationId: destinationId, want: model.ErrActiveClientNotFound, wantResult: protocol.ContractError_NoPermission},
			{name: "missing source and inactive destination", sourceId: server.NewId(), destinationId: destinationId, want: model.ErrActiveClientNotFound, wantResult: protocol.ContractError_NoPermission},
			{name: "missing destination", sourceId: sourceId, destinationId: server.NewId(), sourceActive: true, want: errContractDestinationInactive, wantResult: protocol.ContractError_Reliability},
			{name: "both missing", sourceId: server.NewId(), destinationId: server.NewId(), want: model.ErrActiveClientNotFound, wantResult: protocol.ContractError_NoPermission},
			{name: "same inactive endpoint", sourceId: sourceId, destinationId: sourceId, want: model.ErrActiveClientNotFound, wantResult: protocol.ContractError_NoPermission},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = CASE client_id WHEN $1 THEN $3::boolean ELSE $4::boolean END WHERE client_id IN ($1,$2)`, sourceId, destinationId, test.sourceActive, test.destinationActive))
			})
			before := contractPairAcquireCount(t)
			contractId, _, _, _, err := newContract(ctx, test.sourceId, test.destinationId, nil, false, true, 1024, model.ProvideModeNetwork, false, 0, connect.DefaultContractManagerSettings())
			if !errors.Is(err, test.want) || contractResultError(err) != test.wantResult || contractId != (server.Id{}) {
				t.Fatalf("%s: changed rejection precedence: %v", test.name, err)
			}
			if got := contractPairAcquireCount(t) - before; got != 1 {
				t.Fatalf("%s: endpoint rejection acquired %.0f connections, want 1", test.name, got)
			}
		}
	})
}

// Keep successful same-client and two-client paths to one preflight acquire
// plus the unchanged write transaction. Warm only the unrelated usage stamp.
func TestNewContractPairReadCreatesActivePairs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, sourceId, destinationId := contractPairTestClients(ctx)
		model.StampTopLevelClientContractTime(ctx, sourceId)
		for _, targetId := range []server.Id{destinationId, sourceId} {
			before := contractPairAcquireCount(t)
			contractId, bytes, priority, streamId, err := newContract(ctx, sourceId, targetId, nil, false, true, 1024, model.ProvideModeNetwork, false, 0, connect.DefaultContractManagerSettings())
			if err != nil || contractId == (server.Id{}) || bytes != max(MinContractTransferByteCount, 1024) || priority != model.TrustedPriority || streamId != nil {
				t.Fatalf("active pair changed contract result: %v", err)
			}
			if got := contractPairAcquireCount(t) - before; got != 2 {
				t.Fatalf("active pair acquired %.0f connections, want one preflight and one transaction", got)
			}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT source_network_id, destination_network_id, source_id, destination_id FROM transfer_contract WHERE contract_id = $1`, contractId)
				server.WithPgResult(result, err, func() {
					if !result.Next() {
						t.Fatal("created contract missing")
					}
					var sourceNetworkId, destinationNetworkId, storedSourceId, storedDestinationId server.Id
					server.Raise(result.Scan(&sourceNetworkId, &destinationNetworkId, &storedSourceId, &storedDestinationId))
					if sourceNetworkId != networkId || destinationNetworkId != networkId || storedSourceId != sourceId || storedDestinationId != targetId {
						t.Fatal("pair lookup changed persisted endpoint mappings")
					}
				})
			})
		}
	})
}

// Earlier relationship eligibility cannot be reused after either endpoint is
// deactivated. Explicit committed mutations force the stale-read boundary.
func TestNewContractPairReadRechecksAfterRelationship(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		_, sourceId, destinationId := contractPairTestClients(ctx)
		for _, test := range []struct {
			id   server.Id
			want error
		}{
			{id: sourceId, want: model.ErrActiveClientNotFound},
			{id: destinationId, want: errContractDestinationInactive},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = true WHERE client_id IN ($1,$2)`, sourceId, destinationId))
			})
			details := getProvideRelationshipDetails(ctx, sourceId, destinationId)
			if !contractDestinationActive(details.SourceLifecycle) || !contractDestinationActive(details.DestinationLifecycle) {
				t.Fatal("fixture relationship is not initially active")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, test.id))
			})
			contractId, _, _, _, err := newContract(ctx, sourceId, destinationId, nil, false, true, 1024, model.ProvideModeNetwork, false, 0, connect.DefaultContractManagerSettings())
			if !errors.Is(err, test.want) || contractId != (server.Id{}) {
				t.Fatalf("stale relationship bypassed fresh endpoint check: %v", err)
			}
		}
	})
}

// Endpoint batching does not absorb or omit intermediary validation before any
// balance reservation. The unfunded public path must reject the stale hop first.
func TestNewContractPairReadPreservesIntermediaryCheck(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, sourceId, destinationId := contractPairTestClients(ctx)
		intermediaryId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), intermediaryId, "", "")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, intermediaryId))
		})
		contractId, _, _, _, err := newContract(ctx, sourceId, destinationId, []server.Id{intermediaryId}, false, true, 1024, model.ProvideModePublic, false, 1, connect.DefaultContractManagerSettings())
		if !errors.Is(err, model.ErrActiveClientNotFound) || contractId != (server.Id{}) {
			t.Fatalf("inactive intermediary reached contract allocation: %v", err)
		}
	})
}
