// `sidecarctl compute policy tickets sign|release` on software test keys and a fake clock (ATTEST-M12-02; §5.4's
// freshness tickets): a sitting's batch written whole into the store, a revocation's starting at the sitting; and the
// ticket host's release, which publishes only what has started, deletes every older batch's unreleased tickets once a
// newer sitting's batch arrives and keeps none it published, lets a revocation's batch bind at once, holds a ticket
// whose policy the tree does not serve yet, and never replaces a newer batch's released ticket with an older one's.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// The names of the files in directory, sorted; none where it does not exist.
func testComputePolicyNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return []string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// The store's batch directories, which a run left: a batch's id each.
func (self *testComputePolicyWorld) storeBatches(t *testing.T) []string {
	t.Helper()
	return testComputePolicyNames(t, self.store)
}

// The tree's released tickets' names.
func (self *testComputePolicyWorld) released(t *testing.T) []string {
	t.Helper()
	return testComputePolicyNames(t, filepath.Join(self.tree, sidecar.ComputePolicyTicketDirectory))
}

// The day file names from first for count days.
func testComputePolicyDays(first time.Time, count int) []string {
	names := []string{}
	for i := range count {
		names = append(names, sidecar.ComputePolicyTicketDayFileName(first.AddDate(0, 0, i)))
	}
	return names
}

// Signs a batch naming version at the clock's now, and answers its directory in the store.
func (self *testComputePolicyWorld) signBatch(t *testing.T, version string, flags ...string) string {
	t.Helper()
	before := self.storeBatches(t)
	self.must(t, "tickets sign", append([]string{"--store=" + self.store, "--key=" + self.policyCertificate, "--policy=" + version}, flags...)...)
	for _, name := range self.storeBatches(t) {
		if !slices.Contains(before, name) {
			return filepath.Join(self.store, name)
		}
	}
	t.Fatal("tickets sign wrote no batch")
	return ""
}

// 00:00 UTC of December day, 2026.
func testComputePolicyDecember(day int) time.Time {
	return time.Date(2026, time.December, day, 0, 0, 0, 0, time.UTC)
}

