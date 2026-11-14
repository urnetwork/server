//go:build sidecar_pending

package model

// FS-120 (§22.8, WK5; §18.7's steps 0, 5 and 6, D-SDK-16; SDK-M8-12): token projection order, row by
// row, through the writers themselves (SetPushToken, TombstonePushTokenIfRevision, the removal's
// TombstonePushTokenInTx) and taskworker's pass (RunPushTokenPass), on every ground the environment
// gives (redis_cluster_harness_test.go): T2's twin cluster over Postgres, and the environment's Redis
// through the real Lua, which is T3's 3-master cluster once server/test-env.sh attests its masters.
//
// The barrier sits after each change's commit and before its apply: the writers' test hook holds a
// writer's try there, so a schedule commits a pair of changes and delivers their applies in reverse commit
// order, and after each apply that lands reads tok against the newest committed row. The clock is the
// ground's: an apply held past 60 s is one whose t0 is over 60 s old when it is let go.
//
//	two POSTs; a POST and an empty POST, each way; a client's removal; a vendor's 410 racing a new POST,
//	each way: tok equals the newest committed row each time, a removed token stays absent with no app
//	launch, and the 410 removes no newer token
//	an apply held past 60 s answers EXPIRED and its retry applies the newest row; with api killed the next
//	pass's sweep does, within 2 min
//	the reconcile pass before, during and after a held apply
//	a master restarted from a dump older than the newest change: nothing reconciles before 90 s from the
//	pass that found it, tok then equals the rows, and no apply begun before the restart lands after it
//	a client whose first POST lands during a reconcile keeps its token
//	a removed client's row and keys are gone 24 h after its removal
//
// FS-41's and FS-42's M8 steps follow: push tokens re-register after a site loss, and after a lost
// Postgres primary every row's revision is raised past what the lost primary applied, a client created
// after the dump re-registering in a row whose revision passes its lost row's, so no later apply is
// refused as stale.
//
// The master's restart runs in the twin, and on a real cluster through the harness; the environment's
// one node is never restarted, and each such row says so where it could not run.

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

// How long a schedule waits on a writer before calling itself broken. Every wait is on an event.
const fs120Timeout = 60 * time.Second

// One try of a writer's apply, held at the barrier.
type fs120HeldApply struct {
	// the row the try carries
	row *SidecarPushTokenRow
	// how far the schedule's clock stood past the master's TIME when the try was held
	agedAtHold time.Duration
	released   chan struct{}
	releaseOne sync.Once
}

// Lets the try go on to its script.
func (self *fs120HeldApply) release() {
	self.releaseOne.Do(func() { close(self.released) })
}

// One writer running on its own goroutine, as an api request does.
type fs120Write struct {
	done chan struct{}
	// a POST's result
	result *SetPushTokenResult
	// whether a vendor's rejection or a removal tombstoned
	tombstoned bool
	err        error
}

// One schedule's world on one ground.
type fs120World struct {
	t      testing.TB
	ctx    context.Context
	ground *testTokenGround
	// undoes the ground's routing of the writers
	restoreWriters func()

	stateLock sync.Mutex
	// how many more tries of each client's writers the barrier holds
	holdCounts map[server.Id]int
	// every try of each client's writers that reached the barrier, held or not
	tryCounts map[server.Id]int
	// every try held, so the world's end lets go of what a failed schedule left
	allHeld []*fs120HeldApply
	held    chan *fs120HeldApply
}

// Runs row on every ground of the environment, each in a world of its own.
func runFs120(t *testing.T, row func(world *fs120World)) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, ground := range testTokenGrounds(t, ctx) {
			func() {
				ground.clearRows()
				world := newFs120World(t, ctx, ground)
				defer world.close()
				row(world)
			}()
		}
	})
}

