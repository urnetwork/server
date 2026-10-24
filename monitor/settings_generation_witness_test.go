// Synthetic callback and sink controls exercise witness authority and
// irreversible write ordering without invoking native unit discovery.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Supply visibly synthetic metadata to the private constructor.
func settingsWitnessTestIdentity() settingsWitnessIdentity {
	return settingsWitnessIdentity{Pid: 17, StartTicks: 42,
		BootId: "11111111-1111-1111-1111-111111111111", InvocationId: strings.Repeat("2", 32), ExecutableSha256: strings.Repeat("3", 64)}
}

// Bind independently frozen launch settings around one counted actual checker.
func settingsWitnessTestFixture(t *testing.T, output io.Writer) (SignalSettings, *SignalSettings, *settingsGenerationWitness, *int) {
	t.Helper()
	startup, current := settingsFreshnessFixture(t)
	startup.Hosts = []HostSettings{{Name: "metrics.example", Roles: []string{"services"}, scopeEndpoints: []string{"192.0.2.1"}}}
	startup.LogServices = []string{"api"}
	startup.LogServiceBlocks = map[string][]string{"api": {"synthetic-block"}}
	*current = startup
	loads := 0
	startup.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { loads++; return *current, nil })
	if startup.Validate() != nil {
		t.Fatal("synthetic witness fixture has invalid settings scope")
	}
	witness, err := newSettingsGenerationWitness(startup, output, settingsWitnessTestIdentity(), DefaultSettingsGenerationWitnessSettings())
	if err != nil {
		t.Fatal("synthetic witness initialization failed")
	}
	startup.settingsGenerationWitness = witness
	return startup, current, witness, &loads
}

// Decode only the fixed record schema, rejecting any unexpected output fields.
func settingsWitnessTestRecords(t *testing.T, output *bytes.Buffer) []settingsWitnessRecord {
	t.Helper()
	var records []settingsWitnessRecord
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	for {
		var record settingsWitnessRecord
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal("invalid synthetic witness record")
		}
		records = append(records, record)
	}
	return records
}

// Each scheduled callback produces exactly one same-launch witness and no load.
func TestSettingsWitnessRecordsActualCallbackWithoutExtraLoadOrComparisonDrift(t *testing.T) {
	var output bytes.Buffer
	startup, _, _, loads := settingsWitnessTestFixture(t, &output)
	// Preserve the original invalid-scope boundary as a negative control:
	// undeclared block owners must fail before either callback or output.
	invalid := startup
	invalid.LogServices = nil
	if _, err := NewSettingsFreshnessSignal().Run(t.Context(), invalid); err == nil || *loads != 0 || output.Len() != 0 {
		t.Fatal("undeclared synthetic log service bypassed settings validation")
	}
	for range 2 {
		alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
		if err != nil || len(alerts) != 0 {
			t.Fatal("witness installation changed unchanged freshness")
		}
	}
	records := settingsWitnessTestRecords(t, &output)
	if *loads != 2 || len(records) != 2 {
		t.Fatal("witness added a settings load or lost an existing callback")
	}
	for i, record := range records {
		if record.Result != "current" || !record.CompleteSettingsEqual || !record.StartupAssociation || !record.ProcessArtifactBinding ||
			record.Identity != settingsWitnessTestIdentity() || record.Sequence != uint64(i+1) || record.RunId != records[0].RunId || record.SnapshotId != records[0].SnapshotId ||
			!settingsWitnessHex(record.RunId, 32) || !settingsWitnessHex(record.SnapshotId, 32) || record.StartupServicesHosts != 1 ||
			record.Scope != "complete_effective_settings_after_cli_overrides" || record.CompletedAt.Before(record.StartedAt) || record.ElapsedNanoseconds < 0 {
			t.Fatal("callback witness lost its fixed process/startup association or result")
		}
	}
	for _, private := range []string{"metrics.example", "192.0.2.1", "synthetic-block", "synthetic-startup-secret", "password", "overlay"} {
		if strings.Contains(output.String(), private) {
			t.Fatal("witness retained a settings value or field outside the fixed schema")
		}
	}
}

