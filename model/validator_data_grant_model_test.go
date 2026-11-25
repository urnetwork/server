package model

// The SN25 validator data grant's settings, pro.yml keys, receipt id and
// balance summary, without a database.

import (
	"crypto/sha256"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// 350 GiB for 30 days, the next grant in the last 2 days (owner 2026-11-25).
func TestValidatorDataGrantDefaults(t *testing.T) {
	settings := DefaultValidatorDataGrantSettings()
	connect.AssertEqual(t, settings, ValidatorDataGrantSettings{
		BalanceByteCount: 350 * Gib,
		Duration:         30 * 24 * time.Hour,
		RenewBefore:      2 * 24 * time.Hour,
		Timeout:          5 * time.Second,
	})
	connect.AssertEqual(t, settings.Enabled(), true)
	// a new instance on every call
	settings.BalanceByteCount = 0
	connect.AssertEqual(t, DefaultValidatorDataGrantSettings().BalanceByteCount, 350*Gib)
}

// The validator_grant keys of pro.yml change the size and the duration; an
// absent key or section keeps its default.
func TestParseValidatorDataGrant(t *testing.T) {
	for _, c := range []struct {
		yaml string
		want ValidatorDataGrantSettings
	}{
		{
			yaml: "free:\n  data: 30gib\n",
			want: DefaultValidatorDataGrantSettings(),
		},
		{
			yaml: "validator_grant:\n  data: 1tib\n",
			want: ValidatorDataGrantSettings{BalanceByteCount: 1024 * Gib, Duration: 30 * 24 * time.Hour, RenewBefore: 2 * 24 * time.Hour, Timeout: 5 * time.Second},
		},
		{
			yaml: "validator_grant:\n  duration: 168h\n",
			want: ValidatorDataGrantSettings{BalanceByteCount: 350 * Gib, Duration: 7 * 24 * time.Hour, RenewBefore: 2 * 24 * time.Hour, Timeout: 5 * time.Second},
		},
		{
			yaml: "validator_grant:\n  data: 100gib\n  duration: 1440h\n",
			want: ValidatorDataGrantSettings{BalanceByteCount: 100 * Gib, Duration: 60 * 24 * time.Hour, RenewBefore: 2 * 24 * time.Hour, Timeout: 5 * time.Second},
		},
		{
			yaml: "validator_grant:\n  data: 0gib\n",
			want: ValidatorDataGrantSettings{BalanceByteCount: 0, Duration: 30 * 24 * time.Hour, RenewBefore: 2 * 24 * time.Hour, Timeout: 5 * time.Second},
		},
	} {
		var y proConfigYaml
		if err := yaml.Unmarshal([]byte(c.yaml), &y); err != nil {
			t.Fatalf("%q: %s", c.yaml, err)
		}
		if got := parseValidatorDataGrant(y.ValidatorGrant); got != c.want {
			t.Errorf("%q: %+v, want %+v", c.yaml, got, c.want)
		}
	}

	for _, malformed := range []string{
		"validator_grant:\n  data: lots\n",
		"validator_grant:\n  duration: a month\n",
	} {
		var y proConfigYaml
		if err := yaml.Unmarshal([]byte(malformed), &y); err != nil {
			t.Fatalf("%q: %s", malformed, err)
		}
		panicked := func() (panicked bool) {
			defer func() {
				panicked = recover() != nil
			}()
			parseValidatorDataGrant(y.ValidatorGrant)
			return
		}()
		if !panicked {
			t.Errorf("%q parsed rather than panicked", malformed)
		}
	}
}

// A mint grants only with an amount, a duration and a renewal window that
// leaves a grant active between mints.
func TestValidatorDataGrantSettingsEnabled(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		settings ValidatorDataGrantSettings
		enabled  bool
	}{
		{settings: DefaultValidatorDataGrantSettings(), enabled: true},
		{settings: ValidatorDataGrantSettings{}, enabled: false},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: 0, Duration: 30 * day, RenewBefore: 2 * day}, enabled: false},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: -1, Duration: 30 * day, RenewBefore: 2 * day}, enabled: false},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: Gib, Duration: 0, RenewBefore: 0}, enabled: false},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: Gib, Duration: 30 * day, RenewBefore: -time.Second}, enabled: false},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: Gib, Duration: 2 * day, RenewBefore: 2 * day}, enabled: false},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: Gib, Duration: 2 * day, RenewBefore: 0}, enabled: true},
		{settings: ValidatorDataGrantSettings{BalanceByteCount: Gib, Duration: 3 * day, RenewBefore: 2 * day}, enabled: true},
	} {
		if got := c.settings.Enabled(); got != c.enabled {
			t.Errorf("%+v enabled = %t, want %t", c.settings, got, c.enabled)
		}
	}
}

// The receipt id is the namespaced sha256 of the hotkey, cut to an id: the
// same for every api instance, different for every hotkey. Changing the
// derivation would give every hotkey a second active grant.
func TestValidatorDataGrantIdPerHotkey(t *testing.T) {
	hotkey := testingValidatorHotkey(0x61)
	otherHotkey := testingValidatorHotkey(0x62)
	sum := sha256.Sum256(append([]byte("urnetwork:sn25-validator-data-grant:v1/"), hotkey[:]...))
	connect.AssertEqual(t, ValidatorDataGrantId(hotkey), server.RequireIdFromBytes(sum[:16]))
	connect.AssertEqual(t, ValidatorDataGrantId(hotkey), ValidatorDataGrantId(hotkey))
	connect.AssertNotEqual(t, ValidatorDataGrantId(hotkey), ValidatorDataGrantId(otherHotkey))
	connect.AssertNotEqual(t, ValidatorDataGrantId(hotkey), server.RequireIdFromBytes(hotkey[:16]))
}

// While a renewed grant overlaps the grant it renews, the summary counts both:
// the old one stays spendable until its own end and is not superseded.
func TestSupersededGrantsKeepsOverlappingValidatorGrants(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	old := &TransferBalance{
		BalanceId:             server.NewId(),
		StartTime:             now.Add(-29 * 24 * time.Hour),
		EndTime:               now.Add(24 * time.Hour),
		StartBalanceByteCount: 350 * Gib,
		BalanceByteCount:      20 * Gib,
		GrantKind:             GrantKindSn25Validator,
	}
	renewed := &TransferBalance{
		BalanceId:             server.NewId(),
		StartTime:             now.Add(-time.Hour),
		EndTime:               now.Add(-time.Hour + 30*24*time.Hour),
		StartBalanceByteCount: 350 * Gib,
		BalanceByteCount:      350 * Gib,
		GrantKind:             GrantKindSn25Validator,
	}
	connect.AssertEqual(t, len(SupersededGrants([]*TransferBalance{old, renewed})), 0)
	summary := SummarizeTransferBalances([]*TransferBalance{old, renewed}, 0, now)
	connect.AssertEqual(t, summary.BalanceByteCount, 370*Gib)
	connect.AssertEqual(t, summary.StartBalanceByteCount, 700*Gib)
}
