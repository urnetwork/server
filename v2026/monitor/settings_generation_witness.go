// Optional same-process settings witnesses retain only fixed callback facts;
// settings values and launch associations remain private and in memory.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Bounds one optional process-owned writer; non-positive limits are invalid.
type SettingsGenerationWitnessSettings struct {
	// Entire encoded jsonl record limit in bytes; non-positive is invalid.
	MaxRecordBytes int
	// One startup executable read limit in bytes; non-positive
	// is invalid. The default accommodates the monitor without unbounded reads.
	MaxExecutableBytes int64
	// Startup local owner-check deadline; non-positive is invalid.
	IdentityTimeout time.Duration
}

// One fixed-schema record fits in 4 KiB; the larger byte budget permits one
// ordinary monitor image read and the deadline bounds local owner discovery.
func DefaultSettingsGenerationWitnessSettings() SettingsGenerationWitnessSettings {
	return SettingsGenerationWitnessSettings{
		MaxRecordBytes: 4096, MaxExecutableBytes: 512 * 1024 * 1024, IdentityTimeout: 5 * time.Second,
	}
}

// Validated actual process and executable provenance, never configuration.
type settingsWitnessIdentity struct {
	Pid              int    `json:"pid"`
	StartTicks       uint64 `json:"start_ticks"`
	BootId           string `json:"boot_id"`
	InvocationId     string `json:"invocation_id"`
	ExecutableSha256 string `json:"executable_sha256"`
}

// Reject incomplete or unvalidated identity fields before encoding a record.
func (self settingsWitnessIdentity) valid() bool {
	return self.Pid > 0 && self.StartTicks > 0 && len(self.BootId) == 36 &&
		self.BootId[8] == '-' && self.BootId[13] == '-' && self.BootId[18] == '-' && self.BootId[23] == '-' &&
		settingsWitnessHex(strings.ReplaceAll(self.BootId, "-", ""), 32) &&
		settingsWitnessHex(self.InvocationId, 32) && settingsWitnessHex(self.ExecutableSha256, 64)
}

// Accept only the fixed lowercase hex vocabulary used by provenance fields.
func settingsWitnessHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// One bounded callback witness; counts describe scope but cannot prove members.
type settingsWitnessRecord struct {
	Schema                 int                     `json:"schema"`
	RunId                  string                  `json:"watcher_run_id"`
	SnapshotId             string                  `json:"startup_snapshot_id"`
	Sequence               uint64                  `json:"sequence"`
	Identity               settingsWitnessIdentity `json:"identity"`
	StartedAt              time.Time               `json:"started_at_utc"`
	CompletedAt            time.Time               `json:"completed_at_utc"`
	ElapsedNanoseconds     int64                   `json:"elapsed_nanoseconds"`
	Scope                  string                  `json:"scope"`
	Result                 string                  `json:"result"`
	CompleteSettingsEqual  bool                    `json:"complete_settings_equal"`
	StartupAssociation     bool                    `json:"startup_association_complete"`
	ProcessArtifactBinding bool                    `json:"process_artifact_binding_complete"`
	StartupServicesHosts   int                     `json:"startup_services_host_count"`
	StartupDisabledHosts   int                     `json:"startup_disabled_host_count"`
	StartupExcludedHosts   int                     `json:"startup_excluded_host_count"`
}

// All methods are safe for concurrent use. A failed stream is never retried.
// The bound settings remain in memory only, with independently owned mutable
// collections so later alias mutation cannot manufacture startup association.
// writeLock serializes irreversible writes and close; stateLock protects only
// the mutable state and is released before calling external output methods.
type settingsGenerationWitness struct {
	writeLock  sync.Mutex
	stateLock  sync.Mutex
	output     io.Writer
	settings   SettingsGenerationWitnessSettings
	startup    SignalSettings
	identity   settingsWitnessIdentity
	runId      string
	snapshotId string
	sequence   uint64
	failed     bool
	closed     bool
}

// Freeze launch inputs without serializing them; the sink and identity are
// supplied by the private file constructor or synthetic controls.
func newSettingsGenerationWitness(startup SignalSettings, output io.Writer, identity settingsWitnessIdentity, options SettingsGenerationWitnessSettings) (*settingsGenerationWitness, error) {
	if output == nil || !identity.valid() || options.MaxRecordBytes <= 0 || options.MaxExecutableBytes <= 0 || options.IdentityTimeout <= 0 || startup.SettingsGenerationCheck == nil {
		return nil, errors.New("settings witness initialization unavailable")
	}
	// Copy only in memory, without serializing settings or deriving a digest.
	// Private immutable scalar fields retain their values. The two private
	// host collections are copied explicitly below as well.
	var clone func(reflect.Value) reflect.Value
	clone = func(value reflect.Value) reflect.Value {
		switch value.Kind() {
		case reflect.Pointer:
			if value.IsNil() {
				return reflect.Zero(value.Type())
			}
			result := reflect.New(value.Type().Elem())
			result.Elem().Set(clone(value.Elem()))
			return result
		case reflect.Slice:
			if value.IsNil() {
				return reflect.Zero(value.Type())
			}
			result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
			for i := range value.Len() {
				result.Index(i).Set(clone(value.Index(i)))
			}
			return result
		case reflect.Map:
			if value.IsNil() {
				return reflect.Zero(value.Type())
			}
			result := reflect.MakeMapWithSize(value.Type(), value.Len())
			iterator := value.MapRange()
			for iterator.Next() {
				result.SetMapIndex(iterator.Key(), clone(iterator.Value()))
			}
			return result
		case reflect.Array:
			result := reflect.New(value.Type()).Elem()
			for i := range value.Len() {
				result.Index(i).Set(clone(value.Index(i)))
			}
			return result
		case reflect.Interface:
			if value.IsNil() {
				return reflect.Zero(value.Type())
			}
			result := reflect.New(value.Type()).Elem()
			result.Set(clone(value.Elem()))
			return result
		case reflect.Struct:
			result := reflect.New(value.Type()).Elem()
			result.Set(value)
			for i := range value.NumField() {
				if value.Type().Field(i).IsExported() {
					result.Field(i).Set(clone(value.Field(i)))
				}
			}
			return result
		default:
			return value
		}
	}
	bound := clone(reflect.ValueOf(comparableSignalSettings(startup))).Interface().(SignalSettings)
	bound.disabledHosts = clone(reflect.ValueOf(bound.disabledHosts)).Interface().([]HostSettings)
	for _, hosts := range [][]HostSettings{bound.Hosts, bound.disabledHosts} {
		for i := range hosts {
			hosts[i].scopeEndpoints = clone(reflect.ValueOf(hosts[i].scopeEndpoints)).Interface().([]string)
		}
	}
	var ids [32]byte
	if _, err := rand.Read(ids[:]); err != nil {
		return nil, errors.New("settings witness association unavailable")
	}
	return &settingsGenerationWitness{
		output: output, settings: options, startup: bound, identity: identity,
		runId: hex.EncodeToString(ids[:16]), snapshotId: hex.EncodeToString(ids[16:]),
	}, nil
}

