// A deterministic in-memory transport exercises retained-write boundaries only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fake owns synthetic payload bytes and has no native-client dependency.
type fakeArchiveTransport struct {
	Policy        archivePolicy
	Protection    *archiveProtection
	Retention     *archiveRetention
	CheckError    error
	ListError     error
	PutError      error
	GetError      error
	CloseError    error
	Occupied      bool
	Readback      []byte
	OverrideBytes bool
	BeforeList    func()
	Payload       []byte
	PutCalls      int
	GetCalls      int
	GetKey        string
	GetVersionId  string
}

// Only synthetic fixed protection facts are returned, unless the test mutates one.
func (self *fakeArchiveTransport) CheckProtection(ctx context.Context) (archiveProtection, error) {
	if ctx.Err() != nil {
		return archiveProtection{}, ctx.Err()
	}
	if self.CheckError != nil {
		return archiveProtection{}, self.CheckError
	}
	if self.Protection != nil {
		return *self.Protection, nil
	}
	return archiveProtection{Authority: "storage.archive.example", Bucket: self.Policy.Bucket,
		ObjectLock: "COMPLIANCE", Versioning: true,
		ReplicationTargets: []string{"replica.archive.example/synthetic-evidence"}}, nil
}

// An occupied prefix includes any version or delete marker, not only current data.
func (self *fakeArchiveTransport) ListVersions(ctx context.Context, prefix string, visit func() error) error {
	if self.BeforeList != nil {
		self.BeforeList()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !strings.HasPrefix(prefix, self.Policy.KeyPrefix+"/") {
		return errors.New("unexpected synthetic prefix")
	}
	if self.ListError != nil {
		return self.ListError
	}
	if self.Occupied {
		return visit()
	}
	return nil
}

// A failed call can still retain bytes, modeling an ambiguous remote write.
func (self *fakeArchiveTransport) PutRetained(ctx context.Context, key, localPath string, until time.Time) (archiveRetention, error) {
	self.PutCalls++
	if ctx.Err() != nil {
		return archiveRetention{}, ctx.Err()
	}
	payload, err := os.ReadFile(localPath)
	if err != nil {
		return archiveRetention{}, err
	}
	self.Payload = payload
	if self.PutError != nil {
		return archiveRetention{}, self.PutError
	}
	if self.Retention != nil {
		return *self.Retention, nil
	}
	return archiveRetention{Key: key, Size: int64(len(payload)), VersionId: "synthetic-version-a",
		Mode: "COMPLIANCE", RetainUntil: until}, nil
}

// Exact key/version arguments are observed separately from returned content.
func (self *fakeArchiveTransport) GetVersion(ctx context.Context, key, versionId string) (io.ReadCloser, error) {
	self.GetCalls++
	self.GetKey, self.GetVersionId = key, versionId
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if self.GetError != nil {
		return nil, self.GetError
	}
	content := self.Payload
	if self.OverrideBytes {
		content = self.Readback
	}
	return &fakeArchiveReader{Reader: bytes.NewReader(content), CloseError: self.CloseError}, nil
}

// Close errors are part of readback failure rather than silently discarded.
type fakeArchiveReader struct {
	Reader     *bytes.Reader
	CloseError error
}

// Read directly delegates to deterministic bytes without background work.
func (self *fakeArchiveReader) Read(content []byte) (int, error) { return self.Reader.Read(content) }

// The fixture can force the final readback close boundary to fail.
func (self *fakeArchiveReader) Close() error { return self.CloseError }

// A complete receipt proves the exact returned version, not replicas or acceptance.
func TestRetainedUploadReadsExactVersionAndNeverClaimsBroaderAcceptance(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.prepare(t)
	fixture.authorizeUpload()
	report, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks)
	if err != nil || report.Mode != "retained-source-version-verified" || report.AcceptanceClaimed ||
		fixture.FactoryCalls != 1 || fixture.Store.PutCalls != 1 || fixture.Store.GetCalls != 1 ||
		fixture.Store.GetKey != fixture.Plan.Key || fixture.Store.GetVersionId != "synthetic-version-a" {
		t.Fatal("one-use exact-version upload scope changed", err)
	}
	content, err := os.ReadFile(report.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(content, &receipt); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sdk_http_retry_count_claimed", "atomic_retained_if_absent_claimed",
		"replica_delivery_readback_verified", "offsite_backup_claimed", "calibration_or_launch_acceptance_claimed"} {
		if receipt[field] != false {
			t.Fatalf("receipt overclaimed %s", field)
		}
	}
	if receipt["exact_version_sha256_readback_equal"] != true || receipt["operator_put_retained_calls"] != float64(1) {
		t.Fatal("receipt omitted its bounded exact-version proof")
	}
	for _, name := range []string{"upload.started.json", "protection.private.json", "retention.private.json", "receipt.private.json"} {
		info, err := os.Lstat(filepath.Join(filepath.Dir(report.Plan.Path), name))
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatal("upload evidence not private and durable")
		}
	}
	if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
		fixture.FactoryCalls != 1 || fixture.Store.PutCalls != 1 {
		t.Fatal("completed upload silently reran")
	}
}

