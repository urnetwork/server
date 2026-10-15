package model

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Provider serving eligibility is shared by score export, location counts,
// request hard exclusions and diagnostics. Reliability, ARIN risk and persistent
// URL security exceptions gate every bucket. Every bucket also requires a
// measured URL success within eight hours, unless the provider has too little
// history to show one or its country is one where the probe itself fails (the
// URL-success rule, egress_online_success.go). That gate is bucket membership
// only: probes, explicit client ids and force_minimum quality or speed answers
// still reach the provider. A force_minimum online answer is the Online pool,
// which does not. Online includes every provider past these gates; quality and
// speed additionally require a passing ratio of accepted URL outcomes within
// eight hours and, while the modern provider gate is on (from its start,
// unless provider.yml switches it), probe evidence of end-to-end
// post-quantum encryption with resizable transfer windows
// (egress_modern_provider.go), which is bucket membership too. Only
// quality excludes ARIN non-quality entries. Performance and the success ratio
// order admitted providers, never add gates.
//
// The Online URL ratio (egress_online_success.go), off by default, is the one
// exception: while it is on, a provider measured repeatedly with a share of
// successes below it leaves every bucket, Online included, as one without a
// success does, under the same kill switch and, unless
// online_url_ratio_country_exemption is false, the same country guard.

// The weights of the index and the bounds of its evidence, with the other
// tunables of the egress rules. The `egress_index` block of provider.yml,
// beside the rollout flag, overrides any of the index's (egressIndexSettings).
type EgressIndexSettings struct {
	// Explicit reader activation after complete native publication is attested.
	// False keeps union-cache reads while receipt writers and publishers roll out.
	NativeReaderEnabled bool
	// Legacy configuration compatibility; measured URLs now have equal weight.
	ClassWeights map[string]int
	// Legacy configuration compatibility, unused by the URL success ratio.
	DefaultClassWeight int
	// Maximum ranking penalty for a measured failure ratio of one.
	MaxFailureIndex int
	// Evidence lifetime, capped at eight hours; the exact boundary is stale.
	EvidenceMaxAge time.Duration
	// A window passes when
	// QualityOkNumerator·total ≤ QualityOkDenominator·ok over all its scored
	// loads, compared in integers so the boundary is exact for every total.
	QualityOkNumerator   int
	QualityOkDenominator int
	// Legacy configuration compatibility; any measured denominator above zero
	// is evidence, regardless of the former minimum sample size.
	MinScoredLoads int
	// Legacy diagnostic setting. Only ARIN risk gates country admission.
	CountryGate bool
	// added to the tier of a provider FindProviders2
	// borrows from the other bucket when a request's own bucket comes up short
	// (GEOMAP §10.3 "Backfill"); a provider borrowed from the online bucket
	// carries twice the offset. The client ranks on the tier plus demerits of
	// its own, up to MaxClientTierDemerit, which the server never sees, so the
	// default holds both edges of the borrowed band with every demerit added
	// on the better side and none on the worse:
	//
	//   - a native carries at most ClientScoreCutoffTier (3), so at most 10
	//     demerited, and a borrowed provider at least the offset: a demerited
	//     native ranks ahead of every borrowed provider from an offset of
	//     3 + 1 + 7 = 11;
	//   - a provider borrowed from the other bucket carries at most
	//     ClientScoreCutoffTier (3) plus the offset, so at most the offset
	//     plus 10 demerited, and an online one twice the offset: a demerited
	//     borrowed provider ranks ahead of the online bucket from an offset of
	//     3 + 1 + 7 = 11.
	//
	// The default is the larger, 11, with online at 22, and the borrowed keep
	// their order among themselves. The client bounds no tier -- a larger one
	// only ranks later -- so it can be raised. It is never set below
	// ClientScoreCutoffTier + 1 (4), the least that keeps every native
	// ahead of every borrowed provider before demerits.
	BackfillTierOffset int
	// how old the settings a request path reads may be. The passes read
	// provider.yml on every call; a request path runs far too often to read
	// and parse a file each time.
	RequestSettingsMaxAge time.Duration
	// The URL-success rule (egress_online_success.go): a provider measured
	// ProviderOnlineMinMeasuredUrlRuns or more times in the evidence window
	// without one success leaves every bucket, Online included. False is the
	// kill switch, `online_requires_url_success: false` in provider.yml.
	OnlineRequiresUrlSuccess bool
	// The rule's country guard: a published country where fewer than this
	// share of the measured Online providers have a success is one where the
	// probe itself fails, and its providers keep the benefit of the doubt.
	// `online_success_country_floor` in provider.yml, within [0, 1].
	OnlineSuccessCountryFloor float64
	// The guard's hysteresis: a country the score export's last trusted
	// published guard exempted stays exempt until its share reaches the floor
	// plus this margin, so a share that hovers at the floor does not move the
	// country's providers in and out of Online on every pass
	// (readProviderOnlineSuccessPreviousBaseline). Zero is the plain floor.
	// `online_success_country_floor_margin` in provider.yml, within [0, 1].
	OnlineSuccessCountryFloorMargin float64
	// The Online URL ratio (egress_online_success.go; the owner is
	// considering it, 2026-10-15): a provider measured
	// ProviderOnlineMinMeasuredUrlRuns or more times in the evidence window,
	// with a success but a share of successes below this, leaves every
	// bucket, Online included, as one without a success does. Compared in
	// exact millionths, like the country floor. Zero, the default, is off:
	// Online keeps the rule of one success. `online_url_success_ratio` in
	// provider.yml, within [0, 1]; a value that cannot be read, or one
	// outside [0, 1], is off.
	OnlineUrlSuccessRatio float64
	// Whether a country the guard exempts keeps its providers below the
	// Online URL ratio, as it keeps those without a success. False applies
	// the ratio in every country, which also takes the providers without a
	// success out of an exempt country's Online.
	// `online_url_ratio_country_exemption` in provider.yml, default true; a
	// value that cannot be read keeps true.
	OnlineUrlRatioCountryExemption bool
	// The modern provider gate (egress_modern_provider.go): a provider the
	// probe has not shown to be modern leaves Quality and Speed, keeping
	// Online. `quality_speed_requires_modern_provider` in provider.yml, nil
	// while the key is absent: the gate then turns on by itself at
	// QualitySpeedModernProviderStart (owner decision 2026-10-15). False is
	// the kill switch and the way to postpone, true turns the gate on at once,
	// and a switch that is unreadable or given without a value is false, as
	// is an absent one beside a start that cannot be read.
	// RequiresModernProvider applies it.
	QualitySpeedRequiresModernProvider *bool
	// When an absent switch turns the gate on, that instant included:
	// `quality_speed_modern_provider_start` in provider.yml, an RFC 3339
	// instant or a date (its 00:00 UTC), or
	// DefaultQualitySpeedModernProviderStart.
	QualitySpeedModernProviderStart time.Time
}

// When the modern provider gate turns on by itself while provider.yml leaves
// quality_speed_requires_modern_provider out (owner decision 2026-10-15),
// unless quality_speed_modern_provider_start moves it.
func DefaultQualitySpeedModernProviderStart() time.Time {
	return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
}

