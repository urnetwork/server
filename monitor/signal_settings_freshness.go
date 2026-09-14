package monitor

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

// NewSettingsFreshnessSignal implements SIGNALS.md §1.6
// (`settings-freshness`). A continuous monitor intentionally keeps one
// immutable SignalSettings snapshot because inventory and standing log tails
// require a controlled handoff. This signal makes a later effective settings
// generation visible instead of letting the old watcher remain silently green.
func NewSettingsFreshnessSignal() Signal { return &settingsFreshnessSignal{} }

type settingsFreshnessSignal struct{}

func (*settingsFreshnessSignal) Number() string         { return "1.6" }
func (*settingsFreshnessSignal) Key() string            { return "settings-freshness" }
func (*settingsFreshnessSignal) ID() string             { return "monitor/settings-generation" }
func (*settingsFreshnessSignal) Name() string           { return "Monitor settings generation freshness" }
func (*settingsFreshnessSignal) Cadence() time.Duration { return time.Minute }

func (s *settingsFreshnessSignal) Run(ctx context.Context, settings SignalSettings) (Alerts, error) {
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		return nil, err
	}
	if settings.SettingsGenerationCheck == nil {
		// Manually assembled and embedded SignalSettings predate this optional
		// local-only observation seam. Production LoadSignalSettings always arms
		// it; absence therefore preserves API compatibility without fabricating a
		// filesystem generation for synthetic callers.
		return nil, nil
	}

	current, err := settings.SettingsGenerationCheck(ctx, settings)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		errorClass := classifyObservationError(err)
		return Alerts{{
			SignalNumber: s.Number(),
			SignalKey:    s.Key(),
			SignalID:     s.ID(),
			SignalName:   s.Name(),
			Severity:     SeverityWarn,
			Class:        "settings-generation-unobservable",
			Target:       "monitor-settings",
			Environment:  settings.Environment,
			ObservedAt:   settings.Now(),
			Sustain:      1,
			PageSustain:  5,
			Symptom:      "The monitor cannot determine whether its captured settings still match the effective Config and Vault inputs.",
			Mechanism:    "The local settings loader failed while checking the current generation. Existing probe results still use the startup snapshot, so configuration-derived targets and expectations are unknown until a fresh loader succeeds.",
			Baseline:     "The effective monitor settings reload successfully and equal the immutable startup snapshot.",
			Observed:     "settings_generation_observable=false error_class=" + errorClass,
			Evidence:     "The failed comparison retained only a fixed observation-error class; resource paths, contents, credentials, and content-derived fingerprints were discarded.",
			Action:       "Repair the local Config/Vault resolution path, run a current-source one-shot, then use the controlled watcher-promotion procedure. Do not trust configuration-derived green findings, print resource values, or hot-reload only part of the inventory.",
			Verify:       "A newly built watcher loads the complete effective settings, starts every expected tail and probe, and this class remains absent through two one-minute cadences after promotion.",
			Playbook:     "SIGNALS.md §1.6 and RUN-MAIN.md Safe watcher promotion",
		}}, nil
	}
	if current {
		return nil, nil
	}

	return Alerts{{
		SignalNumber: s.Number(),
		SignalKey:    s.Key(),
		SignalID:     s.ID(),
		SignalName:   s.Name(),
		Severity:     SeverityWarn,
		Class:        "settings-generation-stale",
		Target:       "monitor-settings",
		Environment:  settings.Environment,
		ObservedAt:   settings.Now(),
		Sustain:      1,
		PageSustain:  5,
		Symptom:      "The monitor is still running with an older effective settings generation.",
		Mechanism:    "SignalSettings, host inventory, standing log-tail ownership, credentials, and desired-state expectations are captured at process start. A later effective Config or Vault change cannot safely update only one of those coupled boundaries in place.",
		Baseline:     "The immutable startup settings equal a fresh load of every effective monitor input.",
		Observed:     "settings_generation_current=false",
		Evidence:     "The startup and current effective settings were compared only in memory. Resource contents and secret-derived hashes are never rendered or persisted by this signal.",
		Context:      "Until controlled promotion completes, configuration-derived findings from this watcher describe its startup generation rather than current desired state.",
		Action:       "Build a fresh immutable monitor, run its current-source one-shot, and perform the controlled overlap/promotion procedure. If a host was newly disabled, stop using the stale watcher for host contact as soon as the replacement is proven. Do not hot-reload a partial inventory or suppress this boundary.",
		Verify:       "The replacement watcher loads the intended generation, owns every expected standing tail and probe, and this class remains absent through two one-minute cadences after the old watcher exits.",
		Playbook:     "SIGNALS.md §1.6 and RUN-MAIN.md Safe watcher promotion",
	}}, nil
}

// NewSettingsGenerationCheck creates the local-only comparison seam used by
// LoadSignalSettings and command wrappers with immutable CLI overrides.
func NewSettingsGenerationCheck(load SignalSettingsLoader) SettingsGenerationCheck {
	return func(ctx context.Context, startup SignalSettings) (bool, error) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		current, err := load()
		if err != nil {
			return false, fmt.Errorf("settings reload: %w", err)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		return reflect.DeepEqual(comparableSignalSettings(startup), comparableSignalSettings(current)), nil
	}
}

// comparableSignalSettings removes process-only seams. The remaining value is
// the complete effective probe input, including credentials that may change
// connectivity. Deep equality remains in memory; callers receive one Boolean,
// never either value or a content-derived fingerprint.
func comparableSignalSettings(settings SignalSettings) SignalSettings {
	settings = settings.withDefaults()
	settings.SettingsGenerationCheck = nil
	settings.Source = nil
	settings.Now = nil
	settings.runtime = nil
	return settings
}