func newFs120World(t testing.TB, ctx context.Context, ground *testTokenGround) *fs120World {
	self := &fs120World{
		t:              t,
		ctx:            ctx,
		ground:         ground,
		restoreWriters: ground.routeWriters(),
		holdCounts:     map[server.Id]int{},
		tryCounts:      map[server.Id]int{},
		held:           make(chan *fs120HeldApply, 64),
	}
	pushTokenApplyTestHook = func(row *SidecarPushTokenRow, t0 int64) int64 {
		held := func() *fs120HeldApply {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.tryCounts[row.ClientId] += 1
			if self.holdCounts[row.ClientId] <= 0 {
				return nil
			}
			self.holdCounts[row.ClientId] -= 1
			held := &fs120HeldApply{row: row, agedAtHold: ground.aged(), released: make(chan struct{})}
			self.allHeld = append(self.allHeld, held)
			return held
		}()
		if held == nil {
			return t0
		}
		self.held <- held
		<-held.released
		// a real master's TIME did not move with the schedule's clock: the try is as old as the clock
		// moved while it was held
		return t0 - (ground.aged() - held.agedAtHold).Milliseconds()
	}
	return self
}

// Ends the world: every try still held goes on, the hook and the writers' routing are as before.
func (self *fs120World) close() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		clear(self.holdCounts)
		for _, held := range self.allHeld {
			held.release()
		}
	}()
	pushTokenApplyTestHook = nil
	pushTokenReconcileTestHook = nil
	self.restoreWriters()
}

// The barrier holds the next count tries of clientId's writers.
func (self *fs120World) hold(clientId server.Id, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holdCounts[clientId] += count
}

// How many tries of clientId's writers have reached the barrier.
func (self *fs120World) tries(clientId server.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.tryCounts[clientId]
}

// The next try held, which must be clientId's: its change has committed, and its apply has not run.
func (self *fs120World) awaitHeld(clientId server.Id) *fs120HeldApply {
	self.t.Helper()
	select {
	case held := <-self.held:
		if held.row.ClientId != clientId {
			self.t.Fatalf("%s: held a try of %s, not %s", self.ground.name, held.row.ClientId, clientId)
		}
		return held
	case <-time.After(fs120Timeout):
		self.t.Fatalf("%s: no try of %s reached the barrier", self.ground.name, clientId)
		return nil
	}
}

// Runs write on its own goroutine, an error or a panic of it kept for await.
func (self *fs120World) run(write func(result *fs120Write) error) *fs120Write {
	result := &fs120Write{done: make(chan struct{})}
	go func() {
		defer close(result.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				result.err = fmt.Errorf("writer: %v", recovered)
			}
		}()
		result.err = write(result)
	}()
	return result
}

// A POST of token for clientId, or an empty POST for "".
func (self *fs120World) post(clientId server.Id, token string, settings *SidecarPushTokenSettings) *fs120Write {
	return self.run(func(result *fs120Write) error {
		var err error
		result.result, err = SetPushToken(self.ctx, clientId, sidecar.PushTokenPlatformApns, token, settings)
		return err
	})
}

// The push sender's tombstone on a vendor's 410 for the token it read at rev.
func (self *fs120World) reject(clientId server.Id, rev int64) *fs120Write {
	return self.run(func(result *fs120Write) error {
		var err error
		result.tombstoned, err = TombstonePushTokenIfRevision(self.ctx, clientId, rev, nil)
		return err
	})
}

// The removal's part for clientId, in a transaction of its own as the removal's is, its apply after the
// commit.
func (self *fs120World) remove(clientId server.Id) *fs120Write {
	return self.run(func(result *fs120Write) error {
		var returnErr error
		server.Tx(self.ctx, func(tx server.PgTx) {
			tombstoned, err := TombstonePushTokenInTx(self.ctx, tx, []server.Id{clientId})
			result.tombstoned, returnErr = len(tombstoned) == 1, err
		})
		return returnErr
	})
}

// Waits for write to return, failing on its error.
func (self *fs120World) await(write *fs120Write) *fs120Write {
	self.t.Helper()
	select {
	case <-write.done:
	case <-time.After(fs120Timeout):
		self.t.Fatalf("%s: a writer did not return", self.ground.name)
	}
	if write.err != nil {
		self.t.Fatalf("%s: %v", self.ground.name, write.err)
	}
	return write
}

// A POST with nothing held, which must land: applied, or current for one equal to the row.
func (self *fs120World) posted(clientId server.Id, token string) *SidecarPushTokenRow {
	self.t.Helper()
	write := self.await(self.post(clientId, token, nil))
	if applied := write.result.Applied; applied != pushservice.WakeTokenApplied && applied != pushservice.WakeTokenCurrent {
		self.t.Fatalf("%s: a POST's apply answered %q", self.ground.name, applied)
	}
	return self.ground.expectNewest(clientId, "after a POST")
}

