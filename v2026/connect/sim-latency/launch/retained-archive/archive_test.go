// Synthetic private trees exercise preparation and validation without services.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// All paths, identities, contents, and storage destinations are test-generated.
type archiveFixture struct {
	Policy       archivePolicy
	Settings     archiveSettings
	Hooks        archiveHooks
	Preparation  prepareRequest
	Request      archiveRequest
	Plan         archivePlan
	Inventory    archiveInventory
	Store        *fakeArchiveTransport
	FactoryCalls int
}

// Test-only mutation deliberately rehashes metadata to exercise semantic guards.
func writeSynthetic(t *testing.T, value string, content []byte) filePin {
	t.Helper()
	if _, err := os.Lstat(value); err == nil {
		if err := os.Chmod(value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(value, content, 0600); err != nil {
		t.Fatal(err)
	}
	return filePin{Path: value, Sha256: digestBytes(content)}
}

// Only small synthetic administrative documents are serialized by the fixture.
func writeSyntheticJson(t *testing.T, value string, document any) filePin {
	t.Helper()
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return writeSynthetic(t, value, content)
}

// The synthetic slot uses a temporary real flock inode, never a live lock.
func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	anchor := t.TempDir()
	source := filepath.Join(anchor, "synthetic-source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	writeSynthetic(t, filepath.Join(source, "empty.log"), nil)
	writeSynthetic(t, filepath.Join(source, "failed-outcome.json"), []byte(`{"complete":false,"accepted":false,"reason":"synthetic failure"}`))
	writeSynthetic(t, filepath.Join(source, "raw.csv"), []byte("synthetic,value\n1,2\n"))
	settings := defaultArchiveSettings()
	settings.MaximumArchiveBytes = 1024 * 1024
	settings.MaximumMetadataBytes = 64 * 1024
	settings.MaximumEntries, settings.MaximumTreeEntries = 32, 64
	settings.DirectoryBatchEntries = 8
	settings.MinimumFreeBytes, settings.MaximumStagingBytes = 4096, 1024*1024
	settings.BufferBytes = 1024
	policy := archivePolicy{OwnerUid: uint32(os.Geteuid()), Anchor: anchor,
		WorkParent: anchor, WorkPrefix: "synthetic-archive-", Context: "synthetic-first-committed-context",
		PublicPlanSha256: digestBytes([]byte("synthetic public commitment")),
		KeyPrefix:        "blob/synthetic-first-committed", Bucket: "synthetic-evidence",
		Owner: "owner@archive.example", RetainUntil: time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceRootUnits: map[string]string{source: "synthetic-unit"}}
	fixture := &archiveFixture{Policy: policy, Settings: settings,
		Preparation: prepareRequest{SourceRoot: source, StorageNonce: strings.Repeat("ab", 24)}}
	fixture.Store = &fakeArchiveTransport{Policy: policy}
	lockPath := filepath.Join(anchor, "synthetic-scoring.lock")
	writeSynthetic(t, lockPath, nil)
	fixture.Hooks = archiveHooks{
		Now:            func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		AvailableBytes: func(string) (int64, error) { return 16 * 1024 * 1024, nil },
		AcquireSlot: func() (io.Closer, error) {
			file, err := os.Open(lockPath)
			if err != nil {
				return nil, err
			}
			if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				file.Close()
				return nil, err
			}
			return file, nil
		},
		NewTransport: func(archivePolicy) (archiveTransport, error) {
			fixture.FactoryCalls++
			return fixture.Store, nil
		},
		AfterStep: func(string) error { return nil },
	}
	return fixture
}

// Preparation uses the public command core; no success criterion is inferred.
func (self *archiveFixture) prepare(t *testing.T) {
	t.Helper()
	report, err := executeArchive(t.Context(), archiveRequest{}, &self.Preparation, self.Policy, self.Settings, self.Hooks)
	if err != nil {
		t.Fatal(err)
	}
	self.Request = archiveRequest{PlanPin: report.Plan}
	content, err := os.ReadFile(report.Plan.Path)
	if err != nil || json.Unmarshal(content, &self.Plan) != nil {
		t.Fatal("synthetic plan unavailable")
	}
	content, err = os.ReadFile(self.Plan.Inventory.Path)
	if err != nil || json.Unmarshal(content, &self.Inventory) != nil {
		t.Fatal("synthetic inventory unavailable")
	}
}

// A rehashed synthetic plan is still subject to fixed context and source guards.
func (self *archiveFixture) rewritePlan(t *testing.T) {
	t.Helper()
	self.Request.PlanPin = writeSyntheticJson(t, self.Request.PlanPin.Path, self.Plan)
	if self.Request.Upload {
		self.Request.UploadAuthorizationSha256 = self.Request.PlanPin.Sha256
	}
}

// Authorization is separate from selecting upload mode.
func (self *archiveFixture) authorizeUpload() {
	self.Request.Upload = true
	self.Request.UploadAuthorizationSha256 = self.Request.PlanPin.Sha256
}

// Failed/partial outcomes remain opaque payload, not a reason to omit evidence.
func TestPreparePreservesFailedEvidenceAndDefaultsToNoNetwork(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.prepare(t)
	report, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks)
	if err != nil || report.Mode != "verified-local-only" || report.PayloadFiles != 3 ||
		report.ArchiveMembers != 4 || report.AcceptanceClaimed || fixture.FactoryCalls != 0 {
		t.Fatal("local preparation or default verification changed its scope", err)
	}
	content, err := os.ReadFile(filepath.Join(fixture.Preparation.SourceRoot, "failed-outcome.json"))
	if err != nil || !bytes.Contains(content, []byte(`"accepted":false`)) {
		t.Fatal("failed original outcome was changed")
	}
	directory := filepath.Dir(fixture.Request.PlanPin.Path)
	for _, name := range []string{"archive-plan.json", "inventory.json", "full-evidence.tar", "prepare.complete.json"} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatal("preparation left a non-private or missing output")
		}
	}
	if _, err := os.Lstat(filepath.Join(directory, "upload.started.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("local verification claimed an upload")
	}
}

