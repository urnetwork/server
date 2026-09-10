package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The welcome offer (mmm/onboarding/PLAN.md "THE OFFER"): 25% off the first year
// of Pro, issued ONCE per network, valid for onboarding.yml offer.validity_days,
// redeemable on every store while unexpired and unredeemed. The row is the single
// source of truth every purchase path consults and stamps.

// Who issued the offer.
const (
	OnboardingOfferIssuedByInApp = "in_app"
	OnboardingOfferIssuedByEmail = "email"
)

// Offer surfaces (where it was issued from / shown).
const (
	OnboardingOfferSurfaceIntroStep   = "intro_step"
	OnboardingOfferSurfaceFinalScreen = "final_screen"
	OnboardingOfferSurfaceEmailLink   = "email_link"
	OnboardingOfferSurfaceAccount     = "account"
)

// Offer states, as reported by OnboardingOfferState.
const (
	OnboardingOfferStateNone     = "none"
	OnboardingOfferStateActive   = "active"
	OnboardingOfferStateRedeemed = "redeemed"
	OnboardingOfferStateExpired  = "expired"
)

// Stores an offer can be redeemed on (the `store` column and the purchase
// events' `store` prop share these values).
const (
	OnboardingStoreApple  = "apple"
	OnboardingStorePlay   = "play"
	OnboardingStoreStripe = "stripe"
	OnboardingStoreSolana = "solana"
)

type OnboardingOffer struct {
	NetworkId  server.Id
	IssuedAt   time.Time
	ExpiresAt  time.Time
	IssuedBy   string
	Surface    string
	Tier       string
	PercentOff int
	MonthsFree int
	RedeemedAt *time.Time
	Store      *string
	// the redemption handles per store, fixed at issue time
	StripeCouponId *string
	AppleOfferCode *string
	PlayOfferTag   *string
}

// OnboardingOfferState is the offer's state at `now`: none (nil), redeemed,
// expired or active. Redeemed wins over expired: a redemption that happened
// before the expiry stays a redemption.
func OnboardingOfferState(offer *OnboardingOffer, now time.Time) string {
	if offer == nil {
		return OnboardingOfferStateNone
	}
	if offer.RedeemedAt != nil {
		return OnboardingOfferStateRedeemed
	}
	if !now.Before(offer.ExpiresAt) {
		return OnboardingOfferStateExpired
	}
	return OnboardingOfferStateActive
}

// Eligible reports whether the offer can be redeemed at `now`: issued, unexpired
// and unredeemed.
func (o *OnboardingOffer) Eligible(now time.Time) bool {
	return OnboardingOfferState(o, now) == OnboardingOfferStateActive
}

type IssueOnboardingOfferArgs struct {
	NetworkId  server.Id
	IssuedBy   string
	Surface    string
	Tier       string
	PercentOff int
	MonthsFree int
	Validity   time.Duration
	// the redemption handles; any may be empty
	StripeCouponId string
	PlayOfferTag   string
	// AppleOfferCode is the fallback custom code; a one-time code from the pool
	// is preferred when one is available
	AppleOfferCode string
	// AppleOfferCodePool draws a one-time code from network_onboarding_apple_offer_code
	AppleOfferCodePool bool
}

var ErrOnboardingOfferNotFound = errors.New("no onboarding offer")

// IssueOnboardingOffer issues the network's offer once. A second call returns the
// existing row unchanged (created = false) whatever its state -- expired offers
// are never re-issued, which is the whole point of a deadline.
func IssueOnboardingOffer(ctx context.Context, args *IssueOnboardingOfferArgs) (offer *OnboardingOffer, created bool, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		offer, created, returnErr = IssueOnboardingOfferInTx(tx, ctx, args)
	}, server.TxReadCommitted)
	return
}

var ErrOnboardingNetworkNotFound = errors.New("network does not exist")

// lockOnboardingNetworkInTx takes a key-share lock on the network row for the
// rest of the tx, so an offer is never issued to a network being deleted, and
// reports whether the network exists.
func lockOnboardingNetworkInTx(tx server.PgTx, ctx context.Context, networkId server.Id) bool {
	var lockedNetworkId server.Id
	err := tx.QueryRow(
		ctx,
		`
			SELECT network_id
			FROM network
			WHERE network_id = $1
			FOR KEY SHARE
		`,
		networkId,
	).Scan(&lockedNetworkId)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	server.Raise(err)
	return true
}

