//go:build sidecar_pending

package handlers

// api's internal quota surface on Postgres (QUOTA-M2-11; QD-055): each route answers what model holds to the
// classes it lists, a batch applies once by its id, and a batch the model refuses applies nothing.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
)

// A balance of the network's, its window around now.
func testQuotaServiceInsertBalance(t testing.TB, networkId server.Id, now time.Time, endTime time.Time, byteCount int64, credit bool) server.Id {
	t.Helper()
	ctx := t.Context()
	balanceId := server.NewId()
	var grantKind *string
	if credit {
		teamCredit := "team_credit"
		grantKind = &teamCredit
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO transfer_balance (
					balance_id,
					network_id,
					start_time,
					end_time,
					start_balance_byte_count,
					net_revenue_nano_cents,
					balance_byte_count,
					pro,
					grant_kind
				)
				VALUES ($1, $2, $3, $4, $5, 0, $5, false, $6)
			`,
			balanceId,
			networkId,
			now.Add(-time.Hour),
			endTime,
			byteCount,
			grantKind,
		))
	})
	return balanceId
}

func TestQuotaServiceAnswersWhatModelHolds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		bearers := newTestQuotaServiceBearers(t)
		now := server.NowUtc()
		serve := testQuotaServiceRouter(t, NewQuotaServiceHandlers(nil, &QuotaServiceDeps{
			Verifier: bearers.verifier,
			Now:      func() time.Time { return now },
		}))
		networkId := server.NewId()

		// limits: Community's, as GET /sidecar/limits answers a network with no subscription
		w := testQuotaServiceServe(serve, "GET", "/internal/gate/limits/"+networkId.String(), bearers.callerBearers[server.InternalGateCallerGkm], nil)
		connect.AssertEqual(t, w.Code, http.StatusOK)
		limits := &sidecar.QuotaLimits{}
		connect.AssertEqual(t, json.Unmarshal(w.Body.Bytes(), limits), nil)
		connect.AssertEqual(t, limits, model.NetworkQuotaLimits(t.Context(), networkId))

		// the draw order: by end time, a team credit line last, an overdrawn balance's credit 0
		credit := testQuotaServiceInsertBalance(t, networkId, now, now.Add(time.Hour), 1000, true)
		late := testQuotaServiceInsertBalance(t, networkId, now, now.Add(3*time.Hour), 300, false)
		early := testQuotaServiceInsertBalance(t, networkId, now, now.Add(2*time.Hour), -50, false)
		w = testQuotaServiceServe(serve, "GET", "/internal/gate/draw-order/"+networkId.String(), bearers.callerBearers[server.InternalGateCallerTlsedge], nil)
		connect.AssertEqual(t, w.Code, http.StatusOK)
		drawOrder := &quotahost.QuotaApiDrawOrder{}
		connect.AssertEqual(t, json.Unmarshal(w.Body.Bytes(), drawOrder), nil)
		connect.AssertEqual(t, len(drawOrder.Balances), 3)
		connect.AssertEqual(t, drawOrder.Balances[0].BalanceId, early)
		connect.AssertEqual(t, drawOrder.Balances[0].CreditByteCount, int64(0))
		connect.AssertEqual(t, drawOrder.Balances[1].BalanceId, late)
		connect.AssertEqual(t, drawOrder.Balances[1].CreditByteCount, int64(300))
		connect.AssertEqual(t, drawOrder.Balances[2].BalanceId, credit)
		connect.AssertEqual(t, drawOrder.Balances[2].Credit, true)

		// the spent mark reaches the over-limit list, spent networks apart
		w = testQuotaServiceServe(serve, "POST", "/internal/gate/spent/"+networkId.String(), bearers.callerBearers[server.InternalGateCallerMxedge], nil)
		connect.AssertEqual(t, w.Code, http.StatusOK)
		w = testQuotaServiceServe(serve, "GET", "/internal/gate/over-limit", bearers.callerBearers[server.InternalGateCallerSockets], nil)
		connect.AssertEqual(t, w.Code, http.StatusOK)
		wire := &quotahost.QuotaApiOverLimit{}
		connect.AssertEqual(t, json.Unmarshal(w.Body.Bytes(), wire), nil)
		overLimit, err := wire.OverLimit()
		connect.AssertEqual(t, err, nil)
		if overLimit.SpentNetworkIdRefusals[connect.Id(networkId)] == nil {
			t.Fatal("the spent network is not in the list")
		}
		connect.AssertEqual(t, overLimit.StorageNetworkIdRefusals[connect.Id(networkId)] == nil, true)

		// a batch applies once by its id
		batch := &sidecar.UsageBatch{
			BatchId:     connect.NewId(),
			Producer:    sidecar.QuotaProducerEvs,
			WindowStart: now.Truncate(time.Minute),
			Events:      []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 100}},
		}
		body, err := json.Marshal(quotahost.NewQuotaApiUsageBatch(batch, nil))
		connect.AssertEqual(t, err, nil)
		evs := bearers.callerBearers[server.InternalGateCallerEvs]
		for _, want := range []bool{true, false} {
			w = testQuotaServiceServe(serve, "POST", "/internal/gate/usage-batch", evs, body)
			connect.AssertEqual(t, w.Code, http.StatusOK)
			result := &quotahost.QuotaApiUsageBatchResult{}
			connect.AssertEqual(t, json.Unmarshal(w.Body.Bytes(), result), nil)
			connect.AssertEqual(t, result.Applied, want)
		}

		// a batch the model refuses applies nothing: a carve on evs, which no lease spends
		refused := &sidecar.UsageBatch{
			BatchId:     connect.NewId(),
			Producer:    sidecar.QuotaProducerEvs,
			WindowStart: now.Truncate(time.Minute),
			Events:      []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 10}},
		}
		carves := []*model.UsageCarve{{
			UsageId:   model.ServiceUsageId(server.Id(refused.BatchId), networkId, sidecar.QuotaMeterEvs, late),
			NetworkId: networkId,
			Meter:     sidecar.QuotaMeterEvs,
			BalanceId: late,
			ByteCount: 10,
		}}
		body, err = json.Marshal(quotahost.NewQuotaApiUsageBatch(refused, carves))
		connect.AssertEqual(t, err, nil)
		w = testQuotaServiceServe(serve, "POST", "/internal/gate/usage-batch", evs, body)
		connect.AssertEqual(t, w.Code, http.StatusBadRequest)
		// its id was left unused: the batch without the carve applies
		body, err = json.Marshal(quotahost.NewQuotaApiUsageBatch(refused, nil))
		connect.AssertEqual(t, err, nil)
		w = testQuotaServiceServe(serve, "POST", "/internal/gate/usage-batch", evs, body)
		connect.AssertEqual(t, w.Code, http.StatusOK)
		result := &quotahost.QuotaApiUsageBatchResult{}
		connect.AssertEqual(t, json.Unmarshal(w.Body.Bytes(), result), nil)
		connect.AssertEqual(t, result.Applied, true)
	})
}
