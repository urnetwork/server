//go:build sidecar_disruption && sidecar_testhooks

// §22.8's Redis disruption row on T3 (TEST-M2-15; disruption_t3_test.go has the other rows and says what each shows):
// an evs world's subscriber through a Redis master's restart. The evs world drives a subscriber's rounds and hints only
// in the sidecar_testhooks build (sidecartest/fault's evs_world_hints.go), so the file builds only with it beside
// sidecar_disruption, as sidecar-t3.sh's nightly form runs it; without it the row fails at its first subscription. It
// needs the private Redis and the local stack's MinIO, and fails rather than skips without them.

package sidecart3

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
)

// An evs world on T3 through a Redis master's restart, FS-62's two clauses (EVS1-M2-41) with the real thing in place of
// LoseSoftState: every key of the test's database gone and every connection to it cut, not four families deleted. What
// is durable is in MinIO and stays: the reader's reads after it are served from the cluster's head, and the next
// commit's push sequence is above every earlier one. The reader's lease is gone, so the commit after the restart is
// hinted to no one; the hourly resync reads main fresh and catches it, the next round leases again, and the commit
// after that is hinted and read.
func disruptionRedisSchedule(net *sidecartest.Net) *fault.Schedule {
	world := fault.NewEvsWorld(net, nil)
	restart := NewRedisRestart()
	sequenceBefore := uint64(0)
	steps := []*fault.Step{
		{Actor: "writerA", Action: world.Setup},
		// §20.4's evs_subscribers_per_repo and evs_repos_per_client, which the lease route reads before it writes
		// (FS-12's step)
		{Actor: "admin", Action: world.SetQuotaLimits(map[string]int64{"evs_subscribers_per_repo": 16, "evs_repos_per_client": 500})},
		{Actor: "writerA", Action: world.Write("writerA", "a.txt", fault.EvsWorldContent("disruption a", 200))},
		{Actor: "reader", Action: world.Subscribe("reader")},
		{Actor: "check", Action: world.ExpectLease("reader", true)},
		// past the reader's 5 s reread, so its round reads for the hint alone
		{Advance: 6 * time.Second},
		{Actor: "writerB", Action: world.Write("writerB", "b.txt", fault.EvsWorldContent("disruption b", 200))},
		{Actor: "reader", Action: world.DeliverHints("reader")},
		{Actor: "check", Action: world.ExpectHinted("reader", "b.txt", true)},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectChange("reader", "b.txt")},
		{Actor: "reader", Action: world.VerifyCached("reader", "a.txt", "b.txt")},
		{Actor: "check", Action: world.Expect("the push sequence stands", func(ctx context.Context) error {
			sequence, err := world.PushSequence(ctx)
			if err != nil {
				return err
			}
			if sequence == 0 {
				return errors.New("no push sequence after two commits")
			}
			sequenceBefore = sequence
			return nil
		})},
		{Advance: time.Second},
		{Actor: "ops", Action: func(ctx context.Context, net *sidecartest.Net) error {
			if err := restart.Disrupt(ctx); err != nil {
				return err
			}
			if restart.KilledCount() == 0 {
				return errors.New("the restart cut no connection of the test's database")
			}
			return nil
		}},
		{Actor: "check", Action: world.ExpectLease("reader", false)},
		{Actor: "reader", Action: world.VerifyCached("reader", "a.txt", "b.txt")},
		{Actor: "writerA", Action: world.Write("writerA", "c.txt", fault.EvsWorldContent("disruption c", 200))},
		{Actor: "check", Action: world.Expect("the next sequence is above every earlier one", func(ctx context.Context) error {
			sequence, err := world.PushSequence(ctx)
			if err != nil {
				return err
			}
			if sequence <= sequenceBefore {
				return fmt.Errorf("the push sequence fell from %d to %d", sequenceBefore, sequence)
			}
			return nil
		})},
		{Actor: "reader", Action: world.DeliverHints("reader")},
		{Actor: "check", Action: world.ExpectHinted("reader", "c.txt", false)},
		// the hourly resync reads main fresh and catches c.txt, and the next round leases again
		{Advance: time.Hour},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectChange("reader", "c.txt")},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectLease("reader", true)},
		{Actor: "writerB", Action: world.Write("writerB", "d.txt", fault.EvsWorldContent("disruption d", 200))},
		{Actor: "reader", Action: world.DeliverHints("reader")},
		{Actor: "check", Action: world.ExpectHinted("reader", "d.txt", true)},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectChange("reader", "d.txt")},
		{Actor: "reader", Action: world.Verify("reader", "a.txt", "b.txt", "c.txt", "d.txt")},
	}
	return &fault.Schedule{Id: "disruption redis restart", Steps: steps, Check: world.Checks(), Tiers: []fault.Tier{fault.TierT3}}
}

func TestT3RedisMasterRestartLosesOnlyRebuildableState(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		Run(tb, disruptionRedisSchedule, nil)
	})
}