// The most a connect client adds to a provider's tier from what it sees
// itself (connect ip_remote_multi_client.go, effectiveTier): +1 while no probe
// or traffic has proven the provider, +2 while its dials are starved, +2 while
// quarantined or remembering a quarantine, +1 for an unhealthy stats window,
// and +1 while a busy-flow probe is unanswered. It is the client's number,
// mirrored here only to size the default backfill offset.
const MaxClientTierDemerit = 7

// The rules as connect/GEOMAP.md §10.3 sets them, before any provider.yml
// override.
func DefaultEgressIndexSettings() *EgressIndexSettings {
	return &EgressIndexSettings{
		ClassWeights: map[string]int{
			"dns":          1,
			"connectivity": 1,
			"cdn":          1,
			"site":         1,
		},
		DefaultClassWeight:    1,
		MaxFailureIndex:       6,
		EvidenceMaxAge:        ProviderEgressHealthMaxAge,
		QualityOkNumerator:    4,
		QualityOkDenominator:  5,
		MinScoredLoads:        1,
		CountryGate:           true,
		BackfillTierOffset:    ClientScoreCutoffTier + 1 + MaxClientTierDemerit,
		RequestSettingsMaxAge: time.Minute,
		// The floor is pending the owner's confirmation. Measured on
		// 2026-10-13, it exempts the countries whose probe failed
		// structurally (VN 0.03, JP 0.24, RU 0.25, TW 0.27, ZA and IN 0.35)
		// and no country of a hundred or more Online providers where the
		// probe worked (the lowest of those, HK, measured 0.46).
		OnlineRequiresUrlSuccess:  true,
		OnlineSuccessCountryFloor: 0.36,
		// Off, Online's rule of one success, until the owner decides
		// (2026-10-15); measured on Main, 0.8 would leave about half of Online
		OnlineUrlSuccessRatio:          0,
		OnlineUrlRatioCountryExemption: true,
		// Pending the owner's confirmation. IN and ZA measured 0.35 against
		// the 0.36 floor on 2026-10-13; the margin keeps such a country's
		// exemption until its share passes the noise of one pass to the next.
		OnlineSuccessCountryFloorMargin: 0.03,
		// Shadow first (owner decision 2026-10-14), then on by itself at the
		// start (owner decision 2026-10-15) unless provider.yml sets the switch.
		QualitySpeedRequiresModernProvider: nil,
		QualitySpeedModernProviderStart:    DefaultQualitySpeedModernProviderStart(),
	}
}

// The `egress_index` block of provider.yml. Every field is optional; an
// absent one keeps its default.
type egressIndexSettingsBlock struct {
	NativeReaderEnabled  *bool          `yaml:"native_reader_enabled"`
	ClassWeights         map[string]int `yaml:"class_weights"`
	DefaultClassWeight   *int           `yaml:"default_class_weight"`
	MaxFailureIndex      *int           `yaml:"max_failure_index"`
	EvidenceMaxAge       *string        `yaml:"evidence_max_age"`
	QualityOkNumerator   *int           `yaml:"quality_ok_numerator"`
	QualityOkDenominator *int           `yaml:"quality_ok_denominator"`
	MinScoredLoads       *int           `yaml:"min_scored_loads"`
	CountryGate          *bool          `yaml:"country_gate"`
	BackfillTierOffset   *int           `yaml:"backfill_tier_offset"`
}