// Open a new private local stream. This does not
// load settings, change cadence or run a probe. Its caller must close the
// returned owner after the monitor has joined all signal invocations.
func (self SignalSettings) WithSettingsGenerationWitness(ctx context.Context, path string, options SettingsGenerationWitnessSettings) (SignalSettings, io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return self, nil, err
	}
	if options.MaxRecordBytes <= 0 || options.MaxExecutableBytes <= 0 || options.IdentityTimeout <= 0 || self.SettingsGenerationCheck == nil || self.settingsGenerationWitness != nil {
		return self, nil, errors.New("settings witness configuration unavailable")
	}
	if err := self.Validate(); err != nil {
		return self, nil, errors.New("settings witness startup invalid")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if path == "" || err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return self, nil, errors.New("settings witness requires a private directory")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return self, nil, errors.New("settings witness requires a new output")
	}
	identityCtx, cancel := context.WithTimeout(ctx, options.IdentityTimeout)
	defer cancel()
	identity, err := settingsWitnessSelfIdentity(identityCtx, options)
	if err != nil || identityCtx.Err() != nil {
		return self, nil, errors.New("settings witness self identity unavailable")
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)
	if err != nil {
		return self, nil, errors.New("settings witness output unavailable")
	}
	witness, err := newSettingsGenerationWitness(self, output, identity, options)
	if err != nil {
		output.Close()
		return self, nil, err
	}
	self.settingsGenerationWitness = witness
	return self, witness, nil
}

// Observe one completed callback. The final context check admits an
// irreversible write: cancellation arriving inside Write cannot retract a
// successful full record, even if the enclosing signal then returns canceled.
func (self *settingsGenerationWitness) observe(ctx context.Context, startup SignalSettings, started, completed time.Time, current bool, checkErr error) error {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	unavailable := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.failed || self.closed
	}()
	if unavailable {
		return errors.New("settings witness output unavailable")
	}
	if ctx.Err() != nil {
		return errors.New("settings witness canceled")
	}
	if !self.identity.valid() || !reflect.DeepEqual(self.startup, comparableSignalSettings(startup)) || started.IsZero() || completed.Before(started) || self.sequence == math.MaxUint64 {
		func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.failed = true }()
		return errors.New("settings witness association unavailable")
	}
	func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.sequence++ }()
	result := "stale"
	if checkErr != nil {
		result = "unavailable"
	} else if current {
		result = "current"
	}
	services := 0
	for _, host := range self.startup.Hosts {
		for _, role := range host.Roles {
			if role == "services" {
				services++
				break
			}
		}
	}
	record := settingsWitnessRecord{
		Schema: 1, RunId: self.runId, SnapshotId: self.snapshotId, Sequence: self.sequence, Identity: self.identity,
		StartedAt: started.UTC(), CompletedAt: completed.UTC(), ElapsedNanoseconds: int64(completed.Sub(started)),
		Scope: "complete_effective_settings_after_cli_overrides", Result: result,
		CompleteSettingsEqual: current && checkErr == nil, StartupAssociation: true, ProcessArtifactBinding: true,
		StartupServicesHosts: services, StartupDisabledHosts: len(self.startup.disabledHosts), StartupExcludedHosts: len(self.startup.ExcludedHosts),
	}
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded)+1 > self.settings.MaxRecordBytes {
		func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.failed = true }()
		return errors.New("settings witness record unavailable")
	}
	// Once this check passes, the write owns its completion. A later cancel
	// can end the signal but cannot roll back bytes accepted by the sink.
	if ctx.Err() != nil {
		return errors.New("settings witness canceled")
	}
	encoded = append(encoded, '\n')
	written, err := self.output.Write(encoded)
	if err != nil || written != len(encoded) {
		func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.failed = true }()
		return errors.New("settings witness output unavailable")
	}
	return nil
}

// Join any admitted write before closing the owned sink; close is idempotent.
func (self *settingsGenerationWitness) Close() error {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	wasClosed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.closed = true
		return false
	}()
	if wasClosed {
		return nil
	}
	if closer, ok := self.output.(io.Closer); ok {
		if closer.Close() != nil {
			return errors.New("settings witness close unavailable")
		}
	}
	return nil
}
