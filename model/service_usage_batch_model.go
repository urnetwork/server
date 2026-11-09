package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Quota.ReportUsage's server side (DESIGN.md 20.6's Usage batches; QUOTA-M2-05): ApplyUsageBatch takes
// one producer's 60 s window of usage and applies it once by its batch id, in one transaction:
//
//	apply   insert service_usage_batch(batch_id); a batch id already present answers success having
//	        done nothing. That insert with the batch's rows is the linearization point.
//	debits  service_debit_journal rows, with no payout anywhere (D-QUOTA-4): no transfer_escrow and no
//	        participant rows. A lease's bytes arrive carved per balance by its host
//	        (plan-QUOTA-lease-debits), each row under the usage id its host named its Redis token for.
//	        evs's bytes have no reservation, so they draw at apply time in draw order
//	        (LoadDrawOrderBalances), each row's usage_id the first 16 B of SHA-256(batch_id ||
//	        network_id || meter || balance_id) (plan-QUOTA-small-readings reading 3). Debit after use is
//	        evs's (§20.10) and balance_byte_count has no CHECK, so bytes past every balance's room go on
//	        the last in-window balance and mark the network spent; with no in-window balance at all they
//	        mark it spent and go unbilled, counted in urnetwork_quota_unbilled_bytes_total (reading 5).
//	state   each network's stored-byte delta and spent mark in network_quota_state, in network-id order
//	        with the rows locked in that order, so two producers' batches cannot deadlock.
//
// A batch is refused whole, with nothing applied and its id left unused, when it names no batch, an
// unknown producer or meter, a zero network, a negative transfer or more events than
// sidecar.QuotaUsageBatchMaxEventCount, or carves that do not fit its events. Relayed bytes settle by
// contract (QUOTA-M2-09), never by batch, so a batch naming meter relay is refused too.
//
// One service_usage_batch row a producer a minute: batch rows live 24 h (§20.14) and a producer retries
// a window for at most 10 min, so the table holds about a day of minutes per producer, never one row
// per network. A Postgres outage past 10 min loses batches by design (§19.10): under-billing, never
// double-billing. The flush (FlushServiceDebits) applies the journal to the balances.

type UsageBatchSettings struct {
	// The apply transaction's statement bound.
	StatementTimeout time.Duration
	// How long the apply waits for a quota state row another batch holds.
	LockTimeout time.Duration
	// The most carves one batch may bring: each event's bytes spread over a few balances.
	MaxCarveCount int
}

func DefaultUsageBatchSettings() *UsageBatchSettings {
	return &UsageBatchSettings{
		StatementTimeout: 10 * time.Second,
		LockTimeout:      5 * time.Second,
		MaxCarveCount:    4 * sidecar.QuotaUsageBatchMaxEventCount,
	}
}

var usageBatchSettings = DefaultUsageBatchSettings()

// Refused whole: nothing applied, and the batch id left unused. The error's text says why.
var ErrUsageBatchRefused = errors.New("usage batch refused")

// Refused: this binary does not link MG2, so the batch tables do not exist for it. The producer retries,
// then drops the batch: under-billing only.
var ErrUsageBatchUnavailable = errors.New("usage batches are not available")

var quotaUnbilledByteCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_quota_unbilled_bytes_total",
	Help: "Metered bytes a usage batch applied for a network with no in-window balance at all, which nothing bills (plan-QUOTA-small-readings reading 5)",
}, []string{"meter"})

func init() {
	prometheus.MustRegister(quotaUnbilledByteCounter)
}

// One debit a lease's host carved out of its reservation for a balance (plan-QUOTA-lease-debits):
// bytes the host moved, in draw order, out of the lease's token into a token named svc/<usage_id>,
// which the flush releases once the debit applies. QUOTA-M2-07's hosts send them with the batch.
type UsageCarve struct {
	UsageId   server.Id
	NetworkId server.Id
	Meter     string
	BalanceId server.Id
	ByteCount int64
}

// A batch row's usage id: the first 16 B of SHA-256(batch_id || network_id || meter || balance_id).
// The two ids around the meter are fixed width, so no two rows hash the same bytes.
func ServiceUsageId(batchId server.Id, networkId server.Id, meter string, balanceId server.Id) server.Id {
	input := make([]byte, 0, 16+16+len(meter)+16)
	input = append(input, batchId[:]...)
	input = append(input, networkId[:]...)
	input = append(input, meter...)
	input = append(input, balanceId[:]...)
	sum := sha256.Sum256(input)
	return server.Id(sum[0:16])
}

