// Optional invocation witnesses describe scheduler call boundaries, not probe health.
package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const runInvocationDiagnostic = "monitor: invocation witness unusable; missing coverage is unknown"

type runInvocationLimits struct {
	recordBytes int
	totalBytes  int64
	records     uint64
}

func defaultRunInvocationLimits() runInvocationLimits {
	return runInvocationLimits{4096, 512 * 1024 * 1024, 1000000}
}

type runInvocationSignal struct {
	Key                      string `json:"key"`
	Number                   string `json:"number"`
	NativeCadenceNanoseconds int64  `json:"startup_native_cadence_nanoseconds"`
}

type runInvocationRecord struct {
	Schema                          int                     `json:"schema"`
	RunId                           string                  `json:"watcher_run_id"`
	Identity                        settingsWitnessIdentity `json:"identity"`
	SelectedKeysSha256              string                  `json:"selected_keys_sha256"`
	Sequence                        uint64                  `json:"sequence"`
	Invocation                      uint64                  `json:"invocation"`
	Key                             string                  `json:"key"`
	Number                          string                  `json:"number"`
	Kind                            string                  `json:"kind"`
	Event                           string                  `json:"event"`
	ObservedAt                      time.Time               `json:"observed_at_utc"`
	DispatchElapsedNanoseconds      int64                   `json:"dispatch_elapsed_nanoseconds"`
	StartupNativeCadenceNanoseconds int64                   `json:"startup_native_cadence_nanoseconds"`
	ConfiguredDelayNanoseconds      int64                   `json:"configured_delay_nanoseconds"`
	Result                          string                  `json:"result,omitempty"`
}

type runInvocationToken struct {
	invocation uint64
	started    time.Time
	signal     runInvocationSignal
	kind       string
	delay      time.Duration
}

// RunInvocationWitness owns one optional private bounded stream. Runtime sink
// failure is sticky and observational only: it never changes a signal outcome.
// A regular-file write has no promised wall-clock bound or fsync durability.
type RunInvocationWitness struct {
	stateLock          sync.Mutex
	output             io.Writer
	diagnostics        io.Writer
	identity           settingsWitnessIdentity
	runId              string
	selectedKeysSha256 string
	signals            map[string]runInvocationSignal
	limits             runInvocationLimits
	sequence           uint64
	invocation         uint64
	bytes              int64
	failed             bool
	closed             bool
}

func runInvocationLabel(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '/') {
			return false
		}
	}
	return true
}

func newRunInvocationWitness(output, diagnostics io.Writer, identity settingsWitnessIdentity, signals []Signal, limits runInvocationLimits) (*RunInvocationWitness, error) {
	maximum := defaultRunInvocationLimits()
	if output == nil || diagnostics == nil || !identity.valid() || len(signals) == 0 || limits.recordBytes <= 0 || limits.recordBytes > maximum.recordBytes || limits.totalBytes <= 0 || limits.totalBytes > maximum.totalBytes || limits.records == 0 || limits.records > maximum.records {
		return nil, errors.New("invocation witness startup unavailable")
	}
	selected := make([]runInvocationSignal, 0, len(signals))
	byKey := map[string]runInvocationSignal{}
	for _, signal := range signals {
		if signal == nil {
			return nil, errors.New("invocation witness signal unavailable")
		}
		entry := runInvocationSignal{signal.Key(), signal.Number(), int64(signal.Cadence())}
		if !runInvocationLabel(entry.Key) || !runInvocationLabel(entry.Number) || entry.NativeCadenceNanoseconds <= 0 {
			return nil, errors.New("invocation witness signal unavailable")
		}
		if _, duplicate := byKey[entry.Key]; duplicate {
			return nil, errors.New("invocation witness signal ambiguous")
		}
		byKey[entry.Key] = entry
		selected = append(selected, entry)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Key < selected[j].Key })
	encoded, err := json.Marshal(selected)
	if err != nil {
		return nil, errors.New("invocation witness selection unavailable")
	}
	digest := sha256.Sum256(encoded)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errors.New("invocation witness startup unavailable")
	}
	return &RunInvocationWitness{output: output, diagnostics: diagnostics, identity: identity, runId: hex.EncodeToString(nonce[:]), selectedKeysSha256: hex.EncodeToString(digest[:]), signals: byKey, limits: limits}, nil
}

