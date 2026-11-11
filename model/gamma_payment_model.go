package model

// Pay with Gamma: the tables (server/gamma_pay_schema.go, mmm/BTCPAY.md §18).
//
// An intent is an invoice payable from an account's deposit hotkey: one the
// customer bought, or one the biller made for a renewal, a data top-up or a
// seat upgrade. Every intent has one public 32-byte random reference, made
// with it, and an automatic intent is unique per (account, purpose, period),
// so a reference is never minted twice for the same thing. The biller records
// the intent's charge before it signs anything; puts at most 512 charges in
// one spend batch; and keeps exactly one batch live (signed or submitted) at a
// time, so a charge is in at most one extrinsic that can still land. A batch
// whose era ends without it landing releases its charges, references
// unchanged, to the next batch. A charge is spent or skipped only by a
// finalized event carrying its reference, an intent is credited only from a
// spent charge, and a skipped charge is re-armed only once its deposit has
// changed.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// SubscriptionMarketGamma is a plan paid from an SN25 gamma deposit.
const SubscriptionMarketGamma = "gamma"

// OnboardingStoreGamma is the store a welcome offer bought with gamma is
// redeemed on.
const OnboardingStoreGamma = "gamma"

// Intent purposes.
const (
	// the customer bought it (POST /pay/gamma/intent)
	GammaPurposePurchase = "purchase"
	// the biller renews a plan paid with gamma at the end of its term
	GammaPurposeRenew = "renew"
	// the biller buys data when the account's balance runs out
	GammaPurposeTopup = "topup"
	// the biller upgrades a free account at its client limit to Pro
	GammaPurposeSeat = "seat"
)

// GammaAutomaticPurposes are the purposes the biller makes intents for.
var GammaAutomaticPurposes = []string{GammaPurposeRenew, GammaPurposeTopup, GammaPurposeSeat}

// Charge states.
const (
	GammaChargePending = "pending"
	GammaChargeSpent   = "spent"
	// the chain skipped the entry; re-armed once the deposit changes
	GammaChargeSkipped = "skipped"
	// the batch landed but spent nothing for the charge (the proxied call
	// failed); re-armed like a skip
	GammaChargeFailed = "failed"
)

// Batch states.
const (
	GammaBatchSigned    = "signed"
	GammaBatchSubmitted = "submitted"
	GammaBatchIncluded  = "included"
	GammaBatchExpired   = "expired"
	// something that must not happen happened; an operator looks, and the
	// biller signs nothing while any batch is in review
	GammaBatchReview = "review"
)

// ----- deposit hotkeys -----

// GammaDepositHotkey is a deposit hotkey we assigned an account, with the
// biller's last look at its deposit.
type GammaDepositHotkey struct {
	Hotkey          string
	NetworkId       server.Id
	DerivationIndex int64
	CreateTime      time.Time

	ObservedBlock *int64
	ObservedAt    *time.Time
	// nil: no deposit at the hotkey when last looked
	DepositColdkey   *string
	DepositSpender   *string
	DepositBalance   *int64
	DepositSpendable *int64
	// nil with a deposit: no daily cap
	DepositDailyCap *int64
	// SubtensorModule.Owner of the hotkey, nil when unassociated
	HotkeyOwner *string
	StrayTaoRao int64
}

// Fingerprint is the deposit as last observed, in one string: a skipped
// charge is re-armed only once this changes.
func (self *GammaDepositHotkey) Fingerprint() string {
	text := func(value *string) string {
		if value == nil {
			return "-"
		}
		return *value
	}
	number := func(value *int64) string {
		if value == nil {
			return "-"
		}
		return strconv.FormatInt(*value, 10)
	}
	return text(self.DepositColdkey) + "|" + text(self.DepositSpender) + "|" + number(self.DepositBalance) + "|" + number(self.DepositSpendable) + "|" + number(self.DepositDailyCap)
}

func scanGammaDepositHotkey(result pgx.Rows) *GammaDepositHotkey {
	hotkey := &GammaDepositHotkey{}
	server.Raise(result.Scan(
		&hotkey.Hotkey,
		&hotkey.NetworkId,
		&hotkey.DerivationIndex,
		&hotkey.CreateTime,
		&hotkey.ObservedBlock,
		&hotkey.ObservedAt,
		&hotkey.DepositColdkey,
		&hotkey.DepositSpender,
		&hotkey.DepositBalance,
		&hotkey.DepositSpendable,
		&hotkey.DepositDailyCap,
		&hotkey.HotkeyOwner,
		&hotkey.StrayTaoRao,
	))
	return hotkey
}

const gammaDepositHotkeyColumns = `
	hotkey, network_id, derivation_index, create_time,
	observed_block, observed_at, deposit_coldkey, deposit_spender,
	deposit_balance, deposit_spendable, deposit_daily_cap, hotkey_owner,
	stray_tao_rao
`

// GetGammaDepositHotkeys are an account's deposit hotkeys, oldest first.
func GetGammaDepositHotkeys(ctx context.Context, networkId server.Id) (hotkeys []*GammaDepositHotkey) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaDepositHotkeyColumns+`
			FROM gamma_deposit_hotkey
			WHERE network_id = $1
			ORDER BY derivation_index`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				hotkeys = append(hotkeys, scanGammaDepositHotkey(result))
			}
		})
	})
	return
}

// GetGammaDepositHotkey is one deposit hotkey, or nil when it is not ours.
func GetGammaDepositHotkey(ctx context.Context, hotkey string) (out *GammaDepositHotkey) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaDepositHotkeyColumns+` FROM gamma_deposit_hotkey WHERE hotkey = $1`,
			hotkey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				out = scanGammaDepositHotkey(result)
			}
		})
	})
	return
}

// GetGammaDepositHotkeysByAddresses are which of the addresses are our
// deposit hotkeys.
func GetGammaDepositHotkeysByAddresses(ctx context.Context, addresses []string) map[string]*GammaDepositHotkey {
	out := map[string]*GammaDepositHotkey{}
	if len(addresses) == 0 {
		return out
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaDepositHotkeyColumns+` FROM gamma_deposit_hotkey WHERE hotkey = ANY($1)`,
			addresses,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				hotkey := scanGammaDepositHotkey(result)
				out[hotkey.Hotkey] = hotkey
			}
		})
	})
	return out
}