// A directory prefix must not make traversal order replace canonical inventory order.
func TestSourceCensusUsesGlobalLexicalMemberOrder(t *testing.T) {
	fixture := newArchiveFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.Preparation.SourceRoot, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	writeSynthetic(t, filepath.Join(fixture.Preparation.SourceRoot, "a", "value"), []byte("nested synthetic value"))
	writeSynthetic(t, filepath.Join(fixture.Preparation.SourceRoot, "a-before-directory"), []byte("synthetic sibling"))
	fixture.prepare(t)
	previous := ""
	for _, entry := range fixture.Inventory.Entries {
		if entry.Name <= previous {
			t.Fatal("inventory was not globally ordered")
		}
		previous = entry.Name
	}
}

// Context, namespace, source, retention, and count remain checked after rehashing.
func TestRehashedPlanMutationsRefuseBeforeTransport(t *testing.T) {
	mutations := []func(*archivePlan){
		func(plan *archivePlan) { plan.Context = "synthetic-other-context" },
		func(plan *archivePlan) { plan.PublicPlanSha256 = digestBytes([]byte("synthetic other commitment")) },
		func(plan *archivePlan) { plan.SourceRoot = filepath.Join(plan.SourceRoot, "other") },
		func(plan *archivePlan) { plan.Unit = "synthetic-other-unit" },
		func(plan *archivePlan) { plan.Key = "blob/synthetic-other-namespace/value" },
		func(plan *archivePlan) { plan.Owner = "other@archive.example" },
		func(plan *archivePlan) { plan.RetainUntil = plan.RetainUntil.Add(-time.Second) },
		func(plan *archivePlan) { plan.ArchiveBytes++ },
		func(plan *archivePlan) { plan.StagingReserveBytes = -1 },
		func(plan *archivePlan) { plan.ArchiveSha256 = strings.Repeat("a", 64) },
	}
	for index, mutate := range mutations {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.authorizeUpload()
		mutate(&fixture.Plan)
		fixture.rewritePlan(t)
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil || fixture.FactoryCalls != 0 {
			t.Fatalf("changed synthetic plan admitted at case %d", index)
		}
	}
}

