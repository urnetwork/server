// App Store offer code supply (SIGNALS.md §2.22d). The welcome offer's App
// Store redemption is a one-time offer code from a pool
// (network_onboarding_apple_offer_code) that a daily task tops up from App
// Store Connect, one batch whenever fewer than the floor of codes would
// outlive an offer issued before its next run. A code is drawn only for a
// reader on an App Store build. On 2026-11-12 every offer drew one whatever
// its reader, 509 in a day, and the pool was days from running dry with
// nothing watching it; a reader whose offer finds no code gets no discount on
// the App Store. This signal warns while the pool, with the top-up when it is
// delivering, covers less than about seven days of draws.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// The App Store offer code supply probe's tunables.
type AppleOfferCodesSettings struct {
	// The days of draws the supply must cover; it warns below. A week leaves
	// time to raise the batch or restore the top-up's App Store Connect key.
	SupplyFloor time.Duration
	// The trailing window the daily draw rate is measured over, in whole
	// days so the daily cycle of sign-ups averages out.
	DrawWindow time.Duration
	// A pool drawn from within this long is in use: an empty one warns. One
	// with no draw in it is not (the offer is off, or never had codes).
	InUseLookback time.Duration
	// Consecutive observations under the floor before the warning opens.
	Sustain int
}

// A new instance on every call.
func DefaultAppleOfferCodesSettings() *AppleOfferCodesSettings {
	return &AppleOfferCodesSettings{
		SupplyFloor: 7 * 24 * time.Hour,
		// three days: a day's swing in sign-ups moves it by a third, and a
		// change in who draws (a deploy) shows within the floor
		DrawWindow:    3 * 24 * time.Hour,
		InUseLookback: 30 * 24 * time.Hour,
		// two hourly observations: a pool caught empty in the hour before
		// the top-up's run does not warn alone
		Sustain: 2,
	}
}

// What the onboarding config says of the pool, as the server reads it
// (model.Onboarding): whether offers are issued, how long one lasts, and
// whether and how the daily top-up refills the pool
// (controller.AppleOfferCodeTopUp).
type appleOfferCodesConfig struct {
	offerEnabled  bool
	offerValidity time.Duration
	// the campaign is on and the offer code id is set, so the top-up runs
	topUp         bool
	batchSize     int
	minAvailable  int
	batchExpiry   time.Duration
	topUpInterval time.Duration
}

func loadAppleOfferCodesConfig() appleOfferCodesConfig {
	cfg := model.Onboarding()
	return appleOfferCodesConfig{
		offerEnabled:  cfg.OfferEnabled(),
		offerValidity: cfg.OfferValidity(),
		topUp:         cfg.Enabled && strings.TrimSpace(cfg.Offer.Apple.OfferCodeId) != "",
		batchSize:     cfg.Offer.Apple.EffectiveBatchSize(),
		minAvailable:  cfg.Offer.Apple.EffectiveMinAvailable(),
		batchExpiry:   time.Duration(cfg.AppleOfferCodeExpiryDays()) * 24 * time.Hour,
		topUpInterval: model.OnboardingAppleOfferCodeTopUpInterval,
	}
}