// The advisory lock that serializes deposit hotkey allocation.
const gammaDepositHotkeyAllocationLock = 0x67616d6d61 // "gamma"

// AllocateGammaDepositHotkey gives an account its next deposit hotkey: the
// next global derivation index, derived by derive. Allocation is serialized,
// so two accounts never derive the same index.
func AllocateGammaDepositHotkey(
	ctx context.Context,
	networkId server.Id,
	derive func(index int64) (string, error),
	now time.Time,
) (hotkey *GammaDepositHotkey, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(gammaDepositHotkeyAllocationLock)))
		var next int64
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT COALESCE(MAX(derivation_index) + 1, 0) FROM gamma_deposit_hotkey`,
		).Scan(&next))
		address, err := derive(next)
		if err != nil {
			returnErr = err
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_deposit_hotkey (hotkey, network_id, derivation_index, create_time)
				VALUES ($1, $2, $3, $4)
			`,
			address,
			networkId,
			next,
			now,
		))
		hotkey = &GammaDepositHotkey{
			Hotkey:          address,
			NetworkId:       networkId,
			DerivationIndex: next,
			CreateTime:      now,
		}
	})
	return
}

// GammaDepositObservation is what the biller saw at a hotkey.
type GammaDepositObservation struct {
	Block int64
	// false: no deposit
	Exists    bool
	Coldkey   string
	Spender   string
	Balance   int64
	Spendable int64
	DailyCap  *int64
	// "" when the hotkey is not associated
	HotkeyOwner string
}

// SetGammaDepositObservation records the biller's look at a hotkey.
func SetGammaDepositObservation(ctx context.Context, hotkey string, observation *GammaDepositObservation, now time.Time) {
	nullable := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	var coldkey, spender *string
	var balance, spendable, dailyCap *int64
	if observation.Exists {
		coldkey = nullable(observation.Coldkey)
		spender = nullable(observation.Spender)
		balance = &observation.Balance
		spendable = &observation.Spendable
		dailyCap = observation.DailyCap
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE gamma_deposit_hotkey
				SET observed_block = $2, observed_at = $3,
				    deposit_coldkey = $4, deposit_spender = $5,
				    deposit_balance = $6, deposit_spendable = $7,
				    deposit_daily_cap = $8, hotkey_owner = $9
				WHERE hotkey = $1
			`,
			hotkey,
			observation.Block,
			now,
			coldkey,
			spender,
			balance,
			spendable,
			dailyCap,
			nullable(observation.HotkeyOwner),
		))
	})
}

// ListGammaHotkeysToObserve are the hotkeys the biller looks at each run:
// those with an open intent or a skipped charge, the hotkeys accounts pay
// automatically from, and those it has never looked at.
func ListGammaHotkeysToObserve(ctx context.Context, now time.Time, limit int) (hotkeys []string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT hotkey FROM (
					SELECT hotkey FROM gamma_payment_intent
					WHERE credited_at IS NULL AND voided_at IS NULL AND $1 < expires_at
					UNION
					SELECT hotkey FROM gamma_account WHERE hotkey IS NOT NULL
					UNION
					SELECT hotkey FROM gamma_deposit_hotkey WHERE observed_at IS NULL
				) t
				ORDER BY hotkey
				LIMIT $2
			`,
			now,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var hotkey string
				server.Raise(result.Scan(&hotkey))
				hotkeys = append(hotkeys, hotkey)
			}
		})
	})
	return
}

// AddGammaStrayTao counts plain TAO sent to one of our deposit hotkeys.
func AddGammaStrayTao(ctx context.Context, hotkey string, amountRao int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gamma_deposit_hotkey SET stray_tao_rao = stray_tao_rao + $2 WHERE hotkey = $1`,
			hotkey,
			amountRao,
		))
	})
}

// ----- accounts -----

// GammaAccount is an account that pays with gamma.
type GammaAccount struct {
	NetworkId server.Id
	// renew a plan paid with gamma at the end of its term
	AutoRenew bool
	// buy data when the balance runs out, and Pro at the client limit
	AutoTopup bool
	TopupItem string
	// the most automatic charges may take in a calendar month (UTC), in gamma
	// units; nil: no limit
	MonthlyLimit *int64
	// the plan a renewal or a seat upgrade buys
	RenewPlan string
	// the price tier of the last plan paid
	PriceTier string
	// the deposit hotkey automatic charges draw from: the one that paid last
	Hotkey           string
	LastBalanceBytes *int64
	LastBalanceAt    *time.Time
	SeatDemandAt     *time.Time
	CreateTime       time.Time
	UpdateTime       time.Time
}

const gammaAccountColumns = `
	network_id, auto_renew, auto_topup, topup_item, monthly_limit, renew_plan,
	price_tier, hotkey, last_balance_bytes, last_balance_at, seat_demand_at,
	create_time, update_time
`

func scanGammaAccount(result pgx.Rows) *GammaAccount {
	account := &GammaAccount{}
	var renewPlan, priceTier, hotkey *string
	server.Raise(result.Scan(
		&account.NetworkId,
		&account.AutoRenew,
		&account.AutoTopup,
		&account.TopupItem,
		&account.MonthlyLimit,
		&renewPlan,
		&priceTier,
		&hotkey,
		&account.LastBalanceBytes,
		&account.LastBalanceAt,
		&account.SeatDemandAt,
		&account.CreateTime,
		&account.UpdateTime,
	))
	if renewPlan != nil {
		account.RenewPlan = *renewPlan
	}
	if priceTier != nil {
		account.PriceTier = *priceTier
	}
	if hotkey != nil {
		account.Hotkey = *hotkey
	}
	return account
}

// GetGammaAccount is an account's gamma settings, or nil when it never paid
// with gamma nor set them.
func GetGammaAccount(ctx context.Context, networkId server.Id) (account *GammaAccount) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaAccountColumns+` FROM gamma_account WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				account = scanGammaAccount(result)
			}
		})
	})
	return
}