// Stale and failed comparisons remain nonaffirmative without hiding alerts.
func TestSettingsWitnessStaleInventoryAndUnavailableCallbackRetainOriginalAlerts(t *testing.T) {
	var output bytes.Buffer
	startup, current, _, loads := settingsWitnessTestFixture(t, &output)
	current.Hosts = []HostSettings{{Name: "replacement.example", Roles: []string{"services"}}}
	alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
	if err != nil {
		t.Fatal("synthetic stale freshness failed")
	}
	requireAlertClass(t, alerts, "settings-generation-stale")
	startup.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		return true, errors.New("synthetic-private-loader-canary")
	}
	alerts, err = NewSettingsFreshnessSignal().Run(t.Context(), startup)
	if err != nil {
		t.Fatal("synthetic unavailable freshness failed")
	}
	requireAlertClass(t, alerts, "settings-generation-unobservable")
	records := settingsWitnessTestRecords(t, &output)
	if *loads != 1 || len(records) != 2 || records[0].Result != "stale" || records[1].Result != "unavailable" || records[0].CompleteSettingsEqual || records[1].CompleteSettingsEqual {
		t.Fatal("stale or failed callback became affirmative")
	}
	if strings.Contains(output.String(), "canary") || strings.Contains(Alerts(alerts).Markdown(), "canary") {
		t.Fatal("raw callback error escaped")
	}
}

// Frozen association survives nil/empty distinctions and refuses alias changes.
func TestSettingsWitnessRejectsMutatedStartupAliases(t *testing.T) {
	for _, mutation := range []string{"empty-private-scope", "hosts", "blocks", "private-scope"} {
		var output bytes.Buffer
		startup, _, _, _ := settingsWitnessTestFixture(t, &output)
		switch mutation {
		case "empty-private-scope":
			// Empty and nil collections compare differently. A fresh launch
			// binding must preserve an unchanged nonnil empty private slice.
			startup.Hosts[0].scopeEndpoints = []string{}
			witness, err := newSettingsGenerationWitness(startup, &output, settingsWitnessTestIdentity(), DefaultSettingsGenerationWitnessSettings())
			if err != nil {
				t.Fatal("synthetic empty collection initialization failed")
			}
			startup.settingsGenerationWitness = witness
			alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
			if err != nil || len(alerts) != 0 || output.Len() == 0 {
				t.Fatal("unchanged empty private collection lost its launch association")
			}
			continue
		case "hosts":
			startup.Hosts[0].Name = "mutated.example"
		case "blocks":
			startup.LogServiceBlocks["api"][0] = "mutated-block"
		case "private-scope":
			startup.Hosts[0].scopeEndpoints[0] = "192.0.2.2"
		}
		// The synthetic current snapshot shares the caller's aliases, so the
		// actual comparison can return true. The frozen launch binding cannot.
		alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
		if err != nil {
			t.Fatal("synthetic alias control failed")
		}
		failure := requireAlertClass(t, alerts, "cannot-observe")
		if failure.Target != "monitor-settings/witness" || output.Len() != 0 {
			t.Fatal("startup alias mutation forged a witness")
		}
	}
}

// Incomplete provenance and exhausted sequence space cannot authorize output.
func TestSettingsWitnessRejectsIncompleteIdentityAndSequenceExhaustion(t *testing.T) {
	for _, field := range []string{"pid", "start", "boot", "invocation", "artifact", "sequence"} {
		var output bytes.Buffer
		startup, _, witness, _ := settingsWitnessTestFixture(t, &output)
		switch field {
		case "pid":
			witness.identity.Pid = 0
		case "start":
			witness.identity.StartTicks = 0
		case "boot":
			witness.identity.BootId = "synthetic-private-identity-canary"
		case "invocation":
			witness.identity.InvocationId = "invalid"
		case "artifact":
			witness.identity.ExecutableSha256 = "invalid"
		case "sequence":
			witness.sequence = ^uint64(0)
		}
		alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
		if err != nil {
			t.Fatal("synthetic identity control failed")
		}
		requireAlertClass(t, alerts, "cannot-observe")
		if output.Len() != 0 {
			t.Fatal("incomplete identity or exhausted sequence produced a witness")
		}
	}
}

