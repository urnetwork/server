// Stream retries retain the first admitted provider identity while actual
// paid, free and companion settlement consumes the same completed bytes.
package model

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The membership change precedes a real retry and both terminal reports.
// It changes no completed snapshot, verdict or expected settlement amount.
func exerciseContractParticipantIdentityRetry(t testing.TB, paid, companion, removed bool) {
	t.Helper()
	ctx := context.Background()
	start := server.NowUtc()
	originNetwork, providerNetwork, replacementNetwork := server.NewId(), server.NewId(), server.NewId()
	if !paid {
		providerNetwork = originNetwork
	}
	origin, first, egress := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: originNetwork, first: providerNetwork, egress: providerNetwork})
	var contractId server.Id
	if paid {
		addContractPayoutTestBalance(ctx, originNetwork, 242)
		contract, err := CreateTransferEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		contractId = contract.ContractId
	} else {
		var err error
		contractId, err = CreateContractNoEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
	}
	streamId := AddToStream(ctx, contractId, origin, egress, []server.Id{first})
	if err := SetContractStream(ctx, contractId, streamId, []server.Id{first}); err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if removed {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=$1`, first))
		} else {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, first, replacementNetwork))
		}
	})
	contracts := []server.Id{contractId}
	if companion {
		reply, err := CreateCompanionTransferEscrow(ctx, providerNetwork, egress, originNetwork, origin, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		joined, ok := AddCompanionContractToStream(ctx, reply.ContractId, contractId, egress, origin)
		if !ok || joined != streamId {
			t.Fatal("original companion stream was not retained")
		}
		if err := SetContractStream(ctx, reply.ContractId, joined, nil); err != nil {
			t.Fatalf("retained stream attachment failed: %v", err)
		}
		contracts = append(contracts, reply.ContractId)
	} else if err := SetContractStream(ctx, contractId, streamId, []server.Id{first, first}); err != nil {
		t.Fatalf("retained stream attachment failed: %v", err)
	}
	var retained server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM contract_participant WHERE stream_id=$1 AND client_id=$2`, streamId, first).Scan(&retained))
	})
	if retained != providerNetwork {
		t.Fatal("stream retry rewrote original participant identity")
	}
	for _, id := range contracts {
		if err := CloseContract(ctx, id, origin, 121, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, egress, 121, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, egress, 121, false); !errors.Is(err, errContractAlreadySettled) {
			t.Fatalf("duplicate completed report changed the original outcome: %v", err)
		}
		got := contractPayoutTestAmounts(t, ctx, id)
		if paid {
			if !maps.Equal(got, map[server.Id]contractPayoutTestAmount{providerNetwork: {byteCount: 121, payout: 121}}) {
				t.Fatalf("retained provider account allocation changed: %v", got)
			}
			assertContractPayoutTestProviderAllocations(t, ctx, id, map[server.Id]int64{first: 61, egress: 60})
		} else if len(got) != 0 {
			t.Fatal("free same-network work invented monetary eligibility")
		}
	}
	usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	gotUsage := map[server.Id]int64{}
	for _, usage := range usages {
		if usage.NetworkId != providerNetwork {
			t.Fatal("completed work inherited current provider membership")
		}
		gotUsage[usage.ClientId] = usage.PayoutByteCount
	}
	count := int64(len(contracts))
	if !maps.Equal(gotUsage, map[server.Id]int64{first: 61 * count, egress: 60 * count}) {
		t.Fatalf("paid/free/companion completed bytes changed: %v", gotUsage)
	}
	// A distinct stream gets its own current identity; protecting the old
	// stream does not turn its snapshot into a process-wide provider cache.
	fresh, err := CreateContractNoEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
	if err != nil {
		t.Fatal(err)
	}
	freshStream := server.NewId()
	if removed {
		if err := SetContractStream(ctx, fresh, freshStream, []server.Id{first}); err == nil || !strings.Contains(err.Error(), "intermediary clients do not exist") {
			t.Fatalf("new stream borrowed a removed participant from old history: %v", err)
		}
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{first: replacementNetwork})
	}
	if err := SetContractStream(ctx, fresh, freshStream, []server.Id{first}); err != nil {
		t.Fatal(err)
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM contract_participant WHERE stream_id=$1 AND client_id=$2`, freshStream, first).Scan(&retained))
	})
	if retained != replacementNetwork {
		t.Fatal("new stream borrowed another stream's provider identity")
	}
}

// A paid stream retry cannot redirect either billing or completed work.
func TestStContractUsagePaidStreamRetryKeepsOriginalIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityRetry(t, true, false, false) })
}

// Same-network free work keeps its original identity and earns no cash sweep.
func TestStContractUsageFreeStreamRetryKeepsOriginalIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityRetry(t, false, false, false) })
}

// A companion inherits the original stream snapshot before either close.
func TestStContractUsageCompanionJoinKeepsOriginalIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityRetry(t, true, true, false) })
}

// Conflicting retained roles cannot be resolved by whichever SQL row the
// collector happened to read first. Their actual close reports remain held;
// independently healthy contracts still settle normally.
func exerciseContractParticipantIdentityConflict(t testing.TB, extender bool) {
	t.Helper()
	ctx := context.Background()
	start := server.NowUtc()
	originNetwork, providerNetwork, foreignNetwork := server.NewId(), server.NewId(), server.NewId()
	origin, provider, intermediary, sibling := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3), contractPayoutTestId(4)
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: originNetwork, provider: providerNetwork, intermediary: providerNetwork, sibling: providerNetwork})
	id, err := CreateContractNoEscrow(ctx, originNetwork, origin, providerNetwork, provider, 120)
	if err != nil {
		t.Fatal(err)
	}
	streamId := server.NewId()
	if extender {
		if err := SetContractStream(ctx, id, streamId, []server.Id{intermediary}); err != nil {
			t.Fatal(err)
		}
		// Model contradictory retained evidence, not a current-directory read:
		// the extender already copied this same provider into another network.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id) VALUES($1,$2,'source',$3,$4)`, id, server.NewId(), intermediary, foreignNetwork))
		})
	} else {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, provider, foreignNetwork))
		})
		if err := SetContractStream(ctx, id, streamId, []server.Id{provider}); err != nil {
			t.Fatal(err)
		}
	}
	if err := CloseContract(ctx, id, origin, 120, false); err != nil {
		t.Fatal(err)
	}
	if err := CloseContract(ctx, id, provider, 120, false); err == nil || !strings.Contains(err.Error(), "conflicting retained networks") {
		t.Fatalf("conflicting retained provider identity reached terminal settlement: %v", err)
	}
	var outcome *string
	var usage []byte
	var reports int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT outcome,provider_usage,(SELECT count(*) FROM contract_close WHERE contract_id=$1) FROM transfer_contract WHERE contract_id=$1`, id).Scan(&outcome, &usage, &reports))
	})
	if outcome != nil || len(usage) != 0 || reports != 2 || len(contractPayoutTestAmounts(t, ctx, id)) != 0 {
		t.Fatal("identity conflict discarded original reports or published settlement")
	}
	healthy, err := CreateContractNoEscrow(ctx, originNetwork, origin, providerNetwork, sibling, 120)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []server.Id{origin, sibling} {
		if err := CloseContract(ctx, healthy, client, 120, false); err != nil {
			t.Fatal(err)
		}
	}
	usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
	if err != nil || len(usages) != 1 || usages[0].ClientId != sibling || usages[0].NetworkId != providerNetwork || usages[0].PayoutByteCount != 120 {
		t.Fatalf("affected provider identity stopped independent completed work: %v %v", usages, err)
	}
}

// Endpoint and stream evidence must agree before terminal publication.
func TestStContractUsageEndpointStreamIdentityConflictStaysLocal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityConflict(t, false) })
}

// Stream and extender evidence must agree without blocking a healthy peer.
func TestStContractUsageStreamExtenderIdentityConflictStaysLocal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityConflict(t, true) })
}

// First admission publishes the contract link and complete participant set in
// one transaction. Missing identities, cancellation and an actual insert abort
// leave no first-writer identity for the healthy retry to inherit.
func exerciseContractParticipantFirstAdmissionFailure(t testing.TB, failure string) {
	t.Helper()
	ctx := context.Background()
	start := server.NowUtc()
	networkId := server.NewId()
	origin, first, second, egress := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3), contractPayoutTestId(4)
	clients := map[server.Id]server.Id{origin: networkId, first: networkId, egress: networkId}
	if failure != "missing" {
		clients[second] = networkId
	}
	addContractPayoutTestClients(ctx, clients)
	contractId, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, egress, 120)
	if err != nil {
		t.Fatal(err)
	}
	streamId := server.NewId()
	requestCtx := ctx
	if failure == "canceled" {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithCancel(ctx)
		cancel()
	}
	if failure == "rollback" {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_participant_abort() RETURNS trigger AS $$
				BEGIN RAISE EXCEPTION 'synthetic participant insertion rollback'; END; $$ LANGUAGE plpgsql;
				CREATE TRIGGER synthetic_participant_abort BEFORE INSERT ON contract_participant
				FOR EACH ROW EXECUTE FUNCTION synthetic_participant_abort()`))
		})
	}
	var refused error
	server.HandleError(func() {
		refused = SetContractStream(requestCtx, contractId, streamId, []server.Id{first, second})
	}, func(err error) { refused = err })
	switch failure {
	case "missing":
		if refused == nil || !strings.Contains(refused.Error(), "intermediary clients do not exist") {
			t.Fatalf("missing first participant was admitted: %v", refused)
		}
	case "canceled":
		if !errors.Is(refused, context.Canceled) && !server.IsDoneError(refused) {
			t.Fatalf("canceled first admission lost its cause: %v", refused)
		}
	case "rollback":
		if refused == nil || !strings.Contains(refused.Error(), "synthetic participant insertion rollback") {
			t.Fatalf("original participant transaction did not abort: %v", refused)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_participant_abort ON contract_participant;
				DROP FUNCTION synthetic_participant_abort()`))
		})
	default:
		t.Fatal("unknown synthetic first admission failure")
	}
	var retainedStream *server.Id
	var participants int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT stream_id,(SELECT count(*) FROM contract_participant WHERE stream_id=$2)
			FROM transfer_contract WHERE contract_id=$1`, contractId, streamId).Scan(&retainedStream, &participants))
	})
	if retainedStream != nil || participants != 0 {
		t.Fatalf("failed first admission retained a partial provider snapshot: %v %d", retainedStream, participants)
	}
	if failure == "missing" {
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{second: networkId})
	}
	if err := SetContractStream(ctx, contractId, streamId, []server.Id{first, second}); err != nil {
		t.Fatal(err)
	}
	foreignStream := server.NewId()
	if err := SetContractStream(ctx, contractId, foreignStream, []server.Id{first}); err == nil {
		t.Fatal("existing contract changed its original stream")
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_participant WHERE stream_id=$1`, foreignStream).Scan(&participants))
	})
	if participants != 0 {
		t.Fatal("rejected stream replacement published provider identities")
	}
	for _, clientId := range []server.Id{origin, egress} {
		if err := CloseContract(ctx, contractId, clientId, 120, false); err != nil {
			t.Fatal(err)
		}
	}
	usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
	if err != nil || len(usages) != 3 {
		t.Fatalf("first admission did not recover complete work: %v %v", usages, err)
	}
	got := map[server.Id]int64{}
	for _, usage := range usages {
		if usage.NetworkId != networkId {
			t.Fatal("recovered first admission changed the provider owner")
		}
		got[usage.ClientId] = usage.PayoutByteCount
	}
	if !maps.Equal(got, map[server.Id]int64{first: 40, second: 40, egress: 40}) {
		t.Fatalf("recovered first admission changed completed bytes: %v", got)
	}
}

// One missing directory identity cannot publish a partial first snapshot.
func TestStContractUsageMissingFirstParticipantRecoversAtomically(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantFirstAdmissionFailure(t, "missing") })
}

// The actual participant insert abort must roll back the preceding stream link.
func TestStContractUsageFirstParticipantInsertRollbackRecovers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantFirstAdmissionFailure(t, "rollback") })
}

// A canceled owner cannot leave a partial identity for a later healthy owner.
func TestStContractUsageCanceledFirstParticipantRecovers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantFirstAdmissionFailure(t, "canceled") })
}

// A retained original path survives removal from the mutable directory.
func TestStContractUsageRemovedParticipantRetryKeepsOriginalIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityRetry(t, true, false, true) })
}

// A companion can inherit the same retained path without re-admitting members.
func TestStContractUsageRemovedParticipantCompanionKeepsOriginalIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseContractParticipantIdentityRetry(t, true, true, true) })
}
