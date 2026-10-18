// Private PG transport facts retain the last proved execution boundary without
// changing the queue-inclusive owner, source error, SQL workload or cadence.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// One PG attempt owns this optional projection. The run-slot dispatcher writes
// before publishing its ready channel; the admitted caller then owns transport
// facts synchronously. Only native Start/Wait supplies process authority.
type pgSampleTransportObservation struct {
	receipt            pgSampleTransportReceipt
	nonce              string
	runSlotRequestedAt time.Time
	startClock         func() time.Time
	localClock         func() time.Time
	localOrigin        time.Time
	localLastNs        int64
}

// Finite source facts only. Joined covers the local SSH process and its pipes,
// not remote descendants. Completed response headers are neither valid samples
// nor SQL authority. Missing/legacy frame counts remain unknown, never zero.
type pgSampleTransportReceipt struct {
	Phase                           string                  `json:"phase"`
	Spawn                           string                  `json:"spawn"`
	RemotePhase                     string                  `json:"remote_phase"`
	RemoteSampleFramesObserved      *int                    `json:"remote_sample_frames_observed"`
	RemainingBudgetMs               int64                   `json:"remaining_budget_ms"`
	RunSlotWaitMs                   *int64                  `json:"run_slot_wait_ms,omitempty"`
	RunSlotGrantedRemainingBudgetMs *int64                  `json:"run_slot_granted_remaining_budget_ms,omitempty"`
	RunSlotHolders                  *pgSampleRunSlotHolders `json:"run_slot_holders,omitempty"`
	SpawnRemainingBudgetMs          *int64                  `json:"spawn_remaining_budget_ms"`
	OwnerDeadlineExceeded           bool                    `json:"owner_deadline_exceeded"`
	Joined                          bool                    `json:"joined"`
	NativeExitCode                  *int                    `json:"native_exit_code"`
	ProgressStatus                  string                  `json:"progress_status"`
	// Only a local refusal populates this field; it is never a native Start fact.
	StartRefusalRemainingBudgetNs *int64               `json:"start_refusal_remaining_budget_ns,omitempty"`
	LocalPhaseTiming              *pgSampleLocalTiming `json:"local_phase_timing,omitempty"`
}

// Offsets describe local elapsed time, including descheduling. Return marks
// do not assert success; only the existing lifecycle supplies native authority.
type pgSampleLocalSpan struct {
	EnteredNs  *int64 `json:"entered_ns"`
	ReturnedNs *int64 `json:"returned_ns"`
}

// Every unsupported or unreached field stays null; no dynamic labels persist.
type pgSampleLocalTiming struct {
	Schema                    int                `json:"schema"`
	Origin                    string             `json:"origin"`
	ClockStatus               string             `json:"clock_status"`
	GlobalGrantedNs           *int64             `json:"global_granted_ns"`
	SamplerEntryNs            *int64             `json:"sampler_entry_ns"`
	SettingsGenerationCheck   *pgSampleLocalSpan `json:"settings_generation_check"`
	CadenceLock               *pgSampleLocalSpan `json:"cadence_lock"`
	CadenceAdmission          *pgSampleLocalSpan `json:"cadence_admission"`
	CadencePublication        *pgSampleLocalSpan `json:"cadence_publication"`
	OneShotMarker             *pgSampleLocalSpan `json:"one_shot_marker"`
	SampleCommandConstruction *pgSampleLocalSpan `json:"sample_command_construction"`
	TransportEntryNs          *int64             `json:"transport_entry_ns"`
	HostAdmission             *pgSampleLocalSpan `json:"host_admission"`
	HostGrantedNs             *int64             `json:"host_granted_ns"`
	SharedAdmission           *pgSampleLocalSpan `json:"shared_admission"`
	SharedGrantedNs           *int64             `json:"shared_granted_ns"`
	CommandReadyNs            *int64             `json:"command_ready_ns"`
	NativeSetup               *pgSampleLocalSpan `json:"native_setup"`
	FinalGuard                *pgSampleLocalSpan `json:"final_guard"`
}

// This enum accepts no caller-provided labels or values.
type pgSampleLocalPhase uint8

