package main

import "testing"

func TestMonitorAdmissionDiagnosticIsDefaultOff(t *testing.T) {
	opts, err := parseMonitorOptions(nil)
	if err != nil || opts.admissionDiagnosticDirectory != "" {
		t.Fatal("admission diagnostic default changed")
	}
}
func TestMonitorAdmissionDiagnosticRequiresContinuousWitness(t *testing.T) {
	for _, args := range [][]string{{"-admission-diagnostic-dir", "d"}, {"-once", "-invocation-witness-output", "w", "-admission-diagnostic-dir", "d"}, {"-list-signals", "-invocation-witness-output", "w", "-admission-diagnostic-dir", "d"}} {
		if _, err := parseMonitorOptions(args); err == nil {
			t.Fatal("unbound admission diagnostic accepted")
		}
	}
	opts, err := parseMonitorOptions([]string{"-invocation-witness-output", "w", "-admission-diagnostic-dir", "d"})
	if err != nil || opts.admissionDiagnosticDirectory != "d" {
		t.Fatal("explicit finite diagnostic lost")
	}
}
