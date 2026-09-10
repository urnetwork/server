package monitor

import (
	"context"
	"fmt"
	"time"
)

// Signal signup-quality implements SIGNALS.md §2.26. It measures the
// device-backed share of a complete UTC day's new networks: an automated
// sign-up wave changes the population the onboarding experiments see, and the
// campaign's enrollment gate (server commit e4c34a21) keeps those networks out
// of the cohort only if the wave is noticed.
func NewSignupQualitySignal() Signal {
	return &signalAdapter{
		number: "2.26", key: "signup-quality", name: "Device-backed share of new networks",
		probe: pgSignupQualityProbe{},
	}
}

/* monitor-signal-2.26-signup-quality */

type pgSignupQualityProbe struct{}

func (pgSignupQualityProbe) id() string             { return "pg/signup-quality" }
func (pgSignupQualityProbe) tier() string           { return tierWarn }
func (pgSignupQualityProbe) cadence() time.Duration { return time.Hour }

const (
	// the day is complete and every network's 48 h window has matured
	signupQualityDayAge = 3
	// under this many networks the share is noise, not a wave
	signupQualityMinNetworks = 100
	// the healthy floor of device-backed networks on a day
	signupQualityMinShare = 0.5
)

// signupQualityQuery counts one complete UTC day's new networks and those
// that registered a device within 48 h. The device test is an existence probe
// per network with the time bound inside: an aggregate over a network's
// clients, or a hash join with network_client, scans every client ever and
// timed out at the statement deadline on Main.
const signupQualityQuery = `
	WITH clock AS MATERIALIZED (
	 SELECT (statement_timestamp() AT TIME ZONE 'UTC')::date AS utc_today
	), bounds AS MATERIALIZED (
	 SELECT
	  (utc_today - 3)::date AS cohort_day,
	  (utc_today - 3)::timestamp without time zone AS start_utc,
	  (utc_today - 2)::timestamp without time zone AS end_utc
	 FROM clock
	), cohort AS (
	 SELECT n.network_id, n.create_time
	 FROM network n
	 CROSS JOIN bounds b
	 WHERE
	  n.create_time >= b.start_utc AND
	  n.create_time < b.end_utc
	)
	SELECT
	 (SELECT to_char(cohort_day, 'YYYY-MM-DD') FROM bounds) AS day,
	 COUNT(*) AS networks,
	 COUNT(*) FILTER (
	  WHERE EXISTS (
	   SELECT 1
	   FROM network_client nc
	   WHERE
	    nc.network_id = cohort.network_id AND
	    nc.create_time < cohort.create_time + interval '48 hours'
	  )
	 ) AS device_networks
	FROM cohort;
`

type signupQualitySnapshot struct {
	day            string
	networks       int64
	deviceNetworks int64
}

func parseSignupQualitySnapshot(rows []pgRow) (signupQualitySnapshot, error) {
	if len(rows) != 1 || len(rows[0]) != 3 {
		return signupQualitySnapshot{}, fmt.Errorf("signup quality query returned an invalid aggregate shape")
	}
	row := rows[0]
	day := row.str(0)
	parsedDay, err := time.Parse("2006-01-02", day)
	if err != nil || parsedDay.Format("2006-01-02") != day {
		return signupQualitySnapshot{}, fmt.Errorf("signup quality query returned an invalid UTC day")
	}
	networks, err := parseStrictInt64(row.str(1))
	if err != nil {
		return signupQualitySnapshot{}, fmt.Errorf("signup quality query returned an invalid network count")
	}
	deviceNetworks, err := parseStrictInt64(row.str(2))
	if err != nil {
		return signupQualitySnapshot{}, fmt.Errorf("signup quality query returned an invalid device-network count")
	}
	if networks < 0 || deviceNetworks < 0 || networks < deviceNetworks {
		return signupQualitySnapshot{}, fmt.Errorf("signup quality query returned contradictory counts networks=%d device_networks=%d", networks, deviceNetworks)
	}
	return signupQualitySnapshot{day: day, networks: networks, deviceNetworks: deviceNetworks}, nil
}

func (pgSignupQualityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, signupQualityQuery)
	if err != nil {
		return nil, err
	}
	snapshot, err := parseSignupQualitySnapshot(rows)
	if err != nil {
		return nil, err
	}
	day := snapshot.day
	networks := snapshot.networks
	deviceNetworks := snapshot.deviceNetworks
	if networks < signupQualityMinNetworks {
		// too few sign-ups to call a wave either way
		return []finding{healthyFinding("pg/signup-quality", tierWarn, "signup-quality", pgTarget(env))}, nil
	}
	share := float64(deviceNetworks) / float64(networks)
	if signupQualityMinShare <= share {
		return []finding{healthyFinding("pg/signup-quality", tierWarn, "signup-quality", pgTarget(env))}, nil
	}
	return []finding{{
		probeId: "pg/signup-quality", tier: tierWarn,
		class: "signup-quality", target: pgTarget(env), frame: "automated-signup-wave", sustain: 1,
		symptom: fmt.Sprintf(
			"%.1f%% of the %d networks created on %s registered a device within 48 h (%d); the floor is %.0f%%",
			100*share, networks, day, deviceNetworks, 100*signupQualityMinShare,
		),
		mechanism: "A seed-phrase account is created through the API without a device, so an automated wave of account creation raises the raw network count while the device-backed count stays flat. Those networks can neither see an offer screen nor be mailed; the onboarding campaign's enrollment gate keeps them out of the cohort, but the raw sign-up count still misleads every sizing, dashboard and store estimate that reads it.",
		baseline:  "On a complete day with at least 100 new networks, at least half registered a device within 48 hours (the quiet weeks of August 2026 ran at about 80%).",
		observed: fmt.Sprintf(
			"day=%s networks=%d device_networks_48h=%d device_share=%.3f",
			day, networks, deviceNetworks, share,
		),
		evidence: "PostgreSQL counts one complete UTC day's networks and, per network, an index-backed existence probe into network_client bounded to 48 hours; only the day and two counts leave the database.",
		context:  "The day is three days back so every network's 48-hour window has matured. A wave that also registers devices (emulated clients) passes this check; the §2.7 connection rate and the onboarding exposure-integrity readout are the next discriminators.",
		action:   "Confirm the source of the wave (API sign-up rate by auth type, abuse controls on network create) and rate-limit or gate it at network create; do not size or judge an onboarding experiment on the raw sign-up count while the share is under the floor. The campaign row is only created for networks with an email login or a device (server commit e4c34a21), so no cohort repair is needed.",
		verify:   "The next complete day reports a device-backed share at or above the floor, and mmm/onboarding research.sh signup-quality agrees.",
		playbook: "SIGNALS.md §2.26; mmm/onboarding/RUN-MAIN.md",
	}}, nil
}
