package monitor

import (
	"strings"
	"testing"
)

// Synthetic signals carrying the tier-0 vital keys among two others.
func tierZeroSelectionSignals() []Signal {
	return []Signal{
		selectionSignal{number: "1.1a", key: "provider-connections", id: "fleet/provider-connections"},
		selectionSignal{number: "1.2", key: "alpha", id: "probe/alpha"},
		selectionSignal{number: "2.11a", key: "pgbouncer-pool-starvation", id: "pg/pgbouncer-pool-starvation"},
		selectionSignal{number: "2.16", key: "connection-orphans", id: "pg/connection-orphans"},
		selectionSignal{number: "3.1", key: "beta", id: "probe/beta"},
	}
}

// A focused selection gains each tier-0 vital it left out, once and in
// registry order, whichever identifier form names a vital.
func TestIncludeSignalsWithTierZeroVitalsAddsEachVitalOnce(t *testing.T) {
	cases := []struct {
		identifiers []string
		want        string
	}{
		{identifiers: []string{"beta"}, want: "provider-connections pgbouncer-pool-starvation connection-orphans beta"},
		{identifiers: []string{"2.11a", "alpha"}, want: "provider-connections alpha pgbouncer-pool-starvation connection-orphans"},
		{identifiers: []string{"fleet/provider-connections", "pgbouncer-pool-starvation"}, want: "provider-connections pgbouncer-pool-starvation connection-orphans"},
	}
	for _, c := range cases {
		selected, err := IncludeSignalsWithTierZeroVitals(tierZeroSelectionSignals(), c.identifiers...)
		if err != nil {
			t.Fatalf("%v: %s", c.identifiers, err)
		}
		if got := strings.Join(signalKeys(selected), " "); got != c.want {
			t.Errorf("%v selected %q, want %q", c.identifiers, got, c.want)
		}
	}
}

// The named identifiers keep IncludeSignals' fail-closed contract, and a
// registry without a vital fails closed instead of running without it.
func TestIncludeSignalsWithTierZeroVitalsFailsClosed(t *testing.T) {
	withoutOrphans := []Signal{}
	for _, signal := range tierZeroSelectionSignals() {
		if signal.Key() != "connection-orphans" {
			withoutOrphans = append(withoutOrphans, signal)
		}
	}
	cases := []struct {
		signals     []Signal
		identifiers []string
		wantError   string
	}{
		{signals: tierZeroSelectionSignals(), identifiers: nil, wantError: "at least one included signal is required"},
		{signals: tierZeroSelectionSignals(), identifiers: []string{"alpha", "missing"}, wantError: `included signal "missing" is not registered`},
		{signals: withoutOrphans, identifiers: []string{"alpha"}, wantError: `included signal "connection-orphans" is not registered`},
	}
	for _, c := range cases {
		selected, err := IncludeSignalsWithTierZeroVitals(c.signals, c.identifiers...)
		if err == nil || !strings.Contains(err.Error(), c.wantError) {
			t.Errorf("%v: error %v, want %q", c.identifiers, err, c.wantError)
		}
		if selected != nil {
			t.Errorf("%v: partial selection %v on error", c.identifiers, signalKeys(selected))
		}
	}
}

// Every tier-0 vital names exactly one production signal, so renaming one
// fails here rather than in a focused production run.
func TestEveryTierZeroVitalIsRegistered(t *testing.T) {
	signals := NewSignals()
	for _, key := range TierZeroVitalSignalKeys {
		matchCount := 0
		for _, signal := range signals {
			if signal.Key() == key {
				matchCount += 1
			}
		}
		if matchCount != 1 {
			t.Errorf("tier-0 vital %q matches %d registered signals, want 1", key, matchCount)
		}
	}
}