const (
	pgLocalSettingsGeneration pgSampleLocalPhase = iota
	pgLocalCadenceLock
	pgLocalCadenceAdmission
	pgLocalCadencePublication
	pgLocalOneShotMarker
	pgLocalSampleCommand
	pgLocalHostAdmission
	pgLocalSharedAdmission
	pgLocalNativeSetup
	pgLocalFinalGuard
	pgLocalGlobalGranted
	pgLocalSamplerEntry
	pgLocalTransportEntry
	pgLocalHostGranted
	pgLocalSharedGranted
	pgLocalCommandReady
)

// Use one attempt-private test clock; production keeps time.Now monotonic time.
func (self *pgSampleTransportObservation) localNow() time.Time {
	if self.localClock != nil {
		return self.localClock()
	}
	return time.Now()
}

// Keep prior valid facts on regression, but withhold every subsequent offset.
func (self *pgSampleTransportObservation) localOffset(now time.Time) *int64 {
	timing := self.receipt.LocalPhaseTiming
	if timing == nil || timing.ClockStatus != "ok" {
		return nil
	}
	elapsed := now.Sub(self.localOrigin).Nanoseconds()
	if elapsed < 0 || elapsed < self.localLastNs {
		timing.ClockStatus = "invalid"
		return nil
	}
	self.localLastNs = elapsed
	return &elapsed
}

// Select only a fixed span; ordinary non-PG callers have no recorder.
func (self *pgSampleTransportObservation) localSpan(phase pgSampleLocalPhase) **pgSampleLocalSpan {
	if self == nil || self.receipt.LocalPhaseTiming == nil {
		return nil
	}
	timing := self.receipt.LocalPhaseTiming
	switch phase {
	case pgLocalSettingsGeneration:
		return &timing.SettingsGenerationCheck
	case pgLocalCadenceLock:
		return &timing.CadenceLock
	case pgLocalCadenceAdmission:
		return &timing.CadenceAdmission
	case pgLocalCadencePublication:
		return &timing.CadencePublication
	case pgLocalOneShotMarker:
		return &timing.OneShotMarker
	case pgLocalSampleCommand:
		return &timing.SampleCommandConstruction
	case pgLocalHostAdmission:
		return &timing.HostAdmission
	case pgLocalSharedAdmission:
		return &timing.SharedAdmission
	case pgLocalNativeSetup:
		return &timing.NativeSetup
	case pgLocalFinalGuard:
		return &timing.FinalGuard
	}
	return nil
}

// Entry alone supplies no return or success authority.
func (self *pgSampleTransportObservation) beginLocalPhase(phase pgSampleLocalPhase) {
	if span := self.localSpan(phase); span != nil {
		*span = &pgSampleLocalSpan{EnteredNs: self.localOffset(self.localNow())}
	}
}

// A return mark also covers errors and never asserts a successful operation.
func (self *pgSampleTransportObservation) endLocalPhase(phase pgSampleLocalPhase) {
	if span := self.localSpan(phase); span != nil && *span != nil {
		(*span).ReturnedNs = self.localOffset(self.localNow())
	}
}

// Select only fixed points at the owning call site.
func (self *pgSampleTransportObservation) localPointAt(phase pgSampleLocalPhase, now time.Time) {
	if self == nil || self.receipt.LocalPhaseTiming == nil {
		return
	}
	timing := self.receipt.LocalPhaseTiming
	var point **int64
	switch phase {
	case pgLocalGlobalGranted:
		point = &timing.GlobalGrantedNs
	case pgLocalSamplerEntry:
		point = &timing.SamplerEntryNs
	case pgLocalTransportEntry:
		point = &timing.TransportEntryNs
	case pgLocalHostGranted:
		point = &timing.HostGrantedNs
	case pgLocalSharedGranted:
		point = &timing.SharedGrantedNs
	case pgLocalCommandReady:
		point = &timing.CommandReadyNs
	}
	if point != nil {
		*point = self.localOffset(now)
	}
}

// Observe a local boundary without affecting its context or result.
func (self *pgSampleTransportObservation) localPoint(phase pgSampleLocalPhase) {
	if self != nil {
		self.localPointAt(phase, self.localNow())
	}
}

