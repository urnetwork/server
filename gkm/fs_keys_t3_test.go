//go:build sidecar_localstack

// T3 of KEYS-M2-27's and KEYS-M2-28's schedules (§22.9, D-TEST-2, C-18: one builder, a T2 and a T3 test): the
// builders sidecartest's T2 tests run, on server's real stores, server.FileClusters over the local stack's MinIO in
// buckets of this test's own and sidecarhost.Redis on the private test Redis that test-env.sh attests. gkm's group
// client on the real MLS subset drives gkmservice's engine through each: FS-15's wrapped rotation, its race for one
// generation, its lost and killed claims, a commit below its predecessor's generation, a delivery to a non-holder, a
// claim at a stale tip, a last-resort KeyPackage in two groups and the kills around its wrap, gen/ and epoch/ writes,
// FS-24's hints with H1 and H2, FS-62's acks lost with Redis, FS-23's depositor and FS-102's key legs, a copied
// Delegation bundle after a compromise and after a removal. Like the other T3 tests here it carries the sidecar_localstack tag and
// fails, rather than skips, when the stack is down.

package gkm

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/gkmschedule"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// Runs a schedule on a world whose engine and reconciler run on the local stack's clusters and the test Redis.
func runFsKeysT3(t *testing.T, schedule func(t gkmschedule.FsKeysT, newWorld gkmschedule.FsKeysNewWorld)) {
	clusters := gkmStackClusters(t)
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		redis := sidecarhost.NewRedis(nil)
		schedule(tb, func(kind protocol.RepoKind) *gkmschedule.FsKeysWorld {
			// the fake clock starts at the stores' own time: MinIO stamps each object's LastModified, which the
			// ReaderKey's daily rule and epoch retention read
			clock := sidecartest.NewFakeClock(time.Now())
			fakeHost := sidecartest.NewFakeHost(tb.Context(), clock, nil, nil, nil)
			return gkmschedule.NewFsKeysWorldOn(tb, kind, clock, fakeHost, func(host *sidecar.ServiceHost) {
				host.Blobs = clusters
				host.Redis = redis
			})
		})
	})
}

func TestFsKeysT3AWrappedRotationLeavesTheRemovedLeafNothing(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleWrappedRotation)
}

func TestFsKeysT3ALeafAndAWrappedRotationRaceForOneGeneration(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleRotationRace)
}

func TestFsKeysT3ALostAnswerAndAKilledClaimEachEndInOneGeneration(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleLostAndKilledClaims)
}

func TestFsKeysT3AnIdleWriterIsToldPendingAtAnUnchangedEpoch(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleIdleWriterHint)
}

func TestFsKeysT3ARotationWithoutACommitRaisesTheGroupTopic(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleRotationGroupHint)
}

func TestFsKeysT3ALateAskIsToldWrapPendingAndAnswered(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleLateAsk)
}

func TestFsKeysT3ACommitBelowItsPredecessorsGenerationIsRefused(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleCommitBelowItsPredecessor)
}

func TestFsKeysT3LostAcksDeleteNoEpochForThirtyDays(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleLostAcks)
}

func TestFsKeysT3ADepositOnlyPrincipalFollowsTheDropKeyAndReadsNothing(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleDepositor)
}

func TestFsKeysT3ADepositorRefusesAnOlderGenRotateAfterARemoval(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleDepositorReplay)
}

func TestFsKeysT3ACopiedBundleAfterACompromiseOpensNothingNew(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleCopiedBundleCompromise)
}

func TestFsKeysT3ACopiedBundleAfterARemovalAdmitsOnlyUncoveredReaders(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleCopiedBundleRemoved)
}

func TestFsKeysT3ADeliveryToANonHolderMakesMembersAlarmAndRotate(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleDeliveryToANonHolder)
}

func TestFsKeysT3AClaimAtAStaleTipIsRefused(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleStaleTipClaimRefused)
}

func TestFsKeysT3ALastResortKeyPackageServesTwoGroups(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleLastResortInTwoGroups)
}

func TestFsKeysT3AKillAroundTheWrapGenAndEpochWritesResumes(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleKillsBetweenWrites)
}

func TestFsKeysT3PendingRepeatsWhileADroppedLeafStandsAndStopsPastIt(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleDroppedHolderHint)
}

func TestFsKeysT3AWaitingLeafsWantRaisesWrapPending(t *testing.T) {
	runFsKeysT3(t, gkmschedule.FsKeysScheduleWaitingLeafWant)
}