// ListGammaAccounts are the accounts that pay with gamma, a page at a time.
func ListGammaAccounts(ctx context.Context, after *server.Id, limit int) (accounts []*GammaAccount) {
	server.Db(ctx, func(conn server.PgConn) {
		var result pgx.Rows
		var err error
		if after == nil {
			result, err = conn.Query(
				ctx,
				`SELECT `+gammaAccountColumns+` FROM gamma_account WHERE hotkey IS NOT NULL ORDER BY network_id LIMIT $1`,
				limit,
			)
		} else {
			result, err = conn.Query(
				ctx,
				`SELECT `+gammaAccountColumns+` FROM gamma_account WHERE hotkey IS NOT NULL AND $1 < network_id ORDER BY network_id LIMIT $2`,
				*after,
				limit,
			)
		}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				accounts = append(accounts, scanGammaAccount(result))
			}
		})
	})
	return
}

// GammaAccountSettings are the switches the customer sets (nil: unchanged).
type GammaAccountSettings struct {
	AutoRenew *bool
	AutoTopup *bool
	TopupItem *string
	// set: the limit, with 0 or less clearing it
	MonthlyLimit *int64
}

// SetGammaAccountSettings applies the customer's switches, creating the
// account's row (both automatic switches on) when it has none.
func SetGammaAccountSettings(ctx context.Context, networkId server.Id, settings *GammaAccountSettings, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_account (network_id, create_time, update_time)
				VALUES ($1, $2, $2)
				ON CONFLICT (network_id) DO NOTHING
			`,
			networkId,
			now,
		))
		if settings.AutoRenew != nil {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE gamma_account SET auto_renew = $2, update_time = $3 WHERE network_id = $1`, networkId, *settings.AutoRenew, now))
		}
		if settings.AutoTopup != nil {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE gamma_account SET auto_topup = $2, update_time = $3 WHERE network_id = $1`, networkId, *settings.AutoTopup, now))
		}
		if settings.TopupItem != nil {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE gamma_account SET topup_item = $2, update_time = $3 WHERE network_id = $1`, networkId, *settings.TopupItem, now))
		}
		if settings.MonthlyLimit != nil {
			var limit *int64
			if 0 < *settings.MonthlyLimit {
				limit = settings.MonthlyLimit
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE gamma_account SET monthly_limit = $2, update_time = $3 WHERE network_id = $1`, networkId, limit, now))
		}
	})
}

// RecordGammaAccountPaymentInTx notes, in a credit's transaction, that the
// account paid with gamma: its row is made with both automatic switches on
// when it has none, and the hotkey (and for a plan, the plan and tier) that
// paid become what renewals and top-ups use.
func RecordGammaAccountPaymentInTx(tx server.PgTx, ctx context.Context, networkId server.Id, hotkey string, renewPlan string, priceTier string, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO gamma_account (network_id, hotkey, renew_plan, price_tier, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $5)
			ON CONFLICT (network_id) DO UPDATE SET
				hotkey = EXCLUDED.hotkey,
				renew_plan = COALESCE(EXCLUDED.renew_plan, gamma_account.renew_plan),
				price_tier = COALESCE(EXCLUDED.price_tier, gamma_account.price_tier),
				update_time = EXCLUDED.update_time
		`,
		networkId,
		hotkey,
		nullIfEmptyString(renewPlan),
		nullIfEmptyString(priceTier),
		now,
	))
}

// SetGammaAccountBalanceSample records the balance the top-up check saw.
func SetGammaAccountBalanceSample(ctx context.Context, networkId server.Id, balanceBytes int64, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gamma_account SET last_balance_bytes = $2, last_balance_at = $3 WHERE network_id = $1`,
			networkId,
			balanceBytes,
			now,
		))
	})
}

// NoteGammaSeatDemand records that a free account that pays with gamma hit
// its client limit. noted = false when the account does not pay with gamma.
func NoteGammaSeatDemand(ctx context.Context, networkId server.Id, now time.Time) (noted bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gamma_account SET seat_demand_at = $2 WHERE network_id = $1 AND hotkey IS NOT NULL AND auto_topup`,
			networkId,
			now,
		))
		noted = tag.RowsAffected() == 1
	})
	return
}

// ClearGammaSeatDemand ends a seat demand the biller has answered.
func ClearGammaSeatDemand(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE gamma_account SET seat_demand_at = NULL WHERE network_id = $1`, networkId))
	})
}

// HasSupporterRenewalEndingAfter reports whether the network has a supporter
// renewal in a market other than excludeMarket that ends after t: Pro paid
// elsewhere past a gamma term, which a gamma renewal must not double.
func HasSupporterRenewalEndingAfter(ctx context.Context, networkId server.Id, t time.Time, excludeMarket string) (has bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1 FROM subscription_renewal
					WHERE network_id = $1 AND subscription_type = $2 AND market <> $3 AND $4 < end_time
				)
			`,
			networkId,
			SubscriptionTypeSupporter,
			excludeMarket,
			t,
		).Scan(&has))
	})
	return
}

// ----- intents -----

// GammaPaymentIntent is an invoice payable from a deposit hotkey.
type GammaPaymentIntent struct {
	IntentId               server.Id
	StatusTokenHash        []byte
	ExtraStatusTokenHashes [][]byte
	NetworkId              server.Id
	Hotkey                 string
	Purpose                string
	// automatic intents: what period the intent is for; nil for a purchase
	PeriodKey *string
	// the public 32-byte spend reference, random, made with the intent
	Reference                []byte
	Delivery                 string
	Item                     string
	Tier                     string
	OfferApplied             bool
	PriceUsdNanoCents        NanoCents
	RegularPriceUsdNanoCents NanoCents
	Email                    string
	// a renewal: the new term starts when the old one ends
	TermStartTime      *time.Time
	CreateTime         time.Time
	ExpiresAt          time.Time
	CreditedAt         *time.Time
	BalanceId          *server.Id
	BalanceCodeId      *server.Id
	CreditFailureCount int
	VoidedAt           *time.Time
}