// The private key never crosses the single admitted PG-attempt context.
type pgSampleTransportContextKey struct{}

// Only the scheduled PG request receives this empty recorder. Every cadence
// gets a fresh context; direct probes and ordinary signals have no grant proof.
func observePgSampleRunSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, pgSampleTransportContextKey{}, &pgSampleTransportObservation{})
}

// Measure the actual request, including pool-lock wait, without changing its
// deadline. Other signals have no recorder and retain their existing path.
func (self *pgSampleTransportObservation) runSlotRequested() {
	if self != nil {
		self.runSlotRequestedAt = self.localNow()
		self.localOrigin = self.runSlotRequestedAt
		self.localLastNs = 0
		self.receipt.RunSlotHolders = nil
		self.receipt.LocalPhaseTiming = &pgSampleLocalTiming{Schema: 1, Origin: "run_slot_request", ClockStatus: "ok"}
	}
}

// The dispatcher owns the grant timestamp. Its ready channel publishes both
// values before the admitted caller can create the transport observation.
func (self *pgSampleTransportObservation) runSlotGranted(ctx context.Context) {
	if self == nil || self.runSlotRequestedAt.IsZero() {
		return
	}
	grantedAt := self.localNow()
	self.localPointAt(pgLocalGlobalGranted, grantedAt)
	deadline, ok := ctx.Deadline()
	if !ok {
		return
	}
	waitMs := grantedAt.Sub(self.runSlotRequestedAt).Milliseconds()
	remainingMs := min(pgQuerySampleBudget.Milliseconds(), max(int64(0), deadline.Sub(grantedAt).Milliseconds()))
	self.receipt.RunSlotWaitMs = &waitMs
	self.receipt.RunSlotGrantedRemainingBudgetMs = &remainingMs
}

// Preserve the inherited owner; observation never creates a fresh deadline.
// The nonce is private stdin correlation, absent from argv and receipts.
func observePgSampleTransport(ctx context.Context) (context.Context, *pgSampleTransportObservation) {
	prior := pgSampleTransportFromContext(ctx)
	entry := time.Now()
	var clock func() time.Time
	if prior != nil && prior.localClock != nil {
		clock = prior.localClock
		entry = clock()
	}
	var nonceBytes [16]byte
	rand.Read(nonceBytes[:])
	observation := &pgSampleTransportObservation{
		receipt: pgSampleTransportReceipt{
			Phase:             "unobserved",
			Spawn:             "unknown",
			RemotePhase:       "unknown",
			RemainingBudgetMs: pgSampleRemainingBudgetMs(ctx),
			ProgressStatus:    "unobserved",
		},
		nonce:       hex.EncodeToString(nonceBytes[:]),
		localClock:  clock,
		localOrigin: entry,
	}
	observation.receipt.LocalPhaseTiming = &pgSampleLocalTiming{Schema: 1, Origin: "sampler_observer_entry", ClockStatus: "ok"}
	if prior != nil {
		observation.receipt.RunSlotWaitMs = prior.receipt.RunSlotWaitMs
		observation.receipt.RunSlotGrantedRemainingBudgetMs = prior.receipt.RunSlotGrantedRemainingBudgetMs
		observation.receipt.RunSlotHolders = prior.receipt.RunSlotHolders
		if !prior.runSlotRequestedAt.IsZero() {
			observation.localOrigin = prior.runSlotRequestedAt
			observation.receipt.LocalPhaseTiming.Origin = "run_slot_request"
			if timing := prior.receipt.LocalPhaseTiming; timing != nil {
				observation.receipt.LocalPhaseTiming.GlobalGrantedNs = timing.GlobalGrantedNs
				observation.receipt.LocalPhaseTiming.ClockStatus = timing.ClockStatus
				observation.localLastNs = prior.localLastNs
			}
		}
	}
	observation.localPointAt(pgLocalSamplerEntry, entry)
	return context.WithValue(ctx, pgSampleTransportContextKey{}, observation), observation
}

// Other SSH callers have no observer and retain their existing behavior.
func pgSampleTransportFromContext(ctx context.Context) *pgSampleTransportObservation {
	observation, _ := ctx.Value(pgSampleTransportContextKey{}).(*pgSampleTransportObservation)
	return observation
}

