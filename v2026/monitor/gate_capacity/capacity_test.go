// Pure typed arithmetic controls; no files, walks, native gates or network.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCapacityAccounting(t *testing.T) {
	gib := int64(1024 * 1024 * 1024)
	base := capacityInput{Schema: "capacity-preflight-v1", Profile: "old-5gib", StreamScope: "qualified-capture-ledger", PhaseScope: "registered-roots-with-phase-once", CaptureBasisSha: strings.Repeat("a", 64), RecordBasisSha: strings.Repeat("b", 64), GlobalPhaseBasisSha: strings.Repeat("c", 64), GlobalIncludesPhaseOnce: true, Streams: 48 * 1024 * 1024, Records: 5 * 1024 * 1024, ExtraProof: 151, RequestedStreams: 5 * 1024 * 1024, MetadataReserve: 512 * 1024, Global: 18 * gib, Phase: 4 * gib, Free: 10 * gib}
	healthy := capacity(base)
	if !healthy.Green || healthy.IncrementalReserve != gib || healthy.GlobalProjected != 19*gib {
		t.Fatal("contained phase was double counted")
	}
	if base.Global+phaseCeiling < globalCeiling {
		t.Fatal("synthetic double-count discriminator invalid")
	}
	for _, c := range []struct {
		name   string
		change func(*capacityInput)
		reason string
	}{
		{name: "basename_scope", change: func(i *capacityInput) { i.StreamScope = "recursive-basename" }, reason: "scope_or_authority_unknown"},
		{name: "phase_scope_unknown", change: func(i *capacityInput) { i.GlobalIncludesPhaseOnce = false }, reason: "scope_or_authority_unknown"},
		{name: "missing_receipt", change: func(i *capacityInput) { i.CaptureBasisSha = "" }, reason: "scope_or_authority_unknown"},
		{name: "missing_reserve", change: func(i *capacityInput) { i.RequestedStreams = 0 }, reason: "profile_reserve_mismatch"},
		{name: "phase_not_contained", change: func(i *capacityInput) { i.Phase = i.Global + 1 }, reason: "phase_not_contained_in_global"},
		{name: "phase_exceeded", change: func(i *capacityInput) { i.Phase = phaseCeiling + 1 }, reason: "phase_ceiling_exceeded"},
		{name: "capture_at_limit", change: func(i *capacityInput) {
			i.Streams = captureCeiling - i.Records - i.ExtraProof - i.RequestedStreams - i.MetadataReserve
		}, reason: "capture_capacity"},
		{name: "global_at_limit", change: func(i *capacityInput) { i.Global = globalCeiling - gib }, reason: "global_capacity"},
		{name: "free_at_reserve", change: func(i *capacityInput) { i.Free = gib }, reason: "free_capacity"},
		{name: "negative", change: func(i *capacityInput) { i.Streams = -1 }, reason: "negative_input"},
		{name: "overflow", change: func(i *capacityInput) { i.Streams = math.MaxInt64 }, reason: "integer_overflow"},
	} {
		input := base
		c.change(&input)
		result := capacity(input)
		found := false
		for _, reason := range result.Reasons {
			if reason == c.reason {
				found = true
			}
		}
		if result.Green || !found {
			t.Fatalf("%s admitted or lost reason", c.name)
		}
		if input.Phase > phaseCeiling && result.IncrementalReserve != 0 {
			t.Fatal("negative reserve discounted global work")
		}
	}
	atPhaseLimit := base
	atPhaseLimit.Phase = phaseCeiling
	result := capacity(atPhaseLimit)
	if !result.Green || result.IncrementalReserve != 0 || result.GlobalProjected != atPhaseLimit.Global {
		t.Fatal("phase equality accounting mismatch")
	}
}

// This is a policy discriminator, not a fresh live-capacity admission.
func TestKnownPhaseProfiles(t *testing.T) {
	input := capacityInput{Schema: "capacity-preflight-v1", Profile: "old-5gib", StreamScope: "qualified-capture-ledger", PhaseScope: "registered-roots-with-phase-once", CaptureBasisSha: strings.Repeat("a", 64), RecordBasisSha: strings.Repeat("b", 64), GlobalPhaseBasisSha: strings.Repeat("c", 64), GlobalIncludesPhaseOnce: true, Streams: 48873683, Records: 4878696, ExtraProof: 151, RequestedStreams: 5 * 1024 * 1024, MetadataReserve: 512 * 1024, Global: 18475689005, Phase: 5407922968, Free: 1776069173248}
	old := capacity(input)
	if old.Green || old.IncrementalReserve != 0 || old.GlobalProjected != input.Global {
		t.Fatal("old five GiB policy did not refuse retained over-cap phase")
	}
	found := false
	for _, reason := range old.Reasons {
		if reason == "phase_ceiling_exceeded" {
			found = true
		}
	}
	if !found {
		t.Fatal("old phase failure reason lost")
	}
	input.Profile = "new-red-6gib"
	current := capacity(input)
	wantReserve := int64(6*1024*1024*1024) - input.Phase
	if !current.Green || current.PhaseCeiling != 6*1024*1024*1024 || current.IncrementalReserve != wantReserve || current.GlobalProjected != input.Global+wantReserve || current.CaptureProjected != 59519698 {
		t.Fatal("explicit six GiB profile arithmetic mismatch")
	}
	input.Profile = "unknown-profile"
	if result := capacity(input); result.Green || len(result.Reasons) != 1 || result.Reasons[0] != "unknown_profile" {
		t.Fatal("unknown policy accepted")
	}
	input.Profile = "new-red-6gib"
	input.GlobalIncludesPhaseOnce = false
	if capacity(input).Green {
		t.Fatal("six GiB profile bypassed scope proof")
	}
}