// AmountGamma is the price in gamma units: one gamma is one dollar.
func (self *GammaPaymentIntent) AmountGamma() int64 {
	return self.PriceUsdNanoCents
}

// Automatic reports whether the biller made the intent.
func (self *GammaPaymentIntent) Automatic() bool {
	return self.Purpose != GammaPurposePurchase
}

// MatchesStatusToken reports whether the hash of a presented status token is
// one this intent handed out, comparing every stored hash in constant time.
func (self *GammaPaymentIntent) MatchesStatusToken(tokenHash []byte) bool {
	match := 0
	for _, stored := range append([][]byte{self.StatusTokenHash}, self.ExtraStatusTokenHashes...) {
		match |= subtle.ConstantTimeCompare(stored, tokenHash)
	}
	return match == 1
}

const gammaPaymentIntentColumns = `
	intent_id, status_token_hash, extra_status_token_hashes, network_id,
	hotkey, purpose, period_key, reference, delivery, item, tier,
	offer_applied, price_usd_nano_cents, regular_price_usd_nano_cents, email,
	term_start_time, create_time, expires_at, credited_at, balance_id,
	balance_code_id, credit_failure_count, voided_at
`

func scanGammaPaymentIntent(result pgx.Rows) *GammaPaymentIntent {
	intent := &GammaPaymentIntent{}
	var tier, email *string
	server.Raise(result.Scan(
		&intent.IntentId,
		&intent.StatusTokenHash,
		&intent.ExtraStatusTokenHashes,
		&intent.NetworkId,
		&intent.Hotkey,
		&intent.Purpose,
		&intent.PeriodKey,
		&intent.Reference,
		&intent.Delivery,
		&intent.Item,
		&tier,
		&intent.OfferApplied,
		&intent.PriceUsdNanoCents,
		&intent.RegularPriceUsdNanoCents,
		&email,
		&intent.TermStartTime,
		&intent.CreateTime,
		&intent.ExpiresAt,
		&intent.CreditedAt,
		&intent.BalanceId,
		&intent.BalanceCodeId,
		&intent.CreditFailureCount,
		&intent.VoidedAt,
	))
	if tier != nil {
		intent.Tier = *tier
	}
	if email != nil {
		intent.Email = *email
	}
	return intent
}

// CreateGammaPaymentIntent records a new intent. created = false when an
// automatic intent for the same account, purpose and period already exists:
// the period's reference was already made, and is never made twice.
func CreateGammaPaymentIntent(ctx context.Context, intent *GammaPaymentIntent) (created bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_payment_intent (
					intent_id, status_token_hash, network_id, hotkey, purpose,
					period_key, reference, delivery, item, tier, offer_applied,
					price_usd_nano_cents, regular_price_usd_nano_cents, email,
					term_start_time, create_time, expires_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
				ON CONFLICT DO NOTHING
			`,
			intent.IntentId,
			intent.StatusTokenHash,
			intent.NetworkId,
			intent.Hotkey,
			intent.Purpose,
			intent.PeriodKey,
			intent.Reference,
			intent.Delivery,
			intent.Item,
			nullIfEmptyString(intent.Tier),
			intent.OfferApplied,
			intent.PriceUsdNanoCents,
			intent.RegularPriceUsdNanoCents,
			nullIfEmptyString(intent.Email),
			intent.TermStartTime,
			intent.CreateTime,
			intent.ExpiresAt,
		))
		created = tag.RowsAffected() == 1
	})
	return
}

// GetGammaPaymentIntent is an intent, or nil.
func GetGammaPaymentIntent(ctx context.Context, intentId server.Id) (intent *GammaPaymentIntent) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaPaymentIntentColumns+` FROM gamma_payment_intent WHERE intent_id = $1`,
			intentId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				intent = scanGammaPaymentIntent(result)
			}
		})
	})
	return
}

// GetGammaPaymentIntentsByReferences are the intents with the given
// references, by reference (as a string of its bytes).
func GetGammaPaymentIntentsByReferences(ctx context.Context, references [][]byte) map[string]*GammaPaymentIntent {
	out := map[string]*GammaPaymentIntent{}
	if len(references) == 0 {
		return out
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaPaymentIntentColumns+` FROM gamma_payment_intent WHERE reference = ANY($1)`,
			references,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intent := scanGammaPaymentIntent(result)
				out[string(intent.Reference)] = intent
			}
		})
	})
	return out
}

// GetOpenGammaPaymentIntentForReuse is the network's open purchase of the
// same item and delivery from the same hotkey: asking again returns it rather
// than a second intent the same deposit would also pay.
func GetOpenGammaPaymentIntentForReuse(ctx context.Context, networkId server.Id, hotkey string, item string, delivery string, now time.Time) (intent *GammaPaymentIntent) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaPaymentIntentColumns+`
			FROM gamma_payment_intent
			WHERE network_id = $1 AND hotkey = $2 AND item = $3 AND delivery = $4
			  AND purpose = 'purchase'
			  AND credited_at IS NULL AND voided_at IS NULL AND $5 < expires_at
			ORDER BY create_time DESC
			LIMIT 1`,
			networkId,
			hotkey,
			item,
			delivery,
			now,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				intent = scanGammaPaymentIntent(result)
			}
		})
	})
	return
}

// CountOpenGammaPaymentIntents is how many unpaid purchases a network has
// open.
func CountOpenGammaPaymentIntents(ctx context.Context, networkId server.Id, now time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*) FROM gamma_payment_intent
				WHERE network_id = $1 AND purpose = 'purchase'
				  AND credited_at IS NULL AND voided_at IS NULL AND $2 < expires_at
			`,
			networkId,
			now,
		).Scan(&count))
	})
	return
}