// This is remaining owner time, not a measurement of any particular queue.
func pgSampleRemainingBudgetMs(ctx context.Context) int64 {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return min(pgQuerySampleBudget.Milliseconds(), max(int64(0), time.Until(deadline).Milliseconds()))
}

// Nil is the ordinary non-PG path. Every stage is entered at its actual owner.
func (self *pgSampleTransportObservation) stage(phase, spawn string) {
	if self != nil {
		self.receipt.Phase, self.receipt.Spawn = phase, spawn
	}
}

// Called only after exec.Cmd.Start succeeds, never for a callback invocation.
func (self *pgSampleTransportObservation) started(ctx context.Context) {
	if self != nil {
		self.stage("ssh_running", "started")
		remaining := pgSampleRemainingBudgetMs(ctx)
		self.receipt.SpawnRemainingBudgetMs = &remaining
	}
}

// Called only after the owned native Wait returns. A killed process may have
// no numeric exit code; neither that absence nor exit 255 locates the cause.
func (self *pgSampleTransportObservation) joined(state *os.ProcessState, stderr string) {
	if self == nil {
		return
	}
	self.receipt.Joined = true
	if state != nil {
		if exitCode := state.ExitCode(); 0 <= exitCode && exitCode <= 255 {
			self.receipt.NativeExitCode = &exitCode
		}
	}
	self.receipt.RemotePhase, self.receipt.ProgressStatus, self.receipt.RemoteSampleFramesObserved = parsePgSampleTransportProgress(stderr, self.nonce)
}

// Only complete, ordered records for this private stdin nonce count. Valid
// foreign records do not consume its lane. Malformed/capped input keeps remote
// progress unknown; none of the raw bytes or correlation nonce is retained.
// Version2 has at most 17 records; its last result never names a failed operation.
func parsePgSampleTransportProgress(stderr, nonce string) (string, string, *int) {
	if len(stderr) > 4*1024 {
		return "unknown", "capped", nil
	}
	phase, status := "unknown", "absent"
	version, records := 0, 0
	var observed *int
	for _, line := range strings.SplitAfter(stderr, "\n") {
		const prefix = "monitor-pg-phase "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		raw := strings.TrimPrefix(line, prefix)
		if !pgSampleUniqueObject(raw) {
			return "unknown", "invalid", nil
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &fields) != nil {
			return "unknown", "invalid", nil
		}
		var routedNonce string
		if json.Unmarshal(fields["nonce"], &routedNonce) != nil || routedNonce != nonce {
			continue
		}
		var schema int
		var next string
		if !strings.HasSuffix(line, "\n") || json.Unmarshal(fields["schema"], &schema) != nil || (schema != 1 && schema != 2) || (version != 0 && version != schema) || json.Unmarshal(fields["phase"], &next) != nil {
			return "unknown", "invalid", nil
		}
		if schema == 1 {
			// The old two-record protocol has no observed-frame count. Never
			// upgrade that absence to zero or combine versions in one attempt.
			if len(fields) != 3 || !((phase == "unknown" && next == "remote_started") || (phase == "remote_started" && next == "database_identity")) {
				return "unknown", "invalid", nil
			}
		} else {
			var samples *int
			if len(fields) != 4 || json.Unmarshal(fields["samples"], &samples) != nil || samples == nil {
				return "unknown", "invalid", nil
			}
			wantPhase, wantSamples := "", 0
			switch {
			case records == 0:
				wantPhase = "remote_started"
			case records == 1:
				wantPhase = "database_identity"
			case records == 2:
				wantPhase = "history_start"
			case 3 <= records && records <= 14:
				wantPhase, wantSamples = "activity", records-2
			case records == 15:
				wantPhase, wantSamples = "blockers", 12
			case records == 16:
				wantPhase, wantSamples = "history_end", 12
			default:
				return "unknown", "invalid", nil
			}
			if next != wantPhase || *samples != wantSamples {
				return "unknown", "invalid", nil
			}
			observed = samples
		}
		version = schema
		records++
		phase, status = next, "valid"
	}
	return phase, status, observed
}
