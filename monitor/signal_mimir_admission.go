// Mimir admission observation stays host-local until sensitive metrics and
// configuration have been reduced to strict numeric frames.
package monitor

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mimirAdmissionMarker                   = "monitor-signal-11.20a-mimir-admission"
	mimirAdmissionQuietWindow              = 2 * time.Hour
	mimirAdmissionStateVersion             = 1
	mimirAdmissionHistoryLimit             = 1024
	mimirAdmissionLazyFamilyVersion        = "3.1.1"
	mimirAdmissionLazyFamilySourceRevision = "a3d6c90f25"
)

// The source allowlist is deliberately exact. An unfamiliar Mimir build can
// still be observed through a valid descriptor, but whole-family absence on
// that build remains unknown until its collector behavior has been reviewed.

// Signal mimir-admission implements SIGNALS.md §11.20a. It reads every
// bundled Mimir child's exact process counters and keeps admission failures
// active until a complete, comparable two-hour quiet window has elapsed.
func NewMimirAdmissionSignal() Signal {
	return &signalAdapter{
		number: "11.20a", key: "mimir-admission", name: "Mimir series admission headroom",
		probe: &mimirAdmissionProbe{},
	}
}

// The state lock makes counter baselines and the fleet quiet hold atomic when
// an embedded caller invokes one signal concurrently.
type mimirAdmissionProbe struct {
	cycleLock         sync.Mutex
	stateLock         sync.Mutex
	stateLoaded       bool
	stateDir          string
	quietResetDone    bool
	histories         map[mimirAdmissionIdentity]mimirAdmissionHistory
	incident          bool
	quietSince        time.Time
	lockProviderState func(context.Context, string, string) (*providerStateLock, error)
	loadProviderState func(string, string, int, any) (bool, error)
	saveProviderState func(string, string, int, any) error
}

// The stable identity is shared by positive and healthy fleet findings.
func (*mimirAdmissionProbe) id() string { return "observability/mimir-admission" }

// Admission loss is an immediate page because rejected samples are dropped.
func (*mimirAdmissionProbe) tier() string { return tierPage }

// A one-minute interval bounds detection and journal-correlation windows.
func (*mimirAdmissionProbe) cadence() time.Duration { return time.Minute }

// A port and exact canonical process start distinguish overlapping children
// on one host without depending on a rounded wall-clock timestamp.
type mimirAdmissionIdentity struct {
	host         string
	port         int
	processStart string
}

// Only monotonic counters need a prior value; gauges are current-page context.
type mimirAdmissionHistory struct {
	discardTotal int64
	createdTotal int64
	removedTotal int64
}

// One strict frame represents either a complete child or an explicitly
// unobservable child. No metric label or rendered configuration is retained.
type mimirAdmissionInstance struct {
	port                 int
	observable           bool
	observableSeen       bool
	processStart         string
	memorySeries         int64
	activeSeries         int64
	createdTotal         int64
	removedTotal         int64
	localLimit           int64
	globalLimit          int64
	discardDescriptor    bool
	discardFamilyAbsent  bool
	discardAbsenceSource bool
	discardPresent       bool
	discardTotal         int64
	seen                 map[string]bool
}

// Host-level journal counts are diagnostic context only. A failed journal
// reduction does not suppress an affirmative direct Mimir counter increase.
type mimirAdmissionHostSample struct {
	instances        []mimirAdmissionInstance
	count            int
	countSeen        bool
	journalComplete  bool
	journalSeen      bool
	publisherStarts  int64
	publisherSeen    bool
	readinessRejects int64
	readinessSeen    bool
	admissionRejects int64
	admissionSeen    bool
}

// A host error is retained beside any confirmed sibling admission failure.
type mimirAdmissionHostResult struct {
	host   *host
	sample mimirAdmissionHostSample
	err    error
}

// The assessment is a privacy-safe fleet reduction used by the page renderer.
type mimirAdmissionAssessment struct {
	configuredHosts     int
	observableHosts     int
	instanceCount       int
	descriptorInstances int
	sourceZeroInstances int
	journalHosts        int
	publisherStarts     int64
	readinessRejects    int64
	admissionRejects    int64
	affectedInstances   int
	initialPositive     int
	discardIncrease     int64
	createdIncrease     int64
	removedIncrease     int64
	memoryMinimum       int64
	memoryMaximum       int64
	activeMinimum       int64
	activeMaximum       int64
	localLimitMinimum   int64
	localLimitMaximum   int64
	globalLimitMinimum  int64
	globalLimitMaximum  int64
	headroomMinimum     int64
	headroomMaximum     int64
	generationChanges   int
	counterResets       int
	directComplete      bool
	comparable          bool
	incidentActive      bool
	quietFor            time.Duration
	visibilityFailures  []mimirAdmissionVisibilityFailure
}

// Visibility failures use fixed messages so remote output cannot enter an
// alert through an error string.
type mimirAdmissionVisibilityFailure struct {
	target string
	err    error
}

// The versioned state contains only the bounded counter identity and values
// needed to survive a watcher replacement without persisting metric labels.
type mimirAdmissionPersistedState struct {
	Incident       bool                             `json:"incident"`
	QuietSinceUnix int64                            `json:"quiet_since_unix"`
	Histories      []mimirAdmissionPersistedHistory `json:"histories"`
}

// One persisted history entry reconstructs a host/process counter baseline.
type mimirAdmissionPersistedHistory struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	ProcessStart string `json:"process_start"`
	DiscardTotal int64  `json:"discard_total"`
	CreatedTotal int64  `json:"created_total"`
	RemovedTotal int64  `json:"removed_total"`
}

type mimirAdmissionRuntimeState struct {
	histories  map[mimirAdmissionIdentity]mimirAdmissionHistory
	incident   bool
	quietSince time.Time
}

var mimirAdmissionProcessStartPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// Process-start exposition is retained exactly enough to distinguish child
// generations, then canonicalized so equivalent decimal spellings compare.
func canonicalMimirAdmissionProcessStart(value string) (string, error) {
	if len(value) == 0 || len(value) > 128 || !mimirAdmissionProcessStartPattern.MatchString(value) {
		return "", fmt.Errorf("invalid process start")
	}
	floatValue, err := strconv.ParseFloat(value, 64)
	if err != nil || floatValue <= 0 || math.IsInf(floatValue, 0) {
		return "", fmt.Errorf("invalid process start")
	}
	number, ok := new(big.Rat).SetString(value)
	if !ok || number.Sign() <= 0 {
		return "", fmt.Errorf("invalid process start")
	}
	return number.RatString(), nil
}

// A fresh signal resets pre-start quiet time once, but every durable cycle
// reloads the latest state while the cross-process lock is held.
func (self *mimirAdmissionProbe) loadState(stateDir string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.stateLoaded && self.stateDir != stateDir {
		return fmt.Errorf("Mimir admission state directory changed")
	}
	if stateDir == "" && self.stateLoaded {
		return nil
	}

	state := mimirAdmissionPersistedState{}
	loader := self.loadProviderState
	if loader == nil {
		loader = loadProviderState
	}
	loaded, err := loader(stateDir, "mimir-admission", mimirAdmissionStateVersion, &state)
	if err != nil {
		return err
	}
	histories := map[mimirAdmissionIdentity]mimirAdmissionHistory{}
	if loaded {
		if err := validateMimirAdmissionPersistedState(state); err != nil {
			return err
		}
		for _, persisted := range state.Histories {
			identity := mimirAdmissionIdentity{
				host:         persisted.Host,
				port:         persisted.Port,
				processStart: persisted.ProcessStart,
			}
			histories[identity] = mimirAdmissionHistory{
				discardTotal: persisted.DiscardTotal,
				createdTotal: persisted.CreatedTotal,
				removedTotal: persisted.RemovedTotal,
			}
		}
	}
	self.stateLoaded = true
	self.stateDir = stateDir
	self.histories = histories
	self.incident = loaded && state.Incident
	self.quietSince = time.Time{}
	if self.quietResetDone && state.QuietSinceUnix > 0 {
		self.quietSince = time.Unix(state.QuietSinceUnix, 0).UTC()
	}
	return nil
}

// Atomic versioned persistence happens after every observation. The caller
// does not adopt a quiet reset or mutation unless this save succeeds.
func (self *mimirAdmissionProbe) saveState(stateDir string) error {
	self.stateLock.Lock()
	histories := make([]mimirAdmissionPersistedHistory, 0, len(self.histories))
	for identity, history := range self.histories {
		histories = append(histories, mimirAdmissionPersistedHistory{
			Host:         identity.host,
			Port:         identity.port,
			ProcessStart: identity.processStart,
			DiscardTotal: history.discardTotal,
			CreatedTotal: history.createdTotal,
			RemovedTotal: history.removedTotal,
		})
	}
	sort.Slice(histories, func(i int, j int) bool {
		if histories[i].Host != histories[j].Host {
			return histories[i].Host < histories[j].Host
		}
		if histories[i].Port != histories[j].Port {
			return histories[i].Port < histories[j].Port
		}
		return histories[i].ProcessStart < histories[j].ProcessStart
	})
	quietSinceUnix := int64(0)
	if !self.quietSince.IsZero() {
		quietSinceUnix = self.quietSince.Unix()
	}
	state := mimirAdmissionPersistedState{
		Incident:       self.incident,
		QuietSinceUnix: quietSinceUnix,
		Histories:      histories,
	}
	self.stateLock.Unlock()
	if err := validateMimirAdmissionPersistedState(state); err != nil {
		return err
	}
	saver := self.saveProviderState
	if saver == nil {
		saver = saveProviderState
	}
	return saver(stateDir, "mimir-admission", mimirAdmissionStateVersion, state)
}

func (self *mimirAdmissionProbe) snapshotState() mimirAdmissionRuntimeState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return mimirAdmissionRuntimeState{
		histories:  cloneMimirAdmissionHistories(self.histories),
		incident:   self.incident,
		quietSince: self.quietSince,
	}
}

func (self *mimirAdmissionProbe) restoreState(state mimirAdmissionRuntimeState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.histories = cloneMimirAdmissionHistories(state.histories)
	self.incident = state.incident
	self.quietSince = state.quietSince
}

func cloneMimirAdmissionHistories(
	histories map[mimirAdmissionIdentity]mimirAdmissionHistory,
) map[mimirAdmissionIdentity]mimirAdmissionHistory {
	cloned := make(map[mimirAdmissionIdentity]mimirAdmissionHistory, len(histories))
	for identity, history := range histories {
		cloned[identity] = history
	}
	return cloned
}

// Loaded state is rejected before adoption if it could collapse identities,
// manufacture negative counters, or exceed the configured fleet bound.
func validateMimirAdmissionPersistedState(state mimirAdmissionPersistedState) error {
	if len(state.Histories) > mimirAdmissionHistoryLimit {
		return fmt.Errorf("Mimir admission state has too many histories")
	}
	if state.QuietSinceUnix < 0 || (!state.Incident && state.QuietSinceUnix != 0) {
		return fmt.Errorf("Mimir admission state has an invalid quiet boundary")
	}
	seenIdentities := map[mimirAdmissionIdentity]bool{}
	for _, persisted := range state.Histories {
		identity := mimirAdmissionIdentity{
			host:         persisted.Host,
			port:         persisted.Port,
			processStart: persisted.ProcessStart,
		}
		processStart, ok := new(big.Rat).SetString(persisted.ProcessStart)
		if strings.TrimSpace(persisted.Host) == "" || persisted.Port < 1 || persisted.Port > 65535 ||
			!ok || processStart.Sign() <= 0 || processStart.RatString() != persisted.ProcessStart ||
			persisted.DiscardTotal < 0 || persisted.CreatedTotal < 0 || persisted.RemovedTotal < 0 {
			return fmt.Errorf("Mimir admission state has an invalid history")
		}
		if seenIdentities[identity] {
			return fmt.Errorf("Mimir admission state repeats a history")
		}
		seenIdentities[identity] = true
	}
	return nil
}

