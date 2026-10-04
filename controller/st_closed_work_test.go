// Actual payout issuance retains same-statement original rows in its signed
// published artifact. This fixture's wallet/binding rows are not a new claim
// that those eligibility inputs have independent measurement provenance.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// Epoch policy comes from the real pinned RPC owner. This explicit fixture
// supplies only an inactive fleet census, not artifact or usage verdicts.
type stClosedWorkClient struct {
	StClient
	bindingCalls int
}

func (self *stClosedWorkClient) BindingsAt(ctx context.Context, clients [][16]byte, epoch, start, end uint64) ([]*StFleetBindingState, error) {
	self.bindingCalls++
	result := make([]*StFleetBindingState, len(clients))
	for index := range result {
		result[index] = &StFleetBindingState{}
	}
	return result, ctx.Err()
}

// Read original jsonb back, rather than predicting PostgreSQL's presentation.
func TestStClosedWorkPublishedArtifactKeepsExactOriginalRowsAndRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, credential, cfg, restart := newStPayoutPolicyFixture(t, 1)
		fixture.configure(275, "")
		client := &stClosedWorkClient{StClient: restart()}
		id := server.NewId()
		provider := *credential.ClientId
		closed := stPayoutPolicyTime(225)
		snapshot := map[string]any{"version": 1, "byte_count": 121, "providers": []map[string]any{{"client_id": provider, "network_id": credential.NetworkId, "byte_count": 121}}}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,outcome,close_time,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,121,'settled',$6,$7,true)`, id, server.NewId(), server.NewId(), provider, credential.NetworkId, closed, snapshot))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_provider_stats(period_start,period_end,client_id,assignments,confirmations) VALUES($1,$2,$3,10,10)`, stPayoutPolicyTime(200), stPayoutPolicyTime(250), provider))
			coldkey := [32]byte{9}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_provider_wallet_history(client_id,network_id,coldkey_ss58,coldkey_pubkey,set_time) VALUES($1,$2,$3,$4,$5)`, provider, credential.NetworkId, "synthetic-closed-work-wallet", coldkey[:], stPayoutPolicyTime(200)))
		})
		root, leaves, err := stComputeReleasePayout(ctx, cfg, client, 1, stPayoutPolicyTime(200), stPayoutPolicyTime(250), 200, 250, nil)
		if err != nil || root == ([32]byte{}) || leaves != 1 || client.bindingCalls != 1 {
			t.Fatal("actual payout issuance did not reach original component publication", root, leaves, err)
		}
		record := model.GetStPayoutArtifact(ctx, cfg.DeploymentKey(), 1, cfg.NoId)
		store, ok := server.LoadBlobStore()
		if record == nil || !ok {
			t.Fatal("original artifact publication is absent")
		}
		artifact, raw, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil || artifact.ClosedWork == nil || artifact.ClosedWork.Count != 1 || artifact.ClosedWork.Records[0].ContractId != [16]byte(id) {
			t.Fatal("published payout lost original complete work census", err)
		}
		var original []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&original))
		})
		if !bytes.Equal(original, artifact.ClosedWork.Records[0].Original) || artifact.ClosedWork.WindowStart != stPayoutPolicyTime(200).UTC().Format(time.RFC3339Nano) || artifact.ClosedWork.Start != artifact.Start || artifact.ClosedWork.PolicyHash != artifact.PolicyHash {
			t.Fatal("issuance reconstructed database bytes or guessed epoch/policy")
		}
		component, err := payoutartifact.VerifyClosedWork(ctx, artifact)
		if err != nil || component.Contracts != 1 || component.Providers != 1 || component.UsageBytes != 121 {
			t.Fatal("independent reconstruction rejected actual published source", component, err)
		}
		fixture.configure(275, "missing-history")
		if _, _, err := stComputeReleasePayout(ctx, cfg, client, 1, time.Time{}, time.Time{}, 0, 0, nil); err != nil || client.bindingCalls != 1 {
			t.Fatal("immutable retry asked for newer source authority", err)
		}
		_, again, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("restart changed exact signed original census", err)
		}
	})
}

// Existing signed artifacts are consumed without a backfill or re-signature.
func TestStClosedWorkLegacyPublishedArtifactRemainsExactOnRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, _, cfg, restart := newStPayoutPolicyFixture(t, 1)
		original, record := stRetainPolicyPayout(t, cfg)
		fixture.configure(275, "missing-history")
		if _, _, err := stComputeReleasePayout(t.Context(), cfg, restart(), 0, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			t.Fatal("legacy original retry demanded component backfill", err)
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			t.Fatal("original store is absent")
		}
		artifact, again, err := startifact.Read(t.Context(), store, record.ContentHash)
		if err != nil || artifact.ClosedWork != nil || !bytes.Equal(original, again) {
			t.Fatal("legacy published bytes changed on rolling retry", err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(again, &wire); err != nil || wire["original_closed_work"] != nil {
			t.Fatal("legacy original acquired a serialized evidence claim", err)
		}
	})
}