// Cancellation observed before write admission prevents any witness bytes.
func TestSettingsWitnessCancellationBeforeWriteEmitsNoAffirmativeRecord(t *testing.T) {
	var output bytes.Buffer
	startup, _, _, _ := settingsWitnessTestFixture(t, &output)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startup.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { cancel(); return true, nil }
	alerts, err := NewSettingsFreshnessSignal().Run(ctx, startup)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || output.Len() != 0 {
		t.Fatal("canceled callback emitted an affirmative witness or alert")
	}
}

// A failing or short sink proves that failed streams are not retried.
type settingsWitnessFailWriter struct {
	calls int
	short bool
}

// Return one deterministic fixed failure mode without emitting private errors.
func (self *settingsWitnessFailWriter) Write(value []byte) (int, error) {
	self.calls++
	if self.short {
		return len(value) / 2, nil
	}
	return 0, errors.New("synthetic-private-writer-canary")
}

// Output refusal leaves the actual stale result intact and makes no retry.
func TestSettingsWitnessOutputFailureIsStickyAndPreservesStaleAlert(t *testing.T) {
	for _, short := range []bool{false, true} {
		output := &settingsWitnessFailWriter{short: short}
		startup, current, _, loads := settingsWitnessTestFixture(t, output)
		current.PostgreSQL.Password = "synthetic-changed-secret"
		for range 2 {
			alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
			if err != nil {
				t.Fatal("writer failure replaced comparison result")
			}
			requireAlertClass(t, alerts, "settings-generation-stale")
			requireAlertClass(t, alerts, "cannot-observe")
			if strings.Contains(Alerts(alerts).Markdown(), "canary") {
				t.Fatal("raw writer error escaped")
			}
		}
		if output.calls != 1 || *loads != 2 {
			t.Fatal("failed writer retried or changed settings cadence")
		}
	}
}

// Invalid bounds, clocks and ended sink ownership remain unobservable.
func TestSettingsWitnessBoundsAndClosedOwnerCannotProduceRecords(t *testing.T) {
	for _, mode := range []string{"record-bound", "closed", "clock"} {
		var output bytes.Buffer
		startup, _, witness, _ := settingsWitnessTestFixture(t, &output)
		started := time.Date(2035, 1, 2, 3, 4, 0, 0, time.UTC)
		completed := started
		switch mode {
		case "record-bound":
			witness.settings.MaxRecordBytes = 1
		case "closed":
			if witness.Close() != nil {
				t.Fatal("synthetic close failed")
			}
		case "clock":
			completed = started.Add(-time.Second)
		}
		if witness.observe(t.Context(), startup, started, completed, true, nil) == nil || output.Len() != 0 {
			t.Fatal("invalid bound, owner or clock produced a record")
		}
	}
}

// Other probes and optional legacy settings do not create freshness witnesses.
func TestSettingsWitnessOnlyFreshnessInvocationEmitsAndManualSettingsStayCompatible(t *testing.T) {
	var output bytes.Buffer
	startup, _, _, loads := settingsWitnessTestFixture(t, &output)
	if current, err := startup.SettingsGenerationCheck(t.Context(), startup); err != nil || !current {
		t.Fatal("synthetic direct checker failed")
	}
	if output.Len() != 0 || *loads != 1 {
		t.Fatal("another probe's checker call emitted a freshness witness")
	}
	startup.settingsGenerationWitness = nil
	startup.SettingsGenerationCheck = nil
	alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
	if err != nil || len(alerts) != 0 || output.Len() != 0 {
		t.Fatal("manual settings lost optional-witness compatibility")
	}
}

// Concurrent completed callbacks retain one serialized sequence apiece.
func TestSettingsWitnessConcurrentInvocationsKeepOneSequencePerCallback(t *testing.T) {
	var output bytes.Buffer
	startup, _, _, _ := settingsWitnessTestFixture(t, &output)
	// This immutable callback is safe for concurrent invocation and performs
	// no settings load. The writer must still join and serialize each result.
	startup.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
	var workers sync.WaitGroup
	var failures bytes.Buffer
	var failureMu sync.Mutex
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			alerts, err := NewSettingsFreshnessSignal().Run(t.Context(), startup)
			if err != nil || len(alerts) != 0 {
				failureMu.Lock()
				failures.WriteByte('x')
				failureMu.Unlock()
			}
		}()
	}
	workers.Wait()
	records := settingsWitnessTestRecords(t, &output)
	if failures.Len() != 0 || len(records) != 4 {
		t.Fatal("concurrent witness callback failed")
	}
	for i, record := range records {
		if record.Sequence != uint64(i+1) {
			t.Fatal("concurrent sequence duplicated or reordered its records")
		}
	}
}

