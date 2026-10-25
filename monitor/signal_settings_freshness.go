package monitor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		return nil, err
	}
	scopeAlerts := Alerts{}
	if len(settings.ExcludedHosts) != 0 {
		scopeAlerts = append(scopeAlerts, alertFromFinding(settings, s.Number(), s.Key(), s.Name(), hostScopeCoverageFinding(settings, 0)))
	}
	if settings.SettingsGenerationCheck == nil {
		// Manually assembled and embedded SignalSettings predate this optional
		// local-only observation seam. Production LoadSignalSettings always arms
		// it; absence therefore preserves API compatibility without fabricating a
		// filesystem generation for synthetic callers.
		return scopeAlerts, nil
	}

	started := time.Now()
	diagnostic := &settingsComparisonDiagnostic{}
	comparisonCtx := context.WithValue(ctx, settingsComparisonDiagnosticKey{}, diagnostic)
	current, err := settings.SettingsGenerationCheck(comparisonCtx, settings)
	completed := time.Now()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if witness := settings.settingsGenerationWitness; witness != nil {
		if witness.observe(ctx, settings, started, completed, current, err) != nil {
			scopeAlerts = append(scopeAlerts, Alert{
				SignalNumber: s.Number(), SignalKey: s.Key(), SignalID: s.ID(), SignalName: s.Name(),
				Severity: SeverityWarn, Class: "cannot-observe", Target: "monitor-settings/witness",
				Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1, unobservable: true,
				Symptom:   "The optional local settings-generation witness is unavailable.",
				Mechanism: "The private writer could not establish its startup association or complete a bounded record. The actual freshness callback result remains independent.",
				Baseline:  "Each existing freshness invocation retains a complete fixed-schema witness bound to this process and startup settings.",
				Observed:  "settings_witness_available=false",
				Evidence:  "Only fixed status is exposed; paths, settings, credentials and writer errors are discarded.",
				Action:    "Preserve the failed private output and qualify a fresh destination through the controlled watcher workflow. Do not infer equality from missing records or retry this stream in place.",
				Verify:    "A separately qualified watcher retains a complete same-process callback witness; the original freshness and ingestion incident requirements still apply.",
				Playbook:  "SIGNALS.md §1.6",
			})
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		errorClass := classifyObservationError(err)
		return append(scopeAlerts, Alert{
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
			unobservable: true,
			PageSustain:  5,
			Symptom:      "The monitor cannot determine whether its captured settings still match the effective Config and Vault inputs.",
			Mechanism:    "The local settings loader failed while checking the current generation. Existing probe results still use the startup snapshot, so configuration-derived targets and expectations are unknown until a fresh loader succeeds.",
			Baseline:     "The effective monitor settings reload successfully and equal the immutable startup snapshot.",
			Observed:     "settings_generation_observable=false error_class=" + errorClass,
			Evidence:     "The failed comparison retained only a fixed observation-error class; resource paths, contents, credentials, and content-derived fingerprints were discarded.",
			Action:       "Repair the local Config/Vault resolution path, run a current-source one-shot, then use the controlled watcher-promotion procedure. Do not trust configuration-derived green findings, print resource values, or hot-reload only part of the inventory.",
			Verify:       "A newly built watcher loads the complete effective settings, starts every expected tail and probe, and this class remains absent through two one-minute cadences after promotion.",
			Playbook:     "SIGNALS.md §1.6 and RUN-MAIN.md Safe watcher promotion",
		}), nil
	}
	if current {
		return scopeAlerts, nil
	}

	return append(scopeAlerts, Alert{
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
		Observed:     "settings_generation_current=false mismatch_categories=" + diagnostic.summary(),
		Evidence:     "The startup and current effective settings were compared only in memory. Resource contents and secret-derived hashes are never rendered or persisted by this signal.",
		Context:      "Until controlled promotion completes, configuration-derived findings from this watcher describe its startup generation rather than current desired state.",
		Action:       "Build a fresh immutable monitor, run its current-source one-shot, and perform the controlled overlap/promotion procedure. If a host was newly disabled, stop using the stale watcher for host contact as soon as the replacement is proven. Do not hot-reload a partial inventory or suppress this boundary.",
		Verify:       "The replacement watcher loads the intended generation, owns every expected standing tail and probe, and this class remains absent through two one-minute cadences after the old watcher exits.",
		Playbook:     "SIGNALS.md §1.6 and RUN-MAIN.md Safe watcher promotion",
	}), nil
}

// NewSettingsGenerationCheck creates the local-only comparison seam used by
// LoadSignalSettings and command wrappers with immutable CLI overrides.
func NewSettingsGenerationCheck(load SignalSettingsLoader) SettingsGenerationCheck {
	return func(ctx context.Context, startup SignalSettings) (bool, error) {
		diagnostic, _ := ctx.Value(settingsComparisonDiagnosticKey{}).(*settingsComparisonDiagnostic)
		if diagnostic != nil {
			diagnostic.stateLock.Lock()
			diagnostic.comparisons++
			diagnostic.inflight++
			diagnostic.stateLock.Unlock()
			defer func() {
				diagnostic.stateLock.Lock()
				diagnostic.inflight--
				diagnostic.stateLock.Unlock()
			}()
		}
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
		startupComparable := comparableSignalSettings(startup)
		currentComparable := comparableSignalSettings(current)
		equal := reflect.DeepEqual(startupComparable, currentComparable)
		if diagnostic != nil {
			diagnostic.stateLock.Lock()
			if !equal {
				diagnostic.categories = settingsMismatchCategories(startupComparable, currentComparable)
			}
			diagnostic.stateLock.Unlock()
		}
		return equal, nil
	}
}

