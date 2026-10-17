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
}

// Finite source facts only. Joined covers the local SSH process and its pipes,
// not remote descendants. Remote progress is neither SQL authority nor health.
type pgSampleTransportReceipt struct {
	Phase                           string `json:"phase"`
	Spawn                           string `json:"spawn"`
	RemotePhase                     string `json:"remote_phase"`
	RemainingBudgetMs               int64  `json:"remaining_budget_ms"`
	RunSlotWaitMs                   *int64 `json:"run_slot_wait_ms,omitempty"`
	RunSlotGrantedRemainingBudgetMs *int64 `json:"run_slot_granted_remaining_budget_ms,omitempty"`
	SpawnRemainingBudgetMs          *int64 `json:"spawn_remaining_budget_ms"`
	OwnerDeadlineExceeded           bool   `json:"owner_deadline_exceeded"`
	Joined                          bool   `json:"joined"`
	NativeExitCode                  *int   `json:"native_exit_code"`
	ProgressStatus                  string `json:"progress_status"`
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
		self.runSlotRequestedAt = time.Now()
	}
}

// The dispatcher owns the grant timestamp. Its ready channel publishes both
// values before the admitted caller can create the transport observation.
func (self *pgSampleTransportObservation) runSlotGranted(ctx context.Context) {
	if self == nil || self.runSlotRequestedAt.IsZero() {
		return
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return
	}
	grantedAt := time.Now()
	waitMs := grantedAt.Sub(self.runSlotRequestedAt).Milliseconds()
	remainingMs := min(pgQuerySampleBudget.Milliseconds(), max(int64(0), deadline.Sub(grantedAt).Milliseconds()))
	self.receipt.RunSlotWaitMs = &waitMs
	self.receipt.RunSlotGrantedRemainingBudgetMs = &remainingMs
}

// Preserve the inherited owner; observation never creates a fresh deadline.
// The nonce is private stdin correlation, absent from argv and receipts.
func observePgSampleTransport(ctx context.Context) (context.Context, *pgSampleTransportObservation) {
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
		nonce: hex.EncodeToString(nonceBytes[:]),
	}
	if prior := pgSampleTransportFromContext(ctx); prior != nil {
		observation.receipt.RunSlotWaitMs = prior.receipt.RunSlotWaitMs
		observation.receipt.RunSlotGrantedRemainingBudgetMs = prior.receipt.RunSlotGrantedRemainingBudgetMs
	}
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
	self.receipt.RemotePhase, self.receipt.ProgressStatus = parsePgSampleTransportProgress(stderr, self.nonce)
}

// Only complete, ordered records for this private stdin nonce count. Valid
// foreign records do not consume its lane. Malformed/capped input keeps remote
// progress unknown; none of the raw bytes or correlation nonce is retained.
func parsePgSampleTransportProgress(stderr, nonce string) (string, string) {
	if len(stderr) > 4*1024 {
		return "unknown", "capped"
	}
	phase, status := "unknown", "absent"
	for _, line := range strings.SplitAfter(stderr, "\n") {
		const prefix = "monitor-pg-phase "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		raw := strings.TrimPrefix(line, prefix)
		if !pgSampleUniqueObject(raw) {
			return "unknown", "invalid"
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &fields) != nil {
			return "unknown", "invalid"
		}
		var routedNonce string
		if json.Unmarshal(fields["nonce"], &routedNonce) != nil || routedNonce != nonce {
			continue
		}
		var schema int
		var next string
		if !strings.HasSuffix(line, "\n") || len(fields) != 3 || json.Unmarshal(fields["schema"], &schema) != nil || schema != 1 || json.Unmarshal(fields["phase"], &next) != nil {
			return "unknown", "invalid"
		}
		if !((phase == "unknown" && next == "remote_started") || (phase == "remote_started" && next == "database_identity")) {
			return "unknown", "invalid"
		}
		phase, status = next, "valid"
	}
	return phase, status
}