// One aggregate of the pool: the codes a new offer can take (unassigned,
// outliving an offer issued now), those an offer issued at the next top-up
// can take, how long the last of the first stays assignable, whether a batch
// arrived within two top-up intervals (its codes expire later than any older
// batch's), and the codes drawn in the window and the lookback. Only counts,
// one age and one flag leave PostgreSQL.
func appleOfferCodesQueryWith(settings *AppleOfferCodesSettings, config appleOfferCodesConfig) string {
	at := func(offset time.Duration) string {
		return fmt.Sprintf("clock.now_utc + interval '%d seconds'", int64(offset/time.Second))
	}
	assignable := `code.network_id IS NULL AND code.expires_at > ` + at(config.offerValidity)
	return `
/* monitor-signal-2.22d-apple-offer-codes */
WITH clock AS MATERIALIZED (
    SELECT (statement_timestamp() AT TIME ZONE 'UTC') AS now_utc
)
SELECT count(code.code) FILTER (WHERE ` + assignable + `)::bigint,
       count(code.code) FILTER (WHERE code.network_id IS NULL AND code.expires_at > ` + at(config.offerValidity+config.topUpInterval) + `)::bigint,
       COALESCE(floor(extract(epoch FROM max(code.expires_at) FILTER (WHERE ` + assignable + `) - (` + at(config.offerValidity) + `))), -1)::bigint,
       COALESCE(bool_or(code.expires_at > ` + at(config.batchExpiry-2*config.topUpInterval) + `), false),
       count(code.code) FILTER (WHERE code.assigned_at > ` + at(-settings.DrawWindow) + `)::bigint,
       count(code.code) FILTER (WHERE code.assigned_at > ` + at(-settings.InUseLookback) + `)::bigint
FROM clock
LEFT JOIN network_onboarding_apple_offer_code code ON true
GROUP BY clock.now_utc
`
}

// The pool as one aggregate reads it.
type appleOfferCodesObservation struct {
	assignable          int64
	nextTopUpAssignable int64
	// seconds until the last assignable code stops outliving a new offer, -1
	// with none
	assignableHorizonSeconds int64
	freshBatch               bool
	drawnWindow              int64
	drawnLookback            int64
}

func parseAppleOfferCodesObservation(rows []pgRow) (appleOfferCodesObservation, error) {
	if len(rows) != 1 || len(rows[0]) != 6 {
		return appleOfferCodesObservation{}, fmt.Errorf("apple offer codes query returned an invalid aggregate shape")
	}
	row := rows[0]
	counts := []int64{}
	for _, i := range []int{0, 1, 2, 4, 5} {
		value, err := parseStrictInt64(row.str(i))
		if err != nil {
			return appleOfferCodesObservation{}, fmt.Errorf("apple offer codes query returned an invalid count")
		}
		counts = append(counts, value)
	}
	fresh := row.str(3)
	if fresh != "t" && fresh != "f" {
		return appleOfferCodesObservation{}, fmt.Errorf("apple offer codes query returned an invalid batch flag")
	}
	observation := appleOfferCodesObservation{
		assignable:               counts[0],
		nextTopUpAssignable:      counts[1],
		assignableHorizonSeconds: counts[2],
		freshBatch:               fresh == "t",
		drawnWindow:              counts[3],
		drawnLookback:            counts[4],
	}
	if observation.assignable < 0 || observation.nextTopUpAssignable < 0 || observation.assignable < observation.nextTopUpAssignable ||
		observation.drawnWindow < 0 || observation.drawnLookback < observation.drawnWindow ||
		(observation.assignable == 0) != (observation.assignableHorizonSeconds == -1) || observation.assignableHorizonSeconds < -1 {
		return appleOfferCodesObservation{}, fmt.Errorf(
			"apple offer codes query returned contradictory counts assignable=%d next_top_up=%d horizon=%d drawn_window=%d drawn_lookback=%d",
			observation.assignable, observation.nextTopUpAssignable, observation.assignableHorizonSeconds, observation.drawnWindow, observation.drawnLookback,
		)
	}
	return observation, nil
}

// The supply the pool and the top-up give at the draw rate.
type appleOfferCodesSupply struct {
	// draws a day over the window
	drawRate float64
	// the top-up is configured and delivering: a batch arrived within two
	// intervals, or the pool is above the floor it refills at, so none was due
	topUpLive bool
	// codes a day a live top-up adds at most: one batch an interval
	topUpCapacity float64
	// the days of draws the supply covers; +Inf while a live top-up keeps up
	days float64
	low  bool
}