// comparableSignalSettings removes process-only seams. The remaining value is
// the complete effective probe input, including credentials that may change
// connectivity. Deep equality remains in memory; callers receive one Boolean,
// never either value or a content-derived fingerprint.
func comparableSignalSettings(settings SignalSettings) SignalSettings {
	settings = settings.withDefaults()
	settings.SettingsGenerationCheck = nil
	settings.settingsGenerationWitness = nil
	settings.UrlProbeCoverageObserver = nil
	settings.ProviderSelectionObserver = nil
	settings.Source = nil
	settings.Now = nil
	settings.runtime = nil
	settings.sharedSshAdmission = nil
	return settings
}

// Only one entered and completed comparison may attribute a freshness mismatch.
// No collector is installed by request-local guards or alternate API callers.
type settingsComparisonDiagnosticKey struct{}

type settingsComparisonDiagnostic struct {
	stateLock   sync.Mutex
	comparisons int
	inflight    int
	categories  []string
}

// Custom Boolean-only checks and ambiguous multi-check callbacks stay unknown.
func (self *settingsComparisonDiagnostic) summary() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.comparisons != 1 || self.inflight != 0 || len(self.categories) == 0 {
		return "unknown"
	}
	return strings.Join(self.categories, ",")
}

// Every published token is a fixed owned category, never a reflected field name,
// map key, value, endpoint, identifier or credential-derived fingerprint.
func settingsMismatchCategories(startup, current SignalSettings) []string {
	groups := []struct {
		category string
		startup  any
		current  any
	}{
		{category: "environment", startup: startup.Environment, current: current.Environment},
		{category: "public_scope", startup: []any{startup.PublicDomain, startup.WebsiteDomain, startup.ManagerHostname}, current: []any{current.PublicDomain, current.WebsiteDomain, current.ManagerHostname}},
		{category: "log_inventory", startup: []any{startup.LogServices, startup.LogServiceBlocks, startup.LogServiceHosts, startup.ProxyPathExpectedHosts}, current: []any{current.LogServices, current.LogServiceBlocks, current.LogServiceHosts, current.ProxyPathExpectedHosts}},
		{category: "verification_policy", startup: []any{startup.VerificationEnabled, startup.STConfigStatus, startup.STDeploymentKey}, current: []any{current.VerificationEnabled, current.STConfigStatus, current.STDeploymentKey}},
		{category: "subnet_policy", startup: []any{startup.SubnetCorrectness, startup.SubnetArtifacts, startup.SubnetReplay, startup.SubnetDeployment}, current: []any{current.SubnetCorrectness, current.SubnetArtifacts, current.SubnetReplay, current.SubnetDeployment}},
		{category: "contract_policy", startup: startup.ContractDegradationConfig, current: current.ContractDegradationConfig},
		{category: "ssh_policy", startup: []any{startup.SSHUser, startup.SSHDevUser, startup.SSHKeyPaths, startup.AddressMode}, current: []any{current.SSHUser, current.SSHDevUser, current.SSHKeyPaths, current.AddressMode}},
		{category: "host_inventory", startup: []any{startup.Hosts, startup.disabledHosts}, current: []any{current.Hosts, current.disabledHosts}},
		{category: "router_inventory", startup: []any{startup.Routers, startup.routerDesiredGeneration}, current: []any{current.Routers, current.routerDesiredGeneration}},
		{category: "public_udp", startup: startup.PublicUdp, current: current.PublicUdp},
		{category: "observer_overlay", startup: startup.ObserverOverlay, current: current.ObserverOverlay},
		{category: "excluded_hosts", startup: startup.ExcludedHosts, current: current.ExcludedHosts},
		{category: "postgresql", startup: startup.PostgreSQL, current: current.PostgreSQL},
		{category: "grafana", startup: startup.Grafana, current: current.Grafana},
		{category: "reporting", startup: []any{startup.GooglePlay, startup.AppleReporting}, current: []any{current.GooglePlay, current.AppleReporting}},
		{category: "credential_requirements", startup: startup.Credentials, current: current.Credentials},
		{category: "source_attribution", startup: startup.SourceAttribution, current: current.SourceAttribution},
		{category: "dns_aliases", startup: startup.DNSAliases, current: current.DNSAliases},
		{category: "mimir_publishers", startup: startup.MimirPublishers, current: current.MimirPublishers},
		{category: "alert_notifications", startup: startup.AlertNotifications, current: current.AlertNotifications},
		{category: "api_release_proof", startup: startup.APIReleaseProof, current: current.APIReleaseProof},
		{category: "pg_sampling_policy", startup: []any{startup.PGQuerySampleContinuous, startup.PGQuerySampleUntil}, current: []any{current.PGQuerySampleContinuous, current.PGQuerySampleUntil}},
		{category: "state_directory", startup: startup.StateDir, current: current.StateDir},
		{category: "command_timeouts", startup: []any{startup.SSHConnectTimeout, startup.CommandTimeout}, current: []any{current.SSHConnectTimeout, current.CommandTimeout}},
	}
	categories := []string{}
	for _, group := range groups {
		if !reflect.DeepEqual(group.startup, group.current) {
			categories = append(categories, group.category)
		}
	}
	if len(categories) == 0 {
		// A future comparable field outside this closed classification must not
		// weaken the equality guard or be guessed into an unrelated category.
		categories = append(categories, "unclassified_input")
	}
	return categories
}
