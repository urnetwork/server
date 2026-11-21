package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// Nightly resolves the same test names, tags and owning packages as the registry, including packages
// outside sidecart3. This reads declarations without running any real-store test.
func TestNightlyT3ListsTheRegisteredClauseTests(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	options := &gateOptions{root: root, list: true, settings: defaultGateSettings()}
	var out bytes.Buffer
	if !runNightlyClauses(t.Context(), options, fault.RegisteredClauses(), &out, &out) {
		t.Fatal(out.String())
	}
	for _, clause := range fault.RegisteredClauses() {
		if clause.T3Package == "" {
			if strings.Contains(out.String(), clause.Id()+": listed\n") {
				t.Errorf("listed T2-only %s", clause.Id())
			}
			continue
		}
		testName := fault.GateClauseT3TestName(clause)
		if !strings.Contains(out.String(), clause.Id()+": listed\n") || !strings.Contains(out.String(), " "+testName+": declared") {
			t.Errorf("did not list %s from %s", testName, clause.T3Package)
		}
	}
	script, err := os.ReadFile("../../sidecar-t3.sh")
	if err != nil || !strings.Contains(string(script), `"$GO" run ./local/sidecar-gate nightly-clauses`) {
		t.Fatalf("the T3 runner does not invoke the nightly clause tests: %v", err)
	}
}

// A named T3 entry must be built under its tags, run in its package, and pass. Neither skipped or absent
// tests nor a wrapper that fails after a passing test satisfy the nightly run.
func TestNightlyT3RequiresEveryClauseToRunAndPass(t *testing.T) {
	options := writeGateFixture(t, "gates: {}\n", "ids: {}\n")
	serverDir := filepath.Join(options.root, "server")
	if err := os.Rename(filepath.Join(options.root, "fixture"), serverDir); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(serverDir, "evshost")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	clauses := []*fault.ClauseRegistration{
		fault.LookupClause("FS-1", "concurrent retries"),
		fault.LookupClause("FS-1", "unknown deletes nothing"),
	}
	for _, clause := range clauses {
		if clause == nil || clause.T3Package != "./evshost" {
			t.Fatal("FS-1's T3 clauses are missing")
		}
	}
	wrapper := filepath.Join(options.root, "stores.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ncd \"$1\" || exit 1\nshift\ngo test \"$@\" || exit $?\nexit \"${FIXTURE_WRAPPER_STATUS:-0}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	options.environ = append(options.environ, gateStoreWrapperVariable+"="+wrapper)
	source := "//go:build sidecar_testhooks && sidecar_localstack && sidecar_pending\n\npackage evshost\nimport \"testing\"\nfunc TestFs1ConcurrentRetriesOnT3(t *testing.T) {}\n"
	for _, reading := range []struct {
		second string
		want   string
	}{
		{second: "", want: gateMissing},
		{second: "func TestFs1UnknownDeletesNothingOnT3(t *testing.T) { t.Skip() }", want: gateOpen},
		{second: "func TestFs1UnknownDeletesNothingOnT3(t *testing.T) {}", want: gatePass},
	} {
		if err := os.WriteFile(filepath.Join(directory, "clause_test.go"), []byte(source+reading.second+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		pass := runNightlyClauses(context.Background(), options, clauses, &out, io.Discard)
		if pass != (reading.want == gatePass) || !strings.Contains(out.String(), clauses[1].Id()+": "+reading.want) {
			t.Errorf("expected %s, got %v: %s", reading.want, pass, &out)
		}
	}
	clockClause := fault.LookupClause("FS-60", "parcels pack at seven days")
	if clockClause == nil {
		t.Fatal("missing FS-60 parcel clause")
	}
	clockSource := "package evshost\nimport \"testing\"\nfunc TestFs60ParcelsPackAtSevenDaysOnT3(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(directory, "clock_test.go"), []byte(clockSource), 0o600); err != nil {
		t.Fatal(err)
	}
	var clockOut bytes.Buffer
	if runNightlyClauses(t.Context(), options, []*fault.ClauseRegistration{clockClause}, &clockOut, io.Discard) || !strings.Contains(clockOut.String(), clockClause.Id()+": "+gateMissing) {
		t.Errorf("nightly accepted FS-60 without an observed translation: %s", &clockOut)
	}
	options.environ = append(options.environ, "FIXTURE_WRAPPER_STATUS=1")
	var out bytes.Buffer
	if runNightlyClauses(context.Background(), options, clauses, &out, io.Discard) || !strings.Contains(out.String(), "test process failed") {
		t.Errorf("lost a wrapper failure after the test passed: %s", &out)
	}
}