// Selecting upload alone cannot substitute for authorization of its exact bytes.
func TestUploadRequiresSeparateExactPlanAuthorization(t *testing.T) {
	for _, authorization := range []string{"", strings.Repeat("a", 64)} {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.Request.Upload = true
		fixture.Request.UploadAuthorizationSha256 = authorization
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.FactoryCalls != 0 {
			t.Fatal("missing or mismatched authorization reached transport")
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(fixture.Request.PlanPin.Path), "upload.started.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unauthorized request spent a namespace")
		}
	}
}

// Every protection fact and version-history uncertainty refuses before a write.
func TestProtectionAndOccupiedHistoryRefuseWithPermanentClaim(t *testing.T) {
	mutations := []func(*fakeArchiveTransport){
		func(store *fakeArchiveTransport) { store.CheckError = errors.New("synthetic check failure") },
		func(store *fakeArchiveTransport) { store.Protection.Authority = "" },
		func(store *fakeArchiveTransport) { store.Protection.Bucket = "synthetic-other-bucket" },
		func(store *fakeArchiveTransport) { store.Protection.Versioning = false },
		func(store *fakeArchiveTransport) { store.Protection.ObjectLock = "GOVERNANCE" },
		func(store *fakeArchiveTransport) { store.Protection.ReplicationTargets = nil },
		func(store *fakeArchiveTransport) { store.Occupied = true },
		func(store *fakeArchiveTransport) { store.ListError = errors.New("synthetic history unavailable") },
	}
	for index, mutate := range mutations {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.authorizeUpload()
		protection, err := fixture.Store.CheckProtection(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fixture.Store.Protection = &protection
		mutate(fixture.Store)
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.Store.PutCalls != 0 {
			t.Fatalf("unsafe synthetic protection admitted at case %d", index)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(fixture.Request.PlanPin.Path), "upload.started.json")); err != nil {
			t.Fatal("failed protection did not retain its claim")
		}
		fixture.Store.Protection = nil
		fixture.Store.CheckError, fixture.Store.ListError, fixture.Store.Occupied = nil, nil, false
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.FactoryCalls != 1 || fixture.Store.PutCalls != 0 {
			t.Fatal("protection failure automatically retried")
		}
	}
}