// A substituted digest is insufficient to authorize an ambiguous inventory.
func TestRehashedInventoryRejectsDuplicatesTraversalMissingAndWrongSource(t *testing.T) {
	mutations := []func(*archiveInventory){
		func(inventory *archiveInventory) { inventory.Entries[1] = inventory.Entries[0] },
		func(inventory *archiveInventory) { inventory.Entries[0].Name = "../synthetic-escape" },
		func(inventory *archiveInventory) { inventory.Entries = inventory.Entries[1:] },
		func(inventory *archiveInventory) { inventory.SourceRoot = filepath.Join(inventory.SourceRoot, "other") },
		func(inventory *archiveInventory) { inventory.Entries[0].Sha256 = strings.Repeat("b", 64) },
		func(inventory *archiveInventory) { inventory.Entries[0].Bytes = -1 },
	}
	for index, mutate := range mutations {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		mutate(&fixture.Inventory)
		fixture.Plan.Inventory = writeSyntheticJson(t, fixture.Plan.Inventory.Path, fixture.Inventory)
		fixture.rewritePlan(t)
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
			t.Fatalf("changed synthetic inventory admitted at case %d", index)
		}
	}
}

// Source additions, substitutions, links, and writable leaves invalidate the snapshot.
func TestSourceMutationsAndAliasesRefuse(t *testing.T) {
	mutations := []func(*testing.T, *archiveFixture){
		func(t *testing.T, fixture *archiveFixture) {
			writeSynthetic(t, filepath.Join(fixture.Preparation.SourceRoot, "extra"), []byte("synthetic extra"))
		},
		func(t *testing.T, fixture *archiveFixture) {
			writeSynthetic(t, filepath.Join(fixture.Preparation.SourceRoot, "raw.csv"), []byte("synthetic changed"))
		},
		func(t *testing.T, fixture *archiveFixture) {
			value := filepath.Join(fixture.Preparation.SourceRoot, "raw.csv")
			if err := os.Rename(value, value+".retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(value+".retained", value); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, fixture *archiveFixture) {
			value := filepath.Join(fixture.Preparation.SourceRoot, "raw.csv")
			if err := os.Link(value, filepath.Join(fixture.Policy.Anchor, "synthetic-alias")); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, fixture *archiveFixture) {
			if err := os.Chmod(filepath.Join(fixture.Preparation.SourceRoot, "raw.csv"), 0660); err != nil {
				t.Fatal(err)
			}
		},
	}
	for index, mutate := range mutations {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		mutate(t, fixture)
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
			t.Fatalf("changed synthetic source admitted at case %d", index)
		}
	}
}

// Disk refusal occurs before preparation creates a directory or any output.
func TestInsufficientDiskHeadroomRefusesBeforePreparation(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.Hooks.AvailableBytes = func(string) (int64, error) { return 0, nil }
	if _, err := executeArchive(t.Context(), archiveRequest{}, &fixture.Preparation, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("insufficient disk admitted preparation")
	}
	directory, err := workDirectory(fixture.Policy, fixture.Preparation.StorageNonce)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disk refusal created partial state")
	}
}

// Interrupted preparation never overwrites or deletes its already-spent namespace.
func TestPartialPreparationRemainsSpentWithoutRollback(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.Hooks.AfterStep = func(step string) error {
		if step == "prepare-claimed" {
			return errors.New("synthetic preparation interruption")
		}
		return nil
	}
	if _, err := executeArchive(t.Context(), archiveRequest{}, &fixture.Preparation, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("synthetic partial preparation did not fail")
	}
	fixture.Hooks.AfterStep = func(string) error { return nil }
	if _, err := executeArchive(t.Context(), archiveRequest{}, &fixture.Preparation, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("partial preparation retried")
	}
	directory, _ := workDirectory(fixture.Policy, fixture.Preparation.StorageNonce)
	if _, err := os.Lstat(filepath.Join(directory, "prepare.started.json")); err != nil {
		t.Fatal("partial preparation claim was removed")
	}
}