// Unsafe destinations fail before native metadata discovery or file mutation.
func TestSettingsWitnessPublicConfigurationRefusesReusedOrUnsafeFileWithoutOwnerReads(t *testing.T) {
	for _, kind := range []string{"existing", "symlink", "public-parent", "canceled"} {
		dir := t.TempDir()
		if os.Chmod(dir, 0700) != nil {
			t.Fatal("synthetic private directory setup failed")
		}
		path := filepath.Join(dir, "witness.jsonl")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		switch kind {
		case "existing":
			if os.WriteFile(path, []byte("synthetic-original"), 0600) != nil {
				t.Fatal("synthetic file setup failed")
			}
		case "symlink":
			if os.Symlink("synthetic-target", path) != nil {
				t.Fatal("synthetic link setup failed")
			}
		case "public-parent":
			if os.Chmod(dir, 0755) != nil {
				t.Fatal("synthetic directory mode setup failed")
			}
		case "canceled":
			cancel()
		}
		startup, _ := settingsFreshnessFixture(t)
		_, owner, err := startup.WithSettingsGenerationWitness(ctx, path, DefaultSettingsGenerationWitnessSettings())
		if err == nil || owner != nil {
			t.Fatal("unsafe output reached native witness construction")
		}
		if kind == "existing" {
			raw, readErr := os.ReadFile(path)
			if readErr != nil || string(raw) != "synthetic-original" {
				t.Fatal("existing output was mutated")
			}
		}
	}
}

// Cancel exactly inside the admitted irreversible write, optionally retaining
// only a partial prefix. No scheduling delay or rollback assumption is used.
type settingsWitnessCancelWriter struct {
	cancel context.CancelFunc
	output bytes.Buffer
	short  bool
	calls  int
}

// Transfer cancellation before accepting bytes, after write admission already
// passed. A full write commits; a short write leaves an unqualified prefix.
func (self *settingsWitnessCancelWriter) Write(value []byte) (int, error) {
	self.calls++
	self.cancel()
	if self.short {
		return self.output.Write(value[:len(value)/2])
	}
	return self.output.Write(value)
}

// A committed callback witness is not a claim that the enclosing signal
// completed: cancellation during Write may end that signal afterward.
func TestSettingsWitnessCancellationDuringWriteKeepsCommittedCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &settingsWitnessCancelWriter{cancel: cancel}
	startup, _, _, loads := settingsWitnessTestFixture(t, output)
	alerts, err := NewSettingsFreshnessSignal().Run(ctx, startup)
	records := settingsWitnessTestRecords(t, &output.output)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || *loads != 1 || output.calls != 1 || len(records) != 1 ||
		records[0].Result != "current" || !records[0].CompleteSettingsEqual || !records[0].StartupAssociation {
		t.Fatal("during-write cancellation lost committed callback facts or invented signal completion")
	}
}

// A canceled partial write cannot be rolled back or retried. Preserve its
// bytes as unqualified evidence and refuse another write on the same stream.
func TestSettingsWitnessCancellationDuringPartialWriteLeavesUnknownStream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &settingsWitnessCancelWriter{cancel: cancel, short: true}
	startup, _, _, loads := settingsWitnessTestFixture(t, output)
	alerts, err := NewSettingsFreshnessSignal().Run(ctx, startup)
	retained := append([]byte(nil), output.output.Bytes()...)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || len(retained) == 0 || bytes.HasSuffix(retained, []byte{'\n'}) {
		t.Fatal("partial canceled write became a complete witness or disappeared")
	}
	alerts, err = NewSettingsFreshnessSignal().Run(t.Context(), startup)
	if err != nil {
		t.Fatal("failed stream replaced the next actual callback result")
	}
	requireAlertClass(t, alerts, "cannot-observe")
	if output.calls != 1 || *loads != 2 || !bytes.Equal(retained, output.output.Bytes()) {
		t.Fatal("partial canceled stream retried or rewrote retained evidence")
	}
}