func TestCapacityCLIHelper(t *testing.T) {
	if os.Getenv("CAPACITY_JSON_HELPER") != "1" {
		return
	}
	main()
	os.Exit(0)
}

func TestTypedCapacitySchema(t *testing.T) {
	_, evidence := controlPaths(t)
	base := capacityInput{Schema: "capacity-preflight-v1", Profile: "old-5gib", StreamScope: "qualified-capture-ledger", PhaseScope: "registered-roots-with-phase-once", CaptureBasisSha: strings.Repeat("a", 64), RecordBasisSha: strings.Repeat("b", 64), GlobalPhaseBasisSha: strings.Repeat("c", 64), GlobalIncludesPhaseOnce: true, Streams: 48 * 1024 * 1024, Records: 5 * 1024 * 1024, ExtraProof: 151, RequestedStreams: 5 * 1024 * 1024, MetadataReserve: 512 * 1024, Global: 18 * 1024 * 1024 * 1024, Phase: 4 * 1024 * 1024 * 1024, Free: 10 * 1024 * 1024 * 1024}
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatal("synthetic encoding")
	}
	var fields map[string]any
	if json.Unmarshal(valid, &fields) != nil {
		t.Fatal("synthetic schema")
	}
	delete(fields, "captured_stream_bytes")
	missing, _ := json.Marshal(fields)
	duplicate := append(append([]byte{}, valid[:len(valid)-1]...), []byte(`,"available_bytes":1}`)...)
	nullValue := bytes.Replace(valid, []byte(`"captured_stream_bytes":50331648`), []byte(`"captured_stream_bytes":null`), 1)
	for _, c := range []struct {
		name  string
		input []byte
		green bool
	}{
		{name: "valid", input: valid, green: true},
		{name: "missing", input: missing},
		{name: "duplicate", input: duplicate},
		{name: "null", input: nullValue},
		{name: "trailing", input: append(append([]byte{}, valid...), []byte(` {}`)...)},
		{name: "oversize", input: bytes.Repeat([]byte("x"), 16*1024+1)},
	} {
		dir, err := os.MkdirTemp(evidence, "schema-"+c.name+"-")
		if err != nil {
			t.Fatal("private fixture allocation")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run", "^TestCapacityCLIHelper$")
		cmd.Env = append(os.Environ(), "CAPACITY_JSON_HELPER=1")
		cmd.Stdin = bytes.NewReader(c.input)
		cmd.WaitDelay = 2 * time.Second
		output, runErr := cmd.CombinedOutput()
		expired := ctx.Err() == context.DeadlineExceeded
		cancel()
		if os.WriteFile(filepath.Join(dir, "result.json"), output, 0600) != nil {
			t.Fatal("result retention")
		}
		if expired || (runErr == nil) != c.green {
			t.Fatalf("%s schema exit", c.name)
		}
		var result capacityResult
		if json.Unmarshal(output, &result) != nil || result.Green != c.green {
			t.Fatalf("%s schema result", c.name)
		}
	}
}

// Receipt-write failure is a failed preflight even when arithmetic is green.
func TestCapacityReceiptFailureBlocksLaunch(t *testing.T) {
	source, evidence := controlPaths(t)
	dir, err := os.MkdirTemp(evidence, "receipt-failure-")
	if err != nil {
		t.Fatal("private fixture allocation")
	}
	path := filepath.Join(dir, "read-only-output")
	if os.WriteFile(path, nil, 0600) != nil {
		t.Fatal("read-only fixture creation")
	}
	readOnly, err := os.Open(path)
	if err != nil {
		t.Fatal("read-only fixture open")
	}
	defer readOnly.Close()
	input := capacityInput{Schema: "capacity-preflight-v1", Profile: "old-5gib", StreamScope: "qualified-capture-ledger", PhaseScope: "registered-roots-with-phase-once", CaptureBasisSha: strings.Repeat("a", 64), RecordBasisSha: strings.Repeat("b", 64), GlobalPhaseBasisSha: strings.Repeat("c", 64), GlobalIncludesPhaseOnce: true, Streams: 1, Records: 1, ExtraProof: 1, RequestedStreams: 5 * 1024 * 1024, MetadataReserve: 512 * 1024, Global: 18 * 1024 * 1024 * 1024, Phase: 4 * 1024 * 1024 * 1024, Free: 10 * 1024 * 1024 * 1024}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal("synthetic input encoding")
	}
	marker := filepath.Join(dir, "launch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", source, "--preflight", os.Args[0], "-test.run", "^TestCapacityCLIHelper$", "--launch", "bash", "-c", `printf launched >"$1"`, "synthetic", marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Env = append(os.Environ(), "CAPACITY_JSON_HELPER=1")
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stdout = readOnly
	cmd.WaitDelay = 2 * time.Second
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err := cmd.Run(); err == nil || ctx.Err() != nil {
		t.Fatal("receipt failure admitted or timed out")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("receipt failure reached launch")
	}
	if os.WriteFile(filepath.Join(dir, "diagnostics"), diagnostics.Bytes(), 0600) != nil {
		t.Fatal("diagnostic retention")
	}
}