// AddGammaPaymentIntentStatusToken hands out an intent again with a new
// token; every token handed out stays valid.
func AddGammaPaymentIntentStatusToken(ctx context.Context, intentId server.Id, tokenHash []byte) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE gamma_payment_intent
				SET extra_status_token_hashes = array_append(extra_status_token_hashes, $2)
				WHERE intent_id = $1 AND cardinality(extra_status_token_hashes) < 64
			`,
			intentId,
			tokenHash,
		))
	})
}

// ListChargeableGammaPaymentIntents are the open intents with no charge yet,
// oldest first: what the biller may charge next.
func ListChargeableGammaPaymentIntents(ctx context.Context, now time.Time, limit int) (intents []*GammaPaymentIntent) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaPaymentIntentColumns+`
			FROM gamma_payment_intent
			WHERE credited_at IS NULL AND voided_at IS NULL AND $1 < expires_at
			  AND NOT EXISTS (
				SELECT 1 FROM gamma_charge WHERE gamma_charge.intent_id = gamma_payment_intent.intent_id
			  )
			ORDER BY create_time
			LIMIT $2`,
			now,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intents = append(intents, scanGammaPaymentIntent(result))
			}
		})
	})
	return
}

// GetLatestGammaPaymentIntent is the account's newest intent of a purpose, or
// nil.
func GetLatestGammaPaymentIntent(ctx context.Context, networkId server.Id, purpose string) (intent *GammaPaymentIntent) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaPaymentIntentColumns+`
			FROM gamma_payment_intent
			WHERE network_id = $1 AND purpose = $2
			ORDER BY create_time DESC
			LIMIT 1`,
			networkId,
			purpose,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				intent = scanGammaPaymentIntent(result)
			}
		})
	})
	return
}

// SumGammaAutomaticSpend is what the account's automatic intents made since
// a time have taken or may still take: those credited, and those whose charge
// is pending or spent.
func SumGammaAutomaticSpend(ctx context.Context, networkId server.Id, since time.Time) (total int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COALESCE(SUM(gamma_payment_intent.price_usd_nano_cents), 0)
				FROM gamma_payment_intent
				LEFT JOIN gamma_charge ON gamma_charge.intent_id = gamma_payment_intent.intent_id
				WHERE gamma_payment_intent.network_id = $1
				  AND gamma_payment_intent.purpose <> 'purchase'
				  AND $2 <= gamma_payment_intent.create_time
				  AND (gamma_payment_intent.credited_at IS NOT NULL OR gamma_charge.state IN ('pending', 'spent'))
			`,
			networkId,
			since,
		).Scan(&total))
	})
	return
}

// MarkGammaPaymentIntentCreditedInTx is the intent's one-shot: credited =
// false when it was already credited or voided, and nothing may be written.
func MarkGammaPaymentIntentCreditedInTx(tx server.PgTx, ctx context.Context, intentId server.Id, now time.Time) (credited bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gamma_payment_intent
			SET credited_at = $2
			WHERE intent_id = $1 AND credited_at IS NULL AND voided_at IS NULL
		`,
		intentId,
		now,
	))
	return tag.RowsAffected() == 1
}

// SetGammaPaymentIntentGrantInTx records what the credit granted.
func SetGammaPaymentIntentGrantInTx(tx server.PgTx, ctx context.Context, intentId server.Id, balanceId *server.Id, balanceCodeId *server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE gamma_payment_intent SET balance_id = $2, balance_code_id = $3 WHERE intent_id = $1`,
		intentId,
		balanceId,
		balanceCodeId,
	))
}

// AddGammaPaymentIntentCreditFailure counts a credit that failed and returns
// the count.
func AddGammaPaymentIntentCreditFailure(ctx context.Context, intentId server.Id) (count int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(tx.QueryRow(
			ctx,
			`
				UPDATE gamma_payment_intent
				SET credit_failure_count = credit_failure_count + 1
				WHERE intent_id = $1
				RETURNING credit_failure_count
			`,
			intentId,
		).Scan(&count))
	})
	return
}

// GammaPaymentPurchaseEventId is the balance-code purchase event of an
// intent's code: one code per intent.
func GammaPaymentPurchaseEventId(intentId server.Id) string {
	return "gamma/" + intentId.String()
}

// ----- charges -----

// GammaCharge is an intent's spend entry.
type GammaCharge struct {
	Reference  []byte
	IntentId   server.Id
	Hotkey     string
	Amount     int64
	State      string
	BatchId    *server.Id
	Attempts   int
	CreateTime time.Time
	ResolvedAt *time.Time
	// where the finalized event that resolved it is
	BlockNumber *int64
	BlockHash   *string
	EventIndex  *int
	SkipReason  string
	// the deposit's fingerprint when it was skipped
	SkipDeposit  string
	NoticeSentAt *time.Time
}

const gammaChargeColumns = `
	reference, intent_id, hotkey, amount, state, batch_id, attempts,
	create_time, resolved_at, block_number, block_hash, event_index,
	skip_reason, skip_deposit, notice_sent_at
`

func scanGammaCharge(result pgx.Rows) *GammaCharge {
	charge := &GammaCharge{}
	var skipReason, skipDeposit *string
	server.Raise(result.Scan(
		&charge.Reference,
		&charge.IntentId,
		&charge.Hotkey,
		&charge.Amount,
		&charge.State,
		&charge.BatchId,
		&charge.Attempts,
		&charge.CreateTime,
		&charge.ResolvedAt,
		&charge.BlockNumber,
		&charge.BlockHash,
		&charge.EventIndex,
		&skipReason,
		&skipDeposit,
		&charge.NoticeSentAt,
	))
	if skipReason != nil {
		charge.SkipReason = *skipReason
	}
	if skipDeposit != nil {
		charge.SkipDeposit = *skipDeposit
	}
	return charge
}