// What a batch moves for one (network, meter).
type usageBatchMeterTotal struct {
	transferByteCount int64
	carves            []*UsageCarve
}

// What a batch moves for one network.
type usageBatchNetworkTotal struct {
	meterTotals     map[string]*usageBatchMeterTotal
	storedByteDelta int64
	hasStoredDelta  bool
}

func usageBatchRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsageBatchRefused, fmt.Sprintf(format, args...))
}

// The batch's totals by network and meter, or the reason it is refused whole.
func usageBatchTotals(batch *sidecar.UsageBatch, carves []*UsageCarve) (map[server.Id]*usageBatchNetworkTotal, error) {
	if batch == nil {
		return nil, usageBatchRefused("no batch")
	}
	if batch.BatchId == (connect.Id{}) {
		return nil, usageBatchRefused("a zero batch id")
	}
	if !sidecar.IsQuotaProducer(batch.Producer) {
		return nil, usageBatchRefused("no producer %q", batch.Producer)
	}
	if sidecar.QuotaUsageBatchMaxEventCount < len(batch.Events) {
		return nil, usageBatchRefused("%d events, past %d", len(batch.Events), sidecar.QuotaUsageBatchMaxEventCount)
	}
	if usageBatchSettings.MaxCarveCount < len(carves) {
		return nil, usageBatchRefused("%d carves, past %d", len(carves), usageBatchSettings.MaxCarveCount)
	}
	networkTotals := map[server.Id]*usageBatchNetworkTotal{}
	meterTotal := func(networkId server.Id, meter string) *usageBatchMeterTotal {
		networkTotal, ok := networkTotals[networkId]
		if !ok {
			networkTotal = &usageBatchNetworkTotal{meterTotals: map[string]*usageBatchMeterTotal{}}
			networkTotals[networkId] = networkTotal
		}
		total, ok := networkTotal.meterTotals[meter]
		if !ok {
			total = &usageBatchMeterTotal{}
			networkTotal.meterTotals[meter] = total
		}
		return total
	}
	for _, event := range batch.Events {
		if event == nil {
			return nil, usageBatchRefused("a nil event")
		}
		if event.NetworkId == (connect.Id{}) {
			return nil, usageBatchRefused("a zero network id")
		}
		if !sidecar.IsQuotaMeter(event.Meter) {
			return nil, usageBatchRefused("no meter %q", event.Meter)
		}
		if event.Meter == sidecar.QuotaMeterRelay {
			return nil, usageBatchRefused("relayed bytes settle by contract, not by batch")
		}
		if event.TransferByteCount < 0 {
			return nil, usageBatchRefused("a negative transfer %d", event.TransferByteCount)
		}
		networkId := server.Id(event.NetworkId)
		total := meterTotal(networkId, event.Meter)
		total.transferByteCount += event.TransferByteCount
		if total.transferByteCount < 0 {
			return nil, usageBatchRefused("a transfer total past int64")
		}
		if event.StoredByteDelta != 0 {
			networkTotal := networkTotals[networkId]
			networkTotal.storedByteDelta += event.StoredByteDelta
			networkTotal.hasStoredDelta = true
		}
	}
	carvedUsageIds := map[server.Id]bool{}
	for _, carve := range carves {
		if carve == nil || carve.UsageId == (server.Id{}) || carve.BalanceId == (server.Id{}) || carve.NetworkId == (server.Id{}) {
			return nil, usageBatchRefused("a carve names a zero id")
		}
		if carvedUsageIds[carve.UsageId] {
			return nil, usageBatchRefused("two carves share usage id %s", carve.UsageId)
		}
		carvedUsageIds[carve.UsageId] = true
		if carve.ByteCount <= 0 {
			return nil, usageBatchRefused("a carve of %d bytes", carve.ByteCount)
		}
		networkTotal, ok := networkTotals[carve.NetworkId]
		if !ok {
			return nil, usageBatchRefused("a carve for a network the batch does not name")
		}
		total, ok := networkTotal.meterTotals[carve.Meter]
		if !ok {
			return nil, usageBatchRefused("a carve for a meter the network's events do not name")
		}
		total.carves = append(total.carves, carve)
	}
	for _, networkTotal := range networkTotals {
		for meter, total := range networkTotal.meterTotals {
			carvedByteCount := int64(0)
			for _, carve := range total.carves {
				carvedByteCount += carve.ByteCount
			}
			if total.transferByteCount < carvedByteCount {
				return nil, usageBatchRefused("%d bytes carved of %d on %s", carvedByteCount, total.transferByteCount, meter)
			}
		}
	}
	return networkTotals, nil
}