// Each configured services host is sampled independently so one unknown
// sibling cannot hide an affirmative exact counter increase.
func (self *mimirAdmissionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("mimir admission: no services hosts in inventory")
	}
	self.cycleLock.Lock()
	defer self.cycleLock.Unlock()
	stateLocker := self.lockProviderState
	if stateLocker == nil {
		stateLocker = lockProviderState
	}
	stateFileLock, err := stateLocker(ctx, env.cfg.stateDir, "mimir-admission")
	if err != nil {
		return self.stateFailureFindings(env.now().UTC(), len(hosts),
			"mimir-admission/state",
			fmt.Errorf("durable state lock is unavailable"),
		), nil
	}
	defer stateFileLock.Close()
	if err := self.loadState(env.cfg.stateDir); err != nil {
		return self.stateFailureFindings(env.now().UTC(), len(hosts),
			"mimir-admission/state",
			fmt.Errorf("durable state is unreadable"),
		), nil
	}
	priorState := self.snapshotState()

	command := mimirAdmissionCommand(env.cfg.env, env.cfg.logServiceBlocks)
	resultValues := make(chan mimirAdmissionHostResult, len(hosts))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				resultValues <- mimirAdmissionHostResult{host: target, err: ctx.Err()}
				return
			}

			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				resultValues <- mimirAdmissionHostResult{host: target, err: err}
				return
			}
			sample, err := parseMimirAdmissionHostSample(output)
			resultValues <- mimirAdmissionHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(resultValues)

	results := make([]mimirAdmissionHostResult, 0, len(hosts))
	for result := range resultValues {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].host.name < results[j].host.name })

	now := env.now().UTC()
	assessment := mimirAdmissionAssessment{}
	stateSaved := false
	stateSaveFailed := false
	if err := validateMimirAdmissionCurrentIdentities(results); err != nil {
		assessment = mimirAdmissionRetainedAssessment(now, len(results), priorState)
		assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
			target: "mimir-admission/state",
			err:    err,
		})
	} else {
		assessment = self.observe(now, results)
		if err := self.saveState(env.cfg.stateDir); err != nil {
			self.restoreState(priorState)
			stateSaveFailed = true
			if priorState.incident {
				assessment.incidentActive = true
				assessment.quietFor = mimirAdmissionQuietDuration(now, priorState.quietSince)
			}
		} else {
			self.stateLock.Lock()
			self.quietResetDone = true
			self.stateLock.Unlock()
			stateSaved = true
		}
	}
	findings := make([]finding, 0, len(assessment.visibilityFailures)+2)
	seenVisibilityTargets := map[string]bool{}
	for _, failure := range assessment.visibilityFailures {
		if seenVisibilityTargets[failure.target] {
			continue
		}
		seenVisibilityTargets[failure.target] = true
		findings = append(findings, cannotObserveFinding(failure.target, failure.err))
	}
	if stateSaveFailed {
		findings = append(findings, cannotObserveFinding(
			"mimir-admission/state",
			fmt.Errorf("durable state save failed"),
		))
	}
	if assessment.incidentActive {
		findings = append(findings, mimirAdmissionLimitFinding(assessment))
	} else if stateSaved && assessment.directComplete && assessment.comparable {
		findings = append(findings, healthyFinding(
			"observability/mimir-admission", tierPage, "mimir-series-limit", "mimir-fleet",
		))
	}
	return findings, nil
}

// Failure before a new observation can still retain an affirmative incident
// already known by this process. The snapshot is rendered but never mutated;
// a caller with no successfully loaded history emits visibility only.
func (self *mimirAdmissionProbe) stateFailureFindings(
	now time.Time,
	configuredHosts int,
	target string,
	err error,
) []finding {
	findings := []finding{cannotObserveFinding(target, err)}
	state := self.snapshotState()
	if state.incident {
		assessment := mimirAdmissionRetainedAssessment(now, configuredHosts, state)
		findings = append(findings, mimirAdmissionLimitFinding(assessment))
	}
	return findings
}

// The fleet observation itself is bounded before it can replace durable
// history. Duplicate child identities are equally unsafe because one row
// could otherwise overwrite another within the same tick.
func validateMimirAdmissionCurrentIdentities(results []mimirAdmissionHostResult) error {
	seen := map[mimirAdmissionIdentity]bool{}
	instanceCount := 0
	for _, result := range results {
		if result.err != nil {
			continue
		}
		for _, instance := range result.sample.instances {
			instanceCount++
			if instanceCount > mimirAdmissionHistoryLimit {
				return fmt.Errorf("current child identity bound exceeded")
			}
			if !instance.observable {
				continue
			}
			identity := mimirAdmissionIdentity{
				host:         result.host.name,
				port:         instance.port,
				processStart: instance.processStart,
			}
			if strings.TrimSpace(identity.host) == "" || identity.port < 1 || identity.port > 65535 ||
				identity.processStart == "" {
				return fmt.Errorf("current child identity is incomplete")
			}
			if seen[identity] {
				return fmt.Errorf("current child identity is duplicated")
			}
			seen[identity] = true
		}
	}
	return nil
}