// CreateGammaCharge records an intent's charge, with the intent's own
// reference, before anything is signed. created = false when the intent
// already has its charge, or is no longer open.
func CreateGammaCharge(ctx context.Context, intent *GammaPaymentIntent, now time.Time) (created bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_charge (reference, intent_id, hotkey, amount, state, create_time)
				SELECT reference, intent_id, hotkey, price_usd_nano_cents, 'pending', $2
				FROM gamma_payment_intent
				WHERE intent_id = $1 AND credited_at IS NULL AND voided_at IS NULL AND $2 < expires_at
				ON CONFLICT DO NOTHING
			`,
			intent.IntentId,
			now,
		))
		created = tag.RowsAffected() == 1
	})
	return
}

// GetGammaChargeForIntent is an intent's charge, or nil.
func GetGammaChargeForIntent(ctx context.Context, intentId server.Id) (charge *GammaCharge) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaChargeColumns+` FROM gamma_charge WHERE intent_id = $1`,
			intentId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				charge = scanGammaCharge(result)
			}
		})
	})
	return
}

// ListUnbatchedGammaCharges are the pending charges in no batch, oldest
// first.
func ListUnbatchedGammaCharges(ctx context.Context, limit int) (charges []*GammaCharge) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaChargeColumns+`
			FROM gamma_charge
			WHERE state = 'pending' AND batch_id IS NULL
			ORDER BY create_time
			LIMIT $1`,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				charges = append(charges, scanGammaCharge(result))
			}
		})
	})
	return
}

// ListGammaChargesForBatch are a batch's charges.
func ListGammaChargesForBatch(ctx context.Context, batchId server.Id) (charges []*GammaCharge) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaChargeColumns+` FROM gamma_charge WHERE batch_id = $1 ORDER BY create_time`,
			batchId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				charges = append(charges, scanGammaCharge(result))
			}
		})
	})
	return
}

// ListSkippedGammaCharges are the skipped and failed charges of intents that
// are still open: the ones the biller may re-arm once their deposit changes.
func ListSkippedGammaCharges(ctx context.Context, now time.Time, limit int) (charges []*GammaCharge) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT gamma_charge.reference, gamma_charge.intent_id, gamma_charge.hotkey,
			       gamma_charge.amount, gamma_charge.state, gamma_charge.batch_id,
			       gamma_charge.attempts, gamma_charge.create_time, gamma_charge.resolved_at,
			       gamma_charge.block_number, gamma_charge.block_hash, gamma_charge.event_index,
			       gamma_charge.skip_reason, gamma_charge.skip_deposit, gamma_charge.notice_sent_at
			FROM gamma_charge
			INNER JOIN gamma_payment_intent ON gamma_payment_intent.intent_id = gamma_charge.intent_id
			WHERE gamma_charge.state IN ('skipped', 'failed')
			  AND gamma_payment_intent.credited_at IS NULL
			  AND gamma_payment_intent.voided_at IS NULL
			  AND $1 < gamma_payment_intent.expires_at
			ORDER BY gamma_charge.create_time
			LIMIT $2`,
			now,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				charges = append(charges, scanGammaCharge(result))
			}
		})
	})
	return
}

// GetGammaChargesByReferences are the charges with the given references, by
// reference (as a string of its bytes).
func GetGammaChargesByReferences(ctx context.Context, references [][]byte) map[string]*GammaCharge {
	out := map[string]*GammaCharge{}
	if len(references) == 0 {
		return out
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaChargeColumns+` FROM gamma_charge WHERE reference = ANY($1)`,
			references,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				charge := scanGammaCharge(result)
				out[string(charge.Reference)] = charge
			}
		})
	})
	return out
}

// SumPendingGammaCharges is the total of the pending charges on each hotkey.
func SumPendingGammaCharges(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT hotkey, SUM(amount)::bigint FROM gamma_charge WHERE state = 'pending' GROUP BY hotkey`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var hotkey string
				var total int64
				server.Raise(result.Scan(&hotkey, &total))
				out[hotkey] = total
			}
		})
	})
	return out
}

// GammaChargeResolution is the finalized event that resolved a charge.
type GammaChargeResolution struct {
	State       string
	BlockNumber int64
	BlockHash   string
	EventIndex  int
	SkipReason  string
	// a skip: the deposit's fingerprint when the charge was batched
	SkipDeposit string
}

// ResolveGammaChargeInTx moves a pending charge to spent or skipped, out of
// its batch. resolved = false when it was not pending.
func ResolveGammaChargeInTx(tx server.PgTx, ctx context.Context, reference []byte, resolution *GammaChargeResolution, now time.Time) (resolved bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gamma_charge
			SET state = $2, resolved_at = $3, block_number = $4, block_hash = $5,
			    event_index = $6, skip_reason = $7, skip_deposit = $8,
			    attempts = attempts + 1
			WHERE reference = $1 AND state = 'pending'
		`,
		reference,
		resolution.State,
		now,
		resolution.BlockNumber,
		resolution.BlockHash,
		resolution.EventIndex,
		nullIfEmptyString(resolution.SkipReason),
		nullIfEmptyString(resolution.SkipDeposit),
	))
	return tag.RowsAffected() == 1
}

// ReArmGammaCharge makes a skipped or failed charge pending again, its
// reference unchanged, out of any batch. re-armed = false when it was not
// skipped or failed.
func ReArmGammaCharge(ctx context.Context, reference []byte) (reArmed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE gamma_charge
				SET state = 'pending', batch_id = NULL, resolved_at = NULL,
				    block_number = NULL, block_hash = NULL, event_index = NULL,
				    skip_deposit = NULL
				WHERE reference = $1 AND state IN ('skipped', 'failed')
			`,
			reference,
		))
		reArmed = tag.RowsAffected() == 1
	})
	return
}

// SetGammaChargeSkipDeposit records a skipped charge's deposit as first
// observed after the skip, once.
func SetGammaChargeSkipDeposit(ctx context.Context, reference []byte, fingerprint string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gamma_charge SET skip_deposit = $2 WHERE reference = $1 AND state IN ('skipped', 'failed') AND skip_deposit IS NULL`,
			reference,
			fingerprint,
		))
	})
}

