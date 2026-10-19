package main

import (
	"testing"
	"time"
)

func TestParseSharedSshAdmissionPreservesCadence(t *testing.T) {
	opts, err := parseMonitorOptions([]string{"-ssh-admission-dir", "/synthetic/private/ssh", "-pg-query-sample-continuous", "-min-probe-cadence", "15m"})
	if err != nil || opts.sshAdmissionDirectory != "/synthetic/private/ssh" || !opts.pgQuerySampleContinuous || opts.minimumProbeCadence != 15*time.Minute {
		t.Fatal("shared SSH flag changed continuous PG or cadence")
	}
	for _, args := range [][]string{
		{"-ssh-admission-dir", "/synthetic/private/ssh", "-once"},
		{"-ssh-admission-dir", "/synthetic/private/ssh", "-list-signals"},
		{"-ssh-admission-dir", "/synthetic/private/ssh", "-pg-query-sample-continuous", "-min-probe-cadence", "1m"},
	} {
		if _, err := parseMonitorOptions(args); err == nil {
			t.Fatal("shared SSH flag bypassed continuous profile guard")
		}
	}
}