// One journal row a batch writes.
type serviceDebitRow struct {
	usageId   server.Id
	balanceId server.Id
	meter     string
	byteCount int64
}

// Seams a test holds the apply at. Nil in production.
type usageBatchHooks struct {
	// inside the transaction, after the batch's rows and before its commit; an error rolls it back
	beforeCommit func() error
}

// Applies batch once by its id at now on the caller's clock, with the carves its host made of its
// leases' bytes (see the file header). applied is false when the id had already applied, which is
// success. A refusal wraps ErrUsageBatchRefused and applies nothing.
func ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*UsageCarve, now time.Time) (applied bool, returnErr error) {
	return applyUsageBatch(ctx, batch, carves, now, nil)
}

func applyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*UsageCarve, now time.Time, hooks *usageBatchHooks) (applied bool, returnErr error) {
	networkTotals, err := usageBatchTotals(batch, carves)
	if err != nil {
		return false, err
	}
	if !server.SidecarMg2Linked() {
		return false, ErrUsageBatchUnavailable
	}
	now = now.UTC()
	networkIds := make([]server.Id, 0, len(networkTotals))
	for networkId := range networkTotals {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	// limits before the transaction, so it holds no lock while a cache loads
	networkIdLimitByteCounts := map[server.Id]int64{}
	for _, networkId := range networkIds {
		if networkTotals[networkId].hasStoredDelta {
			networkIdLimitByteCounts[networkId] = NetworkStorageLimitByteCount(ctx, networkId)
		}
	}
	batchId := server.Id(batch.BatchId)
	settings := usageBatchSettings

	outcome := &usageBatchOutcome{}
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			applyUsageBatchInTx(ctx, tx, batch, batchId, networkIds, networkTotals, networkIdLimitByteCounts, now, hooks, outcome)
		}, server.TxLockTimeout(settings.LockTimeout))
	}, func(err error) {
		returnErr = err
	})
	if returnErr != nil {
		return false, returnErr
	}
	if outcome.applied {
		for meter, byteCount := range outcome.unbilledMeterByteCounts {
			quotaUnbilledByteCounter.WithLabelValues(meter).Add(float64(byteCount))
			glog.Infof("[quota]batch %s: %d %s bytes for a network with no balance, unbilled\n", batchId, byteCount, meter)
		}
		// the flush chains of the shards written, backed off while their shards were empty; a failed
		// wake leaves the chain at its idle delay
		_, _ = wakeServiceDebitFlushes(context.WithoutCancel(ctx), outcome.balanceIds)
	}
	return outcome.applied, nil
}

// What one attempt of the apply's transaction did.
type usageBatchOutcome struct {
	applied                 bool
	unbilledMeterByteCounts map[string]int64
	// the balances the journal rows name
	balanceIds []server.Id
}