// The defaults with the overrides of provider.yml, read on every call like the
// rollout flag, so a process applies a changed file at its next pass. A warp
// container sees only the config version it was created with, so a config
// release reaches a running service with its restart (capprobe/README.md,
// "Switching it on"). A missing provider.yml is the defaults, as it is for the
// rollout flag; an unreadable value, or one out of range, keeps the default and says
// so, since a typo in one weight must not take the index away from the whole
// fleet. The file is parsed once, and each top-level key read here is decoded
// on its own: the egress_index block, the URL-success rule's keys and the
// modern provider gate's switch and start beside
// subscriber_quality_policy_version. A value of the wrong type, or a key the
// file gives twice, fails only that key; a bad value inside the block fails
// the block. An unreadable kill switch, or a file that is not yaml at all,
// disables the rule: it may be an attempt to turn the rule off, and the
// behaviour before the rule is the one to fall back to. For the same reason
// an unreadable modern provider switch, one given without a value, or such a
// file turns that gate off, where an absent switch leaves it to its start;
// and while the switch is absent, a start that is given but cannot be read,
// empty included, turns it off too, since each is most likely a postponement
// that failed. Each is logged.
func egressIndexSettings() *EgressIndexSettings {
	settings := DefaultEgressIndexSettings()
	resource, err := server.Config.SimpleResource(providerConfigResourceName)
	if err != nil || resource == nil {
		return settings
	}
	// Each top-level key with its value and how many times the file gives it.
	// A parse into nodes checks no types and no duplicate keys, so only a file
	// that is not yaml, or whose top level is not a mapping, fails here.
	keyValues, keyCounts, err := func() (map[string]*yaml.Node, map[string]int, error) {
		value, err := resource.BytesE()
		if err != nil {
			return nil, nil, err
		}
		var root yaml.Node
		if err := yaml.Unmarshal(value, &root); err != nil {
			return nil, nil, err
		}
		keyValues := map[string]*yaml.Node{}
		keyCounts := map[string]int{}
		if len(root.Content) == 0 {
			// empty, or only comments
			return keyValues, keyCounts, nil
		}
		mapping := root.Content[0]
		if mapping.Kind == yaml.ScalarNode && mapping.Tag == "!!null" {
			return keyValues, keyCounts, nil
		}
		if mapping.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("the top level is not a mapping")
		}
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			key := mapping.Content[i].Value
			keyValues[key] = mapping.Content[i+1]
			keyCounts[key] += 1
		}
		return keyValues, keyCounts, nil
	}()
	// the modern provider gate's switch, set false
	modernGateOff := func() {
		off := false
		settings.QualitySpeedRequiresModernProvider = &off
	}
	if err != nil {
		glog.Errorf("[egress]provider config is unreadable (%s); the egress index keeps its defaults, the online url-success rule is disabled and the modern provider gate is off\n", err)
		settings.OnlineRequiresUrlSuccess = false
		modernGateOff()
		return settings
	}
	// decodes one top-level key into target; an absent key leaves it as it is
	decode := func(key string, target any) error {
		switch count := keyCounts[key]; count {
		case 0:
			return nil
		case 1:
			return keyValues[key].Decode(target)
		default:
			return fmt.Errorf("the file gives %s %d times", key, count)
		}
	}

	var enabled *bool
	if err := decode("online_requires_url_success", &enabled); err != nil {
		glog.Errorf("[egress]provider config online_requires_url_success is unreadable (%s); the online url-success rule is disabled\n", err)
		settings.OnlineRequiresUrlSuccess = false
	} else if enabled != nil {
		settings.OnlineRequiresUrlSuccess = *enabled
	}
	var floor *float64
	if err := decode("online_success_country_floor", &floor); err != nil {
		glog.Errorf("[egress]provider config online_success_country_floor is unreadable (%s); kept %v\n", err, settings.OnlineSuccessCountryFloor)
	} else if floor != nil {
		// a share outside [0, 1], NaN included, would exempt every country or none
		if 0 <= *floor && *floor <= 1 {
			settings.OnlineSuccessCountryFloor = *floor
		} else {
			glog.Errorf("[egress]provider config online_success_country_floor = %v is outside [0, 1]; kept %v\n", *floor, settings.OnlineSuccessCountryFloor)
		}
	}
	var margin *float64
	if err := decode("online_success_country_floor_margin", &margin); err != nil {
		glog.Errorf("[egress]provider config online_success_country_floor_margin is unreadable (%s); kept %v\n", err, settings.OnlineSuccessCountryFloorMargin)
	} else if margin != nil {
		if 0 <= *margin && *margin <= 1 {
			settings.OnlineSuccessCountryFloorMargin = *margin
		} else {
			glog.Errorf("[egress]provider config online_success_country_floor_margin = %v is outside [0, 1]; kept %v\n", *margin, settings.OnlineSuccessCountryFloorMargin)
		}
	}
	// A ratio that cannot be read, or one outside [0, 1], NaN included, is
	// off: Online's rule of one success, the behaviour before the ratio, and
	// the larger Online.
	var urlSuccessRatio *float64
	if err := decode("online_url_success_ratio", &urlSuccessRatio); err != nil {
		glog.Errorf("[egress]provider config online_url_success_ratio is unreadable (%s); the online url ratio is off\n", err)
		settings.OnlineUrlSuccessRatio = 0
	} else if urlSuccessRatio != nil {
		if 0 <= *urlSuccessRatio && *urlSuccessRatio <= 1 {
			settings.OnlineUrlSuccessRatio = *urlSuccessRatio
		} else {
			glog.Errorf("[egress]provider config online_url_success_ratio = %v is outside [0, 1]; the online url ratio is off\n", *urlSuccessRatio)
			settings.OnlineUrlSuccessRatio = 0
		}
	}
	// an unreadable exemption switch keeps the exemption, the larger Online
	var urlRatioCountryExemption *bool
	if err := decode("online_url_ratio_country_exemption", &urlRatioCountryExemption); err != nil {
		glog.Errorf("[egress]provider config online_url_ratio_country_exemption is unreadable (%s); kept %t\n", err, settings.OnlineUrlRatioCountryExemption)
	} else if urlRatioCountryExemption != nil {
		settings.OnlineUrlRatioCountryExemption = *urlRatioCountryExemption
	}
	// A switch given without a value (empty, ~ or null) decodes as absent,
	// which would leave the gate to its start; like a start that cannot be
	// read, it is most likely a postponement that failed, so both hold the
	// gate off. A start only decides while the switch is absent.
	var modernGate *bool
	if err := decode("quality_speed_requires_modern_provider", &modernGate); err != nil {
		glog.Errorf("[egress]provider config quality_speed_requires_modern_provider is unreadable (%s); the modern provider gate is off\n", err)
		modernGateOff()
	} else if modernGate != nil {
		settings.QualitySpeedRequiresModernProvider = modernGate
	} else if 0 < keyCounts["quality_speed_requires_modern_provider"] {
		glog.Errorf("[egress]provider config quality_speed_requires_modern_provider has no value; the modern provider gate is off\n")
		modernGateOff()
	}
	modernStartUnreadable := func(reason string) {
		if settings.QualitySpeedRequiresModernProvider != nil {
			glog.Errorf("[egress]provider config quality_speed_modern_provider_start %s; quality_speed_requires_modern_provider decides the modern provider gate\n", reason)
			return
		}
		glog.Errorf("[egress]provider config quality_speed_modern_provider_start %s; the modern provider gate is off\n", reason)
		modernGateOff()
	}
	// any scalar decodes into a string as written, so a timestamp yaml
	// resolves on its own still reaches the parse; a date is its 00:00 UTC
	var modernStart *string
	if err := decode("quality_speed_modern_provider_start", &modernStart); err != nil {
		modernStartUnreadable(fmt.Sprintf("is unreadable (%s)", err))
	} else if modernStart != nil {
		if start, err := time.Parse(time.RFC3339, *modernStart); err == nil {
			settings.QualitySpeedModernProviderStart = start.UTC()
		} else if date, err := time.Parse(time.DateOnly, *modernStart); err == nil {
			settings.QualitySpeedModernProviderStart = date.UTC()
		} else {
			modernStartUnreadable(fmt.Sprintf("= %q is neither an RFC 3339 instant nor a date", *modernStart))
		}
	} else if 0 < keyCounts["quality_speed_modern_provider_start"] {
		modernStartUnreadable("has no value")
	}

	var overrides *egressIndexSettingsBlock
	if err := decode("egress_index", &overrides); err != nil {
		glog.Errorf("[egress]provider config egress_index is unreadable (%s); the egress index keeps its defaults\n", err)
		return settings
	}
	if overrides == nil {
		return settings
	}

	if overrides.NativeReaderEnabled != nil {
		settings.NativeReaderEnabled = *overrides.NativeReaderEnabled
	}

	// every value is stored or multiplied into a smallint score, so each is
	// held to what that column can carry
	setInt := func(name string, value *int, minValue int, target *int) {
		if value == nil {
			return
		}
		if *value < minValue || math.MaxInt16 < *value {
			glog.Errorf("[egress]provider config egress_index.%s = %d is outside [%d, %d]; kept %d\n", name, *value, minValue, math.MaxInt16, *target)
			return
		}
		*target = *value
	}
	for class, weight := range overrides.ClassWeights {
		defaultWeight := settings.classWeight(class)
		setInt(fmt.Sprintf("class_weights.%s", class), &weight, 0, &defaultWeight)
		settings.ClassWeights[class] = defaultWeight
	}
	setInt("default_class_weight", overrides.DefaultClassWeight, 0, &settings.DefaultClassWeight)
	setInt("max_failure_index", overrides.MaxFailureIndex, 0, &settings.MaxFailureIndex)
	// a run of no loads measures nothing, so at least one is required
	setInt("min_scored_loads", overrides.MinScoredLoads, 1, &settings.MinScoredLoads)
	// below one past the highest native tier a borrowed provider could tie
	// with, or rank ahead of, a native one even before the client's demerits
	setInt("backfill_tier_offset", overrides.BackfillTierOffset, ClientScoreCutoffTier+1, &settings.BackfillTierOffset)

	numerator := settings.QualityOkNumerator
	denominator := settings.QualityOkDenominator
	setInt("quality_ok_numerator", overrides.QualityOkNumerator, 0, &numerator)
	setInt("quality_ok_denominator", overrides.QualityOkDenominator, 1, &denominator)
	// a rule above one no run can pass, which would empty quality fleet-wide
	if denominator < numerator {
		glog.Errorf("[egress]provider config egress_index quality ratio %d/%d is above one; kept %d/%d\n", numerator, denominator, settings.QualityOkNumerator, settings.QualityOkDenominator)
	} else {
		settings.QualityOkNumerator = numerator
		settings.QualityOkDenominator = denominator
	}

	if overrides.EvidenceMaxAge != nil {
		if evidenceMaxAge, err := time.ParseDuration(*overrides.EvidenceMaxAge); err == nil && 0 < evidenceMaxAge {
			settings.EvidenceMaxAge = evidenceMaxAge
		} else {
			glog.Errorf("[egress]provider config egress_index.evidence_max_age = %q is not a positive duration; kept %s\n", *overrides.EvidenceMaxAge, settings.EvidenceMaxAge)
		}
	}
	if overrides.CountryGate != nil {
		settings.CountryGate = *overrides.CountryGate
	}
	return settings
}

