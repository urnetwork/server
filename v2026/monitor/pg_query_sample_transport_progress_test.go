package monitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const pgTransportFixtureNonce = "000102030405060708090a0b0c0d0e0f"

// This independent fixture guard accepts the 17 exact finite emissions only.
// A child diagnostic, traceback, unexpected field or extra line still fails.
func pgTransportFixtureProgress(t *testing.T, input, stderr string) string {
	t.Helper()
	if stderr == "" {
		return "unknown"
	}
	var cfg struct {
		Nonce string `json:"transport_nonce"`
	}
	if json.Unmarshal([]byte(input), &cfg) != nil || cfg.Nonce != pgTransportFixtureNonce {
		t.Fatal("remote adapter emitted before validated fixture input")
	}
	if len(stderr) > 4096 || !strings.HasSuffix(stderr, "\n") {
		t.Fatal("remote adapter emitted unbounded or partial progress")
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) > 17 {
		t.Fatal("remote adapter emitted additional diagnostic lines")
	}
	phases := []string{"remote_started", "database_identity", "history_start"}
	counts := []int{0, 0, 0}
	for count := 1; count <= 12; count++ {
		phases = append(phases, "activity")
		counts = append(counts, count)
	}
	phases = append(phases, "blockers", "history_end")
	counts = append(counts, 12, 12)
	for i, line := range lines {
		const prefix = "monitor-pg-phase "
		if !strings.HasPrefix(line, prefix) {
			t.Fatal("remote adapter leaked a traceback or child stderr")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &fields) != nil || len(fields) != 4 {
			t.Fatal("remote adapter emitted unexpected progress fields")
		}
		var schema int
		var nonce, phase string
		var count *int
		if json.Unmarshal(fields["schema"], &schema) != nil || schema != 2 || json.Unmarshal(fields["nonce"], &nonce) != nil || nonce != cfg.Nonce || json.Unmarshal(fields["phase"], &phase) != nil || phase != phases[i] || json.Unmarshal(fields["samples"], &count) != nil || count == nil || *count != counts[i] {
			t.Fatal("remote adapter emitted unordered or unbound progress")
		}
	}
	return phases[len(lines)-1]
}

// Foreign marker-shaped banners cannot claim this attempt. Invalid own
// records and capped input cannot retain an apparently later successful phase.
func TestPgTransportAuthorityProgressRejectsAmbiguousOrForeignRecords(t *testing.T) {
	frame := func(nonce, phase string) string {
		raw, _ := json.Marshal(map[string]any{"schema": 1, "nonce": nonce, "phase": phase})
		return "monitor-pg-phase " + string(raw) + "\n"
	}
	start := frame(pgTransportFixtureNonce, "remote_started")
	identity := frame(pgTransportFixtureNonce, "database_identity")
	foreign := frame("101112131415161718191a1b1c1d1e1f", "database_identity")
	for _, testCase := range []struct{ name, raw, phase, status string }{
		{name: "absent", phase: "unknown", status: "absent"},
		{name: "raw-private", raw: "private-pg-transport-stderr-canary\n", phase: "unknown", status: "absent"},
		{name: "static-banner", raw: "monitor-pg-phase {\"schema\":1,\"phase\":\"database_identity\"}\n", phase: "unknown", status: "absent"},
		{name: "foreign", raw: foreign, phase: "unknown", status: "absent"},
		{name: "started", raw: start, phase: "remote_started", status: "valid"},
		{name: "identity", raw: start + identity, phase: "database_identity", status: "valid"},
		{name: "foreign-does-not-consume-lane", raw: foreign + start + foreign + identity + foreign, phase: "database_identity", status: "valid"},
		{name: "duplicate", raw: start + start, phase: "unknown", status: "invalid"},
		{name: "out-of-order", raw: identity + start, phase: "unknown", status: "invalid"},
		{name: "after-complete", raw: start + identity + start, phase: "unknown", status: "invalid"},
		{name: "own-unknown-phase", raw: frame(pgTransportFixtureNonce, "private-pg-transport-stderr-canary"), phase: "unknown", status: "invalid"},
		{name: "unknown-field", raw: strings.Replace(start, "{", "{\"raw\":\"private-canary\",", 1), phase: "unknown", status: "invalid"},
		{name: "duplicate-field", raw: strings.Replace(start, "{", "{\"schema\":1,", 1), phase: "unknown", status: "invalid"},
		{name: "future-schema", raw: strings.Replace(start, "\"schema\":1", "\"schema\":3", 1), phase: "unknown", status: "invalid"},
		{name: "partial-own", raw: strings.TrimSuffix(start, "\n"), phase: "unknown", status: "invalid"},
		{name: "cap-after-positive", raw: start + identity + strings.Repeat("x", 4096), phase: "unknown", status: "capped"},
		{name: "cap-before-positive", raw: strings.Repeat("x", 4096) + "\n" + start + identity, phase: "unknown", status: "capped"},
	} {
		phase, status, count := parsePgSampleTransportProgress(testCase.raw, pgTransportFixtureNonce)
		if phase != testCase.phase || status != testCase.status || count != nil {
			t.Fatalf("%s: progress authority got (%s,%s), want (%s,%s)", testCase.name, phase, status, testCase.phase, testCase.status)
		}
	}
}