// assessAppleOfferCodeSupply: an empty pool in use is out. A live top-up
// that adds more a day than is drawn keeps the supply up. Otherwise the pool
// alone covers its codes over the draw rate, and, while no top-up delivers,
// no longer than its last code stays assignable: the batches expire days
// after they arrive, so a stalled top-up empties the pool within days
// whatever the count.
func assessAppleOfferCodeSupply(observation appleOfferCodesObservation, config appleOfferCodesConfig, settings *AppleOfferCodesSettings) appleOfferCodesSupply {
	day := 24 * time.Hour
	supply := appleOfferCodesSupply{
		drawRate: float64(observation.drawnWindow) / (float64(settings.DrawWindow) / float64(day)),
		topUpLive: config.topUp && 0 < config.topUpInterval &&
			(observation.freshBatch || int64(config.minAvailable) <= observation.nextTopUpAssignable),
		days: math.Inf(1),
	}
	if supply.topUpLive {
		supply.topUpCapacity = float64(config.batchSize) * float64(day) / float64(config.topUpInterval)
	}
	floorDays := float64(settings.SupplyFloor) / float64(day)
	switch {
	case observation.assignable == 0:
		supply.days = 0
		supply.low = 0 < observation.drawnLookback
	case supply.drawRate == 0:
	case supply.topUpLive && supply.drawRate < supply.topUpCapacity:
	default:
		supply.days = float64(observation.assignable) / supply.drawRate
		if !supply.topUpLive {
			supply.days = min(supply.days, float64(observation.assignableHorizonSeconds)/day.Seconds())
		}
		supply.low = supply.days < floorDays
	}
	return supply
}

// SIGNALS.md §2.22d maps to signal_apple_offer_codes.go and
// signal_apple_offer_codes_test.go.
func NewAppleOfferCodesSignal() Signal {
	return &signalAdapter{
		number: "2.22d",
		key:    "apple-offer-codes",
		name:   "App Store offer code supply",
		probe:  appleOfferCodesProbe{settings: DefaultAppleOfferCodesSettings(), loadConfig: loadAppleOfferCodesConfig},
	}
}

// The App Store offer code supply probe, at its settings and the onboarding
// config.
type appleOfferCodesProbe struct {
	settings   *AppleOfferCodesSettings
	loadConfig func() appleOfferCodesConfig
}

// The probe's alert identity.
func (self appleOfferCodesProbe) id() string { return "pg/apple-offer-codes" }

// Its tier: a supply under the floor warns, with days to act.
func (self appleOfferCodesProbe) tier() string { return tierWarn }

// Its cadence: the supply moves over days.
func (self appleOfferCodesProbe) cadence() time.Duration { return time.Hour }