// The settings as a request path last read them.
type egressIndexSettingsSnapshot struct {
	settings *EgressIndexSettings
	loadTime time.Time
}

// The settings a request path reads: at most their own RequestSettingsMaxAge
// old, re-read by the first request past that age. Two requests racing past
// it both read the file, which is harmless.
var requestEgressIndexSettingsSnapshot atomic.Pointer[egressIndexSettingsSnapshot]

// One immutable settings pointer is captured by each discovery request, so a
// refresh cannot switch cache schemas between its primary and alternate loads.
func requestEgressIndexSettings() *EgressIndexSettings {
	snapshot := requestEgressIndexSettingsSnapshot.Load()
	if snapshot == nil || snapshot.settings.RequestSettingsMaxAge <= time.Since(snapshot.loadTime) {
		snapshot = &egressIndexSettingsSnapshot{settings: egressIndexSettings(), loadTime: time.Now()}
		requestEgressIndexSettingsSnapshot.Store(snapshot)
	}
	return snapshot.settings
}

// Quiet processes also expose their effective setting. Scraping uses the same
// bounded local-config cache as requests; it does not read provider storage.
var findProviders2NativeReaderEnabled = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_native_reader_enabled",
	Help: "Effective cached native-reader configuration for non-forced discovery requests; does not attest publication or request outcomes",
}, func() float64 {
	if requestEgressIndexSettings().NativeReaderEnabled {
		return 1
	}
	return 0
})

// Registers the backfill metrics, and forgets the request settings on a test
// reset so a test's provider.yml is read.
func init() {
	prometheus.MustRegister(findProviders2BackfillProviders, findProviders2AnsweredProviders, findProviders2NativeReaderEnabled)
	server.OnReset(func() {
		requestEgressIndexSettingsSnapshot.Store(nil)
	})
}

// The cost of one failed load of `class`.
func (self *EgressIndexSettings) classWeight(class string) int {
	if weight, ok := self.ClassWeights[class]; ok {
		return weight
	}
	return self.DefaultClassWeight
}

// The largest index these settings can produce, which bounds the dashboard's
// index labels.
func (self *EgressIndexSettings) MaxIndex() int {
	return min(self.MaxFailureIndex, math.MaxInt16)
}

// Whether the modern provider gate holds Quality and Speed to modern
// providers at now: the switch when provider.yml gives one, otherwise from
// QualitySpeedModernProviderStart on, that instant included. Each reader
// passes the instant it decides at, a pass's or a request's filter time and
// never a fresh clock read, so one pass decides every provider alike.
func (self *EgressIndexSettings) RequiresModernProvider(now time.Time) bool {
	if self.QualitySpeedRequiresModernProvider != nil {
		return *self.QualitySpeedRequiresModernProvider
	}
	return !now.Before(self.QualitySpeedModernProviderStart)
}

// How provider.yml sets the modern provider gate, for the logs and the
// inspection: the switch when it is given, otherwise the start.
func (self *EgressIndexSettings) ModernProviderGateSchedule() string {
	if self.QualitySpeedRequiresModernProvider != nil {
		return fmt.Sprintf("quality_speed_requires_modern_provider: %t", *self.QualitySpeedRequiresModernProvider)
	}
	return fmt.Sprintf("quality_speed_requires_modern_provider absent, on from %s", self.QualitySpeedModernProviderStart.UTC().Format(time.RFC3339))
}

// The modern provider gate as this process's provider.yml sets it, at now:
// whether it holds Quality and Speed, and while the switch is absent the
// instant it turns on by itself, nil while the switch is given (true, false
// or unreadable), when no flip is scheduled. The stats collector publishes
// both for the monitor's readiness signal.
func GetModernProviderGateSchedule(now time.Time) (on bool, scheduledStart *time.Time) {
	settings := egressIndexSettings()
	on = settings.RequiresModernProvider(now)
	if settings.QualitySpeedRequiresModernProvider == nil {
		start := settings.QualitySpeedModernProviderStart
		scheduledStart = &start
	}
	return
}

// The egress settings this process's provider.yml sets, decoded as every
// pass decodes them. The monitor reads the deployment's desired modern
// provider gate with it, deciding the gate at its own clock
// (RequiresModernProvider).
func GetEgressIndexSettings() *EgressIndexSettings {
	return egressIndexSettings()
}

// The part of a provider's latest health run the index reads: its scored
// loads and their per-class tally. Nothing of the reputation tally is read;
// after GEOMAP §11 there is no reputation class, and before it the tally was
// never a health figure.
type EgressHealthRun struct {
	MeasuredAt   time.Time
	OkCount      int
	Total        int
	ClassResults map[string]ProviderEgressHealthClassResult
}

// One provider's index and the verdict it was decided with.
type EgressIndex struct {
	Index int
	// whether a usable run decided the index
	Evidence bool
	// the success-ratio verdict over measured URLs, meaningful only with
	// Evidence
	Quality bool
}

// Quality as the egress_quality column holds it: nil when there is no
// evidence, since "not probed" must stay distinguishable from "probed and
// failing".
func (self EgressIndex) QualityVerdict() *bool {
	if !self.Evidence {
		return nil
	}
	quality := self.Quality
	return &quality
}

// Computes admission and ranking from accepted URL outcomes in the evidence
// window. Zero measured outcomes are unknown. The index is the ceiling of the
// failure fraction times MaxFailureIndex; every measured URL has equal weight.
func ComputeEgressIndex(run *EgressHealthRun, now time.Time, settings *EgressIndexSettings) EgressIndex {
	if run == nil || run.Total <= 0 || !run.MeasuredAt.After(now.Add(-min(settings.EvidenceMaxAge, ProviderEgressHealthMaxAge))) {
		return EgressIndex{}
	}

	// Normalize by measured URLs so collecting more samples cannot turn the
	// same success ratio into a worse tier merely by accumulating failures.
	failures := max(0, run.Total-run.OkCount)
	failureIndex := (int64(failures)*int64(settings.MaxFailureIndex) + int64(run.Total) - 1) / int64(run.Total)
	return EgressIndex{
		Index:    int(min(failureIndex, int64(settings.MaxFailureIndex), int64(math.MaxInt16))),
		Evidence: true,
		Quality:  settings.QualityOkNumerator*run.Total <= settings.QualityOkDenominator*run.OkCount,
	}
}

// A measured success ratio contributes a positive selection weight alongside
// reliability. Missing evidence is neutral and remains available online.
func providerUrlProbeSuccessWeight(counts ProviderEgressHealthCounts) float64 {
	if counts.Total <= 0 {
		return 1
	}
	ratio := max(0.0, min(1.0, float64(counts.OKCount)/float64(counts.Total)))
	return 0.1 + 0.9*ratio
}

// Common gate failures exclude every bucket. No success, the URL-success rule,
// and low success, the Online URL ratio while it is on, also exclude every
// bucket but are no common gate: probes, explicit client ids and
// force_minimum quality or speed answers keep the provider (a force_minimum
// online answer is the Online pool, which does not).
// Health/unprobed, not modern (the modern provider gate, while it is on) and
// ARIN non-quality explain missing native membership while preserving online
// supply.
// Legacy blackhole/country reason labels remain for metric compatibility only.
const (
	ProviderExcludedBlackhole      = "blackhole"
	ProviderExcludedTls            = "tls"
	ProviderExcludedCountry        = "country"
	ProviderExcludedHealth         = "health"
	ProviderExcludedUnprobed       = "unprobed"
	ProviderExcludedArinRisk       = "arin_risk"
	ProviderExcludedArinNonQuality = "arin_non_quality"
	ProviderExcludedReliability    = "reliability"
	ProviderExcludedNoSuccess      = "no_success"
	ProviderExcludedNotModern      = "not_modern"
	ProviderExcludedLowSuccess     = "low_success"
)