// A server response may not shorten, lengthen, weaken, or rename the pinned version.
func TestReturnedRetentionMustExactlyMatchPinnedPlan(t *testing.T) {
	mutations := []func(*archiveRetention){
		func(retention *archiveRetention) { retention.Key += ".other" },
		func(retention *archiveRetention) { retention.Size-- },
		func(retention *archiveRetention) { retention.VersionId = "" },
		func(retention *archiveRetention) { retention.VersionId = "null" },
		func(retention *archiveRetention) { retention.VersionId = "synthetic\nversion" },
		func(retention *archiveRetention) { retention.Mode = "GOVERNANCE" },
		func(retention *archiveRetention) { retention.RetainUntil = retention.RetainUntil.Add(-time.Second) },
		func(retention *archiveRetention) { retention.RetainUntil = retention.RetainUntil.Add(time.Second) },
	}
	for index, mutate := range mutations {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.authorizeUpload()
		retention := archiveRetention{Key: fixture.Plan.Key, Size: fixture.Plan.ArchiveBytes,
			VersionId: "synthetic-version-a", Mode: "COMPLIANCE", RetainUntil: fixture.Plan.RetainUntil}
		mutate(&retention)
		fixture.Store.Retention = &retention
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.Store.PutCalls != 1 || fixture.Store.GetCalls != 0 {
			t.Fatalf("changed retention admitted at case %d", index)
		}
		directory := filepath.Dir(fixture.Request.PlanPin.Path)
		if _, err := os.Lstat(filepath.Join(directory, "retention.private.json")); err != nil {
			t.Fatal("malformed response was discarded")
		}
		if _, err := os.Lstat(filepath.Join(directory, "receipt.private.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("malformed retention acquired a success receipt")
		}
	}
}

// A write or source-version readback failure spends its namespace without retry.
func TestAmbiguousWriteAndReadbackFailuresRemainSpent(t *testing.T) {
	for _, variant := range []string{"put", "get", "changed-bytes", "extra-bytes", "close", "after-readback"} {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.authorizeUpload()
		switch variant {
		case "put":
			fixture.Store.PutError = errors.New("synthetic ambiguous result")
		case "get":
			fixture.Store.GetError = errors.New("synthetic read unavailable")
		case "changed-bytes":
			fixture.Store.OverrideBytes, fixture.Store.Readback = true, []byte("synthetic substituted content")
		case "extra-bytes":
			content, err := os.ReadFile(fixture.Plan.ArchivePath)
			if err != nil {
				t.Fatal(err)
			}
			fixture.Store.OverrideBytes, fixture.Store.Readback = true, append(content, 0)
		case "close":
			fixture.Store.CloseError = errors.New("synthetic close failure")
		case "after-readback":
			fixture.Hooks.AfterStep = func(step string) error {
				if step == "source-version-read-back" {
					return errors.New("synthetic receipt interruption")
				}
				return nil
			}
		}
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.Store.PutCalls != 1 {
			t.Fatalf("synthetic failure did not hold: %s", variant)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(fixture.Request.PlanPin.Path), "receipt.private.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("incomplete remote proof acquired a success receipt")
		}
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.Store.PutCalls != 1 || fixture.FactoryCalls != 1 {
			t.Fatal("ambiguous or partial attempt reran")
		}
	}
}

// Equal-length corruption must fail the digest check, not merely the byte count.
func TestSameLengthReadbackCorruptionRemainsSpent(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.prepare(t)
	fixture.authorizeUpload()
	content, err := os.ReadFile(fixture.Plan.ArchivePath)
	if err != nil || len(content) == 0 || int64(len(content)) != fixture.Plan.ArchiveBytes {
		t.Fatal("synthetic archive unavailable", err)
	}
	content[len(content)/2] ^= 1
	fixture.Store.OverrideBytes, fixture.Store.Readback = true, content
	_, err = executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks)
	if err == nil || !strings.Contains(err.Error(), "content readback mismatch") ||
		fixture.Store.PutCalls != 1 || fixture.Store.GetCalls != 1 || fixture.FactoryCalls != 1 {
		t.Fatal("equal-length corruption did not fail exact-version content verification", err)
	}
	directory := filepath.Dir(fixture.Request.PlanPin.Path)
	if _, err := os.Lstat(filepath.Join(directory, "receipt.private.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupted readback acquired a success receipt")
	}
	if _, err := os.Lstat(filepath.Join(directory, "upload.started.json")); err != nil {
		t.Fatal("corrupted readback lost its spent claim", err)
	}
	if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
		fixture.Store.PutCalls != 1 || fixture.Store.GetCalls != 1 || fixture.FactoryCalls != 1 {
		t.Fatal("corrupted readback was automatically retried", err)
	}
}

