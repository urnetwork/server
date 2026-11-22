//go:build sidecar_localstack

// T3 of KEYS-M2-27's and KEYS-M2-28's schedules (§22.9, D-TEST-2, C-18: one builder, a T2 and a T3 test): the
// builders sidecartest's T2 tests run, on server's real stores, server.FileClusters over the local stack's MinIO in
// buckets of this test's own and sidecarhost.Redis on the private test Redis that test-env.sh attests. gkm's group
// client on the real MLS subset drives gkmservice's engine through each: FS-15's wrapped rotation, its race for one
// generation, its lost and killed claims, a commit below its predecessor's generation, a delivery to a non-holder, a
// claim at a stale tip, a last-resort KeyPackage in two groups and the kills around its wrap, gen/ and epoch/ writes,
// FS-24's hints with H1 and H2, FS-62's acks lost with Redis, FS-23's depositor and FS-102's key legs, a copied
// Delegation bundle after a compromise and after a removal, and KEYS-M2-29's FS-103 (late delivery identity across key
// replacement, an older key's object aging out, a same-generation RecoveryRebind) and FS-106 (35 days of key upkeep
// under r, a parked device's ReaderKey), KEYS-M6-03's FS-87 key legs with S4b's and S3b's key parts in the team
// directory's group (a MemberRevoke reaching leaves and wraps, a MemberAdmit adding and wrapping only readers), and
// KEYS-M7-01's FS-23 mailbox case (a tag:mail receiver's drop keys and a removal, and the drop key's renewal and lapse
// with the readers offline, owner decision 357); its seats row at 1,000 leaves runs on T2 alone. Like the other T3
// tests here it carries the sidecar_localstack tag and fails, rather than skips, when the stack is down.
//
// The schedules run on the draft MLS wire, which gkmschedule's worlds keep on as legacy fixtures while v0 keeps it
// dark (owner decision 610). A case catalog.yml flags store_clock, FS-103's and FS-106's, ages stored objects across
// its clock's moves, so it runs with T3's store clock (server/sidecart3/clock.go; QD-264): MinIO stamps each object's
// LastModified on real time, and without the translation an object written after an Advance reads as old as the
// Advance, as FS-103's newer delivery did, aging the older one out a day early. Every other case runs on the stores'
// own times, its fake clock starting at them.

package gkm

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
	"github.com/urnetwork/sidecar/sidecartest/gkmschedule"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// Runs a schedule of the catalog's case caseName of rowId, empty for the row's main part, on a world whose engine and
// reconciler run on the local stack's clusters and the test Redis, with the store clock as the case's flag says.
func runFsKeysT3(t *testing.T, rowId string, caseName string, schedule func(t gkmschedule.FsKeysT, newWorld gkmschedule.FsKeysNewWorld)) {
	testCase := fault.DefaultCatalog().Case(rowId, caseName)
	if testCase == nil {
		t.Fatalf("catalog.yml has no case %q of %s", caseName, rowId)
	}
	clusters := gkmStackClusters(t)
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		redis := sidecarhost.NewRedis(nil)
		// each world's store clock; a schedule builds its worlds on its own goroutine, so this takes no lock
		storeClocks := []*sidecart3.StoreClock{}
		schedule(tb, func(kind protocol.RepoKind) *gkmschedule.FsKeysWorld {
			// the fake clock starts at the stores' own time, which a case without the store clock reads
			clock := sidecartest.NewFakeClock(time.Now())
			fakeHost := sidecartest.NewFakeHost(tb.Context(), clock, nil, nil, nil)
			var blobs sidecar.BlobStore = clusters
			if testCase.StoreClock {
				storeClock, storeClockBlobs, err := sidecart3.NewWorldStoreClock(clock, sidecar.RealClock{}, sidecart3.DefaultStoreClockSettings(), clusters, nil)
				if err != nil {
					tb.Fatal(err)
				}
				storeClocks = append(storeClocks, storeClock)
				blobs = storeClockBlobs
			}
			return gkmschedule.NewFsKeysWorldOn(tb, kind, clock, fakeHost, func(host *sidecar.ServiceHost) {
				host.Blobs = blobs
				host.Redis = redis
			})
		})
		// a read the store clock could not translate aged on real time, which the case's pass cannot rest on
		for _, storeClock := range storeClocks {
			for _, read := range storeClock.Untranslated() {
				tb.Errorf("the store clock read an object no write it recorded made: %s", read)
			}
		}
	})
}

func TestFsKeysT3AWrappedRotationLeavesTheRemovedLeafNothing(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleWrappedRotation)
}

func TestFsKeysT3ALeafAndAWrappedRotationRaceForOneGeneration(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleRotationRace)
}

func TestFsKeysT3ALostAnswerAndAKilledClaimEachEndInOneGeneration(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleLostAndKilledClaims)
}

func TestFsKeysT3AnIdleWriterIsToldPendingAtAnUnchangedEpoch(t *testing.T) {
	runFsKeysT3(t, "FS-24", "", gkmschedule.FsKeysScheduleIdleWriterHint)
}

func TestFsKeysT3ARotationWithoutACommitRaisesTheGroupTopic(t *testing.T) {
	runFsKeysT3(t, "FS-24", "", gkmschedule.FsKeysScheduleRotationGroupHint)
}