func mimirAdmissionRetainedAssessment(
	now time.Time,
	configuredHosts int,
	state mimirAdmissionRuntimeState,
) mimirAdmissionAssessment {
	return mimirAdmissionAssessment{
		configuredHosts: configuredHosts,
		directComplete:  false,
		comparable:      false,
		incidentActive:  state.incident,
		quietFor:        mimirAdmissionQuietDuration(now, state.quietSince),
	}
}

func mimirAdmissionQuietDuration(now time.Time, quietSince time.Time) time.Duration {
	if quietSince.IsZero() || !now.After(quietSince) {
		return 0
	}
	return now.Sub(quietSince)
}

// Counter comparison and quiet-hold mutation occur under one lock. Unknown
// hosts retain their last baseline, while a complete host replaces stale
// process generations with the exact children observed on this tick.
func (self *mimirAdmissionProbe) observe(now time.Time, results []mimirAdmissionHostResult) mimirAdmissionAssessment {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.histories == nil {
		self.histories = map[mimirAdmissionIdentity]mimirAdmissionHistory{}
	}
	assessment := mimirAdmissionAssessment{
		configuredHosts: len(results),
		directComplete:  true,
		comparable:      true,
	}
	nextHistories := map[mimirAdmissionIdentity]mimirAdmissionHistory{}
	positive := false
	metricRangeInitialized := false

	for _, result := range results {
		hostName := result.host.name
		preserveHostHistory := func() {
			for identity, history := range self.histories {
				if _, updated := nextHistories[identity]; identity.host == hostName && !updated {
					nextHistories[identity] = history
				}
			}
		}
		if result.err != nil {
			assessment.directComplete = false
			assessment.comparable = false
			preserveHostHistory()
			assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
				target: hostName + "/mimir-admission",
				err:    fmt.Errorf("host observation failed"),
			})
			continue
		}
		if result.sample.count == 0 {
			assessment.directComplete = false
			assessment.comparable = false
			preserveHostHistory()
			assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
				target: hostName + "/mimir-admission",
				err:    fmt.Errorf("no local Mimir child was identified"),
			})
			continue
		}

		assessment.instanceCount += len(result.sample.instances)
		if result.sample.journalComplete {
			assessment.journalHosts++
			assessment.publisherStarts += result.sample.publisherStarts
			assessment.readinessRejects += result.sample.readinessRejects
			assessment.admissionRejects += result.sample.admissionRejects
		}

		hostComplete := true
		for _, instance := range result.sample.instances {
			if !instance.observable {
				hostComplete = false
				assessment.directComplete = false
				assessment.comparable = false
				assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
					target: hostName + "/mimir-admission",
					err:    fmt.Errorf("a local Mimir child omitted its bounded observation"),
				})
				continue
			}
			if !instance.discardDescriptor && !(instance.discardFamilyAbsent && instance.discardAbsenceSource) {
				hostComplete = false
				assessment.directComplete = false
				assessment.comparable = false
				visibilityErr := fmt.Errorf("discard counter descriptor is unavailable")
				if instance.discardFamilyAbsent && !instance.discardAbsenceSource {
					visibilityErr = fmt.Errorf("discard counter family is absent outside the recognized source contract")
				}
				assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
					target: hostName + "/mimir-admission",
					err:    visibilityErr,
				})
				continue
			}

			if instance.discardDescriptor {
				assessment.descriptorInstances++
			} else {
				assessment.sourceZeroInstances++
			}

			headroom := instance.localLimit - instance.memorySeries
			if !metricRangeInitialized {
				assessment.memoryMinimum = instance.memorySeries
				assessment.memoryMaximum = instance.memorySeries
				assessment.activeMinimum = instance.activeSeries
				assessment.activeMaximum = instance.activeSeries
				assessment.localLimitMinimum = instance.localLimit
				assessment.localLimitMaximum = instance.localLimit
				assessment.globalLimitMinimum = instance.globalLimit
				assessment.globalLimitMaximum = instance.globalLimit
				assessment.headroomMinimum = headroom
				assessment.headroomMaximum = headroom
				metricRangeInitialized = true
			} else {
				assessment.memoryMinimum = min(assessment.memoryMinimum, instance.memorySeries)
				assessment.memoryMaximum = max(assessment.memoryMaximum, instance.memorySeries)
				assessment.activeMinimum = min(assessment.activeMinimum, instance.activeSeries)
				assessment.activeMaximum = max(assessment.activeMaximum, instance.activeSeries)
				assessment.localLimitMinimum = min(assessment.localLimitMinimum, instance.localLimit)
				assessment.localLimitMaximum = max(assessment.localLimitMaximum, instance.localLimit)
				assessment.globalLimitMinimum = min(assessment.globalLimitMinimum, instance.globalLimit)
				assessment.globalLimitMaximum = max(assessment.globalLimitMaximum, instance.globalLimit)
				assessment.headroomMinimum = min(assessment.headroomMinimum, headroom)
				assessment.headroomMaximum = max(assessment.headroomMaximum, headroom)
			}

			identity := mimirAdmissionIdentity{
				host:         hostName,
				port:         instance.port,
				processStart: instance.processStart,
			}
			current := mimirAdmissionHistory{
				discardTotal: instance.discardTotal,
				createdTotal: instance.createdTotal,
				removedTotal: instance.removedTotal,
			}
			previous, observedBefore := self.histories[identity]
			if !observedBefore {
				assessment.comparable = false
				for previousIdentity := range self.histories {
					if previousIdentity.host == hostName {
						assessment.generationChanges++
						break
					}
				}
				if instance.discardTotal > 0 {
					positive = true
					assessment.affectedInstances++
					assessment.initialPositive++
					assessment.discardIncrease += instance.discardTotal
				}
			} else if instance.discardTotal < previous.discardTotal ||
				instance.createdTotal < previous.createdTotal ||
				instance.removedTotal < previous.removedTotal {
				assessment.directComplete = false
				assessment.comparable = false
				assessment.counterResets++
				assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
					target: hostName + "/mimir-admission",
					err:    fmt.Errorf("a monotonic counter decreased within one process generation"),
				})
			} else {
				discardDelta := instance.discardTotal - previous.discardTotal
				if discardDelta > 0 {
					positive = true
					assessment.affectedInstances++
					assessment.discardIncrease += discardDelta
				}
				assessment.createdIncrease += instance.createdTotal - previous.createdTotal
				assessment.removedIncrease += instance.removedTotal - previous.removedTotal
			}
			nextHistories[identity] = current
		}
		if !hostComplete {
			preserveHostHistory()
		} else {
			assessment.observableHosts++
		}
	}

	self.histories = nextHistories
	if positive {
		self.incident = true
		self.quietSince = time.Time{}
	} else if self.incident {
		if assessment.directComplete && assessment.comparable {
			if self.quietSince.IsZero() {
				self.quietSince = now
			}
			if now.Sub(self.quietSince) >= mimirAdmissionQuietWindow {
				self.incident = false
				self.quietSince = time.Time{}
			}
		} else {
			self.quietSince = time.Time{}
		}
	}
	assessment.incidentActive = self.incident
	if self.incident && !self.quietSince.IsZero() && now.After(self.quietSince) {
		assessment.quietFor = now.Sub(self.quietSince)
	}
	return assessment
}

