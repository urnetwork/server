//go:build linux

// Native-boundary regressions use private fake files only. The production
// constructor never accepts their paths or their non-kernel filesystem.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fixture exposes every relevant leaf explicitly, with one fixed engine child.
func cacheNativeFixture(t *testing.T) (*nativeCacheKernel, string) {
	t.Helper()
	settings := defaultCacheSettings()
	parent := t.TempDir()
	enginePath := filepath.Join(parent, cacheEngineName)
	if err := os.Mkdir(enginePath, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := cacheStarvedSnapshot()
	for directory, values := range map[string]map[string]string{
		parent: {"memory.max": strconv.FormatInt(snapshot.PrivateMaximumBytes, 10), "memory.current": strconv.FormatInt(snapshot.PrivateCurrentBytes, 10),
			"memory.swap.max": "0", "cpuset.cpus.effective": "2,4", "cgroup.type": "domain"},
		enginePath: {"memory.swap.current": "0", "cpuset.cpus.effective": "2,4", "cgroup.type": "domain", "memory.reclaim": "",
			"memory.stat": fmt.Sprintf("file %d\ninactive_file %d\nshmem 0\nfile_dirty 0\nfile_writeback 0", snapshot.EngineFileBytes, snapshot.InactiveFileBytes)},
	} {
		for name, value := range values {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	parentFile, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	engineFile, err := os.Open(enginePath)
	if err != nil {
		parentFile.Close()
		t.Fatal(err)
	}
	kernel := &nativeCacheKernel{settings: settings, parent: parentFile, engine: engineFile, parentPath: parent}
	t.Cleanup(func() { _ = kernel.Close() })
	return kernel, enginePath
}

// Real issue follows both the protected policy and a fresh positive file-cache read.
func TestCacheNativeRechecksOnlyFixedEngineBeforeBoundedIssue(t *testing.T) {
	kernel, enginePath := cacheNativeFixture(t)
	if _, err := kernel.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}
	issued, err := kernel.Reclaim(t.Context(), kernel.settings.MaximumReclaimBytes)
	content, readErr := os.ReadFile(filepath.Join(enginePath, "memory.reclaim"))
	if err != nil || readErr != nil || !issued || strings.TrimSpace(string(content)) != strconv.FormatInt(kernel.settings.MaximumReclaimBytes, 10) {
		t.Fatal("positive bounded request did not target its exact held engine", issued, err, readErr)
	}
	native := *kernel
	native.requireNative = true
	if _, err := native.Observe(t.Context()); err == nil {
		t.Fatal("private ordinary files became native cgroup authority")
	}
}

// Cache can disappear after the initial decision and before the durable debit/issue.
func TestCacheNativeRefusesChangedInactiveCacheAndShmemBeforeIssue(t *testing.T) {
	for _, stats := range []string{
		"file 1073741824\ninactive_file 1\nshmem 0\nfile_dirty 0\nfile_writeback 0\n",
		"file 1073741824\ninactive_file 1073741824\nshmem 1\nfile_dirty 0\nfile_writeback 0\n",
	} {
		kernel, enginePath := cacheNativeFixture(t)
		if _, err := kernel.Observe(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(enginePath, "memory.stat"), []byte(stats), 0600); err != nil {
			t.Fatal(err)
		}
		issued, err := kernel.Reclaim(t.Context(), kernel.settings.MaximumReclaimBytes)
		content, readErr := os.ReadFile(filepath.Join(enginePath, "memory.reclaim"))
		if err == nil || issued || readErr != nil || len(strings.TrimSpace(string(content))) != 0 {
			t.Fatal("stale file-cache proof authorized mutation", issued, err, readErr)
		}
	}
}

// No descendant census may accidentally include active evaluation ownership.
func TestCacheNativeRefusesEvaluationDescendantsAndPopulatedControl(t *testing.T) {
	for _, child := range []string{"synthetic-evaluation.service", ".control"} {
		kernel, enginePath := cacheNativeFixture(t)
		path := filepath.Join(enginePath, child)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 1\nfrozen 0\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if issued, err := kernel.Reclaim(t.Context(), kernel.settings.MaximumReclaimBytes); err == nil || issued {
			t.Fatal("active descendant was included in cache reclaim", child, issued, err)
		}
	}
}

// Identically named replacement directories cannot inherit old held authority.
func TestCacheNativeRefusesChangedGenerationAndAliasedReclaimLeaf(t *testing.T) {
	kernel, enginePath := cacheNativeFixture(t)
	if err := os.Rename(enginePath, enginePath+"-retired"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(enginePath, 0700); err != nil {
		t.Fatal(err)
	}
	if issued, err := kernel.Reclaim(t.Context(), kernel.settings.MaximumReclaimBytes); err == nil || issued {
		t.Fatal("replacement engine generation borrowed old authority", issued, err)
	}
	other, otherEngine := cacheNativeFixture(t)
	path := filepath.Join(otherEngine, "memory.reclaim")
	outside := filepath.Join(t.TempDir(), "synthetic-unrelated-control")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if issued, err := other.Reclaim(t.Context(), other.settings.MaximumReclaimBytes); err == nil || issued {
		t.Fatal("aliased control file expanded reclaim authority", issued, err)
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "untouched" {
		t.Fatal("unrelated file was mutated", err)
	}
}

// An already recovered observation avoids even a previously debited request.
func TestCacheNativeSkipsNaturalRecoveryAndCanceledOwner(t *testing.T) {
	kernel, enginePath := cacheNativeFixture(t)
	current := cacheStarvedSnapshot().PrivateMaximumBytes - kernel.settings.TargetHeadroomBytes
	if err := os.WriteFile(filepath.Join(kernel.parentPath, "memory.current"), []byte(strconv.FormatInt(current, 10)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if issued, err := kernel.Reclaim(t.Context(), kernel.settings.MaximumReclaimBytes); err != nil || issued {
		t.Fatal("natural headroom recovery caused unnecessary reclaim", issued, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if issued, err := kernel.Reclaim(ctx, kernel.settings.MaximumReclaimBytes); err == nil || issued {
		t.Fatal("canceled owner issued kernel mutation", issued, err)
	}
	content, err := os.ReadFile(filepath.Join(enginePath, "memory.reclaim"))
	if err != nil || len(strings.TrimSpace(string(content))) != 0 {
		t.Fatal("unneeded request changed a control file", err)
	}
}

// Malformed counters and unbounded tree examination cannot supply a native proof.
func TestCacheNativeBoundsCountersAndExaminedCgroupEntries(t *testing.T) {
	for _, stats := range []string{
		"file 1\ninactive_file 1\nshmem 0\nfile_dirty 0\nfile_writeback 0\nfile 1\n",
		"file 1\ninactive_file 01\nshmem 0\nfile_dirty 0\nfile_writeback 0\n",
		"file 1\ninactive_file 1\nshmem 0\nfile_dirty 0\n",
	} {
		if _, err := cacheMemoryCounters([]byte(stats), defaultCacheSettings().MaximumStatKeys); err == nil {
			t.Fatal("invalid kernel counter map was accepted")
		}
	}
	kernel, _ := cacheNativeFixture(t)
	kernel.settings.MaximumCgroupEntries = 1
	if _, err := kernel.Observe(t.Context()); err == nil {
		t.Fatal("cgroup census examined an unbounded number of entries")
	}
}

// A pre-issue debit survives reconstruction from the actual persisted bytes.
func TestCacheJournalPersistsDebitAndRejectsHardlinkedState(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	journal := &nativeCacheJournal{directory: directory, settings: defaultCacheSettings()}
	state := cacheState{Schema: 1, Active: true, Attempts: 1, RequestedBytes: journal.settings.MaximumReclaimBytes}
	if err := journal.Save(state); err != nil {
		t.Fatal(err)
	}
	restarted := &nativeCacheJournal{directory: directory, settings: journal.settings}
	actual, err := restarted.Load()
	if err != nil || actual != state {
		t.Fatal("owner reconstruction lost its pre-issue debit", actual, err)
	}
	if err := os.Link(filepath.Join(directory.Name(), "state.json"), filepath.Join(directory.Name(), "synthetic-ambiguous-state")); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Load(); err == nil {
		t.Fatal("hardlinked state supplied maintenance authority")
	}
}

// An interrupted pre-rename commit must not become unbounded orphan creation
// or permission to repeat a request whose durable debit remains uncertain.
func TestCacheJournalRetainsOneInterruptedCommitWithoutRetryingIssue(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	unknown := []byte("synthetic interrupted pre-rename commit")
	if err := os.WriteFile(filepath.Join(root, "state.pending"), unknown, 0600); err != nil {
		t.Fatal(err)
	}
	journal := &nativeCacheJournal{directory: directory, settings: defaultCacheSettings()}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	for range journal.settings.MaximumEpisodeAttempts {
		if _, err := maintainCache(t.Context(), journal.settings, kernel, journal); err == nil || len(kernel.requests) != 0 {
			t.Fatal("unknown journal commit authorized repeated native issue", err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.pending" {
		t.Fatal("interrupted commits accumulated or erased custody", entries, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "state.pending"))
	if err != nil || string(content) != string(unknown) {
		t.Fatal("uncertain journal bytes were overwritten", err)
	}
}