// Exercise the real Python reader without changing its successful stdout.
// Completed response headers establish progress, never SQL result authority.
func TestPgTransportAuthorityRemoteProgramProgressAndPrivacy(t *testing.T) {
	frames := pgSampleTestEncode(t, pgSampleTestFrames(time.Now().UTC().Truncate(time.Second)))
	for _, testCase := range []struct{ name, output, childStderr, phase string }{
		{name: "complete", output: frames, phase: "history_end"},
		{name: "identity-does-not-prove-primary", output: strings.Replace(frames, "\"primary\":true", "\"primary\":false", 1), phase: "history_end"},
		{name: "missing-identity", output: "{}\n", phase: "remote_started"},
		{name: "partial-identity", output: strings.Split(frames, "\n")[0], phase: "remote_started"},
		{name: "duplicate-identity-key", output: strings.Replace(frames, "\"kind\":\"identity\"", "\"kind\":\"identity\",\"kind\":\"identity\"", 1), phase: "remote_started"},
		{name: "oversize-first-line", output: strings.Repeat(" ", 4097) + frames, phase: "remote_started"},
		{name: "private-child-stderr", output: frames, childStderr: "private-pg-transport-stderr-canary", phase: "history_end"},
		{name: "child-cannot-forge-progress", childStderr: "monitor-pg-phase {\"schema\":1,\"nonce\":\"" + pgTransportFixtureNonce + "\",\"phase\":\"database_identity\"}\n", phase: "remote_started"},
	} {
		quotedOutput, _ := json.Marshal(testCase.output)
		quotedStderr, _ := json.Marshal(testCase.childStderr)
		stub := "sys.stdin.read()\nsys.stderr.write(" + string(quotedStderr) + ")\nsys.stdout.write(" + string(quotedOutput) + ")\n"
		input := pgSampleProgramInput(t)
		out, stderr, err := runPgSampleProgramOutputFixture(t, pgQuerySampleProgram, stub, input)
		if err != nil || out != testCase.output {
			t.Fatalf("%s: progress changed the existing successful stdout", testCase.name)
		}
		if phase := pgTransportFixtureProgress(t, input, stderr); phase != testCase.phase {
			t.Fatalf("%s: reader invented or lost an identity response", testCase.name)
		}
		if strings.Contains(stderr, "private-") {
			t.Fatal("raw child diagnostic escaped the remote adapter")
		}
	}
}

// The phase channel is authorized only after the same stdin frame validates.
// Missing/malformed nonce input neither starts psql nor emits reader progress.
func TestPgTransportAuthorityRemoteProgramNonceValidation(t *testing.T) {
	for _, invalid := range []any{nil, "", "012345", strings.Repeat("A", 32), 7} {
		var cfg map[string]any
		if json.Unmarshal([]byte(pgSampleProgramInput(t)), &cfg) != nil {
			t.Fatal("fixture input")
		}
		cfg["transport_nonce"] = invalid
		if invalid == nil {
			delete(cfg, "transport_nonce")
		}
		raw, _ := json.Marshal(cfg)
		out, stderr, err := runPgSampleProgramOutputFixture(t, pgQuerySampleProgram, "sys.exit(97)\n", string(raw))
		failure, valid := parsePgSampleSourceFailure(out)
		if err == nil || !valid || failure.Phase != "bootstrap" || failure.Cause != "invalid_input" || stderr != "" {
			t.Fatal("invalid nonce emitted progress or entered the psql child")
		}
	}
}