func TestFsKeysT3ALateAskIsToldWrapPendingAndAnswered(t *testing.T) {
	runFsKeysT3(t, "FS-24", "", gkmschedule.FsKeysScheduleLateAsk)
}

func TestFsKeysT3ACommitBelowItsPredecessorsGenerationIsRefused(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleCommitBelowItsPredecessor)
}

func TestFsKeysT3LostAcksDeleteNoEpochForThirtyDays(t *testing.T) {
	runFsKeysT3(t, "FS-62", "", gkmschedule.FsKeysScheduleLostAcks)
}

func TestFsKeysT3ADepositOnlyPrincipalFollowsTheDropKeyAndReadsNothing(t *testing.T) {
	runFsKeysT3(t, "FS-23", "", gkmschedule.FsKeysScheduleDepositor)
}

func TestFsKeysT3ADepositorRefusesAnOlderGenRotateAfterARemoval(t *testing.T) {
	runFsKeysT3(t, "FS-23", "", gkmschedule.FsKeysScheduleDepositorReplay)
}

func TestFsKeysT3MailboxTheReceiverFollowsTheDropKeyAndReadsNothing(t *testing.T) {
	runFsKeysT3(t, "FS-23", "mailbox", gkmschedule.FsKeysScheduleMailboxReceiver)
}

func TestFsKeysT3MailboxTheReceiverRefusesAnUncoveredDropKeyAfterARemoval(t *testing.T) {
	runFsKeysT3(t, "FS-23", "mailbox", gkmschedule.FsKeysScheduleMailboxReceiverAfterARemoval)
}

func TestFsKeysT3MailboxAWriterBackRenewsTheDropKeyBeforeItLapses(t *testing.T) {
	runFsKeysT3(t, "FS-23", "mailbox", gkmschedule.FsKeysScheduleMailboxRenewal)
}

func TestFsKeysT3MailboxReadersOfflinePastTheDropKeysLife(t *testing.T) {
	runFsKeysT3(t, "FS-23", "mailbox", gkmschedule.FsKeysScheduleMailboxReadersOffline)
}

func TestFsKeysT3ACopiedBundleAfterACompromiseOpensNothingNew(t *testing.T) {
	runFsKeysT3(t, "FS-102", "", gkmschedule.FsKeysScheduleCopiedBundleCompromise)
}

func TestFsKeysT3ACopiedBundleAfterARemovalAdmitsOnlyUncoveredReaders(t *testing.T) {
	runFsKeysT3(t, "FS-102", "", gkmschedule.FsKeysScheduleCopiedBundleRemoved)
}

func TestFsKeysT3ADeliveryToANonHolderMakesMembersAlarmAndRotate(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleDeliveryToANonHolder)
}

func TestFsKeysT3AClaimAtAStaleTipIsRefused(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleStaleTipClaimRefused)
}

func TestFsKeysT3ALastResortKeyPackageServesTwoGroups(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleLastResortInTwoGroups)
}

func TestFsKeysT3AKillAroundTheWrapGenAndEpochWritesResumes(t *testing.T) {
	runFsKeysT3(t, "FS-15", "", gkmschedule.FsKeysScheduleKillsBetweenWrites)
}

func TestFsKeysT3PendingRepeatsWhileADroppedLeafStandsAndStopsPastIt(t *testing.T) {
	runFsKeysT3(t, "FS-24", "", gkmschedule.FsKeysScheduleDroppedHolderHint)
}

func TestFsKeysT3AWaitingLeafsWantRaisesWrapPending(t *testing.T) {
	runFsKeysT3(t, "FS-24", "", gkmschedule.FsKeysScheduleWaitingLeafWant)
}

func TestFsKeysT3ALateDeliveryKeepsItsIdentityAcrossKeyReplacement(t *testing.T) {
	runFsKeysT3(t, "FS-103", "", gkmschedule.FsKeysScheduleLateDeliveryIdentity)
}

func TestFsKeysT3AnOlderKeysObjectAgesOutThirtyDaysPastTheNewerOnes(t *testing.T) {
	runFsKeysT3(t, "FS-103", "", gkmschedule.FsKeysScheduleOlderKeyDeliveryAgesOut)
}

func TestFsKeysT3ASameGenerationRebindLeavesBothBindingsAtAKill(t *testing.T) {
	runFsKeysT3(t, "FS-103", "", gkmschedule.FsKeysScheduleSameGenerationRebind)
}

func TestFsKeysT3UpkeepUnderRHoldsThroughTheRetentionHorizon(t *testing.T) {
	runFsKeysT3(t, "FS-106", "", gkmschedule.FsKeysScheduleKeyUpkeepUnderR)
}

func TestFsKeysT3AParkedDevicesReaderKeyWaitsForItsLift(t *testing.T) {
	runFsKeysT3(t, "FS-106", "", gkmschedule.FsKeysScheduleParkedReaderKeyWaits)
}

func TestFsKeysT3AMemberRevokeReachesTheDirectoryGroupsLeavesAndWraps(t *testing.T) {
	runFsKeysT3(t, "FS-87", "", gkmschedule.FsKeysScheduleDirectoryMemberRemoval)
}

func TestFsKeysT3AMemberAdmitAddsAndWrapsOnlyTheDirectorysReaders(t *testing.T) {
	runFsKeysT3(t, "FS-87", "", gkmschedule.FsKeysScheduleDirectoryMemberAdd)
}
