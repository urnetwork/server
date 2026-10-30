// Administrative parsing rejects aliases before interpreting bounded schemas.
package main

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Metadata fields are deliberately small and visibly synthetic.
type syntheticMetadata struct {
	Value string  `json:"value"`
	Pin   filePin `json:"pin"`
}

// Every recursive object, including escaped keys, has one canonical spelling.
func TestMetadataRejectsDuplicateCaseUnknownAndTrailingAliases(t *testing.T) {
	settings := defaultArchiveSettings()
	for _, content := range []string{
		`{"value":"a","value":"b"}`,
		`{"Value":"a"}`,
		`{"VALUE":"a"}`,
		`{"value":"a","\u0076alue":"b"}`,
		`{"value":"a","unknown":true}`,
		`{"pin":{"path":"a","Path":"b"}}`,
		`{"pin":{"path":"a","path":"b"}}`,
		`{"pin":{"path":"a","sha256":"b","other":true}}`,
		`{"value":"a"} {"value":"b"}`,
		`{"value":"a"} trailing`,
		`{"value":"a","non-ascii-é":true}`,
	} {
		var target syntheticMetadata
		if err := decodeMetadata([]byte(content), &target, settings); err == nil {
			t.Fatal("noncanonical synthetic metadata accepted")
		}
	}
	var target syntheticMetadata
	if err := decodeMetadata([]byte(`{"value":"synthetic","pin":{"path":"synthetic","sha256":"synthetic"}}`), &target, settings); err != nil ||
		target.Value != "synthetic" || target.Pin.Path != "synthetic" {
		t.Fatal("canonical synthetic metadata refused", err)
	}
}

// Both nested arrays and objects consume the same finite parsing-depth budget.
func TestMetadataDepthBoundAppliesRecursively(t *testing.T) {
	settings := defaultArchiveSettings()
	settings.MaximumMetadataDepth = 2
	var target any
	if err := decodeMetadata([]byte(`{"value":[[["synthetic"]]]}`), &target, settings); err == nil {
		t.Fatal("nested metadata bypassed its depth bound")
	}
}

// Parser mode boundaries have no filesystem or network observation dependency.
func TestArgumentParserPreservesDefaultVerifyAndExplicitPrepare(t *testing.T) {
	planSha256 := digestBytes([]byte("synthetic selected plan"))
	request, preparation, err := parseArguments([]string{"-plan", "/synthetic/archive-plan.json", "-plan-sha256", planSha256})
	if err != nil || preparation != nil || request.Upload || request.UploadAuthorizationSha256 != "" {
		t.Fatal("default argument mode acquired write authority", err)
	}
	request, preparation, err = parseArguments([]string{"-prepare", "-source", "/synthetic/source",
		"-nonce", strings.Repeat("ab", 24), "-staging-reserve-bytes", "4096"})
	if err != nil || preparation == nil || preparation.StagingReserveBytes != 4096 || request.Upload {
		t.Fatal("explicit preparation mode changed", err)
	}
	for _, arguments := range [][]string{
		nil,
		{"-plan", "/synthetic/archive-plan.json", "-plan-sha256", strings.Repeat("A", 64)},
		{"-prepare", "-upload"},
		{"-prepare", "-plan", "/synthetic/archive-plan.json"},
		{"-prepare", "-authorize-upload-plan-sha256", planSha256},
		{"-plan", "/synthetic/archive-plan.json", "-plan-sha256", planSha256, "-source", "/synthetic/source"},
		{"-plan", "/synthetic/archive-plan.json", "-plan-sha256", planSha256, "positional"},
		{"-unknown"},
	} {
		if _, _, err := parseArguments(arguments); err == nil {
			t.Fatal("ambiguous synthetic argument mode accepted")
		}
	}
}

// Member identities exclude aliases and extraction-unsafe control characters.
func TestMemberNamesRejectTraversalAliasesAndControls(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../escape", "a/../b", "/absolute", "a//b", "a\\b", "a\nb", "a\rb", "a\x00b", "a\tb", "a\x7fb"} {
		if validMember(value) {
			t.Fatal("unsafe synthetic member accepted")
		}
	}
	for _, value := range []string{"synthetic.csv", "nested/synthetic-value.json"} {
		if !validMember(value) {
			t.Fatal("canonical synthetic member refused")
		}
	}
}