// Reads the pool's aggregate at the onboarding config, and warns while the
// supply covers fewer days of draws than the floor. Healthy while the
// welcome offer is off: nothing draws.
func (self appleOfferCodesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultAppleOfferCodesSettings()
	}
	loadConfig := self.loadConfig
	if loadConfig == nil {
		loadConfig = loadAppleOfferCodesConfig
	}
	config := loadConfig()
	healthy := []finding{healthyFinding("pg/apple-offer-codes", tierWarn, "apple-offer-code-supply-low", pgTarget(env))}
	if !config.offerEnabled {
		return healthy, nil
	}
	rows, err := env.runner.pg(ctx, appleOfferCodesQueryWith(settings, config))
	if err != nil {
		return nil, err
	}
	observation, err := parseAppleOfferCodesObservation(rows)
	if err != nil {
		return nil, err
	}
	supply := assessAppleOfferCodeSupply(observation, config, settings)
	if !supply.low {
		return healthy, nil
	}

	floorDays := settings.SupplyFloor.Hours() / 24
	windowDays := settings.DrawWindow.Hours() / 24
	symptom := fmt.Sprintf(
		"App Store offer codes cover about %.1f days of welcome offers, under the %.0f-day floor: %d codes left that a new offer can take, %d drawn in the last %.0f days (%.0f a day)",
		supply.days, floorDays, observation.assignable, observation.drawnWindow, windowDays, supply.drawRate,
	)
	if observation.assignable == 0 {
		symptom = fmt.Sprintf(
			"No App Store offer code is left that a new welcome offer can take, while %d were drawn in the last %.0f days",
			observation.drawnLookback, settings.InUseLookback.Hours()/24,
		)
	}
	topUp := "not configured (onboarding.yml enabled and offer.apple.offer_code_id)"
	switch {
	case supply.topUpLive:
		topUp = fmt.Sprintf("delivering, up to %.0f codes a day", supply.topUpCapacity)
	case config.topUp:
		topUp = fmt.Sprintf("configured, but no batch arrived in two intervals while fewer than %d codes outlive the next offer", config.minAvailable)
	}
	return []finding{{
		probeId: "pg/apple-offer-codes", tier: tierWarn, class: "apple-offer-code-supply-low", target: pgTarget(env), sustain: settings.Sustain,
		symptom: symptom + "; the daily top-up is " + topUp,
		mechanism: fmt.Sprintf(
			"An App Store reader's welcome offer redeems only with a one-time code from the pool (or the custom code, when one is configured). A code is drawn when the App Store app issues the offer, or when an App Store build first reads an offer issued without one; other readers draw none. A code is handed only to an offer it outlives, so a batch serves new offers for about %.0f days of its %.0f. The daily top-up asks App Store Connect for a batch of %d whenever fewer than %d codes would outlive an offer issued before its next run, so it keeps up with at most a batch a day; when it stalls, the pool runs out within days. A reader whose offer finds no code sees no discount in the App Store app.",
			(config.batchExpiry-config.offerValidity).Hours()/24, config.batchExpiry.Hours()/24, config.batchSize, config.minAvailable,
		),
		baseline: fmt.Sprintf("A delivering top-up adds up to %d codes a day, more than App Store readers draw; without it, the pool holds at least %.0f days of draws.", config.batchSize, floorDays),
		observed: fmt.Sprintf(
			"assignable=%d next_top_up_assignable=%d assignable_horizon_seconds=%d fresh_batch=%t drawn_window=%d window_seconds=%d drawn_lookback=%d draw_rate_per_day=%.1f top_up_configured=%t top_up_live=%t top_up_capacity_per_day=%.0f supply_days=%.2f floor_days=%.0f",
			observation.assignable, observation.nextTopUpAssignable, observation.assignableHorizonSeconds, observation.freshBatch,
			observation.drawnWindow, int64(settings.DrawWindow/time.Second), observation.drawnLookback, supply.drawRate,
			config.topUp, supply.topUpLive, supply.topUpCapacity, supply.days, floorDays,
		),
		evidence: "PostgreSQL counts network_onboarding_apple_offer_code by assignment and expiry; only counts, one age and one flag leave the database. No code, network or offer is selected.",
		context:  "urnetwork_onboarding_apple_offer_codes_available (set by the top-up), urnetwork_onboarding_apple_offer_code_batches_total{result} and urnetwork_onboarding_apple_offer_code_draws_total{trigger,result} are on the onboarding dashboard. Readers on Android, the web, Windows, Linux and the direct-download Mac redeem through Google Play or Stripe and never draw a code.",
		action:   "Read the top-up's batches (created, skipped, failed) and the taskworker's [onboarding]apple offer code top-up lines. A skipped or failed top-up: restore vault/main/apple_app_store_connect.yml or its App Store Connect key's access through the vault owner, or onboarding.yml offer.apple.offer_code_id. Draws above the daily batch: raise offer.apple.batch_size (Apple generates up to 25,000 a batch and 1,000,000 a quarter per app). Draws that rose with no more App Store readers: read the draws by trigger and result; only App Store builds draw. Never hand codes out by hand.",
		verify:   "A batch arrives (the batches counter's created rises) and the draw rate stays under the top-up's daily batch, or the pool again holds the floor's days of draws.",
		playbook: "SIGNALS.md §2.22d",
	}}, nil
}
