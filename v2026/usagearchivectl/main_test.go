package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/usagearchive"
)

// A plain directory, such as an encrypted dataset's mount point while the
// dataset is not mounted, is refused.
func TestRequireOwnMountRefusesAPlainDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage-archive")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := requireOwnMount(dir); err == nil {
		t.Fatal("a plain directory passed as a mount point")
	}
}

// The resource needs its user, authority and db; a missing file fails.
func TestConnectNeedsTheResourceFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg.yml")
	if err := os.WriteFile(path, []byte("user: u\npassword: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connect(t.Context(), path); err == nil {
		t.Fatal("a resource without authority and db connected")
	}
	if _, err := connect(t.Context(), filepath.Join(dir, "absent.yml")); err == nil {
		t.Fatal("a missing resource connected")
	}
}

// The export runs under --floor=rolling, as the backup host's unit sets it: no
// date, so it copies every week that ended 42 days before now. A date still
// binds once it is the earlier; a time or anything else is refused, so the
// unit fails rather than copy under a floor nobody set.
func TestExportFloor(t *testing.T) {
	now := time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC)
	floor, effective, err := exportFloor("rolling", now)
	if err != nil || !floor.Equal(usagearchive.RollingCloseTimeFloor) || !effective.Equal(time.Date(2026, 10, 10, 22, 30, 0, 0, time.UTC)) {
		t.Fatalf("rolling: floor %s, in force %s, %v", floor, effective, err)
	}
	// the unit's floor until now, 2026-11-05: 42 days bind it on 11-21, the
	// date from 2026-12-17
	for _, test := range []struct {
		now       time.Time
		effective time.Time
	}{
		{now, time.Date(2026, 10, 10, 22, 30, 0, 0, time.UTC)},
		{time.Date(2026, 12, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
		{time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
	} {
		floor, effective, err := exportFloor("2026-11-05", test.now)
		if err != nil || !floor.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) || !effective.Equal(test.effective) {
			t.Fatalf("2026-11-05 at %s: floor %s, in force %s, %v", test.now, floor, effective, err)
		}
	}
	for _, value := range []string{"", "Rolling", "2026-11-05T00:00:00Z", "11/05/2026"} {
		if _, _, err := exportFloor(value, now); err == nil {
			t.Fatalf("--floor=%q was accepted", value)
		}
	}
}