// Lets held go and waits for its writer, whose POST's apply must have answered want.
func (self *fs120World) deliver(held *fs120HeldApply, write *fs120Write, want pushservice.WakeTokenApplyResult) {
	self.t.Helper()
	held.release()
	self.await(write)
	if write.result != nil && write.result.Applied != want {
		self.t.Fatalf("%s: the apply of rev %d answered %q, not %q", self.ground.name, held.row.Rev, write.result.Applied, want)
	}
}

// Fails unless tok holds token for clientId, "" for none.
func (self *fs120World) expectToken(clientId server.Id, token string, when string) {
	self.t.Helper()
	want := ""
	if token != "" {
		want = string(pushservice.WakeTokenValue(sidecar.PushTokenPlatformApns, token))
	}
	if tok, _ := self.ground.tokenKeys(clientId); tok != want {
		self.t.Fatalf("%s, %s: tok %q, not %q", self.ground.name, when, tok, want)
	}
}

// Two POSTs, the newer's apply delivered first: tok is the newer token from then on, and the older apply,
// arriving last, is stale and writes nothing.
func TestFs120TwoPostsApplyInReverseCommitOrder(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		clientId := world.ground.newClient(0)
		world.hold(clientId, 2)
		first := world.post(clientId, "first-token", nil)
		firstHeld := world.awaitHeld(clientId)
		second := world.post(clientId, "second-token", nil)
		secondHeld := world.awaitHeld(clientId)
		if secondHeld.row.Rev != firstHeld.row.Rev+1 {
			t.Fatalf("%s: revisions %d then %d", world.ground.name, firstHeld.row.Rev, secondHeld.row.Rev)
		}
		// both committed, neither applied
		if tok, tokrev := world.ground.tokenKeys(clientId); tok != "" || tokrev != "" {
			t.Fatalf("%s: before any apply: %q %q", world.ground.name, tok, tokrev)
		}
		world.deliver(secondHeld, second, pushservice.WakeTokenApplied)
		world.ground.expectNewest(clientId, "after the newer POST's apply")
		world.deliver(firstHeld, first, pushservice.WakeTokenStale)
		world.ground.expectNewest(clientId, "after the older POST's apply")
		world.expectToken(clientId, "second-token", "after both")
	})
}

// A POST and an empty POST, applies reversed, each way round: after a POST then an empty POST the token is
// gone once the tombstone lands and stays gone when the POST's apply arrives, through a pass too, with no
// app launch; after an empty POST then a POST the new token stands.
func TestFs120PostAndEmptyPostApplyInReverseCommitOrder(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		removed := world.ground.newClient(0)
		world.posted(removed, "standing-token")
		world.hold(removed, 2)
		post := world.post(removed, "replaced-token", nil)
		postHeld := world.awaitHeld(removed)
		empty := world.post(removed, "", nil)
		emptyHeld := world.awaitHeld(removed)
		world.expectToken(removed, "standing-token", "before either apply")
		world.deliver(emptyHeld, empty, pushservice.WakeTokenApplied)
		world.ground.expectNewest(removed, "after the empty POST's apply")
		world.deliver(postHeld, post, pushservice.WakeTokenStale)
		world.ground.expectNewest(removed, "after the POST's apply")
		world.expectToken(removed, "", "after both")
		world.ground.advance(60 * time.Second)
		world.ground.pass()
		world.expectToken(removed, "", "after a pass")

		registered := world.ground.newClient(1)
		world.posted(registered, "standing-token")
		world.hold(registered, 2)
		empty = world.post(registered, "", nil)
		emptyHeld = world.awaitHeld(registered)
		post = world.post(registered, "new-token", nil)
		postHeld = world.awaitHeld(registered)
		world.deliver(postHeld, post, pushservice.WakeTokenApplied)
		world.ground.expectNewest(registered, "after the POST's apply")
		world.deliver(emptyHeld, empty, pushservice.WakeTokenStale)
		world.ground.expectNewest(registered, "after the empty POST's apply")
		world.expectToken(registered, "new-token", "after both")
	})
}