func IssueOnboardingOfferInTx(tx server.PgTx, ctx context.Context, args *IssueOnboardingOfferArgs) (*OnboardingOffer, bool, error) {
	if !lockOnboardingNetworkInTx(tx, ctx, args.NetworkId) {
		return nil, false, ErrOnboardingNetworkNotFound
	}
	if existing := GetOnboardingOfferInTx(tx, ctx, args.NetworkId); existing != nil {
		return existing, false, nil
	}
	now := server.NowUtc()
	expiresAt := now.Add(args.Validity)

	appleOfferCode := strings.TrimSpace(args.AppleOfferCode)
	if args.AppleOfferCodePool {
		if code, ok := assignAppleOfferCodeInTx(tx, ctx, args.NetworkId, expiresAt); ok {
			appleOfferCode = code
		}
	}

	tag, err := tx.Exec(
		ctx,
		`
			INSERT INTO network_onboarding_offer (
				network_id,
				issued_at,
				expires_at,
				issued_by,
				surface,
				tier,
				percent_off,
				months_free,
				stripe_coupon_id,
				apple_offer_code,
				play_offer_tag
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (network_id) DO NOTHING
		`,
		args.NetworkId,
		now,
		expiresAt,
		args.IssuedBy,
		args.Surface,
		args.Tier,
		args.PercentOff,
		args.MonthsFree,
		nullIfEmpty(args.StripeCouponId),
		nullIfEmpty(appleOfferCode),
		nullIfEmpty(args.PlayOfferTag),
	)
	if err != nil {
		return nil, false, err
	}
	offer := GetOnboardingOfferInTx(tx, ctx, args.NetworkId)
	if offer == nil {
		return nil, false, ErrOnboardingOfferNotFound
	}
	return offer, tag.RowsAffected() == 1, nil
}

func nullIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// GetOnboardingOffer reads the network's offer, or nil when none was issued.
func GetOnboardingOffer(ctx context.Context, networkId server.Id) (offer *OnboardingOffer) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, onboardingOfferSelect, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				offer = scanOnboardingOffer(result)
			}
		})
	})
	return
}

func GetOnboardingOfferInTx(tx server.PgTx, ctx context.Context, networkId server.Id) (offer *OnboardingOffer) {
	result, err := tx.Query(ctx, onboardingOfferSelect, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			offer = scanOnboardingOffer(result)
		}
	})
	return
}

const onboardingOfferSelect = `
	SELECT
		network_id,
		issued_at,
		expires_at,
		issued_by,
		surface,
		tier,
		percent_off,
		months_free,
		redeemed_at,
		store,
		stripe_coupon_id,
		apple_offer_code,
		play_offer_tag
	FROM network_onboarding_offer
	WHERE network_id = $1
`

func scanOnboardingOffer(result pgx.Rows) *OnboardingOffer {
	offer := &OnboardingOffer{}
	server.Raise(result.Scan(
		&offer.NetworkId,
		&offer.IssuedAt,
		&offer.ExpiresAt,
		&offer.IssuedBy,
		&offer.Surface,
		&offer.Tier,
		&offer.PercentOff,
		&offer.MonthsFree,
		&offer.RedeemedAt,
		&offer.Store,
		&offer.StripeCouponId,
		&offer.AppleOfferCode,
		&offer.PlayOfferTag,
	))
	return offer
}