// MarkGammaChargeNoticeSent records that the skip notice went out; sent =
// false when it already had.
func MarkGammaChargeNoticeSentInTx(tx server.PgTx, ctx context.Context, reference []byte, now time.Time) (marked bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE gamma_charge SET notice_sent_at = $2 WHERE reference = $1 AND notice_sent_at IS NULL`,
		reference,
		now,
	))
	return tag.RowsAffected() == 1
}

// ListSpentUncreditedGammaCharges are spent charges whose intent is not yet
// credited (nor voided), oldest first.
func ListSpentUncreditedGammaCharges(ctx context.Context, limit int) (charges []*GammaCharge) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT gamma_charge.reference, gamma_charge.intent_id, gamma_charge.hotkey,
			       gamma_charge.amount, gamma_charge.state, gamma_charge.batch_id,
			       gamma_charge.attempts, gamma_charge.create_time, gamma_charge.resolved_at,
			       gamma_charge.block_number, gamma_charge.block_hash, gamma_charge.event_index,
			       gamma_charge.skip_reason, gamma_charge.skip_deposit, gamma_charge.notice_sent_at
			FROM gamma_charge
			INNER JOIN gamma_payment_intent ON gamma_payment_intent.intent_id = gamma_charge.intent_id
			WHERE gamma_charge.state = 'spent'
			  AND gamma_payment_intent.credited_at IS NULL
			  AND gamma_payment_intent.voided_at IS NULL
			ORDER BY gamma_charge.create_time
			LIMIT $1`,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				charges = append(charges, scanGammaCharge(result))
			}
		})
	})
	return
}

// ----- spend batches -----

// GammaSpendBatch is one signed spend extrinsic.
type GammaSpendBatch struct {
	BatchId        server.Id
	Nonce          int64
	EraBirth       int64
	EraPeriod      int
	Extrinsic      []byte
	ExtrinsicHash  string
	State          string
	CreateTime     time.Time
	SubmittedAt    *time.Time
	ResolvedAt     *time.Time
	BlockNumber    *int64
	BlockHash      *string
	ExtrinsicIndex *int
	FeeRao         *int64
	Note           string
}

const gammaSpendBatchColumns = `
	batch_id, nonce, era_birth, era_period, extrinsic, extrinsic_hash, state,
	create_time, submitted_at, resolved_at, block_number, block_hash,
	extrinsic_index, fee_rao, note
`

func scanGammaSpendBatch(result pgx.Rows) *GammaSpendBatch {
	batch := &GammaSpendBatch{}
	var note *string
	server.Raise(result.Scan(
		&batch.BatchId,
		&batch.Nonce,
		&batch.EraBirth,
		&batch.EraPeriod,
		&batch.Extrinsic,
		&batch.ExtrinsicHash,
		&batch.State,
		&batch.CreateTime,
		&batch.SubmittedAt,
		&batch.ResolvedAt,
		&batch.BlockNumber,
		&batch.BlockHash,
		&batch.ExtrinsicIndex,
		&batch.FeeRao,
		&note,
	))
	if note != nil {
		batch.Note = *note
	}
	return batch
}

// ErrGammaBatchLive: another batch is still live, or a charge was taken by
// another batch; nothing was written.
var ErrGammaBatchLive = errors.New("a gamma spend batch is already live, or a charge is no longer free")

// CreateGammaSpendBatch records a signed batch and assigns its charges, in
// one transaction, before the batch is ever submitted. It fails, writing
// nothing, when a batch is already live or any charge is no longer a pending
// charge in no batch.
func CreateGammaSpendBatch(ctx context.Context, batch *GammaSpendBatch, references [][]byte) (returnErr error) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var live int
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM gamma_spend_batch WHERE state IN ('signed', 'submitted')`,
			).Scan(&live))
			if 0 < live {
				panic(ErrGammaBatchLive)
			}
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO gamma_spend_batch (
						batch_id, nonce, era_birth, era_period, extrinsic, extrinsic_hash,
						state, create_time
					)
					VALUES ($1, $2, $3, $4, $5, $6, 'signed', $7)
					ON CONFLICT DO NOTHING
				`,
				batch.BatchId,
				batch.Nonce,
				batch.EraBirth,
				batch.EraPeriod,
				batch.Extrinsic,
				batch.ExtrinsicHash,
				batch.CreateTime,
			))
			if tag.RowsAffected() != 1 {
				panic(ErrGammaBatchLive)
			}
			tag = server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE gamma_charge SET batch_id = $1
					WHERE reference = ANY($2) AND state = 'pending' AND batch_id IS NULL
				`,
				batch.BatchId,
				references,
			))
			if tag.RowsAffected() != int64(len(references)) {
				// rolls the batch row back with the charges
				panic(ErrGammaBatchLive)
			}
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// GetLiveGammaSpendBatch is the batch that is signed or submitted, or nil.
func GetLiveGammaSpendBatch(ctx context.Context) (batch *GammaSpendBatch) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaSpendBatchColumns+` FROM gamma_spend_batch WHERE state IN ('signed', 'submitted')`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				batch = scanGammaSpendBatch(result)
			}
		})
	})
	return
}

// GetGammaSpendBatchByExtrinsicHash is the batch an extrinsic is, or nil.
func GetGammaSpendBatchByExtrinsicHash(ctx context.Context, extrinsicHash string) (batch *GammaSpendBatch) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaSpendBatchColumns+` FROM gamma_spend_batch WHERE extrinsic_hash = $1`,
			extrinsicHash,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				batch = scanGammaSpendBatch(result)
			}
		})
	})
	return
}

// MarkGammaSpendBatchSubmitted records that the pool took the batch.
func MarkGammaSpendBatchSubmitted(ctx context.Context, batchId server.Id, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gamma_spend_batch SET state = 'submitted', submitted_at = $2 WHERE batch_id = $1 AND state = 'signed'`,
			batchId,
			now,
		))
	})
}

// GammaBatchInclusion is where a batch landed.
type GammaBatchInclusion struct {
	BlockNumber    int64
	BlockHash      string
	ExtrinsicIndex int
	FeeRao         int64
}