// A POST, then the client's removal, applies reversed: the removal's tombstone lands first and the token
// is gone; the POST's apply, arriving after it, is stale, and the token stays absent through a pass, with no
// app launch.
func TestFs120RemovalApplyInReverseCommitOrder(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		clientId := world.ground.newClient(0)
		world.posted(clientId, "standing-token")
		world.hold(clientId, 2)
		post := world.post(clientId, "last-token", nil)
		postHeld := world.awaitHeld(clientId)
		removal := world.remove(clientId)
		removalHeld := world.awaitHeld(clientId)
		if !removalHeld.row.Tombstone() || removalHeld.row.RemovedTime == nil || removalHeld.row.Rev != postHeld.row.Rev+1 {
			t.Fatalf("%s: the removal's row %s", world.ground.name, removalHeld.row)
		}
		world.expectToken(clientId, "standing-token", "before either apply")
		removalHeld.release()
		if !world.await(removal).tombstoned {
			t.Fatalf("%s: the removal tombstoned nothing", world.ground.name)
		}
		world.ground.expectNewest(clientId, "after the removal's apply")
		world.deliver(postHeld, post, pushservice.WakeTokenStale)
		world.ground.expectNewest(clientId, "after the POST's apply")
		world.expectToken(clientId, "", "after both")
		world.ground.advance(60 * time.Second)
		world.ground.pass()
		world.expectToken(clientId, "", "after a pass")
	})
}

// A vendor's 410 racing a new POST, each way. The 410 commits first, at the revision the sender read,
// and the POST after it: delivered in reverse, the POST's token stands and the 410's tombstone, arriving
// last, is stale. The POST commits first: the 410, for the revision read with the old token, changes no
// row and applies nothing, and the new token stands once its apply lands. Either way the 410 removes no
// newer token.
func TestFs120Vendor410RacingANewPost(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		rejectedFirst := world.ground.newClient(0)
		read := world.posted(rejectedFirst, "rejected-token")
		world.hold(rejectedFirst, 2)
		rejection := world.reject(rejectedFirst, read.Rev)
		rejectionHeld := world.awaitHeld(rejectedFirst)
		post := world.post(rejectedFirst, "newer-token", nil)
		postHeld := world.awaitHeld(rejectedFirst)
		if !rejectionHeld.row.Tombstone() || postHeld.row.Rev != rejectionHeld.row.Rev+1 {
			t.Fatalf("%s: the 410's row %s, the POST's %s", world.ground.name, rejectionHeld.row, postHeld.row)
		}
		world.deliver(postHeld, post, pushservice.WakeTokenApplied)
		world.ground.expectNewest(rejectedFirst, "after the POST's apply")
		rejectionHeld.release()
		if !world.await(rejection).tombstoned {
			t.Fatalf("%s: the 410 at the read revision tombstoned nothing", world.ground.name)
		}
		world.ground.expectNewest(rejectedFirst, "after the 410's apply")
		world.expectToken(rejectedFirst, "newer-token", "after both")

		postedFirst := world.ground.newClient(1)
		read = world.posted(postedFirst, "rejected-token")
		world.hold(postedFirst, 1)
		post = world.post(postedFirst, "newer-token", nil)
		postHeld = world.awaitHeld(postedFirst)
		if world.await(world.reject(postedFirst, read.Rev)).tombstoned {
			t.Fatalf("%s: a 410 for the old revision tombstoned the newer registration", world.ground.name)
		}
		if row := testPushTokenRow(t, world.ctx, postedFirst); row.Token != "newer-token" || row.Rev != postHeld.row.Rev {
			t.Fatalf("%s: the 410 changed the row: %s", world.ground.name, row)
		}
		world.deliver(postHeld, post, pushservice.WakeTokenApplied)
		world.ground.expectNewest(postedFirst, "after the POST's apply")
		world.expectToken(postedFirst, "newer-token", "after both")
	})
}

