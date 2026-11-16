package model

// An issue that loses its insert to a concurrent issue of the same network
// must not spend a one-time App Store code. The key-share lock on the network
// does not serialize two issues: both can find no offer, and the issue drew
// its pool code before its insert, so the one whose insert did nothing kept a
// code assigned to the network that its offer never named. Uses only entry
// points that predate the fix, so it runs against the code before it too.

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A trigger stands in for the concurrent winner deterministically: before the
// issue's own insert, it inserts the network's offer the way the other issue
// would have, after the issue found none. The issue's insert then conflicts
// and does nothing, as when the winner commits first.
func TestOfferIssueThatLosesItsInsertDrawsNoAppleCode(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "offerissuerace"+networkId.String(), server.NewId())
		added, err := LoadAppleOfferCodePool(ctx, []*AppleOfferCode{
			{Code: "SYNTHETIC-RACE-1", ExpiresAt: server.NowUtc().Add(30 * 24 * time.Hour)},
		})
		if err != nil || added != 1 {
			t.Fatalf("could not load the pool: added %d, err %v", added, err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				CREATE OR REPLACE FUNCTION synthetic_concurrent_offer_issue() RETURNS trigger
				LANGUAGE plpgsql AS $$
				BEGIN
					IF pg_trigger_depth() = 1 THEN
						INSERT INTO network_onboarding_offer (
							network_id,
							issued_at,
							expires_at,
							issued_by,
							surface,
							tier,
							percent_off,
							months_free
						)
						VALUES (
							NEW.network_id,
							NEW.issued_at,
							NEW.expires_at,
							'concurrent',
							NEW.surface,
							NEW.tier,
							NEW.percent_off,
							NEW.months_free
						);
					END IF;
					RETURN NEW;
				END
				$$
				`,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				CREATE TRIGGER synthetic_concurrent_offer_issue
				BEFORE INSERT ON network_onboarding_offer
				FOR EACH ROW EXECUTE FUNCTION synthetic_concurrent_offer_issue()
				`,
			))
		})

		offer, created, err := IssueOnboardingOffer(ctx, &IssueOnboardingOfferArgs{
			NetworkId:          networkId,
			IssuedBy:           OnboardingOfferIssuedByInApp,
			Surface:            OnboardingOfferSurfaceIntroStep,
			Tier:               "standard",
			PercentOff:         25,
			MonthsFree:         3,
			Validity:           5 * 24 * time.Hour,
			AppleOfferCodePool: true,
		})
		if err != nil || created || offer == nil || offer.IssuedBy != "concurrent" {
			t.Fatalf("the issue did not lose its insert to the concurrent one: offer %+v, created %t, err %v", offer, created, err)
		}
		if available := CountAvailableAppleOfferCodes(ctx, server.NowUtc()); available != 1 {
			t.Errorf("the issue that lost its insert spent %d of the pool's 1 code", 1-available)
		}
		var assigned []string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT code FROM network_onboarding_apple_offer_code WHERE network_id = $1`, networkId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var code string
					server.Raise(result.Scan(&code))
					assigned = append(assigned, code)
				}
			})
		})
		if 0 < len(assigned) && (offer.AppleOfferCode == nil || *offer.AppleOfferCode != assigned[0]) {
			t.Errorf("codes %s are assigned to the network but its offer does not name them", strings.Join(assigned, ", "))
		}
	})
}