// Every reason label, in the order the labels were added. A provider counts
// under the first reason decideProviderEgress finds, in this precedence:
// reliability, arin_risk, tls, no_success, low_success, then unprobed or
// health (exclusive of each other), then not_modern, then arin_non_quality.
// Blackhole and country are never reported any more.
var ProviderExcludedReasons = []string{
	ProviderExcludedBlackhole,
	ProviderExcludedTls,
	ProviderExcludedCountry,
	ProviderExcludedHealth,
	ProviderExcludedUnprobed,
	ProviderExcludedArinRisk,
	ProviderExcludedArinNonQuality,
	ProviderExcludedReliability,
	ProviderExcludedNoSuccess,
	ProviderExcludedNotModern,
	ProviderExcludedLowSuccess,
}

// What the rules read about one provider.
type providerEgressFacts struct {
	arinRisk                bool
	arinNonQuality          bool
	reliabilityFailed       bool
	blackholed              bool
	tlsAuthenticationFailed bool
	// a fresh probe observed the exit in a country other than the published one
	countryMismatch bool
	// The rollup index is a ranking diagnostic; the verdict uses current history.
	egressIndex   *int
	egressQuality *bool
	// the URL-success rule holds: measured often enough without one success,
	// and the filter's guard applies the rule in the published country (it
	// is enabled there and the country is not exempt), or applies the Online
	// URL ratio there, which every provider without a success is below
	noUrlSuccess bool
	// the Online URL ratio holds: measured often enough, with a success but
	// below the filter's guard's ratio, which applies in the published
	// country
	lowUrlSuccess bool
	// the modern provider gate: the outcome the latest probe evidence gives
	// the provider, "" read as no evidence, and whether the gate holds
	// Quality and Speed to it at the filter's instant
	modernOutcome string
	modernGate    bool
}

// What the rules do with one provider.
type providerEgressDecision struct {
	// the first rule that took the provider out of a bucket, "" for none
	reason string
	// absent from every result and count, force_minimum and an explicit client
	// id included
	hardExcluded bool
	// Native membership in each request mode; performance adds no gate.
	quality bool
	speed   bool
	// Every common-gate pass the URL-success rule keeps belongs to online and
	// advertised supply.
	online  bool
	counted bool
	// Native membership in each mode before the modern provider gate, and
	// the gate's outcome for a provider admitted to either ("" otherwise).
	// The gate's shadow counts read them, so they read the same whether the
	// gate is on or off.
	qualityBeforeModernGate bool
	speedBeforeModernGate   bool
	modernOutcome           string
}

// Gathers what the rules read about one provider from this pass's bulk loads
// and the provider's rollup row. The published country is the country of the
// rollup's location, the observed one a fresh probe's.
func (self providerCountFilter) egressFacts(
	clientId server.Id,
	rollupCountryCode *string,
	egressIndex *int,
	egressQuality *bool,
	settings *EgressIndexSettings,
) *providerEgressFacts {
	publishedCountryCode := normalizeCountryCode(rollupCountryCode)
	observedCountryCode := self.countryCodes[clientId]
	// Read accepted health directly so an old rollup cannot extend freshness.
	counts, urlIndex := self.urlIndex(clientId, settings)
	return &providerEgressFacts{
		arinRisk:                self.arinRisk[clientId],
		arinNonQuality:          self.arinNonQuality[clientId],
		reliabilityFailed:       self.reliabilityFailed[clientId],
		blackholed:              self.isBlackholed(clientId),
		tlsAuthenticationFailed: self.tlsAuthenticationFailed[clientId],
		// a provider nobody has located, or whose own country is unknown,
		// cannot contradict itself
		countryMismatch: settings.CountryGate &&
			observedCountryCode != "" &&
			publishedCountryCode != "" &&
			observedCountryCode != publishedCountryCode,
		egressIndex:   egressIndex,
		egressQuality: urlIndex.QualityVerdict(),
		// the guard carries the kill switch and the ratio it was decided
		// under: a fleet pass's own settings, or the score export's for a
		// scoped reader
		noUrlSuccess: providerCarriesNoUrl(counts, urlIndex) &&
			self.onlineSuccessBaseline.noSuccessAppliesIn(publishedCountryCode),
		lowUrlSuccess: self.onlineSuccessBaseline.belowUrlRatioIn(counts, urlIndex, publishedCountryCode),
		modernOutcome: classifyProviderModernEvidence(self.modernEvidence[clientId], self.now),
		// at the filter's instant, so the gate's start flips a pass whole
		modernGate: settings.RequiresModernProvider(self.now),
	}
}

// A provider's accepted URL outcomes in the evidence window and the index
// they give now: no Evidence when there are none, or none current.
func (self providerCountFilter) urlIndex(clientId server.Id, settings *EgressIndexSettings) (ProviderEgressHealthCounts, EgressIndex) {
	counts, ok := self.healthCounts[clientId]
	if !ok {
		return counts, EgressIndex{}
	}
	return counts, ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount, Total: counts.Total}, self.now, settings)
}

// Decides bucket membership once. The historical rollout flag has no authority
// to admit unmeasured rows or bypass common gates.
func decideProviderEgress(facts *providerEgressFacts, egressTestEnabled bool) providerEgressDecision {
	switch {
	case facts.reliabilityFailed:
		return providerEgressDecision{
			reason:       ProviderExcludedReliability,
			hardExcluded: true,
		}
	case facts.arinRisk:
		return providerEgressDecision{
			reason:       ProviderExcludedArinRisk,
			hardExcluded: true,
		}
	case facts.tlsAuthenticationFailed:
		return providerEgressDecision{
			reason:       ProviderExcludedTls,
			hardExcluded: true,
		}
	}

	switch {
	case facts.noUrlSuccess:
		// Measured repeatedly and never carried a URL: out of every bucket
		// and the counts, but no hard exclusion. Probing continues, and the
		// first success brings the provider back on the next pass.
		return providerEgressDecision{
			reason: ProviderExcludedNoSuccess,
		}
	case facts.lowUrlSuccess:
		// Measured repeatedly with successes, but too few for the Online URL
		// ratio: out of every bucket and the counts as above, and no hard
		// exclusion either. Probing continues, and a share back at the ratio
		// brings the provider back on the next pass.
		return providerEgressDecision{
			reason: ProviderExcludedLowSuccess,
		}
	case facts.egressQuality == nil:
		// no probe verdict at all, no negative mark but the exclusions and
		// no positive one: the online bucket, and counted, since it never
		// fails closed
		return providerEgressDecision{
			reason:  ProviderExcludedUnprobed,
			online:  true,
			counted: true,
		}
	case !*facts.egressQuality:
		// A measured failing ratio belongs to online, not quality or speed.
		return providerEgressDecision{
			reason:  ProviderExcludedHealth,
			online:  true,
			counted: true,
		}
	default:
		modernOutcome := facts.modernOutcome
		if modernOutcome == "" {
			modernOutcome = ProviderModernOutcomeNoEvidence
		}
		decision := providerEgressDecision{
			quality:                 !facts.arinNonQuality,
			speed:                   true,
			online:                  true,
			counted:                 true,
			qualityBeforeModernGate: !facts.arinNonQuality,
			speedBeforeModernGate:   true,
			modernOutcome:           modernOutcome,
		}
		switch {
		case facts.modernGate && modernOutcome != ProviderModernOutcomeModern:
			// The probe has not shown end-to-end post-quantum encryption with
			// resizable transfer windows: out of both native buckets, still
			// online and counted. Probing goes on, and evidence of both
			// brings the provider back on the next pass.
			decision.quality = false
			decision.speed = false
			decision.reason = ProviderExcludedNotModern
		case facts.arinNonQuality:
			decision.reason = ProviderExcludedArinNonQuality
		}
		return decision
	}
}

