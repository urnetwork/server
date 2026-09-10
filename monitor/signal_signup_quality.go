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
	WITH cohort AS (
	 SELECT network_id, create_time
	 FROM network
	 WHERE
	  create_time >= date_trunc('day', now()) - interval '3 days' AND
	  create_time < date_trunc('day', now()) - interval '2 days'
	)
	SELECT
	 (date_trunc('day', now()) - interval '3 days')::date AS day,
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

func (pgSignupQualityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, signupQualityQuery)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) < 3 {
		return nil, fmt.Errorf("signup quality query returned %d malformed rows", len(rows))
	}
	row := rows[0]
	day := row.str(0)
	networks := atoiRow(row, 1)
	deviceNetworks := atoiRow(row, 2)
	if networks < 0 || deviceNetworks < 0 || networks < deviceNetworks {
		return nil, fmt.Errorf("signup quality query returned contradictory counts networks=%d device_networks=%d", networks, deviceNetworks)
	}
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