// An apply held past 60 s answers EXPIRED and writes nothing; the writer's retry rereads t0 and the row
// and applies the newest. With api killed after the expired try, no retry, the row stands unprojected until
// the next pass's sweep applies it, inside 2 min of its commit, with no app launch.
func TestFs120HeldApplyExpiresThenRetryOrSweepLands(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		retried := world.ground.newClient(0)
		world.hold(retried, 1)
		post := world.post(retried, "held-token", nil)
		held := world.awaitHeld(retried)
		world.ground.advance(61 * time.Second)
		world.deliver(held, post, pushservice.WakeTokenApplied)
		if tries := world.tries(retried); tries != 2 {
			t.Fatalf("%s: %d tries, not the expired one and its retry", world.ground.name, tries)
		}
		world.ground.expectNewest(retried, "after the retry")

		// api killed: the expired try has no retry
		killed := world.ground.newClient(1)
		noRetry := DefaultSidecarPushTokenSettings()
		noRetry.ApplyRetryCount = 0
		world.hold(killed, 1)
		post = world.post(killed, "swept-token", noRetry)
		held = world.awaitHeld(killed)
		world.ground.advance(61 * time.Second)
		world.deliver(held, post, pushservice.WakeTokenExpired)
		if tok, tokrev := world.ground.tokenKeys(killed); tok != "" || tokrev != "" {
			t.Fatalf("%s: an expired apply wrote %q %q", world.ground.name, tok, tokrev)
		}
		world.ground.advance(59 * time.Second)
		if report := world.ground.pass(); report.SweptRowCount < 1 {
			t.Fatalf("%s: the sweep read no row: %+v", world.ground.name, report)
		}
		world.ground.expectNewest(killed, "after the next pass's sweep, 2 min after the commit")
		world.expectToken(killed, "swept-token", "after the sweep")
	})
}

// The reconcile pass before, during and after a held apply, in a slot whose sentinel is gone, as under a
// master that newly serves it. Before: the reconcile applies the committed row while its writer's apply is
// held, and the writer's try, over 60 s old by then, expires and its retry finds the row current. During:
// the apply lands between the reconcile's listing and its rows, or after its rows, and the reconcile's own
// apply or the writer's finds the row current. After: the reconcile lands the row first and the apply is
// current. tok equals the newest row after each.
func TestFs120ReconcileBeforeDuringAndAfterAHeldApply(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		ground := world.ground
		before, listed, applied, after := ground.newClient(0), ground.newClient(1), ground.newClient(2), ground.newClient(3)
		for n, clientId := range []server.Id{before, listed, applied, after} {
			world.posted(clientId, "standing-token")
			ground.loseSentinel(ground.slots[n])
		}
		// the pass that finds the slots; the first to reconcile them is 90 s on
		ground.pass()
		world.hold(before, 1)
		beforePost := world.post(before, "before-token", nil)
		beforeHeld := world.awaitHeld(before)
		ground.advance(90 * time.Second)

		world.hold(listed, 1)
		listedPost := world.post(listed, "listed-token", nil)
		listedHeld := world.awaitHeld(listed)
		world.hold(applied, 1)
		appliedPost := world.post(applied, "applied-token", nil)
		appliedHeld := world.awaitHeld(applied)
		world.hold(after, 1)
		afterPost := world.post(after, "after-token", nil)
		afterHeld := world.awaitHeld(after)

		during := map[string]bool{}
		pushTokenReconcileTestHook = func(stage string, slot int) {
			switch {
			case stage == "listed" && slot == ground.slots[1]:
				world.deliver(listedHeld, listedPost, pushservice.WakeTokenApplied)
				world.ground.expectNewest(listed, "an apply between the listing and the rows")
				during["listed"] = true
			case stage == "applied" && slot == ground.slots[2]:
				world.ground.expectNewest(applied, "the reconcile's rows, the writer's apply still held")
				world.deliver(appliedHeld, appliedPost, pushservice.WakeTokenCurrent)
				during["applied"] = true
			}
		}
		ground.pass()
		pushTokenReconcileTestHook = nil
		if len(during) != 2 {
			t.Fatalf("%s: the reconcile reached %v", ground.name, during)
		}
		for n, clientId := range []server.Id{before, listed, applied, after} {
			ground.expectNewest(clientId, "after the reconcile")
			if ground.sentinel(ground.slots[n]) == "" {
				t.Fatalf("%s: slot %d has no sentinel after its reconcile", ground.name, ground.slots[n])
			}
		}
		// before: begun over 60 s before the reconcile, its first try expires and its retry is current
		world.deliver(beforeHeld, beforePost, pushservice.WakeTokenCurrent)
		if tries := world.tries(before); tries != 3 {
			// the standing POST's one try, the expired one and its retry
			t.Fatalf("%s: %d tries of the apply the reconcile overtook", ground.name, tries)
		}
		// after: the reconcile landed its row first
		world.deliver(afterHeld, afterPost, pushservice.WakeTokenCurrent)
		for _, clientId := range []server.Id{before, listed, applied, after} {
			ground.expectNewest(clientId, "after every apply")
		}
	})
}

