// These controls use the real store and inherited context, with private queue
// files and attempt-local clocks. No native transport or external resource runs.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The diagnostic and native guard clocks remain explicit and independent.
func sharedDetailContext(t *testing.T) (context.Context, *pgSampleTransportObservation, *time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 32*time.Second)
	t.Cleanup(cancel)
	ctx, observation := observePgSampleTransport(ctx)
	now := observation.localOrigin
	observation.localClock = func() time.Time { return now }
	observation.startClock = func() time.Time { return now }
	return ctx, observation, &now
}

// This is the missing discriminator: equal overall delay can be validation or
// publication rather than a capacity wait; neither may be fabricated as wait.
func TestPgSharedDetailComposedValidationAndPublication(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	ctx, observation, now := sharedDetailContext(t)
	if observation.receipt.SharedAdmissionDetail != nil || observation.receipt.LocalPhaseTiming.ClockStatus != "ok" || observation.localLastNs != 0 {
		t.Fatal("control precondition: fresh observation and valid zero-origin clock required")
	}
	origin := *now
	validationCalls, publicationCalls := 0, 0
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus {
		validationCalls++
		*now = now.Add(6 * time.Second)
		return sshAdmissionOwnerLive
	}
	store.afterPublish = func() error { publicationCalls++; *now = now.Add(5 * time.Second); return nil }
	release, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if validationCalls != 1 || publicationCalls != 1 || now.Sub(origin) != 11*time.Second || observation.receipt.LocalPhaseTiming.ClockStatus != "ok" {
		t.Fatal("control precondition: successful real store path and exact diagnostic clock not reached")
	}
	current := readSshRecoveryState(t, store)
	if len(current.Requests) != 1 || !current.Requests[0].Active {
		t.Fatal("control precondition: real store did not publish an active reservation")
	}
	detail := observation.receipt.SharedAdmissionDetail
	if detail == nil {
		t.Fatal("missing shared-admission detail after successful real store acquisition")
	}
	if detail == nil || detail.Validation == nil || detail.Validation.TotalNs == nil || *detail.Validation.TotalNs != int64(6*time.Second) || detail.Publication == nil || detail.Publication.TotalNs == nil || *detail.Publication.TotalNs != int64(5*time.Second) || detail.EligibleWait != nil || detail.Grants != 1 || detail.Decisions != 1 || detail.Outcome != "returned_grant" || detail.ClaimDisposition != "returned_reservation" {
		t.Fatal("composed acquisition lost the owning delay or invented capacity wait")
	}
	if !errors.Is(observation.admitNativeStart(ctx), errPgSampleStartBudget) {
		t.Fatal("diagnostics changed the unchanged 22-second refusal")
	}
	before, _ := json.Marshal(detail)
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return sshAdmissionOwnerLive }
	store.afterPublish = nil
	if err := release(); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(detail)
	if string(before) != string(after) {
		t.Fatal("release rewrote acquisition evidence")
	}
	for _, secret := range []string{"a.example", state.Watcher.Unit, state.Watcher.InvocationId, state.BootId, store.directory} {
		if strings.Contains(string(after), secret) {
			t.Fatal("detail leaked private queue identity")
		}
	}
	if len(after) > 4096 {
		t.Fatal("fixed detail exceeds its encoding bound")
	}
}

