package model

// Pay with Gamma: claims of direct payments (controller/gamma_claim.go).
//
// Since runtime 493 anyone holding an SN25 gamma deposit can spend it straight
// at our listed service from their own hotkey, without our API. The chain
// keeps what each hotkey has spent at the service (GammaSpent, a total that
// only grows), so such a payment is never lost, but nothing says which
// account it was for. A claim says it: a network that proved it holds the
// hotkey (a signature of our challenge) takes the part of that hotkey's total
// that no claim has taken yet.
//
// A claim is an intent of purpose claim, already paid: its hotkey is the
// payer's own (never one we assigned), and its charge is recorded spent at the
// finalized block whose total the claim was read at. The biller credits it
// like any other spent charge. What has been claimed of a hotkey is the sum
// of its claims not voided, so no table is added for it: one hotkey's claims
// are serialized under an advisory lock, and the same gamma is never claimed
// twice.
//
// The watcher also keeps a record of each direct payment it sees, keyed by
// the payer's hotkey and the block (gamma_alarm, kind direct_payment): a
// record for support, not an alarm. A claim does not depend on it, since it
// reads the chain's total: a payment made before the watcher ran is claimable
// too.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// GammaPurposeClaim: a network claimed a direct payment (POST /pay/gamma/claim).
const GammaPurposeClaim = "claim"

// GammaRecordKindDirectPayment is the gamma_alarm kind of the watcher's
// record of a direct payment: a record, not an alarm (GammaAlarmKinds in the
// controller does not list it).
const GammaRecordKindDirectPayment = "direct_payment"

// The advisory lock class one hotkey's claims are serialized under.
const gammaClaimLockClass = 0x67636c61 // "gcla"

// A claim that credits data names its byte count in its item:
// claim_data_<bytes>, so what it credits is what the claim answered.
const gammaClaimDataItemPrefix = "claim_data_"

// GammaClaimDataItem is the item of a claim that credits byteCount of data.
func GammaClaimDataItem(byteCount ByteCount) string {
	return gammaClaimDataItemPrefix + strconv.FormatInt(int64(byteCount), 10)
}

// ParseGammaClaimDataItem is the byte count a claim's data item credits; ok =
// false for any other item.
func ParseGammaClaimDataItem(item string) (byteCount ByteCount, ok bool) {
	digits, found := strings.CutPrefix(item, gammaClaimDataItemPrefix)
	if !found || digits == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return ByteCount(value), true
}

// GetGammaClaimedTotal is how much of a hotkey's direct payments has been
// claimed: the amount of its claims not voided, in gamma units.
func GetGammaClaimedTotal(ctx context.Context, hotkey string) (total int64) {
	server.Db(ctx, func(conn server.PgConn) {
		total = getGammaClaimedTotal(ctx, conn, hotkey)
	})
	return
}

func getGammaClaimedTotal(ctx context.Context, query server.PgCanQuery, hotkey string) (total int64) {
	result, err := query.Query(
		ctx,
		`
			SELECT COALESCE(SUM(price_usd_nano_cents), 0)::bigint
			FROM gamma_payment_intent
			WHERE purpose = 'claim' AND hotkey = $1 AND voided_at IS NULL
		`,
		hotkey,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&total))
		}
	})
	return
}

// GammaClaimSpend is where a claim's total was read: the finalized block.
type GammaClaimSpend struct {
	// the hotkey's GammaSpent total at our service at the block
	OnChainTotal int64
	BlockNumber  int64
	BlockHash    string
}

// CreateGammaClaim records a claim, in one transaction under the hotkey's
// claim lock: the intent (purpose claim, the payer's hotkey, the amount) and
// its charge, spent at the block the on-chain total was read at, but only
// while that total less what was claimed before still covers the amount.
// created = false otherwise, or when the network is gone; claimable is what
// the total left before this claim.
func CreateGammaClaim(ctx context.Context, intent *GammaPaymentIntent, spend *GammaClaimSpend, now time.Time) (created bool, claimable int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		created = false
		server.RaisePgResult(tx.Exec(
			ctx,
			`/* gamma-claim-hotkey-lock */ SELECT pg_advisory_xact_lock($1, hashtext($2))`,
			gammaClaimLockClass,
			intent.Hotkey,
		))
		claimable = spend.OnChainTotal - getGammaClaimedTotal(ctx, tx, intent.Hotkey)
		if intent.PriceUsdNanoCents <= 0 || claimable < intent.PriceUsdNanoCents {
			return
		}
		if !CreateGammaPaymentIntentInTx(tx, ctx, intent) {
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_charge (
					reference, intent_id, hotkey, amount, state, create_time,
					resolved_at, block_number, block_hash, event_index
				)
				VALUES ($1, $2, $3, $4, 'spent', $5, $5, $6, $7, -1)
			`,
			intent.Reference,
			intent.IntentId,
			intent.Hotkey,
			intent.PriceUsdNanoCents,
			now,
			spend.BlockNumber,
			spend.BlockHash,
		))
		created = tag.RowsAffected() == 1
		if !created {
			// rolls the intent back with it
			panic("gamma claim charge not recorded")
		}
	}, server.TxReadCommitted)
	return
}

// GammaDirectPayment is the watcher's record of a direct payment.
type GammaDirectPayment struct {
	Hotkey      string
	BlockNumber int64
	EventIndex  int
	Amount      int64
	Detail      string
	CreateTime  time.Time
}

// GammaDirectPaymentKey is the record key of a direct payment: the payer's
// hotkey, the block and the event.
func GammaDirectPaymentKey(hotkey string, blockNumber int64, eventIndex int) string {
	return GammaRecordKindDirectPayment + "/" + hotkey + "/" + strconv.FormatInt(blockNumber, 10) + "/" + strconv.Itoa(eventIndex)
}

// ListGammaDirectPayments are the direct payments the watcher recorded for a
// hotkey, oldest first.
func ListGammaDirectPayments(ctx context.Context, hotkey string) (payments []*GammaDirectPayment) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT alarm_key, block_number, detail, create_time
				FROM gamma_alarm
				WHERE kind = $1 AND alarm_key LIKE $2
				ORDER BY block_number, alarm_key
			`,
			GammaRecordKindDirectPayment,
			GammaRecordKindDirectPayment+"/"+hotkey+"/%",
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var key string
				var blockNumber *int64
				payment := &GammaDirectPayment{Hotkey: hotkey}
				server.Raise(result.Scan(&key, &blockNumber, &payment.Detail, &payment.CreateTime))
				if blockNumber != nil {
					payment.BlockNumber = *blockNumber
				}
				parts := strings.Split(key, "/")
				if len(parts) == 4 {
					payment.EventIndex, _ = strconv.Atoi(parts[3])
				}
				for _, field := range strings.Fields(payment.Detail) {
					if value, ok := strings.CutPrefix(field, "amount="); ok {
						payment.Amount, _ = strconv.ParseInt(value, 10, 64)
					}
				}
				payments = append(payments, payment)
			}
		})
	})
	return
}