// A master restarted from a dump older than the newest change. The dump holds an older token of one
// client and the token another has since removed, both changes older than the sweep's 2 min, and a third
// client's change is committed just before the restart, its apply begun and held. After the restart tok is
// the dump's again, which WK5 allows until the reconcile: the pass that finds the slots reconciles nothing,
// nor does the next, 60 s on, their sweep landing only the change the last 2 min committed; the first pass
// at least 90 s after the finding reconciles, and tok then equals the rows, the removed token gone with no
// app launch. The apply begun before the restart is let go after the reconcile: it expires and does not
// land.
func TestFs120MasterRestartedFromAnOlderDump(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		ground := world.ground
		if !ground.restarts() {
			ground.logNoRestart("FS-120's restart from an older dump")
			return
		}
		changed, removed, begun := ground.newClient(0), ground.newClient(1), ground.newClient(2)
		world.posted(changed, "dumped-token")
		world.posted(removed, "since-removed-token")
		world.posted(begun, "standing-token")
		runIdBefore := ground.sentinel(ground.slots[0])
		dump := ground.dump()
		world.posted(changed, "newer-token")
		world.posted(removed, "")
		// both changes pass out of the sweep's overlap: only a reconcile lands them again
		ground.advance(3 * time.Minute)
		if report := ground.pass(); report.FoundSlotCount != 0 || report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: a pass before the restart found slots: %+v", ground.name, report)
		}
		noRetry := DefaultSidecarPushTokenSettings()
		noRetry.ApplyRetryCount = 0
		world.hold(begun, 1)
		begunPost := world.post(begun, "newest-token", noRetry)
		begunHeld := world.awaitHeld(begun)

		ground.restart(dump)
		world.expectToken(changed, "dumped-token", "after the restart from the older dump")
		world.expectToken(removed, "since-removed-token", "after the restart from the older dump")
		world.expectToken(begun, "standing-token", "after the restart from the older dump")
		if report := ground.pass(); report.FoundSlotCount < 3 || report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: the finding pass: %+v", ground.name, report)
		}
		ground.advance(60 * time.Second)
		if report := ground.pass(); report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: a pass 60 s after the finding reconciled: %+v", ground.name, report)
		}
		world.expectToken(changed, "dumped-token", "60 s after the finding")
		world.expectToken(removed, "since-removed-token", "60 s after the finding")
		// the change of the last 2 min is the sweep's, with no wait for the reconcile
		ground.expectNewest(begun, "after the sweep that followed the restart")
		ground.advance(30 * time.Second)
		if report := ground.pass(); report.ReconciledSlotCount < 3 {
			t.Fatalf("%s: the pass 90 s after the finding: %+v", ground.name, report)
		}
		for _, clientId := range []server.Id{changed, removed, begun} {
			ground.expectNewest(clientId, "after the reconcile")
		}
		world.expectToken(changed, "newer-token", "after the reconcile")
		world.expectToken(removed, "", "after the reconcile")
		world.expectToken(begun, "newest-token", "after the reconcile")
		if runId := ground.sentinel(ground.slots[0]); runId == "" || runId == runIdBefore {
			t.Fatalf("%s: the sentinel %q does not name the restarted master", ground.name, runId)
		}
		// begun before the restart, let go after the slot's reconcile: expired, landing nothing
		world.deliver(begunHeld, begunPost, pushservice.WakeTokenExpired)
		ground.advance(60 * time.Second)
		if report := ground.pass(); report.FoundSlotCount != 0 || report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: a later pass found slots again: %+v", ground.name, report)
		}
		for _, clientId := range []server.Id{changed, removed, begun} {
			ground.expectNewest(clientId, "after a later pass")
		}
	})
}