// File count, directory count/depth, and byte bounds apply before output creation.
func TestSourceResourceBoundsRefuseBeforePreparation(t *testing.T) {
	for _, variant := range []string{"files", "tree", "depth", "bytes", "metadata"} {
		fixture := newArchiveFixture(t)
		switch variant {
		case "files":
			fixture.Settings.MaximumEntries = 2
		case "tree":
			fixture.Settings.MaximumEntries = 3
			fixture.Settings.MaximumTreeEntries = 3
			fixture.Settings.DirectoryBatchEntries = 2
		case "depth":
			fixture.Settings.MaximumSourceDepth = 1
			directory := filepath.Join(fixture.Preparation.SourceRoot, "nested", "deeper")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			writeSynthetic(t, filepath.Join(directory, "synthetic"), []byte("synthetic depth"))
		case "bytes":
			fixture.Settings.MaximumArchiveBytes = 1024
		case "metadata":
			fixture.Settings.MaximumMetadataBytes, fixture.Settings.BufferBytes = 128, 64
		}
		if _, err := executeArchive(t.Context(), archiveRequest{}, &fixture.Preparation, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.FactoryCalls != 0 {
			t.Fatalf("synthetic source bound admitted: %s", variant)
		}
		directory, err := workDirectory(fixture.Policy, fixture.Preparation.StorageNonce)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("synthetic source bound created output: %s", variant)
		}
	}
}

// A canceled owner cannot prepare, upload, or claim a later attempt.
func TestCanceledArchiveOwnerDoesNoRemoteWork(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.prepare(t)
	fixture.authorizeUpload()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := executeArchive(ctx, fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil || fixture.FactoryCalls != 0 {
		t.Fatal("canceled owner reached the transport")
	}
}

// Header mutations retain a valid outer digest so tar validation is exercised.
func TestArchiveRejectsDuplicateTraversalLinkedAndChangedMembers(t *testing.T) {
	for _, variant := range []string{"duplicate", "traversal", "link", "payload", "mode", "owner", "timestamp"} {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		content, err := os.ReadFile(fixture.Plan.ArchivePath)
		if err != nil {
			t.Fatal(err)
		}
		reader := tar.NewReader(bytes.NewReader(content))
		var buffer bytes.Buffer
		writer := tar.NewWriter(&buffer)
		index := 0
		firstName := ""
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			copyHeader := *header
			if index == 0 {
				firstName = header.Name
			}
			if variant == "duplicate" && index == 1 {
				copyHeader.Name = firstName
			}
			if variant == "traversal" && index == 0 {
				copyHeader.Name = "../synthetic-escape"
			}
			if variant == "link" && index == 0 {
				copyHeader.Typeflag, copyHeader.Linkname = tar.TypeSymlink, "synthetic-target"
			}
			if variant == "payload" && len(payload) > 0 && index == 1 {
				payload[0] ^= 1
			}
			if variant == "mode" && index == 0 {
				copyHeader.Mode = 04700
			}
			if variant == "owner" && index == 0 {
				copyHeader.Uid = 12345
			}
			if variant == "timestamp" && index == 0 {
				copyHeader.ModTime = time.Unix(1, 0)
			}
			if err := writer.WriteHeader(&copyHeader); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(payload); err != nil {
				t.Fatal(err)
			}
			index++
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		fixture.Plan.ArchiveSha256 = writeSynthetic(t, fixture.Plan.ArchivePath, buffer.Bytes()).Sha256
		fixture.Plan.Key = objectPrefix(fixture.Plan, fixture.Policy) + fixture.Plan.ArchiveSha256 + ".tar"
		fixture.rewritePlan(t)
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
			t.Fatalf("synthetic tar mutation admitted: %s", variant)
		}
	}
}