// The fixed finding keeps replicated child increments distinct from unique
// rejected requests and never elevates publisher correlation into causation.
func mimirAdmissionLimitFinding(assessment mimirAdmissionAssessment) finding {
	state := "new direct counter increase"
	if assessment.discardIncrease == 0 {
		state = "two-hour complete quiet hold in progress"
	}
	return finding{
		probeId: "observability/mimir-admission", tier: tierPage,
		class: "mimir-series-limit", target: "mimir-fleet", frame: "per-user-series-limit", sustain: 1,
		symptom:   "Mimir series admission rejected samples or remains inside its required quiet hold",
		mechanism: "An exact Mimir child counter for the per-user series limit increased. The child increment is affirmative ingestion loss, but replicated child counters are not unique rejected requests. A rollout can exhaust headroom through rejected-candidate publisher churn, while steady exporter families or unrelated cardinality growth can reach the same limit.",
		baseline: fmt.Sprintf(
			"Every enabled services host has a complete exact-child observation, no per-user-series discard counter increases, and the fleet remains complete and comparable for %s after the last increase.",
			mimirAdmissionQuietWindow,
		),
		observed: fmt.Sprintf(
			"state=%s configured_hosts=%d observable_hosts=%d mimir_instances=%d descriptor_instances=%d source_zero_instances=%d affected_instances=%d initial_positive_instances=%d discard_counter_increase=%d memory_series=%d..%d active_series=%d..%d local_limit=%d..%d global_limit=%d..%d local_headroom=%d..%d created_increase=%d removed_increase=%d generation_changes=%d counter_resets=%d direct_complete=%t comparable=%t quiet_complete=%s journal_hosts=%d publisher_starts=%d readiness_rejects=%d admission_rejects=%d",
			state,
			assessment.configuredHosts,
			assessment.observableHosts,
			assessment.instanceCount,
			assessment.descriptorInstances,
			assessment.sourceZeroInstances,
			assessment.affectedInstances,
			assessment.initialPositive,
			assessment.discardIncrease,
			assessment.memoryMinimum,
			assessment.memoryMaximum,
			assessment.activeMinimum,
			assessment.activeMaximum,
			assessment.localLimitMinimum,
			assessment.localLimitMaximum,
			assessment.globalLimitMinimum,
			assessment.globalLimitMaximum,
			assessment.headroomMinimum,
			assessment.headroomMaximum,
			assessment.createdIncrease,
			assessment.removedIncrease,
			assessment.generationChanges,
			assessment.counterResets,
			assessment.directComplete,
			assessment.comparable,
			assessment.quietFor.Round(time.Second),
			assessment.journalHosts,
			assessment.publisherStarts,
			assessment.readinessRejects,
			assessment.admissionRejects,
		),
		evidence: "Each host identifies Mimir through its loopback build-info response, reduces the exact process metrics and two allowlisted limit fields locally, and returns only fixed numeric fields. Rendered configuration, metric labels, tenant values, and journal lines never leave the host.",
		context:  "Publisher starts, readiness rejects, and admission-rejection log matches are aggregate same-window context only. Equality can support a rejected-candidate amplification hypothesis after exact artifact and rollout correlation; inequality cannot name a different cause, and none of the journal counts changes this direct admission classification. A process replacement or counter decrease starts a new baseline and cannot clear this incident.",
		action:   "Stop treating retries or a limit increase as recovery. Compare exact running Server and Warp artifacts, migration readiness, publisher starts, and exporter cardinality. If rejected candidates publish, deploy the established post-admission metrics fix through the ordinary authorized rollout. Otherwise reduce the first proven unnecessary metric family or correct the genuine cardinality source. Preserve process instance identity and do not restart Mimir merely to reset its head.",
		verify: fmt.Sprintf(
			"Require complete exact-child observations with no counter reset or generation gap, zero new per-user-series discard increments, two fresh independent application-metric reads, and measured headroom for the next rollout through %s. Historical continuity remains independently governed by §11.20.",
			mimirAdmissionQuietWindow,
		),
		playbook: "SIGNALS.md §11.20a, §11.20, §8.11, and §8.12",
	}
}