// A client whose first POST lands during a reconcile of its slot keeps its token: one between the
// listing and the rows, whose row the reconcile reads, and one after the rows, which the listing never
// held.
func TestFs120FirstPostDuringAReconcileKeepsItsToken(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		ground := world.ground
		slot := ground.slots[0]
		ground.loseSentinel(slot)
		ground.pass()
		ground.advance(90 * time.Second)
		firstPosts := map[string]server.Id{}
		pushTokenReconcileTestHook = func(stage string, hookSlot int) {
			if hookSlot != slot {
				return
			}
			clientId := ground.newClient(0)
			world.posted(clientId, "first-post-"+stage)
			firstPosts[stage] = clientId
		}
		report := ground.pass()
		pushTokenReconcileTestHook = nil
		if len(firstPosts) != 2 || report.ReconciledSlotCount < 1 {
			t.Fatalf("%s: %d first POSTs in a reconcile: %+v", ground.name, len(firstPosts), report)
		}
		for stage, clientId := range firstPosts {
			ground.expectNewest(clientId, "after the reconcile")
			world.expectToken(clientId, "first-post-"+stage, "after the reconcile")
		}
		if ground.sentinel(slot) == "" {
			t.Fatalf("%s: the slot has no sentinel after its reconcile", ground.name)
		}
	})
}

// A removed client's row and keys are gone 24 h after its removal: until then its tombstone stands, tok
// absent and tokrev at the tombstone's revision; the first pass past 24 h deletes tok and tokrev and then
// the row.
func TestFs120RemovedClientsRowAndKeysGoneAt24h(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		ground := world.ground
		clientId := ground.newClient(0)
		world.posted(clientId, "removed-clients-token")
		if !world.await(world.remove(clientId)).tombstoned {
			t.Fatalf("%s: the removal tombstoned nothing", ground.name)
		}
		tombstone := ground.expectNewest(clientId, "after the removal")
		if !tombstone.Tombstone() || tombstone.RemovedTime == nil {
			t.Fatalf("%s: the removal left %s", ground.name, tombstone)
		}
		ground.advance(24*time.Hour - 10*time.Minute)
		if report := ground.pass(); report.DeletedRowCount != 0 {
			t.Fatalf("%s: a row deleted under 24 h after its removal: %+v", ground.name, report)
		}
		if _, tokrev := ground.tokenKeys(clientId); tokrev != strconv.FormatInt(tombstone.Rev, 10) {
			t.Fatalf("%s: tokrev %q before 24 h", ground.name, tokrev)
		}
		ground.advance(20 * time.Minute)
		if report := ground.pass(); report.DeletedRowCount != 1 {
			t.Fatalf("%s: the pass past 24 h: %+v", ground.name, report)
		}
		if row := ground.expectNewest(clientId, "24 h after the removal"); row != nil {
			t.Fatalf("%s: the row stayed: %s", ground.name, row)
		}
	})
}

// Puts clientId's row back as a dump held it: token at rev, its update_time long before, as a restore
// of an older dump leaves it.
func fs120RestoreRow(t testing.TB, ctx context.Context, clientId server.Id, token string, rev int64) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE sidecar_push_token
				SET platform = $2, token = $3, rev = $4, update_time = update_time - interval '1 day'
				WHERE client_id = $1
			`,
			clientId,
			sidecar.PushTokenPlatformApns,
			token,
			rev,
		))
	})
}

// Deletes clientId's row, as a restore of a dump taken before the row existed leaves none.
func fs120LoseRow(t testing.TB, ctx context.Context, clientId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sidecar_push_token WHERE client_id = $1`, clientId))
	})
}