// The bucket a request in rankMode borrows from first when its own comes up
// short, false for a mode that is neither. Online borrows from nothing: it
// already holds every online provider.
func backfillRankMode(rankMode RankMode) (RankMode, bool) {
	switch rankMode {
	case RankModeQuality:
		return RankModeSpeed, true
	case RankModeSpeed:
		return RankModeQuality, true
	default:
		return "", false
	}
}

// How many providers each FindProviders2 answer borrowed from outside its own
// bucket (GEOMAP §10.3 "Backfill"), per
// requested rank mode, so a mass probe failure that empties a bucket shows as
// a wave of backfill rather than as a silent change in what users are handed.
// Every non-forced answer to a location request is observed, a zero included,
// so the answer rate and the borrowed per answer read from the one series.
var findProviders2BackfillProviders = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "urnetwork_provider_backfill",
	Help:    "Providers a FindProviders2 answer borrowed from outside its own bucket, per requested rank mode",
	Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100},
}, []string{"rank_mode"})

// Every provider those answers held, native and borrowed, so the borrowed share of what users were handed reads
// as one ratio -- what SIGNALS.md §2.19b alerts on when it stays above half.
var findProviders2AnsweredProviders = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_answered_total",
	Help: "Providers FindProviders2 location answers held, native and borrowed, per requested rank mode",
}, []string{"rank_mode"})

// Cached common gate failures cover normal, explicit client and force_minimum
// paths. Candidates outside a published cohort use bounded database reads.
const providerHardExclusionsKey = "{provider_hard_exclusions}"

// The same atomic set carries evidence that even an empty publication exists.
// This reserved member cannot be a provider id. Older writers omit it.
const providerHardExclusionsReadyMember = "ready:v2"

// A cohort publication identifies each client whose evidence was loaded. It
// deliberately omits ready:v2: older readers must read through, not mistake a
// missing historical client for an allowed one.
const providerHardExclusionsCohortReadyMember = "ready:v3:cohort"

// A reserved namespace keeps coverage distinct from positive exclusions.
func providerHardExclusionsCheckedMember(clientId server.Id) string {
	return "checked:" + clientId.String()
}

// Replace exclusions, checked identities and their expiry in one transaction.
func publishProviderHardExclusions(ctx context.Context, clientIds []server.Id, filter providerCountFilter, ttl time.Duration) error {
	members := []any{providerHardExclusionsCohortReadyMember}
	seen := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if seen[clientId] {
			continue
		}
		seen[clientId] = true
		members = append(members, providerHardExclusionsCheckedMember(clientId))
		if filter.hasHardEgressFailure(clientId) {
			members = append(members, clientId.String())
		}
	}
	var err error
	server.Redis(ctx, func(r server.RedisClient) {
		_, err = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, providerHardExclusionsKey)
			pipe.SAdd(ctx, providerHardExclusionsKey, members...)
			pipe.Expire(ctx, providerHardExclusionsKey, ttl)
			return nil
		})
	})
	return err
}

// Bound every input before evaluating the shared fleet policy. Without these
// request-only relation names, hashed EXISTS alternatives can scan the fleet
// even though the outer candidate list and returned rows are bounded.
func providerHardExclusionsSql() string {
	return `
		WITH client_connection_reliability_score AS MATERIALIZED (
			SELECT client_id, lookback_index, independent_reliability_weight
			FROM client_connection_reliability_score WHERE client_id = ANY($1)
		), provider_egress_health AS MATERIALIZED (
			SELECT client_id, tls_authentication_failure, legacy_tls_authentication_failure
			FROM provider_egress_health WHERE client_id = ANY($1)
		), provider_egress_url_security AS MATERIALIZED (
			SELECT client_id, tls_failure
			FROM provider_egress_url_security WHERE client_id = ANY($1) AND tls_failure
		)
		SELECT client_id
		FROM network_client_location_reliability AS provider_location
		WHERE client_id = ANY($1) AND NOT (` + providerEgressEligibilitySql("provider_location") + `)
		UNION
		SELECT client_id
		FROM provider_egress_health
		WHERE client_id = ANY($1) AND tls_authentication_failure = true
		UNION
		SELECT client_id FROM provider_egress_url_security
		WHERE client_id = ANY($1) AND tls_failure
	`
}