// A held flock is measured independently of state/owner validation.
func TestPgSharedDetailComposedFlockAndRecovery(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	ctx, observation, now := sharedDetailContext(t)
	claim, err := store.trackClaim(strings.Repeat("f", 32), state.Watcher)
	if err != nil {
		t.Fatal(err)
	}
	claim.ready = true
	lock, err := os.OpenFile(filepath.Join(store.directory, "queue.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	store.lockWait = func(context.Context) error {
		*now = now.Add(3 * time.Second)
		return unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	}
	release, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	detail := observation.receipt.SharedAdmissionDetail
	if detail.Recovery == nil || detail.Recovery.TotalNs == nil || *detail.Recovery.TotalNs != int64(3*time.Second) || detail.Flock == nil || detail.Flock.Calls != 2 || detail.Flock.TotalNs == nil || *detail.Flock.TotalNs != int64(3*time.Second) || len(store.claims.records) != 1 {
		t.Fatal("recovery did not retain nested flock attribution or remove only ready claim")
	}
}

// A real non-grant reaches the existing wait; cancellation retains its identity
// and cleans only the unstarted claim. Four existing active claims stay owned.
func TestPgSharedDetailComposedNonGrantCancellation(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	var releases []func() error
	defer func() {
		for _, release := range releases {
			if err := release(); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, host := range state.Hosts[:4] {
		release, err := store.acquire(t.Context(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{host})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, observation, _ := sharedDetailContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	store.afterPublish = func() error { cancel(); return nil }
	_, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{state.Hosts[4]})
	store.afterPublish = nil
	detail := observation.receipt.SharedAdmissionDetail
	if !errors.Is(err, context.Canceled) || detail.Decisions != 1 || detail.Grants != 0 || detail.EligibleWait == nil || detail.EligibleWait.Errors != 1 || detail.Outcome != "canceled" || detail.ClaimDisposition != "cleanup_succeeded" {
		t.Fatal("non-grant cancellation lost wait/cleanup authority")
	}
	current := readSshRecoveryState(t, store)
	if len(current.Requests) != 4 {
		t.Fatal("cancellation changed existing capacity accounting")
	}
}

// Corrupt queue state is an unavailable validation result, never healthy zero.
func TestPgSharedDetailComposedInvalidState(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	if err := os.WriteFile(filepath.Join(store.directory, "queue.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, observation, _ := sharedDetailContext(t)
	_, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	detail := observation.receipt.SharedAdmissionDetail
	if err == nil || detail.Validation == nil || detail.Validation.Errors != 1 || detail.Publication != nil || detail.Decisions != 0 || detail.Outcome != "error" || detail.ClaimDisposition != "forgot_unpublished" {
		t.Fatal("invalid queue fabricated publication or grant")
	}
}

// Legacy/injected paths have no internal measurement. A regressing diagnostic
// clock withholds totals and never changes the queue outcome or native guard.
func TestPgSharedDetailUnknownAndInvalidClock(t *testing.T) {
	ctx, observation, now := sharedDetailContext(t)
	if observation.receipt.SharedAdmissionDetail != nil {
		t.Fatal("unentered store invented detail")
	}
	if beginPgSharedAdmission(context.Background()) != nil {
		t.Fatal("non-PG call acquired a recorder")
	}
	detail := beginPgSharedAdmission(ctx)
	finish := beginPgSharedPhase(ctx, pgSharedValidation)
	*now = now.Add(-time.Second)
	finish(nil)
	finish(errors.New("second finish must be ignored"))
	if observation.receipt.LocalPhaseTiming.ClockStatus != "invalid" || detail.Validation.TotalNs != nil || detail.Validation.Errors != 0 {
		t.Fatal("invalid clock fabricated a valid total")
	}
}

// The receipt path must retain the real store detail through the sampler and
// native refusal, not merely through a fake admission backend or helper call.
func TestPgSharedDetailSamplerReceiptUsesActualStore(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	store, state := newSyntheticSshAdmissionStore(t)
	if err := store.update(t.Context(), func(current *sshAdmissionState, _ int64) error { current.Hosts = []string{"pg-1"}; return nil }); err != nil {
		t.Fatal(err)
	}
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus {
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		return sshAdmissionOwnerLive
	}
	fixture.env.cfg.sharedSshAdmission = &sharedSshAdmission{store: store, owner: state.Watcher}
	control, _ := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	requirePgLocalRefusal(t, fixture, control)
	_, raw := pgTransportReadControlReceipt(t, fixture.env)
	var wire struct {
		Transport struct {
			Detail *pgSampleSharedAdmissionDetail `json:"shared_admission_detail"`
		} `json:"transport_observation"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	detail := wire.Transport.Detail
	if detail == nil || detail.Validation == nil || detail.Validation.TotalNs == nil || *detail.Validation.TotalNs != int64(23*time.Second) || detail.EligibleWait != nil || detail.Outcome != "returned_grant" {
		t.Fatal("actual sampler receipt lost validation delay or invented queue wait")
	}
	if current := readSshRecoveryState(t, store); len(current.Requests) != 0 {
		t.Fatal("refused native start retained an already released grant")
	}
}

// A published in-memory grant is not an acquisition return when publication
// acknowledgement fails. Preserve both the error and bounded cleanup result.
func TestPgSharedDetailPublicationErrorDoesNotInventReturnedGrant(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	ctx, observation, _ := sharedDetailContext(t)
	publicationErr := errors.New("synthetic acknowledgement failure")
	store.afterPublish = func() error { return publicationErr }
	_, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	store.afterPublish = nil
	detail := observation.receipt.SharedAdmissionDetail
	if !errors.Is(err, publicationErr) || detail.Outcome != "error" || detail.Grants != 1 || detail.PublicationDurable != 1 || detail.Publication.Errors != 1 || detail.ClaimDisposition != "cleanup_error" {
		t.Fatal("publication ambiguity became a returned reservation")
	}
	// The failed cleanup remains retained locally even when its durable removal
	// completed; the next ordinary recovery must resolve it using existing code.
	release, err := store.acquire(t.Context(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