// RedeemOnboardingOfferInTx stamps the offer redeemed on `store` if it is still
// eligible. Returns true only for the call that redeemed it; a second redemption
// (a renewal, a redelivered webhook, a second store) changes nothing.
func RedeemOnboardingOfferInTx(tx server.PgTx, ctx context.Context, networkId server.Id, store string, now time.Time) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_onboarding_offer
			SET redeemed_at = $2, store = $3
			WHERE network_id = $1 AND redeemed_at IS NULL AND $2 < expires_at
		`,
		networkId,
		now,
		store,
	))
	return tag.RowsAffected() == 1
}

// RedeemOnboardingOffer is RedeemOnboardingOfferInTx in its own transaction.
func RedeemOnboardingOffer(ctx context.Context, networkId server.Id, store string, now time.Time) (redeemed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		redeemed = RedeemOnboardingOfferInTx(tx, ctx, networkId, store, now)
	}, server.TxReadCommitted)
	return
}

// OnboardingPathForNetwork is the campaign path the network is on, derived from
// the offer row: "A" once the in-app offer was issued, "B" when the offer came
// (or will come) by email, "" before any offer exists.
func OnboardingPathForNetwork(offer *OnboardingOffer) string {
	if offer == nil {
		return ""
	}
	switch offer.IssuedBy {
	case OnboardingOfferIssuedByInApp:
		return "A"
	case OnboardingOfferIssuedByEmail:
		return "B"
	default:
		return ""
	}
}

// ----- App Store one-time offer code pool -----

// AppleOfferCode is one row of an App Store Connect one-time offer code batch.
type AppleOfferCode struct {
	Code      string
	ExpiresAt time.Time
}

// LoadAppleOfferCodePool adds a batch of one-time codes to the pool. Codes already
// present are left as they are (including their assignment), so a batch can be
// loaded again safely. Returns the number of new codes.
func LoadAppleOfferCodePool(ctx context.Context, codes []*AppleOfferCode) (added int, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		for _, code := range codes {
			value := strings.TrimSpace(code.Code)
			if value == "" {
				continue
			}
			tag, err := tx.Exec(
				ctx,
				`
					INSERT INTO network_onboarding_apple_offer_code (code, expires_at)
					VALUES ($1, $2)
					ON CONFLICT (code) DO NOTHING
				`,
				value,
				code.ExpiresAt,
			)
			if err != nil {
				returnErr = err
				return
			}
			added += int(tag.RowsAffected())
		}
	}, server.TxReadCommitted)
	return
}

// assignAppleOfferCodeInTx hands out the pool's soonest-expiring unassigned code
// that outlives the offer. SKIP LOCKED keeps two concurrent issues from drawing
// the same code.
func assignAppleOfferCodeInTx(tx server.PgTx, ctx context.Context, networkId server.Id, offerExpiresAt time.Time) (string, bool) {
	var code string
	err := tx.QueryRow(
		ctx,
		`
			UPDATE network_onboarding_apple_offer_code
			SET network_id = $1, assigned_at = $2
			WHERE code = (
				SELECT code
				FROM network_onboarding_apple_offer_code
				WHERE network_id IS NULL AND $3 < expires_at
				ORDER BY expires_at ASC, code ASC
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING code
		`,
		networkId,
		server.NowUtc(),
		offerExpiresAt,
	).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	server.Raise(err)
	return code, true
}

// CountAvailableAppleOfferCodes is the number of unassigned, unexpired codes in
// the pool (for monitoring).
func CountAvailableAppleOfferCodes(ctx context.Context, now time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COUNT(*)
				FROM network_onboarding_apple_offer_code
				WHERE network_id IS NULL AND $1 < expires_at
			`,
			now,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// ParseAppleOfferCodeCsv parses an App Store Connect one-time code export:
// `code,expires` rows (a header row is skipped), expires as YYYY-MM-DD or RFC3339.
func ParseAppleOfferCodeCsv(data string) ([]*AppleOfferCode, error) {
	codes := []*AppleOfferCode{}
	for lineIndex, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: expected `code,expires`", lineIndex+1)
		}
		code := strings.TrimSpace(fields[0])
		expires := strings.TrimSpace(fields[1])
		if strings.EqualFold(code, "code") {
			// header
			continue
		}
		var expiresAt time.Time
		parsed := false
		for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, expires); err == nil {
				expiresAt = t.UTC()
				parsed = true
				break
			}
		}
		if !parsed {
			return nil, fmt.Errorf("line %d: invalid expires %q", lineIndex+1, expires)
		}
		codes = append(codes, &AppleOfferCode{Code: code, ExpiresAt: expiresAt})
	}
	return codes, nil
}