// A busy canonical slot prevents source scans even for explicit preparation.
func TestBusySlotRefusesBeforeSourceOrDiskObservation(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.Hooks.AcquireSlot = func() (io.Closer, error) { return nil, errors.New("synthetic busy slot") }
	fixture.Hooks.AvailableBytes = func(string) (int64, error) {
		t.Fatal("busy owner inspected disk")
		return 0, nil
	}
	fixture.Preparation.SourceRoot = filepath.Join(fixture.Policy.Anchor, "missing-synthetic-source")
	if _, err := executeArchive(context.Background(), archiveRequest{}, &fixture.Preparation, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
		fixture.FactoryCalls != 0 {
		t.Fatal("busy canonical slot admitted work")
	}
}

// Plans and inventories must be private, single-link regular files.
func TestMetadataPinsRejectSymlinksHardlinksAndNonPrivateLeaves(t *testing.T) {
	for _, variant := range []string{"symlink", "hardlink", "world-readable", "wrong-hash", "oversized"} {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		pin := fixture.Request.PlanPin
		switch variant {
		case "symlink":
			if err := os.Rename(pin.Path, pin.Path+".retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(pin.Path+".retained", pin.Path); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(pin.Path, filepath.Join(fixture.Policy.Anchor, "synthetic-plan-alias")); err != nil {
				t.Fatal(err)
			}
		case "world-readable":
			if err := os.Chmod(pin.Path, 0644); err != nil {
				t.Fatal(err)
			}
		case "wrong-hash":
			pin.Sha256 = strings.Repeat("f", 64)
		case "oversized":
			pin = writeSynthetic(t, pin.Path, []byte(strings.Repeat("x", int(fixture.Settings.MaximumMetadataBytes+1))))
		}
		if _, err := readPinned(pin, fixture.Policy, fixture.Settings); err == nil {
			t.Fatalf("unsafe synthetic metadata admitted: %s", variant)
		}
	}
}

// Non-positive settings never disable validation, and stream lengths cannot overflow.
func TestInvalidSettingsRefuseBeforeSlotOrTransport(t *testing.T) {
	mutations := []func(*archiveSettings){
		func(settings *archiveSettings) { settings.MaximumArchiveBytes = 0 },
		func(settings *archiveSettings) { settings.MaximumArchiveBytes = math.MaxInt64 },
		func(settings *archiveSettings) { settings.MaximumMetadataBytes = 0 },
		func(settings *archiveSettings) { settings.MaximumEntries = 0 },
		func(settings *archiveSettings) { settings.MaximumTreeEntries = 0 },
		func(settings *archiveSettings) { settings.MaximumSourceDepth = 0 },
		func(settings *archiveSettings) { settings.DirectoryBatchEntries = 0 },
		func(settings *archiveSettings) { settings.MaximumMetadataDepth = 0 },
		func(settings *archiveSettings) { settings.MinimumFreeBytes = 0 },
		func(settings *archiveSettings) { settings.MaximumStagingBytes = 0 },
		func(settings *archiveSettings) { settings.BufferBytes = 0 },
		func(settings *archiveSettings) { settings.VerificationTimeout = 0 },
		func(settings *archiveSettings) { settings.UploadTimeout = 0 },
	}
	for index, mutate := range mutations {
		fixture := newArchiveFixture(t)
		mutate(&fixture.Settings)
		fixture.Hooks.AcquireSlot = func() (io.Closer, error) {
			t.Error("invalid settings reached canonical slot")
			return nil, errors.New("synthetic forbidden observation")
		}
		if _, err := executeArchive(t.Context(), archiveRequest{}, &fixture.Preparation, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.FactoryCalls != 0 {
			t.Fatalf("invalid synthetic settings admitted at case %d", index)
		}
	}
}