// One attempt of the apply's transaction. A rerun after an ambiguous commit finds its batch id and
// applies nothing again.
func applyUsageBatchInTx(
	ctx context.Context,
	tx server.PgTx,
	batch *sidecar.UsageBatch,
	batchId server.Id,
	networkIds []server.Id,
	networkTotals map[server.Id]*usageBatchNetworkTotal,
	networkIdLimitByteCounts map[server.Id]int64,
	now time.Time,
	hooks *usageBatchHooks,
	outcome *usageBatchOutcome,
) {
	*outcome = usageBatchOutcome{}
	server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
		`SET LOCAL statement_timeout = %d`,
		max(usageBatchSettings.StatementTimeout.Milliseconds(), 1),
	)))
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO service_usage_batch (batch_id, producer, window_start, applied_time)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (batch_id) DO NOTHING
		`,
		batchId,
		batch.Producer,
		batch.WindowStart.UTC(),
		now,
	))
	if tag.RowsAffected() == 0 {
		// applied before: success, having done nothing
		return
	}

	rows := []*serviceDebitRow{}
	networkIdChanges := map[server.Id]*networkQuotaChange{}
	unbilledMeterByteCounts := map[string]int64{}
	for _, networkId := range networkIds {
		networkTotal := networkTotals[networkId]
		change := &networkQuotaChange{}
		if networkTotal.hasStoredDelta {
			change.storedByteDelta = networkTotal.storedByteDelta
			change.limitByteCount = networkIdLimitByteCounts[networkId]
			change.hasStoredDelta = true
		}
		meters := make([]string, 0, len(networkTotal.meterTotals))
		for meter, total := range networkTotal.meterTotals {
			if 0 < total.transferByteCount {
				meters = append(meters, meter)
			}
		}
		slices.Sort(meters)
		var balances []*DrawOrderBalance
		if 0 < len(meters) {
			balances = loadDrawOrderBalancesWithQuery(ctx, tx, networkId, now)
		}
		idBalances := map[server.Id]*DrawOrderBalance{}
		for _, balance := range balances {
			idBalances[balance.BalanceId] = balance
		}
		for _, meter := range meters {
			total := networkTotal.meterTotals[meter]
			rest := total.transferByteCount
			for _, carve := range total.carves {
				rows = append(rows, &serviceDebitRow{
					usageId:   carve.UsageId,
					balanceId: carve.BalanceId,
					meter:     meter,
					byteCount: carve.ByteCount,
				})
				rest -= carve.ByteCount
				// a later draw in this batch sees the carve as pending
				if balance, ok := idBalances[carve.BalanceId]; ok {
					balance.PendingByteCount += carve.ByteCount
				}
			}
			if rest <= 0 {
				continue
			}
			if len(balances) == 0 {
				unbilledMeterByteCounts[meter] += rest
				change.spentTime = now
				change.hasSpentTime = true
				continue
			}
			balanceIdTakes := map[server.Id]int64{}
			for _, balance := range balances {
				take := min(rest, balance.RoomByteCount())
				if take <= 0 {
					continue
				}
				balanceIdTakes[balance.BalanceId] += take
				balance.PendingByteCount += take
				rest -= take
				if rest == 0 {
					break
				}
			}
			if 0 < rest {
				// past every balance's room: the last in-window balance overdraws
				last := balances[len(balances)-1]
				balanceIdTakes[last.BalanceId] += rest
				last.PendingByteCount += rest
				change.spentTime = now
				change.hasSpentTime = true
			}
			for _, balance := range balances {
				if take := balanceIdTakes[balance.BalanceId]; 0 < take {
					rows = append(rows, &serviceDebitRow{
						usageId:   ServiceUsageId(batchId, networkId, meter, balance.BalanceId),
						balanceId: balance.BalanceId,
						meter:     meter,
						byteCount: take,
					})
				}
			}
		}
		if change.hasStoredDelta || change.hasSpentTime {
			networkIdChanges[networkId] = change
		}
	}

	if 0 < len(rows) {
		usageIds := make([]server.Id, 0, len(rows))
		balanceIds := make([]server.Id, 0, len(rows))
		meters := make([]string, 0, len(rows))
		byteCounts := make([]int64, 0, len(rows))
		for _, row := range rows {
			usageIds = append(usageIds, row.usageId)
			balanceIds = append(balanceIds, row.balanceId)
			meters = append(meters, row.meter)
			byteCounts = append(byteCounts, row.byteCount)
		}
		// a carve's row already journaled, under the same usage id, is that carve: it debits once
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO service_debit_journal (usage_id, balance_id, meter, debit_byte_count, batch_id)
				SELECT usage_id, balance_id, meter, debit_byte_count, $5
				FROM unnest($1::uuid[], $2::uuid[], $3::varchar[], $4::bigint[])
					AS row(usage_id, balance_id, meter, debit_byte_count)
				ORDER BY balance_id, usage_id
				ON CONFLICT (balance_id, usage_id) DO NOTHING
			`,
			usageIds,
			balanceIds,
			meters,
			byteCounts,
			batchId,
		))
	}
	applyNetworkQuotaChangesInTx(ctx, tx, networkIdChanges)

	if hooks != nil && hooks.beforeCommit != nil {
		server.Raise(hooks.beforeCommit())
	}
	balanceIds := []server.Id{}
	for _, row := range rows {
		if !slices.Contains(balanceIds, row.balanceId) {
			balanceIds = append(balanceIds, row.balanceId)
		}
	}
	*outcome = usageBatchOutcome{
		applied:                 true,
		unbilledMeterByteCounts: unbilledMeterByteCounts,
		balanceIds:              balanceIds,
	}
}