// History checks cannot hide a changed source, metadata pin, or local archive.
func TestSourceAndPinnedMetadataRecheckedImmediatelyBeforeWrite(t *testing.T) {
	for _, variant := range []string{"source", "plan", "inventory", "archive"} {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.authorizeUpload()
		fixture.Store.BeforeList = func() {
			switch variant {
			case "source":
				writeSynthetic(t, filepath.Join(fixture.Preparation.SourceRoot, "new-late-evidence"), []byte("synthetic late evidence"))
			case "plan":
				writeSynthetic(t, fixture.Request.PlanPin.Path, []byte("{}"))
			case "inventory":
				writeSynthetic(t, fixture.Plan.Inventory.Path, []byte("{}"))
			case "archive":
				writeSynthetic(t, fixture.Plan.ArchivePath, []byte("synthetic replacement"))
			}
		}
		if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
			fixture.Store.PutCalls != 0 {
			t.Fatalf("late synthetic change reached retained write: %s", variant)
		}
	}
}

// The durable claim precedes even native transport construction.
func TestInterruptedUploadClaimCannotReachOrRetryTransport(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.prepare(t)
	fixture.authorizeUpload()
	fixture.Hooks.AfterStep = func(step string) error {
		if step == "upload-claimed" {
			return errors.New("synthetic interruption")
		}
		return nil
	}
	if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
		fixture.FactoryCalls != 0 {
		t.Fatal("claim interruption reached transport")
	}
	fixture.Hooks.AfterStep = func(string) error { return nil }
	if _, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil ||
		fixture.FactoryCalls != 0 {
		t.Fatal("interrupted one-use claim retried")
	}
}

// Cancellation remains owned through claim, retained write, and receipt completion.
func TestCancellationAtUploadBoundariesCannotYieldAReceipt(t *testing.T) {
	for _, boundary := range []string{"upload-claimed", "before-write", "source-version-read-back"} {
		fixture := newArchiveFixture(t)
		fixture.prepare(t)
		fixture.authorizeUpload()
		ctx, cancel := context.WithCancel(t.Context())
		fixture.Hooks.AfterStep = func(step string) error {
			if step == boundary {
				cancel()
			}
			return nil
		}
		if boundary == "before-write" {
			fixture.Store.BeforeList = cancel
		}
		_, err := executeArchive(ctx, fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks)
		cancel()
		if err == nil {
			t.Fatalf("canceled synthetic boundary admitted: %s", boundary)
		}
		directory := filepath.Dir(fixture.Request.PlanPin.Path)
		if _, err := os.Lstat(filepath.Join(directory, "receipt.private.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("canceled owner acquired a completed receipt")
		}
		if _, err := os.Lstat(filepath.Join(directory, "upload.started.json")); err != nil {
			t.Fatal("canceled owner removed its permanent claim")
		}
		if boundary == "upload-claimed" && fixture.FactoryCalls != 0 {
			t.Fatal("canceled claimed owner constructed a client")
		}
		if boundary != "source-version-read-back" && fixture.Store.PutCalls != 0 {
			t.Fatal("canceled pre-write owner issued a retained write")
		}
	}
}

// A deterministic barrier proves the canonical slot excludes a concurrent owner.
func TestConcurrentArchiveOwnerRefusesBeforeScanningOrTransport(t *testing.T) {
	fixture := newArchiveFixture(t)
	fixture.prepare(t)
	fixture.authorizeUpload()
	claimed, release := make(chan struct{}), make(chan struct{})
	fixture.Hooks.AfterStep = func(step string) error {
		if step == "upload-claimed" {
			close(claimed)
			select {
			case <-release:
			case <-t.Context().Done():
				return t.Context().Err()
			}
		}
		return nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, fixture.Hooks)
		finished <- err
	}()
	select {
	case <-claimed:
	case err := <-finished:
		t.Fatal("first synthetic owner failed before barrier", err)
	}
	secondHooks := fixture.Hooks
	secondHooks.AvailableBytes = func(string) (int64, error) {
		t.Error("concurrent owner reached evidence/disk observation")
		return 0, nil
	}
	secondHooks.NewTransport = func(archivePolicy) (archiveTransport, error) {
		t.Error("concurrent owner constructed a client")
		return nil, errors.New("synthetic forbidden client")
	}
	_, secondErr := executeArchive(t.Context(), fixture.Request, nil, fixture.Policy, fixture.Settings, secondHooks)
	close(release)
	firstErr := <-finished
	if secondErr == nil || firstErr != nil || fixture.Store.PutCalls != 1 {
		t.Fatal("slot ownership did not isolate the two owners", firstErr, secondErr)
	}
}