// The command embeds only the active block tags needed for aggregate journal
// context. All values are single-quoted before entering the remote shell.
func mimirAdmissionCommand(environment string, blockValues map[string][]string) string {
	identifiers := []string{}
	for _, service := range []string{"api", "connect", "taskworker"} {
		blocks := append([]string(nil), blockValues[service]...)
		sort.Strings(blocks)
		for _, block := range blocks {
			identifiers = append(identifiers, fmt.Sprintf("warp|%s|%s|%s", environment, service, block))
		}
	}
	return "# " + mimirAdmissionMarker + "\n" +
		"journal_identifiers=" + shellSingleQuote(strings.Join(identifiers, " ")) + "\n" +
		mimirAdmissionScript("127.0.0.1")
}

// The injected address keeps child discovery and every loopback request on
// one explicit boundary while allowing documentation-only test fixtures.
func mimirAdmissionScript(loopbackAddress string) string {
	return "loopback_address=" + shellSingleQuote(loopbackAddress) + "\n" +
		"lazy_family_version=" + shellSingleQuote(mimirAdmissionLazyFamilyVersion) + "\n" +
		"lazy_family_source_revision=" + shellSingleQuote(mimirAdmissionLazyFamilySourceRevision) + "\n" +
		mimirAdmissionScriptBody
}

// Strict framing rejects duplicates, omissions, malformed numbers, and any
// trailing field that the reducer contract does not define.
func parseMimirAdmissionHostSample(output string) (mimirAdmissionHostSample, error) {
	sample := mimirAdmissionHostSample{}
	current := -1
	for lineNumber, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		key := fields[0]
		switch key {
		case "instance_begin":
			if len(fields) != 2 || current >= 0 || sample.countSeen {
				return sample, fmt.Errorf("mimir admission line %d: invalid instance_begin", lineNumber+1)
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil || port < 1 || port > 65535 {
				return sample, fmt.Errorf("mimir admission line %d: invalid port", lineNumber+1)
			}
			sample.instances = append(sample.instances, mimirAdmissionInstance{port: port, seen: map[string]bool{}})
			current = len(sample.instances) - 1
		case "observable":
			if len(fields) != 2 || current < 0 || sample.instances[current].observableSeen {
				return sample, fmt.Errorf("mimir admission line %d: invalid observable", lineNumber+1)
			}
			value, err := parseMimirAdmissionBoolean(fields[1])
			if err != nil {
				return sample, fmt.Errorf("mimir admission line %d: invalid observable", lineNumber+1)
			}
			sample.instances[current].observable = value
			sample.instances[current].observableSeen = true
		case "process_start":
			if len(fields) != 2 || current < 0 {
				return sample, fmt.Errorf("mimir admission line %d: invalid process_start", lineNumber+1)
			}
			instance := &sample.instances[current]
			if instance.seen[key] {
				return sample, fmt.Errorf("mimir admission line %d: duplicate process_start", lineNumber+1)
			}
			value, err := canonicalMimirAdmissionProcessStart(fields[1])
			if err != nil {
				return sample, fmt.Errorf("mimir admission line %d: invalid process_start", lineNumber+1)
			}
			instance.seen[key] = true
			instance.processStart = value
		case "memory_series", "active_series", "created_total", "removed_total", "local_limit", "global_limit", "discard_total":
			if len(fields) != 2 || current < 0 {
				return sample, fmt.Errorf("mimir admission line %d: invalid %s", lineNumber+1, key)
			}
			instance := &sample.instances[current]
			if instance.seen[key] {
				return sample, fmt.Errorf("mimir admission line %d: duplicate %s", lineNumber+1, key)
			}
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || value < 0 || ((key == "local_limit" || key == "global_limit") && value == 0) {
				return sample, fmt.Errorf("mimir admission line %d: invalid %s", lineNumber+1, key)
			}
			instance.seen[key] = true
			switch key {
			case "memory_series":
				instance.memorySeries = value
			case "active_series":
				instance.activeSeries = value
			case "created_total":
				instance.createdTotal = value
			case "removed_total":
				instance.removedTotal = value
			case "local_limit":
				instance.localLimit = value
			case "global_limit":
				instance.globalLimit = value
			case "discard_total":
				instance.discardTotal = value
			}
		case "discard_descriptor", "discard_family_absent", "discard_absence_source", "discard_present":
			if len(fields) != 2 || current < 0 {
				return sample, fmt.Errorf("mimir admission line %d: invalid %s", lineNumber+1, key)
			}
			instance := &sample.instances[current]
			if instance.seen[key] {
				return sample, fmt.Errorf("mimir admission line %d: duplicate %s", lineNumber+1, key)
			}
			value, err := parseMimirAdmissionBoolean(fields[1])
			if err != nil {
				return sample, fmt.Errorf("mimir admission line %d: invalid %s", lineNumber+1, key)
			}
			instance.seen[key] = true
			if key == "discard_descriptor" {
				instance.discardDescriptor = value
			} else if key == "discard_family_absent" {
				instance.discardFamilyAbsent = value
			} else if key == "discard_absence_source" {
				instance.discardAbsenceSource = value
			} else {
				instance.discardPresent = value
			}
		case "instance_end":
			if len(fields) != 1 || current < 0 {
				return sample, fmt.Errorf("mimir admission line %d: unexpected instance_end", lineNumber+1)
			}
			instance := sample.instances[current]
			if !instance.observableSeen {
				return sample, fmt.Errorf("mimir admission line %d: instance omitted observable", lineNumber+1)
			}
			if instance.observable {
				for _, required := range []string{
					"process_start", "memory_series", "active_series", "created_total", "removed_total",
					"local_limit", "global_limit", "discard_descriptor", "discard_family_absent",
					"discard_absence_source", "discard_present", "discard_total",
				} {
					if !instance.seen[required] {
						return sample, fmt.Errorf("mimir admission line %d: instance omitted %s", lineNumber+1, required)
					}
				}
				if !instance.discardPresent && instance.discardTotal != 0 {
					return sample, fmt.Errorf("mimir admission line %d: absent discard row has a nonzero total", lineNumber+1)
				}
				if instance.discardDescriptor && instance.discardFamilyAbsent {
					return sample, fmt.Errorf("mimir admission line %d: present descriptor contradicts absent family", lineNumber+1)
				}
				if instance.discardFamilyAbsent && instance.discardPresent {
					return sample, fmt.Errorf("mimir admission line %d: absent family contains an exact counter row", lineNumber+1)
				}
			} else if len(instance.seen) != 0 {
				return sample, fmt.Errorf("mimir admission line %d: unobservable instance contains metric fields", lineNumber+1)
			}
			current = -1
		case "mimir_count":
			if len(fields) != 2 || current >= 0 || sample.countSeen {
				return sample, fmt.Errorf("mimir admission line %d: invalid mimir_count", lineNumber+1)
			}
			value, err := strconv.Atoi(fields[1])
			if err != nil || value < 0 {
				return sample, fmt.Errorf("mimir admission line %d: invalid mimir_count", lineNumber+1)
			}
			sample.count = value
			sample.countSeen = true
		case "journal_complete":
			if len(fields) != 2 || !sample.countSeen || sample.journalSeen {
				return sample, fmt.Errorf("mimir admission line %d: invalid journal_complete", lineNumber+1)
			}
			value, err := parseMimirAdmissionBoolean(fields[1])
			if err != nil {
				return sample, fmt.Errorf("mimir admission line %d: invalid journal_complete", lineNumber+1)
			}
			sample.journalComplete = value
			sample.journalSeen = true
		case "publisher_starts", "readiness_rejects", "admission_rejects":
			if len(fields) != 2 || !sample.journalSeen {
				return sample, fmt.Errorf("mimir admission line %d: invalid %s", lineNumber+1, key)
			}
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || value < 0 {
				return sample, fmt.Errorf("mimir admission line %d: invalid %s", lineNumber+1, key)
			}
			if key == "publisher_starts" {
				if sample.publisherSeen {
					return sample, fmt.Errorf("mimir admission line %d: duplicate publisher_starts", lineNumber+1)
				}
				sample.publisherStarts = value
				sample.publisherSeen = true
			} else if key == "readiness_rejects" {
				if sample.readinessSeen {
					return sample, fmt.Errorf("mimir admission line %d: duplicate readiness_rejects", lineNumber+1)
				}
				sample.readinessRejects = value
				sample.readinessSeen = true
			} else {
				if sample.admissionSeen {
					return sample, fmt.Errorf("mimir admission line %d: duplicate admission_rejects", lineNumber+1)
				}
				sample.admissionRejects = value
				sample.admissionSeen = true
			}
		default:
			return sample, fmt.Errorf("mimir admission line %d: unknown field %q", lineNumber+1, key)
		}
	}
	if current >= 0 {
		return sample, fmt.Errorf("mimir admission: unterminated instance")
	}
	if !sample.countSeen || sample.count != len(sample.instances) {
		return sample, fmt.Errorf("mimir admission: invalid instance count")
	}
	if !sample.journalSeen || !sample.publisherSeen || !sample.readinessSeen || !sample.admissionSeen {
		return sample, fmt.Errorf("mimir admission: missing journal context field")
	}
	if !sample.journalComplete && (sample.publisherStarts != 0 || sample.readinessRejects != 0 || sample.admissionRejects != 0) {
		return sample, fmt.Errorf("mimir admission: incomplete journal context contains counts")
	}
	return sample, nil
}