// Read complete v2 snapshots during a rolling upgrade, or the checked members
// of a v3 cohort. Missing coverage uses only those candidates on the primary
// database. Backend errors never become an empty exclusion set.
func getProviderHardExclusions(ctx context.Context, clientIds []server.Id) (excludedClientIds map[server.Id]bool, returnErr error) {
	excludedClientIds = map[server.Id]bool{}
	if len(clientIds) == 0 {
		return
	}
	members := make([]any, 0, 2*len(clientIds)+2)
	members = append(members, providerHardExclusionsReadyMember, providerHardExclusionsCohortReadyMember)
	for _, clientId := range clientIds {
		members = append(members, clientId.String(), providerHardExclusionsCheckedMember(clientId))
	}
	unknownClientIds := make([]server.Id, 0, len(clientIds))
	server.Redis(ctx, func(r server.RedisClient) {
		memberships, err := r.SMIsMember(ctx, providerHardExclusionsKey, members...).Result()
		if err != nil {
			returnErr = err
			return
		}
		if len(memberships) != len(members) {
			returnErr = fmt.Errorf("incomplete provider hard-exclusion membership response")
			return
		}
		complete, cohort := memberships[0], memberships[1]
		for i, clientId := range clientIds {
			// A mixed-version set is not a valid atomic publication.
			covered := complete && !cohort || cohort && !complete && memberships[2*i+3]
			if !covered {
				unknownClientIds = append(unknownClientIds, clientId)
			} else if memberships[2*i+2] {
				excludedClientIds[clientId] = true
			}
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	if len(unknownClientIds) != 0 {
		unknownExcluded, err := readProviderHardExclusions(ctx, unknownClientIds)
		if err != nil {
			return nil, err
		}
		for clientId := range unknownExcluded {
			excludedClientIds[clientId] = true
		}
	}
	return
}

// Read-through uses the exporter's reliability, ARIN-risk and security rules without loading
// the fleet. Each query has at most 256 distinct candidates and result rows;
// it observes the caller's context and never publishes a partial global set.
func readProviderHardExclusions(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	const chunkSize = 256
	uniqueClientIds := make([]server.Id, 0, len(clientIds))
	seenClientIds := make(map[server.Id]bool, len(clientIds))
	for _, clientId := range clientIds {
		if !seenClientIds[clientId] {
			seenClientIds[clientId] = true
			uniqueClientIds = append(uniqueClientIds, clientId)
		}
	}
	excludedClientIds := map[server.Id]bool{}
	if len(uniqueClientIds) == 0 {
		return excludedClientIds, nil
	}
	query := providerHardExclusionsSql()
	var returnErr error
	// This is a safety decision, not an analytics read: do not use a replica
	// whose lag could re-admit a provider with a newly accepted hard verdict.
	server.Db(ctx, func(conn server.PgConn) {
		for start := 0; start < len(uniqueClientIds); start += chunkSize {
			chunkClientIds := uniqueClientIds[start:min(start+chunkSize, len(uniqueClientIds))]
			chunkClientIdSet := make(map[server.Id]bool, len(chunkClientIds))
			for _, clientId := range chunkClientIds {
				chunkClientIdSet[clientId] = true
			}
			if err := ctx.Err(); err != nil {
				returnErr = err
				return
			}
			rows, err := conn.Query(ctx, query, chunkClientIds)
			if err != nil {
				returnErr = err
				return
			}
			func() {
				defer rows.Close()
				for rows.Next() {
					var clientId server.Id
					if err := rows.Scan(&clientId); err != nil {
						returnErr = err
						return
					}
					if !chunkClientIdSet[clientId] || excludedClientIds[clientId] {
						returnErr = fmt.Errorf("invalid provider hard-exclusion result population")
						return
					}
					excludedClientIds[clientId] = true
				}
				returnErr = rows.Err()
			}()
			if returnErr != nil {
				return
			}
		}
	}, server.OptReadOnly(), server.OptNoRetry())
	if returnErr != nil {
		return nil, returnErr
	}
	return excludedClientIds, nil
}

// The newer of a provider's two runs, the health run and the location probe,
// whatever their age, so a reader can tell stale evidence from none; nil when
// there is neither.
func egressEvidenceTime(run *EgressHealthRun, observedAt *time.Time) *time.Time {
	var evidenceTime *time.Time
	if run != nil {
		measuredAt := run.MeasuredAt.UTC()
		evidenceTime = &measuredAt
	}
	if observedAt != nil && (evidenceTime == nil || evidenceTime.Before(*observedAt)) {
		observedTime := observedAt.UTC()
		evidenceTime = &observedTime
	}
	return evidenceTime
}

// The dashboard's index label for a provider whose row the new rollup has not
// written.
const ProviderEgressIndexNone = "none"

// The online bucket's name where the buckets are labelled beside the two rank
// modes, and the rank mode a request names it by (RankModeOnline).
const ProviderEgressBucketOnline = RankModeOnline

// The three buckets, in the order the backfill borrows toward.
var ProviderEgressBuckets = []string{
	RankModeQuality,
	RankModeSpeed,
	ProviderEgressBucketOnline,
}

// The connected public pool as the rules see it, for the providers dashboard:
// how many providers of each bucket carry each index, and how many each rule
// leaves out.
type ProviderEgressCounts struct {
	// bucket (a rank mode, or online), then index label, to providers.
	// Membership uses the shared common gates, the URL-success rule and the
	// URL ratio.
	BucketIndexCounts map[string]map[string]int64
	// reason to providers, each under the first rule that took it out of a
	// bucket, every reason present
	ReasonCounts map[string]int64
	// the largest index the settings can produce
	MaxIndex int
	// the modern provider gate's shadow: native bucket (quality or speed),
	// then the gate's outcome, to the providers the other rules admit to
	// the bucket, whether or not the gate is on; every bucket and outcome
	// present
	ModernGateShadowCounts map[string]map[string]int64
}

// Decides every connected, valid, active, top-level provider with a Public
// provide key -- the population the online provider gauge counts -- as the
// client-score job would, with the rollout flag as it is now.
func CountProviderEgress(ctx context.Context) *ProviderEgressCounts {
	settings := egressIndexSettings()
	egressTestEnabled := providerEgressTestEnabled()

	counts := &ProviderEgressCounts{
		BucketIndexCounts:      map[string]map[string]int64{},
		ReasonCounts:           map[string]int64{},
		MaxIndex:               settings.MaxIndex(),
		ModernGateShadowCounts: newProviderModernGateShadowCounts(),
	}
	for _, bucket := range ProviderEgressBuckets {
		counts.BucketIndexCounts[bucket] = map[string]int64{}
	}
	for _, reason := range ProviderExcludedReasons {
		counts.ReasonCounts[reason] = 0
	}

	rows := []providerCountRow{}
	clientIds := []server.Id{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id,
				network_client_location_reliability.egress_index,
				network_client_location_reliability.egress_quality,
				country_location.country_code
			FROM network_client_location_reliability
			INNER JOIN network_client ON
				network_client.client_id = network_client_location_reliability.client_id
			LEFT JOIN location AS country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			WHERE
				network_client.active = true AND
				network_client.source_client_id IS NULL AND
				network_client_location_reliability.connected = true AND
				network_client_location_reliability.valid = true AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE
						provide_key.client_id = network_client_location_reliability.client_id AND
						provide_key.provide_mode = $1
				)
			`,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row providerCountRow
				server.Raise(result.Scan(&row.clientId, &row.egressIndex, &row.egressQuality, &row.claimedCountryCode))
				rows = append(rows, row)
				clientIds = append(clientIds, row.clientId)
			}
		})
	})
	// Capture the exact dashboard population before loading its evidence.
	countFilter := newProviderCountFilterForClients(ctx, clientIds)
	// the URL-success rule's country guard over this same population, its
	// margin held against the export's last published guard
	countFilter.onlineSuccessBaseline = newProviderOnlineSuccessBaseline(countFilter, providerCountRowCountryCodes(rows), settings, readProviderOnlineSuccessPreviousBaseline(ctx, "the dashboard counts"))
	for _, row := range rows {
		decision := decideProviderEgress(
			countFilter.egressFacts(row.clientId, row.claimedCountryCode, row.egressIndex, row.egressQuality, settings),
			egressTestEnabled,
		)
		// the index as the dashboard labels it
		indexLabel := ProviderEgressIndexNone
		if row.egressIndex != nil {
			indexLabel = strconv.Itoa(*row.egressIndex)
		}
		if decision.quality {
			counts.BucketIndexCounts[RankModeQuality][indexLabel] += 1
		}
		if decision.speed {
			counts.BucketIndexCounts[RankModeSpeed][indexLabel] += 1
		}
		if decision.online {
			counts.BucketIndexCounts[ProviderEgressBucketOnline][indexLabel] += 1
		}
		if decision.reason != "" {
			counts.ReasonCounts[decision.reason] += 1
		}
		countProviderModernGateShadow(counts.ModernGateShadowCounts, decision)
	}
	return counts
}

// One provider as the rules of GEOMAP §10.3 see it, for `bringyourctl provider
// inspect`.
type ProviderEgressInspection struct {
	ClientId  server.Id
	Connected bool
	Valid     bool
	// the country the provider is published under, "" when unknown
	PublishedCountryCode string
	MaxNetTypeScore      int
	MaxNetTypeScoreSpeed int
	// the rollup's columns
	EgressIndex        *int
	EgressQuality      *bool
	EgressEvidenceTime *time.Time
	// the latest health run, and the index it gives now, which the next
	// rollup will store
	HealthRun    *ProviderEgressHealth
	CurrentIndex EgressIndex
	// the country a fresh probe observed the exit in, "" for none
	ObservedCountryCode     string
	Blackholed              bool
	TlsAuthenticationFailed bool
	ArinRisk                bool
	ArinNonQuality          bool
	ReliabilityFailed       bool
	UrlSuccessCount         int
	UrlErrorCount           int
	// measured at least ProviderOnlineMinMeasuredUrlRuns times in the
	// evidence window without one success, whatever the rule then decides
	NoUrlSuccess bool
	// measured as often, with a success but below the Online URL ratio the
	// score export published, whatever the rule then decides; false while
	// the published ratio is off or no trusted guard is readable
	BelowOnlineUrlRatio bool
	// the URL-success rule as the score export last published it, which the
	// decision below follows rather than Settings: whether a trusted guard
	// was readable, when it was computed and published, the kill switch,
	// floor and margin the export applied, the published country's measured
	// Online providers and those with a success, and whether the country is
	// exempt
	OnlineSuccessBaselineKnown       bool
	OnlineSuccessBaselineTime        time.Time
	OnlineSuccessBaselinePublishTime time.Time
	OnlineSuccessBaselineEnabled     bool
	OnlineSuccessBaselineFloor       float64
	OnlineSuccessBaselineMargin      float64
	OnlineSuccessCountryMeasured     int
	OnlineSuccessCountrySucceeded    int
	OnlineSuccessCountryExempt       bool
	// the Online URL ratio the score export applied, zero for off, and
	// whether it kept the country exemption
	OnlineUrlSuccessBaselineRatio          float64
	OnlineUrlRatioBaselineCountryExemption bool
	// the modern provider gate: the provider's evidence row, nil without
	// one, the outcome it gives now, and whether the gate holds Quality and
	// Speed under Settings at ModernGateTime, the instant this inspection
	// decided at; the decision below applies the outcome when it does
	ModernEvidence    *ProviderModernEvidenceRecord
	ModernOutcome     string
	ModernGate        bool
	ModernGateTime    time.Time
	EgressTestEnabled bool
	Settings          *EgressIndexSettings
	// the decision, as the client-score job makes it
	Reason       string
	HardExcluded bool
	Quality      bool
	Speed        bool
	Online       bool
	Counted      bool
}

// Reads one provider's rollup and accepted URL evidence for operator inspection.
// This diagnostic reads the publication pass's evidence for this provider only.
func InspectProviderEgress(ctx context.Context, clientId server.Id) *ProviderEgressInspection {
	var inspection *ProviderEgressInspection
	var publishedCountryCode *string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.connected,
				network_client_location_reliability.valid,
				network_client_location_reliability.max_net_type_score,
				network_client_location_reliability.max_net_type_score_speed,
				network_client_location_reliability.egress_index,
				network_client_location_reliability.egress_quality,
				network_client_location_reliability.egress_evidence_time,
				country_location.country_code
			FROM network_client_location_reliability
			LEFT JOIN location AS country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			WHERE network_client_location_reliability.client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				inspection = &ProviderEgressInspection{
					ClientId: clientId,
				}
				server.Raise(result.Scan(
					&inspection.Connected,
					&inspection.Valid,
					&inspection.MaxNetTypeScore,
					&inspection.MaxNetTypeScoreSpeed,
					&inspection.EgressIndex,
					&inspection.EgressQuality,
					&inspection.EgressEvidenceTime,
					&publishedCountryCode,
				))
			}
		})
	})
	if inspection == nil {
		return nil
	}

	inspection.Settings = egressIndexSettings()
	inspection.EgressTestEnabled = providerEgressTestEnabled()
	countFilter := newProviderCountFilterForClients(ctx, []server.Id{clientId})
	// one provider cannot measure its country, so the guard, its kill switch
	// and floor included, is the score export's
	baseline := getProviderOnlineSuccessBaseline(ctx)
	countFilter.onlineSuccessBaseline = baseline

	facts := countFilter.egressFacts(
		clientId,
		publishedCountryCode,
		inspection.EgressIndex,
		inspection.EgressQuality,
		inspection.Settings,
	)
	decision := decideProviderEgress(facts, inspection.EgressTestEnabled)
	inspection.PublishedCountryCode = normalizeCountryCode(publishedCountryCode)
	if baseline != nil {
		inspection.OnlineSuccessBaselineKnown = true
		inspection.OnlineSuccessBaselineTime = baseline.computeTime
		inspection.OnlineSuccessBaselinePublishTime = baseline.publishTime
		inspection.OnlineSuccessBaselineEnabled = baseline.enabled
		inspection.OnlineSuccessBaselineFloor = baseline.floor
		inspection.OnlineSuccessBaselineMargin = baseline.margin
		count := baseline.countryCodeCounts[inspection.PublishedCountryCode]
		inspection.OnlineSuccessCountryMeasured = count.Measured
		inspection.OnlineSuccessCountrySucceeded = count.Succeeded
		inspection.OnlineUrlSuccessBaselineRatio = baseline.urlSuccessRatio
		inspection.OnlineUrlRatioBaselineCountryExemption = baseline.urlRatioCountryExemption
	}
	inspection.OnlineSuccessCountryExempt = baseline.exempt(inspection.PublishedCountryCode)
	inspection.ObservedCountryCode = countFilter.countryCodes[clientId]
	inspection.Blackholed = facts.blackholed
	inspection.TlsAuthenticationFailed = facts.tlsAuthenticationFailed
	inspection.ArinRisk = facts.arinRisk
	inspection.ArinNonQuality = facts.arinNonQuality
	inspection.ReliabilityFailed = facts.reliabilityFailed
	inspection.ModernEvidence = countFilter.modernEvidence[clientId]
	inspection.ModernOutcome = facts.modernOutcome
	inspection.ModernGate = facts.modernGate
	inspection.ModernGateTime = countFilter.now
	inspection.Reason = decision.reason
	inspection.HardExcluded = decision.hardExcluded
	inspection.Quality = decision.quality
	inspection.Speed = decision.speed
	inspection.Online = decision.online
	inspection.Counted = decision.counted

	inspection.HealthRun = GetProviderEgressHealth(ctx, clientId)
	var run *EgressHealthRun
	if counts, ok := countFilter.healthCounts[clientId]; ok {
		inspection.UrlSuccessCount = counts.OKCount
		inspection.UrlErrorCount = counts.Total - counts.OKCount
		run = &EgressHealthRun{
			MeasuredAt: counts.MeasuredAt,
			OkCount:    counts.OKCount,
			Total:      counts.Total,
		}
	}
	inspection.NoUrlSuccess = providerCarriesNoUrl(countFilter.urlIndex(clientId, inspection.Settings))
	if baseline != nil {
		counts, urlIndex := countFilter.urlIndex(clientId, inspection.Settings)
		inspection.BelowOnlineUrlRatio = providerBelowOnlineUrlRatio(counts, urlIndex, baseline.urlSuccessRatio)
	}
	inspection.CurrentIndex = ComputeEgressIndex(run, server.NowUtc(), inspection.Settings)
	return inspection
}

// A stored country code as the rules compare it: lower case, without the
// padding a char(2) column can carry, "" for none.
func normalizeCountryCode(countryCode *string) string {
	if countryCode == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*countryCode))
}