// FS-41's M8 step: push tokens re-register. After a site loss Redis holds nothing and Postgres is the
// dump's: one client's row older than its last registration, one's gone, one's as it was. Until its app
// launches a client has no tok. Each app's launch posts its token as ever: a changed one raises the
// restored row's revision, the lost row is made again, and a POST equal to the row commits nothing and
// applies it as it stands, so every client's tok equals its row again.
func TestFs41PushTokensReRegister(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		ground := world.ground
		older, lost, same := ground.newClient(0), ground.newClient(1), ground.newClient(2)
		dumped := world.posted(older, "dumped-token")
		world.posted(older, "current-token")
		world.posted(lost, "created-after-the-dump")
		world.posted(same, "unchanged-token")

		ground.flush()
		fs120RestoreRow(t, world.ctx, older, dumped.Token, dumped.Rev)
		fs120LoseRow(t, world.ctx, lost)
		if _, err := RaisePushTokenRevisions(world.ctx, nil); err != nil {
			t.Fatal(err)
		}
		for _, clientId := range []server.Id{older, lost, same} {
			world.expectToken(clientId, "", "after the site loss, before a launch")
		}

		reregister := func(clientId server.Id, token string, outcome PushTokenOutcome) {
			write := world.await(world.post(clientId, token, nil))
			if write.result.Outcome != outcome || write.result.Applied != pushservice.WakeTokenApplied {
				t.Fatalf("%s: re-registering %s: %s, applied %q", ground.name, token, write.result.Outcome, write.result.Applied)
			}
			ground.expectNewest(clientId, "after its launch")
			world.expectToken(clientId, token, "after its launch")
		}
		reregister(older, "current-token", PushTokenChanged)
		reregister(lost, "created-after-the-dump", PushTokenInserted)
		reregister(same, "unchanged-token", PushTokenUnchanged)
	})
}

// FS-42's M8 step. The Postgres primary is lost and Redis is not, so tokrev holds what the lost primary
// applied. A row the dump holds comes back below it: applied as restored it is stale, and tok keeps the
// lost primary's token. §19.10's raise lifts every row's revision past any the lost primary applied, and
// the next pass's sweep lands each row, so tok equals the restored row with no app launch, and a later POST
// lands above it. A client created after the dump has no row: re-registering a changed token makes one
// whose revision, its insert's millisecond, passes its lost row's, so its apply is not refused as stale.
func TestFs42PushTokenRevisionsRaisedPastTheLostPrimary(t *testing.T) {
	runFs120(t, func(world *fs120World) {
		ground := world.ground
		restored, created := ground.newClient(0), ground.newClient(1)
		dumped := world.posted(restored, "dumped-token")
		world.posted(restored, "lost-primary-token-1")
		lostNewest := world.posted(restored, "lost-primary-token-2")
		world.posted(created, "created-after-the-dump")
		lostCreated := world.posted(created, "changed-after-the-dump")

		// the restore: the dump's copy of one row, and no row of the client created since
		fs120RestoreRow(t, world.ctx, restored, dumped.Token, dumped.Rev)
		fs120LoseRow(t, world.ctx, created)
		// the negative twin: without the raise the restored row is refused as stale
		if _, applied := ApplyPushToken(world.ctx, restored, nil); applied != pushservice.WakeTokenStale {
			t.Fatalf("%s: the restored row applied before the raise answered %q", ground.name, applied)
		}
		world.expectToken(restored, "lost-primary-token-2", "before the raise")

		raised, err := RaisePushTokenRevisions(world.ctx, nil)
		if err != nil || raised != 1 {
			t.Fatalf("%s: raised %d rows: %v", ground.name, raised, err)
		}
		row := testPushTokenRow(t, world.ctx, restored)
		if row.Rev != dumped.Rev+PushTokenRestoreRevisionRaise || row.Rev <= lostNewest.Rev {
			t.Fatalf("%s: the raised row %s does not pass the lost primary's revision %d", ground.name, row, lostNewest.Rev)
		}
		ground.advance(60 * time.Second)
		if report := ground.pass(); report.SweptRowCount < 1 {
			t.Fatalf("%s: the sweep read no raised row: %+v", ground.name, report)
		}
		ground.expectNewest(restored, "after the raise and a pass")
		world.expectToken(restored, "dumped-token", "after the raise and a pass")
		world.posted(restored, "later-token")
		world.expectToken(restored, "later-token", "after a later POST")

		// created after the dump: its new row's revision passes its lost row's
		write := world.await(world.post(created, "re-registered-token", nil))
		if write.result.Outcome != PushTokenInserted || write.result.Applied != pushservice.WakeTokenApplied || write.result.Row.Rev <= lostCreated.Rev {
			t.Fatalf("%s: re-registering after the dump: %s at rev %d, the lost row's %d, applied %q", ground.name, write.result.Outcome, write.result.Row.Rev, lostCreated.Rev, write.result.Applied)
		}
		ground.expectNewest(created, "after re-registering")
		world.expectToken(created, "re-registered-token", "after re-registering")
	})
}