// Reducer booleans are deliberately numeric so arbitrary shell text cannot be
// interpreted as a true value.
func parseMimirAdmissionBoolean(value string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("invalid Boolean")
	}
}

const mimirAdmissionScriptBody = `set -u
for required in ss curl awk sort; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'mimir admission probe prerequisite missing: %s\n' "$required" >&2
    exit 1
  fi
done
mimir_count=0
ports=$(ss -ltnH 2>/dev/null | awk -v address="$loopback_address" '
  index($4, address ":") == 1 {
    port=$4
    sub(/.*:/, "", port)
    if (port ~ /^[0-9]+$/) print port
  }
' | sort -n -u)
for port in $ports; do
  build_info=$(curl -fsS --max-time 2 "http://${loopback_address}:${port}/api/v1/status/buildinfo" 2>/dev/null || true)
  case "$build_info" in
    *'"application":"Grafana Mimir"'*) ;;
    *) continue ;;
  esac

  discard_absence_source=$(printf '%s\n' "$build_info" | awk \
    -v version="$lazy_family_version" \
    -v revision="$lazy_family_source_revision" '
    function occurrences(text, needle, count, position) {
      count=0
      while ((position=index(text, needle)) > 0) {
        count++
        text=substr(text, position+length(needle))
      }
      return count
    }
    BEGIN {
      version_key="\"version\":\"" version "\""
      revision_key="\"revision\":\"" revision "\""
    }
    {
      version_count+=occurrences($0, version_key)
      revision_count+=occurrences($0, revision_key)
    }
    END {print (version_count == 1 && revision_count == 1) ? 1 : 0}
  ')

  mimir_count=$((mimir_count+1))
  printf 'instance_begin %s\n' "$port"
  global_limit=$(curl -fsS --max-time 10 "http://${loopback_address}:${port}/config" 2>/dev/null | awk '
    function yaml_key(value) {sub(/:$/, "", value); return value}
    function clear_path() {
      for (level in path_key) delete path_key[level]
      for (level in path_indent) delete path_indent[level]
      path_depth = 0
    }
    function leave_to_parent(indent) {
      while (path_depth > 0 && indent <= path_indent[path_depth]) {
        delete path_key[path_depth]
        delete path_indent[path_depth]
        path_depth--
      }
    }
    {
      if ($0 ~ /^[ ]*$/ || $0 ~ /^[ ]*#/ || index($0, "\t")) next
      match($0, /[^ ]/)
      if (RSTART == 0) next
      indent = RSTART - 1
      leave_to_parent(indent)
      if ($1 !~ /:$/) next
      key = yaml_key($1)
      if (path_depth == 1 && path_key[1] == "limits" && key == "max_global_series_per_user") {
        count++
        value = $2
      }
      path_depth++
      path_indent[path_depth] = indent
      path_key[path_depth] = key
    }
    END {
      if (count == 1 && value ~ /^[0-9]+$/ && value > 0) print value
      else exit 41
    }
  ')
  config_status=$?
  metrics=$(curl -fsS --max-time 10 "http://${loopback_address}:${port}/metrics" 2>/dev/null)
  metrics_status=$?
  if [ "$config_status" -ne 0 ] || [ "$metrics_status" -ne 0 ]; then
    printf 'observable 0\ninstance_end\n'
    continue
  fi
  reduced=$(printf '%s\n' "$metrics" | awk '
    function numeric(value) {return value ~ /^[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$/}
    /^# HELP cortex_discarded_samples_total / {descriptor_help++}
    /^# TYPE cortex_discarded_samples_total counter$/ {descriptor_type++}
	/^cortex_discarded_samples_total([{ \t])/ {discard_family_rows++}
    /^process_start_time_seconds[ \t]/ && numeric($NF) {
      process_count++
      process_start_value=$NF
      process_start_text=$NF
    }
    /^cortex_ingester_memory_series[ \t]/ && numeric($NF) {memory_count++; memory_series=$NF}
    /^cortex_ingester_active_series[{]/ && numeric($NF) {active_count++; active_series+=$NF}
    /^cortex_ingester_memory_series_created_total[{]/ && numeric($NF) {created_count++; created_total+=$NF}
    /^cortex_ingester_memory_series_removed_total[{]/ && numeric($NF) {removed_count++; removed_total+=$NF}
    /^cortex_ingester_local_limits[{]/ && /limit="max_global_series_per_user"/ && numeric($NF) {
      local_count++
      if (local_count == 1 || $NF < local_minimum) local_minimum=$NF
      if (local_count == 1 || $NF > local_maximum) local_maximum=$NF
    }
    /^cortex_discarded_samples_total[{]/ && index($0, "reason=\"per_user_series_limit\"") > 0 {
      discard_seen++
      if ($0 !~ /(^|[{,])reason="per_user_series_limit"([,}])/ || !numeric($NF)) {
        discard_invalid++
        next
      }
      discard_count++
      discard_total+=$NF
    }
    END {
      if (process_count != 1 || process_start_value <= 0 || memory_count != 1 ||
          active_count < 1 || created_count < 1 || removed_count < 1 ||
          local_count < 1 || local_minimum <= 0 || local_minimum != local_maximum ||
          discard_invalid > 0 || discard_seen != discard_count) exit 42
      descriptor=(descriptor_help == 1 && descriptor_type == 1)
	  family_absent=(descriptor_help == 0 && descriptor_type == 0 && discard_family_rows == 0)
      printf "process_start %s\n", process_start_text
      printf "memory_series %.0f\n", memory_series
      printf "active_series %.0f\n", active_series
      printf "created_total %.0f\n", created_total
      printf "removed_total %.0f\n", removed_total
      printf "local_limit %.0f\n", local_minimum
      printf "discard_descriptor %d\n", descriptor
	  printf "discard_family_absent %d\n", family_absent
      printf "discard_present %d\n", (discard_count > 0)
      printf "discard_total %.0f\n", discard_total
    }
  ')
  reduced_status=$?
  if [ "$reduced_status" -ne 0 ]; then
    printf 'observable 0\ninstance_end\n'
    continue
  fi
  printf 'observable 1\n%s\nglobal_limit %s\ndiscard_absence_source %s\ninstance_end\n' \
    "$reduced" "$global_limit" "$discard_absence_source"
done
printf 'mimir_count %s\n' "$mimir_count"

journal_complete=0
publisher_starts=0
readiness_rejects=0
admission_rejects=0
if [ -n "$journal_identifiers" ] && command -v journalctl >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
  set -- journalctl --no-pager --quiet -o cat --since '2 minutes ago' -n 2000 --grep='\[stats\]publishing|\[(api|connect|taskworker)\]not ready|Stats push rejected \(400\):.*per-user series limit'
  for identifier in $journal_identifiers; do
    set -- "$@" "SYSLOG_IDENTIFIER=$identifier"
  done
  journal_output=$(timeout 10s "$@" 2>/dev/null)
  journal_status=$?
  case "$journal_status" in
    0|1)
      journal_complete=1
      publisher_starts=$(printf '%s\n' "$journal_output" | awk '/\[stats\]publishing/ {count++} END {print count+0}')
      readiness_rejects=$(printf '%s\n' "$journal_output" | awk '/\[(api|connect|taskworker)\]not ready/ {count++} END {print count+0}')
      admission_rejects=$(printf '%s\n' "$journal_output" | awk 'index($0, "Stats push rejected (400):") && index($0, "per-user series limit") {count++} END {print count+0}')
      ;;
  esac
fi
printf 'journal_complete %s\npublisher_starts %s\nreadiness_rejects %s\nadmission_rejects %s\n' \
  "$journal_complete" "$publisher_starts" "$readiness_rejects" "$admission_rejects"
`