// NewRunInvocationWitness validates actual startup identity before any candidate
// watcher starts. Its selected keys bind source enrollment, not enabled/due or
// target completeness. The owner must close it after RunExecutionLoop joins.
func NewRunInvocationWitness(ctx context.Context, path string, settings SignalSettings, signals []Signal, diagnostics io.Writer) (*RunInvocationWitness, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if path == "" || err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invocation witness requires a private directory")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("invocation witness requires a new output")
	}
	options := DefaultSettingsGenerationWitnessSettings()
	identityCtx, cancel := context.WithTimeout(ctx, options.IdentityTimeout)
	defer cancel()
	var identity settingsWitnessIdentity
	if existing := settings.settingsGenerationWitness; existing != nil {
		existing.stateLock.Lock()
		identity = existing.identity
		unavailable := existing.failed || existing.closed
		existing.stateLock.Unlock()
		if unavailable {
			return nil, errors.New("invocation witness startup identity unavailable")
		}
	} else {
		identity, err = settingsWitnessSelfIdentity(identityCtx, options)
	}
	if err != nil || !identity.valid() || identityCtx.Err() != nil {
		return nil, errors.New("invocation witness self identity unavailable")
	}
	// Validate the signal association before creating the owned file.
	witness, err := newRunInvocationWitness(io.Discard, diagnostics, identity, signals, defaultRunInvocationLimits())
	if err != nil {
		return nil, err
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)
	if err != nil {
		return nil, errors.New("invocation witness output unavailable")
	}
	witness.output = output
	return witness, nil
}

// Called only under the witness lock, never a scheduler/gate lock. Diagnostic
// errors are intentionally ignored; no raw sink error can reach product logs.
func (self *RunInvocationWitness) fail() {
	if !self.failed {
		self.failed = true
		_, _ = fmt.Fprintln(self.diagnostics, runInvocationDiagnostic)
	}
}

func (self *RunInvocationWitness) write(token runInvocationToken, event, result string) bool {
	if self.failed || self.closed {
		return false
	}
	if self.sequence == math.MaxUint64 || self.sequence >= self.limits.records {
		self.fail()
		return false
	}
	now := time.Now()
	record := runInvocationRecord{Schema: 1, RunId: self.runId, Identity: self.identity, SelectedKeysSha256: self.selectedKeysSha256, Sequence: self.sequence + 1, Invocation: token.invocation, Key: token.signal.Key, Number: token.signal.Number, Kind: token.kind, Event: event, ObservedAt: now.UTC(), DispatchElapsedNanoseconds: int64(now.Sub(token.started)), StartupNativeCadenceNanoseconds: token.signal.NativeCadenceNanoseconds, ConfiguredDelayNanoseconds: int64(token.delay), Result: result}
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded)+1 > self.limits.recordBytes || int64(len(encoded)+1) > self.limits.totalBytes-self.bytes {
		self.fail()
		return false
	}
	encoded = append(encoded, '\n')
	written, err := self.output.Write(encoded)
	if err != nil || written != len(encoded) {
		self.fail()
		return false
	}
	self.sequence++
	self.bytes += int64(written)
	return true
}

func (self *RunInvocationWitness) begin(signal Signal, kind string, floor time.Duration) runInvocationToken {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failed || self.closed {
		return runInvocationToken{}
	}
	entry, ok := self.signals[signal.Key()]
	if !ok || entry.Number != signal.Number() || self.invocation == math.MaxUint64 {
		self.fail()
		return runInvocationToken{}
	}
	self.invocation++
	delay := time.Duration(entry.NativeCadenceNanoseconds)
	if kind == "active" {
		delay = max(delay, floor)
	}
	token := runInvocationToken{self.invocation, time.Now(), entry, kind, delay}
	if !self.write(token, "dispatch_begin", "") {
		return runInvocationToken{}
	}
	return token
}

func (self *RunInvocationWitness) returned(ctx context.Context, token runInvocationToken, err error) {
	if token.invocation == 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := "returned_nil"
	if err != nil {
		result = "returned_error"
		if errors.Is(err, context.Canceled) {
			result = "returned_canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			result = "returned_deadline"
		}
	} else if ctx.Err() != nil {
		result = "returned_nil_context_ended"
	}
	self.write(token, "run_return", result)
}

// Close errors also disable only this witness. No monitor/process outcome is
// replaced, and no record is written after the owner has closed.
func (self *RunInvocationWitness) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil
	}
	self.closed = true
	if closer, ok := self.output.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			self.fail()
		}
	}
	return nil
}

func runSignalWithWitness(ctx context.Context, signal Signal, settings SignalSettings, witness *RunInvocationWitness, kind string, floor time.Duration) (Alerts, error) {
	if witness == nil {
		return signal.Run(ctx, settings)
	}
	token := witness.begin(signal, kind, floor)
	// No defer/recover: unwinding is not a returned Signal.Run invocation.
	alerts, err := signal.Run(ctx, settings)
	witness.returned(ctx, token, err)
	return alerts, err
}
