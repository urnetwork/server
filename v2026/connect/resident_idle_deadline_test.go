package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A real profile query spends part of the resident's existing idle allowance
// behind PostgreSQL's table lock. Once the query returns, the watcher must use
// the remaining allowance, not start an additional whole allowance. This
// exercises the actual nomination path with no replacement clock or worker.
func TestResidentIdleDeadlineAfterBlockedConstruction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		const allowance = 2 * time.Second
		exchange.settings.ExchangeResidentTtl = allowance
		exchange.settings.StreamHopsPollInterval = time.Hour
		defer func() {
			exchange.Close()
			if !exchange.WaitForIdle(ctx) {
				t.Error("resident ownership did not join")
			}
		}()
		release := holdResidentProfileTable(t, ctx)
		defer release()
		captured := make(chan *Resident, 1)
		exchange.beforeResidentProfileForTest = func(r *Resident) { captured <- r }
		admitted := make(chan bool, 1)
		go func() {
			admitted <- exchange.NominateLocalResidentWithContext(ctx, server.NewId(), server.NewId(), nil)
		}()
		var resident *Resident
		select {
		case resident = <-captured:
		case <-ctx.Done():
			t.Fatal("resident did not reach actual profile query")
		}
		waitForBlockedResidentProfiles(t, ctx, 1)
		lastActivity := time.Unix(0, resident.lastActivityNanos.Load())
		deadline := lastActivity.Add(allowance)
		time.Sleep(time.Until(lastActivity.Add(3 * allowance / 4)))
		// Keep the distinct Redis nomination lease live, just as another
		// transport lookup may do. Its expiry must not mask the idle watcher.
		registration := model.GetResidentForClientWithInstance(ctx, resident.clientId, resident.instanceId, allowance)
		if registration == nil || registration.ResidentId != resident.residentId {
			t.Fatal("fixture lost the independent resident registration before release")
		}
		release()
		select {
		case ok := <-admitted:
			if !ok {
				t.Fatal("healthy released nomination was refused")
			}
		case <-ctx.Done():
			t.Fatal("released nomination did not join")
		}
		if remaining := time.Until(deadline); remaining < allowance/10 {
			t.Fatalf("fixture did not leave a measurable existing allowance: %s", remaining)
		}
		if resident.IsDone() {
			t.Fatal("resident retired before its existing idle allowance")
		}
		select {
		case <-resident.Done():
			elapsed := time.Since(lastActivity)
			if elapsed < allowance {
				t.Fatalf("resident retired early: %s < %s", elapsed, allowance)
			}
			t.Logf("actual blocked-profile resident retired after %s, existing allowance %s", elapsed, allowance)
		case <-time.After(time.Until(deadline.Add(allowance / 4))):
			t.Fatal("resident watcher granted another complete idle allowance after the blocked profile returned")
		case <-ctx.Done():
			t.Fatal("idle owner control exceeded its deadline")
		}
	})
}

// Activity after nomination extends the same allowance. The watcher must not
// cancel on its earlier timer wake, and must then wait only until the revised
// deadline. Neither the configured allowance nor control-tail ownership moves.
func TestResidentIdleActivityKeepsExistingAllowance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		const allowance = 2 * time.Second
		exchange.settings.ExchangeResidentTtl = allowance
		exchange.settings.StreamHopsPollInterval = time.Hour
		defer func() {
			exchange.Close()
			if !exchange.WaitForIdle(ctx) {
				t.Error("resident ownership did not join")
			}
		}()
		clientID := server.NewId()
		if !exchange.NominateLocalResidentWithContext(ctx, clientID, server.NewId(), nil) {
			t.Fatal("nomination refused")
		}
		exchange.stateLock.Lock()
		resident := exchange.residents[clientID]
		exchange.stateLock.Unlock()
		initialActivity := time.Unix(0, resident.lastActivityNanos.Load())
		time.Sleep(time.Until(initialActivity.Add(allowance / 2)))
		if !resident.UpdateActivity() {
			t.Fatal("live resident refused activity")
		}
		lastActivity := time.Unix(0, resident.lastActivityNanos.Load())
		deadline := lastActivity.Add(allowance)
		select {
		case <-resident.Done():
			t.Fatal("resident ignored activity and retired at its old deadline")
		case <-time.After(time.Until(deadline.Add(-allowance / 4))):
		case <-ctx.Done():
			t.Fatal("activity control exceeded its deadline")
		}
		select {
		case <-resident.Done():
			if elapsed := time.Since(lastActivity); elapsed < allowance {
				t.Fatalf("activity allowance shortened: %s", elapsed)
			}
		case <-time.After(time.Until(deadline.Add(allowance / 4))):
			t.Fatal("resident watcher rounded the renewed idle deadline up to another complete allowance")
		case <-ctx.Done():
			t.Fatal("renewed idle owner did not complete")
		}
	})
}