// IncludeGammaSpendBatchInTx records where a live batch landed, in the
// transaction that resolved its charges from their own events. A charge of
// the batch still pending is failed: the batch landed and is final, so it
// spent nothing for that charge and never will; it is re-armed like a skip.
func IncludeGammaSpendBatchInTx(tx server.PgTx, ctx context.Context, batchId server.Id, inclusion *GammaBatchInclusion, note string, now time.Time) (included bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gamma_spend_batch
			SET state = 'included', resolved_at = $2, block_number = $3, block_hash = $4,
			    extrinsic_index = $5, fee_rao = $6, note = $7
			WHERE batch_id = $1 AND state IN ('signed', 'submitted')
		`,
		batchId,
		now,
		inclusion.BlockNumber,
		inclusion.BlockHash,
		inclusion.ExtrinsicIndex,
		inclusion.FeeRao,
		nullIfEmptyString(note),
	))
	if tag.RowsAffected() != 1 {
		return false
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gamma_charge
			SET state = 'failed', resolved_at = $2, block_number = $3, block_hash = $4,
			    skip_reason = 'spend_failed', attempts = attempts + 1
			WHERE batch_id = $1 AND state = 'pending'
		`,
		batchId,
		now,
		inclusion.BlockNumber,
		inclusion.BlockHash,
	))
	return true
}

// ExpireGammaSpendBatch ends a batch whose era passed without it landing:
// its charges are released, references unchanged, to the next batch.
func ExpireGammaSpendBatch(ctx context.Context, batchId server.Id, note string, now time.Time) (expired bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE gamma_spend_batch
				SET state = 'expired', resolved_at = $2, note = $3
				WHERE batch_id = $1 AND state IN ('signed', 'submitted')
			`,
			batchId,
			now,
			nullIfEmptyString(note),
		))
		if tag.RowsAffected() != 1 {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gamma_charge SET batch_id = NULL WHERE batch_id = $1 AND state = 'pending'`,
			batchId,
		))
		expired = true
	})
	return
}

// ReviewGammaSpendBatch holds a live batch for an operator: its charges stay
// pending and in the batch, so nothing re-batches them, and the biller signs
// nothing while any batch is in review.
func ReviewGammaSpendBatch(ctx context.Context, batchId server.Id, note string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE gamma_spend_batch
				SET state = 'review', resolved_at = $2, note = $3
				WHERE batch_id = $1 AND state IN ('signed', 'submitted')
			`,
			batchId,
			now,
			nullIfEmptyString(note),
		))
	})
}

// CountGammaSpendBatchesInReview is how many batches wait for an operator.
func CountGammaSpendBatchesInReview(ctx context.Context) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'review'`).Scan(&count))
	})
	return
}

// GetLastIncludedGammaSpendBatch is the included batch with the highest
// nonce, or nil.
func GetLastIncludedGammaSpendBatch(ctx context.Context) (batch *GammaSpendBatch) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+gammaSpendBatchColumns+` FROM gamma_spend_batch WHERE state = 'included' ORDER BY nonce DESC LIMIT 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				batch = scanGammaSpendBatch(result)
			}
		})
	})
	return
}

// ----- the watcher's state -----

// GammaChainState is the watcher's cursor and what the terms endpoint shows.
type GammaChainState struct {
	ScannedBlock   *int64
	ScannedHash    *string
	FinalizedBlock *int64
	RuntimeSpec    *int
	// "ready", or why selling is paused
	Readiness string
	// "ready", or why minting fails now
	MintReadiness string
	Terms         json.RawMessage
	UpdateTime    time.Time
}

// GetGammaChainState is the watcher's state, or nil before its first run.
func GetGammaChainState(ctx context.Context) (state *GammaChainState) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT scanned_block, scanned_hash, finalized_block, runtime_spec,
				       readiness, mint_readiness, terms, update_time
				FROM gamma_chain_state
				WHERE singleton
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				state = &GammaChainState{}
				var terms []byte
				server.Raise(result.Scan(
					&state.ScannedBlock,
					&state.ScannedHash,
					&state.FinalizedBlock,
					&state.RuntimeSpec,
					&state.Readiness,
					&state.MintReadiness,
					&terms,
					&state.UpdateTime,
				))
				state.Terms = terms
			}
		})
	})
	return
}

// SetGammaChainState records the watcher's state.
func SetGammaChainState(ctx context.Context, state *GammaChainState) {
	var terms any
	if 0 < len(state.Terms) {
		terms = string(state.Terms)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_chain_state (
					singleton, scanned_block, scanned_hash, finalized_block, runtime_spec,
					readiness, mint_readiness, terms, update_time
				)
				VALUES (true, $1, $2, $3, $4, $5, $6, $7::jsonb, $8)
				ON CONFLICT (singleton) DO UPDATE SET
					scanned_block = EXCLUDED.scanned_block,
					scanned_hash = EXCLUDED.scanned_hash,
					finalized_block = EXCLUDED.finalized_block,
					runtime_spec = EXCLUDED.runtime_spec,
					readiness = EXCLUDED.readiness,
					mint_readiness = EXCLUDED.mint_readiness,
					terms = EXCLUDED.terms,
					update_time = EXCLUDED.update_time
			`,
			state.ScannedBlock,
			state.ScannedHash,
			state.FinalizedBlock,
			state.RuntimeSpec,
			state.Readiness,
			state.MintReadiness,
			terms,
			state.UpdateTime,
		))
	})
}

// ----- alarms -----

// GammaAlarm is one recorded alarm.
type GammaAlarm struct {
	AlarmKey    string
	Kind        string
	BlockNumber *int64
	Detail      string
	CreateTime  time.Time
}

// AddGammaAlarm records an alarm once per key. added = false when the key
// was already recorded.
func AddGammaAlarm(ctx context.Context, alarmKey string, kind string, blockNumber *int64, detail string, now time.Time) (added bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gamma_alarm (alarm_key, kind, block_number, detail, create_time)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`,
			alarmKey,
			kind,
			blockNumber,
			detail,
			now,
		))
		added = tag.RowsAffected() == 1
	})
	return
}

// ListGammaAlarms are the alarms since a time, newest first.
func ListGammaAlarms(ctx context.Context, since time.Time, limit int) (alarms []*GammaAlarm) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT alarm_key, kind, block_number, detail, create_time FROM gamma_alarm WHERE $1 <= create_time ORDER BY create_time DESC LIMIT $2`,
			since,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				alarm := &GammaAlarm{}
				server.Raise(result.Scan(&alarm.AlarmKey, &alarm.Kind, &alarm.BlockNumber, &alarm.Detail, &alarm.CreateTime))
				alarms = append(alarms, alarm)
			}
		})
	})
	return
}