// A sitting's batch is written whole into the store: 14 tickets named for their days, from the day after the sitting,
// each verifying as a device verifies it, and no temporary directory left; a revocation's holds 15, the first for
// the sitting's own day. A batch past 14 days, one with no version, and one whose certificate ends first are refused.
func TestComputePolicyTicketsSignWritesTheBatchWhole(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	batch := world.signBatch(t, "1")
	if names := testComputePolicyNames(t, batch); !slices.Equal(names, testComputePolicyDays(testComputePolicyDecember(2), 14)) {
		t.Errorf("the batch's files %q", names)
	}
	builder := newComputePolicyCommandBuilder(world.settings, false)
	tickets, refusals, err := readComputePolicyTicketFiles(builder, batch, world.settings)
	if err != nil || len(refusals) != 0 || len(tickets) != 14 {
		t.Fatalf("%d tickets, %q, %v", len(tickets), refusals, err)
	}
	for _, entry := range tickets {
		if filepath.Base(entry.path) != sidecar.ComputePolicyTicketDayFileName(entry.ticket.NotBefore) || entry.ticket.PolicyVersion != 1 ||
			!entry.ticket.Batch.SignedAt.Equal(testComputePolicySitting) {
			t.Errorf("%s: %+v", entry.path, entry.ticket)
		}
	}
	revocation := world.signBatch(t, "2", "--revocation")
	names := testComputePolicyNames(t, revocation)
	if len(names) != 15 || names[0] != "2026-12-01.bin" || names[14] != "2026-12-15.bin" {
		t.Errorf("the revocation's files %q", names)
	}
	for _, name := range world.storeBatches(t) {
		if strings.HasPrefix(name, ".") {
			t.Errorf("a temporary directory was left: %s", name)
		}
	}
	late := testComputePolicySitting.Add(15 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := world.run("tickets sign", "--store="+world.store, "--key="+world.policyCertificate, "--policy=1", "--expires="+late); err == nil {
		t.Error("a batch past 14 days")
	}
	if _, err := world.run("tickets sign", "--store="+world.store, "--key="+world.policyCertificate); err == nil {
		t.Error("a batch naming no version")
	}
	_, shortKey := testComputePolicyKey(t, "short")
	short := world.writeCertificate(t, "short", testComputePolicyCertify(t, shortKey, world.releaseKey, sidecar.ComputePolicyNamespace,
		testComputePolicySitting.Add(-24*time.Hour), testComputePolicySitting.Add(10*24*time.Hour)))
	if _, err := world.run("tickets sign", "--store="+world.store, "--key="+short, "--policy=1"); err == nil || !strings.Contains(err.Error(), "before") {
		t.Errorf("a certificate ending before the batch: %v", err)
	}
	if len(world.storeBatches(t)) != 2 {
		t.Errorf("a refused batch was written: %q", world.storeBatches(t))
	}
}

// Release publishes a ticket only once its not_before has come: nothing the instant before the first ticket starts,
// then that ticket alone, as tickets/<day>.bin, deleted from the store once published.
func TestTicketReleasePublishesOnlyStarted(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	batch := world.signBatch(t, "1")
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	world.clock.AdvanceTo(testComputePolicyDecember(2).Add(-time.Millisecond))
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	if released := world.released(t); len(released) != 0 {
		t.Fatalf("released before any start: %q", released)
	}
	world.clock.AdvanceTo(testComputePolicyDecember(2))
	out := world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	if released := world.released(t); !slices.Equal(released, []string{"2026-12-02.bin"}) {
		t.Fatalf("released %q\n%s", released, out)
	}
	if names := testComputePolicyNames(t, batch); len(names) != 13 || slices.Contains(names, "2026-12-02.bin") {
		t.Errorf("the store after the release: %q", names)
	}
	builder := newComputePolicyCommandBuilder(world.settings, false)
	ticket, err := builder.VerifyTicket(testComputePolicyRead(t, filepath.Join(world.tree, "tickets", "2026-12-02.bin")), nil)
	if err != nil || !ticket.NotBefore.Equal(testComputePolicyDecember(2)) {
		t.Fatalf("the released ticket: %v", err)
	}
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	if released := world.released(t); len(released) != 1 {
		t.Errorf("a second run released %q", released)
	}
}

// Once a newer sitting's batch is in the store, release deletes every older batch's unreleased tickets, so a breach of
// the host holds nothing of the batch it superseded, and releases from the newest alone; the tickets the older batch
// released stay served.
func TestTicketReleaseDeletesOlderUnreleased(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	first := world.signBatch(t, "1")
	for day := 2; day <= 8; day += 1 {
		world.clock.AdvanceTo(testComputePolicyDecember(day))
		world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	}
	if released := world.released(t); !slices.Equal(released, testComputePolicyDays(testComputePolicyDecember(2), 7)) {
		t.Fatalf("a week of releases: %q", released)
	}
	if names := testComputePolicyNames(t, first); len(names) != 7 {
		t.Fatalf("the first batch's unreleased: %q", names)
	}
	world.clock.AdvanceTo(testComputePolicyDecember(8).Add(10 * time.Hour))
	second := world.signBatch(t, "1")
	world.clock.AdvanceTo(testComputePolicyDecember(8).Add(10*time.Hour + time.Minute))
	out := world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	if !strings.Contains(out, "deleted 7 unreleased tickets of batch "+filepath.Base(first)) {
		t.Errorf("the release printed:\n%s", out)
	}
	if testComputePolicyExists(first) {
		t.Errorf("the superseded batch's tickets are still held: %q", testComputePolicyNames(t, first))
	}
	if names := testComputePolicyNames(t, second); !slices.Equal(names, testComputePolicyDays(testComputePolicyDecember(9), 14)) {
		t.Errorf("the newest batch: %q", names)
	}
	if released := world.released(t); !slices.Equal(released, testComputePolicyDays(testComputePolicyDecember(2), 7)) {
		t.Errorf("the released tickets moved: %q", released)
	}
	world.clock.AdvanceTo(testComputePolicyDecember(9))
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	builder := newComputePolicyCommandBuilder(world.settings, false)
	ticket, err := builder.VerifyTicket(testComputePolicyRead(t, filepath.Join(world.tree, "tickets", "2026-12-09.bin")), nil)
	if err != nil || ticket.Batch.Id.String() != filepath.Base(second) {
		t.Errorf("December 9's ticket is of batch %s: %v", ticket.Batch.Id, err)
	}
}

// A revocation binds at once: its new policy published and its batch in the store, release deletes the old batch's
// unreleased tickets and publishes the revocation's first ticket at its sitting, in place of the day's old one.
func TestTicketReleaseRevocationBindsAtOnce(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	world.signBatch(t, "1")
	world.clock.AdvanceTo(testComputePolicyDecember(3))
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	revokedAt := testComputePolicyDecember(3).Add(15 * time.Hour)
	world.clock.AdvanceTo(revokedAt)
	second := filepath.Join(world.directory, "work-2.json")
	world.must(t, "build", second, "--previous="+filepath.Join(world.tree, "1.json"), "--changes="+world.writeChanges(t, "revoke.json",
		`{"revoke": {"profiles": ["teetest-2026.12.1"], "chips": [], "gpu_serials": []}}`))
	world.must(t, "sign", second, "--key="+world.policyCertificate)
	world.must(t, "publish", second, "--tree="+world.tree)
	revocation := world.signBatch(t, "2", "--revocation")
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	if batches := world.storeBatches(t); !slices.Equal(batches, []string{filepath.Base(revocation)}) {
		t.Errorf("the store holds %q", batches)
	}
	builder := newComputePolicyCommandBuilder(world.settings, false)
	ticket, err := builder.VerifyTicket(testComputePolicyRead(t, filepath.Join(world.tree, "tickets", "2026-12-03.bin")), nil)
	if err != nil || ticket.PolicyVersion != 2 || !ticket.NotBefore.Equal(revokedAt) || !ticket.Current(revokedAt, world.settings.build.Attest) {
		t.Errorf("December 3's ticket after the revocation: %+v, %v", ticket, err)
	}
	if released := world.released(t); !slices.Equal(released, []string{"2026-12-02.bin", "2026-12-03.bin"}) {
		t.Errorf("released %q", released)
	}
}

// A ticket naming a policy version the tree does not serve is held back, and release fails saying so, since devices
// that took it would stop remote work until they could fetch that version; once it is published the ticket goes.
func TestTicketReleaseHoldsATicketWhosePolicyIsNotServed(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	batch := world.signBatch(t, "2")
	world.clock.AdvanceTo(testComputePolicyDecember(2))
	out, err := world.run("tickets release", "--store="+world.store, "--tree="+world.tree)
	if err == nil || !strings.Contains(out, "names version 2, which the tree does not serve") || len(world.released(t)) != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	if names := testComputePolicyNames(t, batch); len(names) != 14 {
		t.Errorf("the held ticket left the store: %q", names)
	}
	second := filepath.Join(world.directory, "work-2.json")
	world.must(t, "build", second, "--previous="+filepath.Join(world.tree, "1.json"))
	world.must(t, "sign", second, "--key="+world.policyCertificate)
	world.must(t, "publish", second, "--tree="+world.tree)
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	if released := world.released(t); !slices.Equal(released, []string{"2026-12-02.bin"}) {
		t.Errorf("released %q", released)
	}
}

// A store that holds an older batch alone, as one restored from a backup would, never replaces the newer batch's
// released ticket for the same day.
func TestTicketReleaseNeverReplacesANewerBatchsTicket(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	older := world.signBatch(t, "1")
	kept := testComputePolicyRead(t, filepath.Join(older, "2026-12-02.bin"))
	world.clock.AdvanceTo(testComputePolicySitting.Add(time.Hour))
	newer := world.signBatch(t, "1")
	world.clock.AdvanceTo(testComputePolicyDecember(2))
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	served := testComputePolicyRead(t, filepath.Join(world.tree, "tickets", "2026-12-02.bin"))
	if testComputePolicyExists(older) {
		t.Fatal("the older batch was not deleted")
	}
	if err := os.RemoveAll(newer); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(older, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(older, "2026-12-02.bin"), kept, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := world.run("tickets release", "--store="+world.store, "--tree="+world.tree)
	if err == nil || !strings.Contains(out, "of the newer batch "+filepath.Base(newer)) {
		t.Errorf("%v\n%s", err, out)
	}
	if !bytes.Equal(testComputePolicyRead(t, filepath.Join(world.tree, "tickets", "2026-12-02.bin")), served) {
		t.Error("the newer batch's ticket was replaced")
	}
}
